package conversation

import (
	"context"

	"nerdola.dev/x/paula/internal/runners/api"
	"nerdola.dev/x/paula/internal/store"
)

// recorder keeps the requests of one purpose under the entry that made them.
// last is the request it kept most recently, which is what a tool call names
// as the round that asked for it.
type recorder struct {
	store   *store.Store
	entry   store.EntryID
	purpose string
	last    int64
}

// recorder is what the requests an attempt makes are kept by.
func (e *Engine) recorder(a *attempt, purpose string) *recorder {
	return &recorder{store: e.store, entry: a.entry.ID, purpose: purpose}
}

func (r *recorder) StartRequest(ctx context.Context, rec *api.Record) error {
	req := &store.Request{
		EntryID:        r.entry,
		Purpose:        r.purpose,
		Runner:         rec.Runner,
		Model:          rec.Model,
		Method:         rec.Method,
		URL:            rec.URL,
		RequestHeaders: rec.RequestHeaders,
		RequestBody:    rec.RequestBody,
		StartedAt:      rec.StartedAt,
	}
	if err := r.store.AddRequest(ctx, req); err != nil {
		return err
	}
	rec.ID = req.ID
	r.last = req.ID
	return nil
}

func (r *recorder) EndRequest(ctx context.Context, rec *api.Record) error {
	req := &store.Request{
		ID:              rec.ID,
		Attempts:        attempts(rec.Attempts),
		Status:          rec.Status,
		ResponseHeaders: rec.ResponseHeaders,
		ResponseBody:    rec.ResponseBody,
		FirstByteAt:     rec.FirstByteAt,
		EndedAt:         rec.EndedAt,
		Error:           rec.Error,
		Provider:        rec.Provider,
		FinishReason:    rec.FinishReason,
	}
	if rec.Usage != nil {
		req.Cost = rec.Usage.Cost
		req.Usage = &store.Usage{
			PromptTokens:     rec.Usage.PromptTokens,
			CachedTokens:     rec.Usage.CachedTokens,
			CacheWriteTokens: rec.Usage.CacheWriteTokens,
			CompletionTokens: rec.Usage.CompletionTokens,
			ReasoningTokens:  rec.Usage.ReasoningTokens,
		}
	}
	// A request that was cancelled still has to be kept.
	return r.store.EndRequest(context.WithoutCancel(ctx), req)
}

func attempts(in []api.Attempt) []store.Attempt {
	out := make([]store.Attempt, len(in))
	for i, a := range in {
		out[i] = store.Attempt{
			StartedAt:   a.StartedAt,
			FirstByteAt: a.FirstByteAt,
			EndedAt:     a.EndedAt,
			Status:      a.Status,
			Error:       a.Error,
			RetryAfter:  a.RetryAfter,
		}
	}
	return out
}
