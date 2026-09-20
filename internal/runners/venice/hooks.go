package venice

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"nerdola.dev/x/paula/internal/runners/api"
	"nerdola.dev/x/paula/internal/runners/openai"
)

// hooks are the parts of a request and a stream only Venice documents.
type hooks struct{}

var _ openai.Hooks = hooks{}

func (hooks) Body(out map[string]any, req api.ChatRequest) error {
	prov, errs := decodeProvider(req.Settings.Provider)
	if len(errs) > 0 {
		return errs[0]
	}
	// Venice documents this for usage in a stream.
	out["stream_options"] = map[string]any{"include_usage": true}
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
	if prov.Cache.Retention != "" {
		out["prompt_cache_retention"] = prov.Cache.Retention
	}
	if len(prov.Fallbacks) > 0 {
		out["fallbacks"] = prov.Fallbacks
	}
	if o := reasoningObject(req.Settings); len(o) > 0 {
		out["reasoning"] = o
	}
	return nil
}

// EmbedBody carries nothing of its own: what venice_parameters holds is about
// writing a reply, and the endpoint that turns text into vectors documents
// none of it.
func (hooks) EmbedBody(map[string]any, api.EmbedRequest) error { return nil }

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
}

func (hooks) Chunk(raw []byte, res *api.Result) (string, error) {
	var c streamChunk
	if err := json.Unmarshal(raw, &c); err != nil {
		return "", err
	}
	if c.Cost != nil {
		res.Usage.Cost = c.Cost.USD
	}
	if len(c.Choices) == 0 {
		return "", nil
	}
	return c.Choices[0].Delta.ReasoningContent, nil
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
