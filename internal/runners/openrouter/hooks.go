package openrouter

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"nerdola.dev/x/paula/internal/runners/api"
	"nerdola.dev/x/paula/internal/runners/openai"
)

// hooks are the parts of a request and a stream only OpenRouter documents.
type hooks struct{}

var _ openai.Hooks = hooks{}

func (hooks) Body(out map[string]any, req api.ChatRequest) error {
	prov, errs := decodeProvider(req.Settings.Provider)
	if len(errs) > 0 {
		return errs[0]
	}
	if o := prov.Routing.object(); len(o) > 0 {
		out["provider"] = o
	}
	if v := prov.Sampling.TopA; v != nil {
		out["top_a"] = *v
	}
	if len(prov.Sampling.LogitBias) > 0 {
		out["logit_bias"] = prov.Sampling.LogitBias
	}
	if c := prov.Cache.Control; c.Type != "" || c.TTL != "" {
		control := map[string]any{}
		if c.Type != "" {
			control["type"] = c.Type
		}
		if c.TTL != "" {
			control["ttl"] = c.TTL
		}
		out["cache_control"] = control
	}
	if prov.ServiceTier != "" {
		out["service_tier"] = prov.ServiceTier
	}
	if o := reasoningObject(req.Settings, prov); len(o) > 0 {
		out["reasoning"] = o
	}
	return nil
}

func reasoningObject(s api.Settings, prov *provider) map[string]any {
	out := map[string]any{}
	if s.Reasoning.Mode == api.ReasoningOff {
		// Reasoning that is off carries nothing else: an effort beside it says
		// two things at once.
		return map[string]any{"enabled": false}
	}
	if s.Reasoning.Mode == api.ReasoningOn {
		out["enabled"] = true
	}
	if s.Reasoning.Effort != "" {
		out["effort"] = s.Reasoning.Effort
	}
	if s.Reasoning.Summary != "" {
		out["summary"] = s.Reasoning.Summary
	}
	if v := prov.Reasoning.MaxTokens; v != nil {
		out["max_tokens"] = *v
	}
	return out
}

type streamChunk struct {
	Provider string `json:"provider"`
	Choices  []struct {
		Delta struct {
			Reasoning string `json:"reasoning"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *struct {
		Cost                float64 `json:"cost"`
		PromptTokensDetails *struct {
			CacheWriteTokens int `json:"cache_write_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

func (hooks) Chunk(raw []byte, res *api.Result) (string, error) {
	var c streamChunk
	if err := json.Unmarshal(raw, &c); err != nil {
		return "", err
	}
	if c.Provider != "" {
		res.Provider = c.Provider
	}
	if u := c.Usage; u != nil {
		res.Usage.Cost = u.Cost
		if d := u.PromptTokensDetails; d != nil {
			res.Usage.CacheWriteTokens = d.CacheWriteTokens
		}
	}
	if len(c.Choices) == 0 {
		return "", nil
	}
	return c.Choices[0].Delta.Reasoning, nil
}

// retryable are the statuses the errors documentation calls safe to send
// again.
var retryable = []int{
	http.StatusRequestTimeout,
	http.StatusTooManyRequests,
	http.StatusBadGateway,
	http.StatusServiceUnavailable,
}

func (hooks) Retry(now time.Time, status int, h http.Header) (time.Duration, bool) {
	for _, s := range retryable {
		if s != status {
			continue
		}
		if v := h.Get("Retry-After"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				return time.Duration(n) * time.Second, true
			}
			if t, err := http.ParseTime(v); err == nil {
				return t.Sub(now), true
			}
		}
		return 0, true
	}
	return 0, false
}

func (hooks) Error(status int, body []byte) *api.APIError {
	var e struct {
		Error struct {
			Code     any    `json:"code"`
			Type     string `json:"type"`
			Message  string `json:"message"`
			Metadata struct {
				Raw string `json:"raw"`
			} `json:"metadata"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil || e.Error.Message == "" {
		return nil
	}
	out := &api.APIError{
		Status:  status,
		Type:    e.Error.Type,
		Message: e.Error.Message,
	}
	// The code is often the status repeated, which says nothing more.
	switch c := e.Error.Code.(type) {
	case string:
		out.Code = c
	case float64:
		if code := strconv.FormatFloat(c, 'f', -1, 64); code != strconv.Itoa(status) {
			out.Code = code
		}
	}
	if e.Error.Metadata.Raw != "" {
		out.Message += ": " + e.Error.Metadata.Raw
	}
	return out
}
