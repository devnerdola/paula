package venice

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"nerdola.dev/x/paula/internal/runners/api"
	"nerdola.dev/x/paula/internal/runners/openai"
	"nerdola.dev/x/paula/internal/runners/transport"
)

// hooks are the parts of a request and a stream only Venice documents, and
// how it answers.
type hooks struct{}

var (
	_ openai.Hooks      = hooks{}
	_ transport.Answers = hooks{}
)

func (hooks) Body(out map[string]any, req api.ChatRequest) error {
	prov, errs := decodeProvider(req.Settings.Provider)
	if len(errs) > 0 {
		return errs[0]
	}
	out["venice_parameters"] = prov.object()

	if v := prov.Sampling.MinTemperature; v != nil {
		out["min_temp"] = *v
	}
	if v := prov.Sampling.MaxTemperature; v != nil {
		out["max_temp"] = *v
	}
	if len(prov.Output.StopTokenIDs) > 0 {
		out["stop_token_ids"] = prov.Output.StopTokenIDs
	}
	if prov.Output.Verbosity != "" {
		out["verbosity"] = prov.Output.Verbosity
	}
	if len(prov.Fallbacks) > 0 {
		out["fallbacks"] = prov.Fallbacks
	}
	if o := reasoningObject(req.Settings); len(o) > 0 {
		out["reasoning"] = o
	}
	return nil
}

func reasoningObject(s api.Settings) map[string]any {
	out := map[string]any{}
	switch s.Reasoning.Mode {
	case api.ReasoningOn:
		out["enabled"] = true
	case api.ReasoningOff:
		out["enabled"] = false
	}
	if s.Reasoning.Effort != "" {
		out["effort"] = s.Reasoning.Effort
	}
	if s.Reasoning.Summary != "" {
		out["summary"] = s.Reasoning.Summary
	}
	return out
}

type streamChunk struct {
	Choices []struct {
		Delta struct {
			ReasoningContent string `json:"reasoning_content"`
		} `json:"delta"`
	} `json:"choices"`
	Cost *struct {
		USD float64 `json:"usd"`
	} `json:"cost"`
	Usage *struct {
		PromptTokensDetails *struct {
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

func (hooks) Chunk(raw []byte, res *api.Result) (string, error) {
	var c streamChunk
	if err := json.Unmarshal(raw, &c); err != nil {
		return "", err
	}
	if c.Cost != nil {
		res.Usage.Cost = c.Cost.USD
	}
	// What a request wrote to the cache is Venice's own field, beside the
	// tokens it read that every OpenAI-compatible API reports.
	if u := c.Usage; u != nil && u.PromptTokensDetails != nil {
		res.Usage.CacheWriteTokens = u.PromptTokensDetails.CacheCreationInputTokens
	}
	if len(c.Choices) == 0 {
		return "", nil
	}
	return c.Choices[0].Delta.ReasoningContent, nil
}

// End keeps the reasoning of the round as it came, as the one detail to hand
// back with it. A reply of several rounds is one message whose reasoning is
// every round's, and what a GPT model thought comes as an encrypted item that
// OpenAI parses only whole and on its own: three rounds' items handed back
// together were refused.
func (hooks) End(res *api.Result) {
	if res.Reasoning == "" {
		return
	}
	raw, err := json.Marshal(res.Reasoning)
	if err != nil {
		return
	}
	res.ReasoningDetails = []json.RawMessage{raw}
}

// Message hands back the reasoning an assistant message came with, in the
// field the stream gave it in: the last round's as it came, or the text of a
// message kept with no detail.
func (hooks) Message(out map[string]any, m api.Message) {
	if m.Reasoning == nil {
		return
	}
	if n := len(m.Reasoning.Details); n > 0 {
		var came string
		if json.Unmarshal(m.Reasoning.Details[n-1], &came) == nil && came != "" {
			out["reasoning_content"] = came
			return
		}
	}
	if m.Reasoning.Text != "" {
		out["reasoning_content"] = m.Reasoning.Text
	}
}

// resetHeader is the Unix time the rate limit documentation says a request may
// be sent again at.
const resetHeader = "x-ratelimit-reset-requests"

func (hooks) Retry(now time.Time, status int, h http.Header) (time.Duration, bool) {
	if status != http.StatusTooManyRequests {
		return 0, false
	}
	if v := h.Get(resetHeader); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			if d := time.Unix(n, 0).Sub(now); d > 0 {
				return d, true
			}
		}
	}
	return 0, true
}

func (hooks) Error(status int, body []byte) *api.APIError {
	var text struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &text); err == nil && text.Error != "" {
		return &api.APIError{Status: status, Message: text.Error}
	}
	var object struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Param   string `json:"param"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &object); err != nil || object.Error.Message == "" {
		return nil
	}
	out := &api.APIError{
		Status:  status,
		Type:    object.Error.Type,
		Code:    object.Error.Code,
		Message: object.Error.Message,
	}
	if object.Error.Param != "" {
		out.Message += " (" + object.Error.Param + ")"
	}
	return out
}
