package openai

import (
	"encoding/base64"
	"fmt"
	"strings"

	"nerdola.dev/x/paula/internal/runners/api"
)

func chatBody(req api.ChatRequest, hooks Hooks) (map[string]any, error) {
	msgs := make([]map[string]any, len(req.Messages))
	for i, m := range req.Messages {
		msgs[i] = message(m)
	}
	body := map[string]any{
		"model":    req.Model,
		"messages": msgs,
		"stream":   true,
	}

	if req.CacheKey != "" {
		body["prompt_cache_key"] = req.CacheKey
	}

	settings(body, req.Settings)
	if err := hooks.Body(body, req); err != nil {
		return nil, err
	}
	return body, nil
}

func message(m api.Message) map[string]any {
	out := map[string]any{"role": m.Role}

	images := false
	for _, p := range m.Parts {
		if p.Type == api.PartImage {
			images = true
		}
	}
	if images {
		parts := make([]map[string]any, 0, len(m.Parts))
		for _, p := range m.Parts {
			switch p.Type {
			case api.PartText:
				parts = append(parts, map[string]any{"type": "text", "text": p.Text})
			case api.PartImage:
				parts = append(parts, map[string]any{
					"type": "image_url",
					"image_url": map[string]any{
						"url": fmt.Sprintf("data:%s;base64,%s", p.MIME,
							base64.StdEncoding.EncodeToString(p.Data)),
					},
				})
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
	return out
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
