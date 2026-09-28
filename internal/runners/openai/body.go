package openai

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"nerdola.dev/x/paula/internal/runners/api"
)

// picture is an image part of a body, and the sha256 of the picture it carries,
// which is the name the picture is kept under in the media directory.
type picture struct {
	part map[string]any
	sha  string
}

// chatBody builds the body of a chat request, and says which of its parts
// carry a picture.
func chatBody(req api.ChatRequest, hooks Hooks) (map[string]any, []picture, error) {
	msgs := make([]map[string]any, len(req.Messages))
	var pictures []picture
	for i, m := range req.Messages {
		var carried []picture
		msgs[i], carried = message(m)
		pictures = append(pictures, carried...)
		hooks.Message(msgs[i], m)
	}
	body := map[string]any{
		"model":    req.Model,
		"messages": msgs,
		"stream":   true,
		// What the prompt came to is read out of the stream, and what every
		// budget is held to is learned from it. A host that sends it unasked
		// loses nothing by being asked.
		"stream_options": map[string]any{"include_usage": true},
	}

	if len(req.Tools) > 0 {
		tools := make([]map[string]any, len(req.Tools))
		for i, t := range req.Tools {
			tools[i] = map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  t.Parameters,
				},
			}
		}
		body["tools"] = tools
	}
	if req.ToolChoice != "" {
		body["tool_choice"] = req.ToolChoice
	}

	settings(body, req.Settings)
	if err := hooks.Body(body, req); err != nil {
		return nil, nil, err
	}
	if req.Settings.Extension != nil {
		req.Settings.Extension.Body(body, req)
	}
	return body, pictures, nil
}

// recorded is a body as the record keeps it, once the body as sent is
// encoded: every picture named by its sha256 in place of its bytes, so a
// prompt of pictures costs the record a line each, and the bytes are read from
// the media directory when they are wanted. A body carrying no picture is
// recorded as it was sent.
func recorded(body map[string]any, pictures []picture) ([]byte, error) {
	if len(pictures) == 0 {
		return nil, nil
	}
	for _, p := range pictures {
		p.part["image_url"] = map[string]any{"url": "sha256:" + p.sha}
	}
	return encode(body)
}

// encode writes a chat body with the conversation last. Everything else is
// the same in every request of a conversation, and a host reads a request from
// its start: what grows goes after what does not.
func encode(body map[string]any) ([]byte, error) {
	keys := slices.Sorted(maps.Keys(body))
	if i := slices.Index(keys, "messages"); i >= 0 {
		keys = append(slices.Delete(keys, i, i+1), "messages")
	}
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		name, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		value, err := json.Marshal(body[k])
		if err != nil {
			return nil, err
		}
		b.Write(name)
		b.WriteByte(':')
		b.Write(value)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func message(m api.Message) (map[string]any, []picture) {
	out := map[string]any{"role": m.Role}

	images := false
	for _, p := range m.Parts {
		if p.Type == api.PartImage {
			images = true
		}
	}
	var pictures []picture
	if images {
		parts := make([]map[string]any, 0, len(m.Parts))
		for _, p := range m.Parts {
			switch p.Type {
			case api.PartText:
				parts = append(parts, map[string]any{"type": "text", "text": p.Text})
			case api.PartImage:
				part := map[string]any{
					"type": "image_url",
					"image_url": map[string]any{
						"url": fmt.Sprintf("data:%s;base64,%s", p.MIME,
							base64.StdEncoding.EncodeToString(p.Data)),
					},
				}
				sum := sha256.Sum256(p.Data)
				pictures = append(pictures, picture{part: part, sha: hex.EncodeToString(sum[:])})
				parts = append(parts, part)
			}
		}
		out["content"] = parts
	} else {
		// The parts are joined the way the conversation joins them when it
		// reads a message back, so a prompt and the log of it read the same.
		var text []string
		for _, p := range m.Parts {
			if p.Type == api.PartText && p.Text != "" {
				text = append(text, p.Text)
			}
		}
		out["content"] = strings.Join(text, "\n\n")
	}

	if len(m.ToolCalls) > 0 {
		calls := make([]map[string]any, len(m.ToolCalls))
		for i, c := range m.ToolCalls {
			calls[i] = map[string]any{
				"id":   c.ID,
				"type": "function",
				"function": map[string]any{
					"name":      c.Name,
					"arguments": c.Arguments,
				},
			}
		}
		out["tool_calls"] = calls
		// A message that is only calls says nothing, and both APIs document
		// what says nothing beside a call as null rather than as text.
		if out["content"] == "" {
			out["content"] = nil
		}
	}
	if m.ToolCallID != "" {
		out["tool_call_id"] = m.ToolCallID
	}
	return out, pictures
}

// settings writes the parameters both APIs document under the same names. The
// reasoning object is left to the hook, since each API documents fields of its
// own inside it.
func settings(body map[string]any, s api.Settings) {
	set(body, "temperature", s.Sampling.Temperature)
	set(body, "top_p", s.Sampling.TopP)
	set(body, "top_k", s.Sampling.TopK)
	set(body, "min_p", s.Sampling.MinP)
	set(body, "repetition_penalty", s.Sampling.RepetitionPenalty)
	set(body, "presence_penalty", s.Sampling.PresencePenalty)
	set(body, "frequency_penalty", s.Sampling.FrequencyPenalty)
	set(body, "seed", s.Sampling.Seed)
	set(body, "max_tokens", s.Output.MaxTokens)
	if len(s.Output.Stop) > 0 {
		body["stop"] = s.Output.Stop
	}
}

func set[T any](body map[string]any, key string, v *T) {
	if v != nil {
		body[key] = *v
	}
}
