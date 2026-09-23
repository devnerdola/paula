package openrouter

import (
	"encoding/json"
	"net/http"
	"slices"
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

// EmbedBody carries the routing settings and nothing else. Which hosts a
// request may go to, and what they may keep of it, is the same question for a
// memory as for a reply; sampling, reasoning and caching are about writing,
// which a model that embeds does not do.
func (hooks) EmbedBody(out map[string]any, req api.EmbedRequest) error {
	prov, errs := decodeProvider(req.Settings.Provider)
	if len(errs) > 0 {
		return errs[0]
	}
	if o := prov.Routing.object(); len(o) > 0 {
		out["provider"] = o
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
			Reasoning        string            `json:"reasoning"`
			ReasoningDetails []json.RawMessage `json:"reasoning_details"`
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
	// The details are what the reasoning documentation asks to be handed back
	// with the reply they came with, so they are kept as they arrived until
	// the stream is done.
	res.ReasoningDetails = append(res.ReasoningDetails, c.Choices[0].Delta.ReasoningDetails...)
	return c.Choices[0].Delta.Reasoning, nil
}

// End makes the details whole. A stream sends an item a fragment at a time
// under the index it has, the way it sends a call, so the fragments of one
// index are the one item they make — which is what a reply that was not
// streamed would have carried, and what goes back.
func (hooks) End(res *api.Result) {
	if len(res.ReasoningDetails) == 0 {
		return
	}
	items := whole(res.ReasoningDetails)
	res.ReasoningDetails = make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		raw, err := json.Marshal(item)
		if err != nil {
			continue
		}
		res.ReasoningDetails = append(res.ReasoningDetails, raw)
	}
}

// Message hands back the reasoning an assistant message came with, as the
// reasoning documentation asks: its details as they came. A message whose
// reasoning came as text alone, from a runner that sends no details, hands
// back the text in the field a reply carries it in.
func (hooks) Message(out map[string]any, m api.Message) {
	switch {
	case m.Reasoning == nil:
	case len(m.Reasoning.Details) > 0:
		out["reasoning_details"] = m.Reasoning.Details
	case m.Reasoning.Text != "":
		out["reasoning"] = m.Reasoning.Text
	}
}

// grown are the fields of a reasoning item that arrive a fragment at a time.
// Every other field is the same in each fragment it comes in, or comes in one
// of them only, as a signature does at the end.
var grown = []string{"text", "summary", "data"}

// whole puts the fragments of each reasoning item back together, in the order
// the items first came. A fragment that names no index is an item of its own.
func whole(fragments []json.RawMessage) []any {
	var items []any
	at := map[float64]map[string]any{}
	for _, raw := range fragments {
		var f map[string]any
		index, ok := 0.0, false
		if json.Unmarshal(raw, &f) == nil {
			index, ok = f["index"].(float64)
		}
		if !ok {
			items = append(items, raw)
			continue
		}
		item, seen := at[index]
		if !seen {
			at[index] = f
			items = append(items, f)
			continue
		}
		for k, v := range f {
			if s, isText := v.(string); isText && slices.Contains(grown, k) {
				was, _ := item[k].(string)
				item[k] = was + s
				continue
			}
			if was, has := item[k]; !has || was == nil || was == "" {
				item[k] = v
			}
		}
	}
	return items
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
