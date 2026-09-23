package openai

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"nerdola.dev/x/paula/internal/runners/api"
)

// errNoEvents says an answer arrived with nothing in it, which the caller
// reports with whatever the body held.
var errNoEvents = errors.New("the answer carried no events")

// errCallback says whoever asked for the reply stopped taking it, so what is
// left of the stream is not read.
var errCallback = errors.New("the answer was not taken")

// lineLimit is the longest SSE line Paula reads. A chunk of a reply is a few
// words, so a line this long is a host that is not sending an answer, and the
// reply it belongs to fails rather than being read into memory without end.
const lineLimit = 4 << 20

type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content   string          `json:"content"`
			ToolCalls []toolCallDelta `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *streamUsage `json:"usage"`
	Error *struct {
		Code    any    `json:"code"`
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// toolCallDelta is a piece of a tool call. A call arrives over several chunks,
// all under the index it has in the reply: its id and name in the first, and
// its arguments a fragment at a time.
type toolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// calls puts the pieces of the tool calls back together.
type calls struct {
	byIndex map[int]*api.ToolCall
	order   []int
}

// add takes one piece. The first id and name a call is given are the ones it
// keeps, and its arguments are every fragment in the order they came.
func (c *calls) add(d toolCallDelta) {
	if c.byIndex == nil {
		c.byIndex = map[int]*api.ToolCall{}
	}
	call, ok := c.byIndex[d.Index]
	if !ok {
		call = &api.ToolCall{}
		c.byIndex[d.Index] = call
		c.order = append(c.order, d.Index)
	}
	if call.ID == "" {
		call.ID = d.ID
	}
	if call.Name == "" {
		call.Name = d.Function.Name
	}
	call.Arguments += d.Function.Arguments
}

// whole is every call, in the order of their indexes.
func (c *calls) whole() []api.ToolCall {
	if len(c.order) == 0 {
		return nil
	}
	slices.Sort(c.order)
	out := make([]api.ToolCall, len(c.order))
	for i, index := range c.order {
		out[i] = *c.byIndex[index]
	}
	return out
}

type streamUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

func (c *Client) stream(r io.Reader, res *api.Result, fn func(api.Chunk) error) error {
	var (
		reasoning strings.Builder
		asked     calls
		arrived   bool
	)

	err := events(r, func(data []byte) error {
		arrived = true
		var chunk streamChunk
		if err := json.Unmarshal(data, &chunk); err != nil {
			return err
		}
		if e := chunk.Error; e != nil {
			// An error written where a chunk belongs is the shape an error
			// with a status of its own has, so it is read the same way, and
			// the code it names is the status it would have had.
			status, _ := strconv.Atoi(code(e.Code))
			if err := c.Hooks.Error(status, data); err != nil {
				return err
			}
			return &api.APIError{Status: status, Code: code(e.Code), Type: e.Type, Message: e.Message}
		}

		text, err := c.Hooks.Chunk(data, res)
		if err != nil {
			return err
		}
		if text != "" {
			reasoning.WriteString(text)
			if err := fn(api.Chunk{Kind: api.ChunkReasoning, Text: text}); err != nil {
				return fmt.Errorf("%w: %w", errCallback, err)
			}
		}

		if u := chunk.Usage; u != nil {
			res.Usage.PromptTokens = u.PromptTokens
			res.Usage.CompletionTokens = u.CompletionTokens
			if d := u.PromptTokensDetails; d != nil {
				res.Usage.CachedTokens = d.CachedTokens
			}
			if d := u.CompletionTokensDetails; d != nil {
				res.Usage.ReasoningTokens = d.ReasoningTokens
			}
		}

		for _, ch := range chunk.Choices {
			if ch.FinishReason != "" {
				res.FinishReason = ch.FinishReason
			}
			if ch.Delta.Content != "" {
				if err := fn(api.Chunk{Kind: api.ChunkText, Text: ch.Delta.Content}); err != nil {
					return fmt.Errorf("%w: %w", errCallback, err)
				}
			}
			for _, d := range ch.Delta.ToolCalls {
				asked.add(d)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !arrived {
		return errNoEvents
	}

	res.Reasoning = reasoning.String()
	res.ToolCalls = asked.whole()
	return nil
}

// events reads server-sent events, skipping the comments both APIs send to
// keep a connection alive, and stops at the end of the stream.
func events(r io.Reader, fn func(data []byte) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), lineLimit)

	var data []string
	dispatch := func() error {
		if len(data) == 0 {
			return nil
		}
		joined := strings.Join(data, "\n")
		data = data[:0]
		switch strings.TrimSpace(joined) {
		case "":
			// A data line with nothing in it, which server-sent events allow.
			return nil
		case "[DONE]":
			return io.EOF
		}
		return fn([]byte(joined))
	}

	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		switch {
		case line == "":
			if err := dispatch(); err != nil {
				if err == io.EOF {
					return nil
				}
				return err
			}
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if err := dispatch(); err != nil && err != io.EOF {
		return err
	}
	return nil
}

// code reads an error code, which the APIs give as a string or a number.
func code(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case float64:
		return strconv.FormatFloat(c, 'f', -1, 64)
	}
	return ""
}
