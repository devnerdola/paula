package conversation

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/runners/api"
	"nerdola.dev/x/paula/internal/store"
)

// embedBatch is how many memories one request turns into vectors. An embedding
// is short and the wait is the same either way, so they go together.
const embedBatch = 100

// embedded is the model memories are embedded by: what serves the role now. A
// run has one, since serve asks for it before the conversation opens.
func (e *Engine) embedded(ctx context.Context) (*model, store.Embedded, error) {
	m, err := e.roleModel(ctx, config.RoleEmbed)
	if err != nil {
		return nil, store.Embedded{}, err
	}
	return m, store.Embedded{Runner: m.Runner.Name(), Model: m.ID}, nil
}

// embed turns the memories that have no vector into vectors, a batch at a
// time, and stops when they all have one. It runs beside the loop and beside
// the fold, so nothing waits for it. Which memories are waiting is read here
// rather than remembered: a fold writes them, a run that ended before it
// embedded leaves them, and a model given the role since leaves every one of
// them waiting under a model that has never seen them.
func (e *Engine) embed(ctx context.Context) error {
	m, by, err := e.embedded(ctx)
	if errors.Is(err, errNoModel) {
		// A conversation the role cannot be filled from embeds nothing, and
		// that is a choice rather than a failure of the fold this runs after.
		// A run that serves asked for one before it started, so this is the
		// role going away under a run that is already up — by a model it was
		// given being taken out of the file it is read from.
		return nil
	}
	if err != nil {
		return err
	}
	// aside is how many of the oldest waiting memories the host would not take.
	// They are asked for and dropped every time, so the window is widened by
	// as many: a window filled with them would hide every memory behind them.
	aside := 0
	for {
		window := embedBatch + aside
		waiting, err := e.store.MemoriesToEmbed(ctx, by, e.width(by), window)
		if err != nil {
			return err
		}
		read := len(waiting)
		// What the host would not take is asked for again by the next run, not
		// by this one: asking again now is asking the same model the same
		// thing, and the memories behind it would wait on the answer.
		aside = 0
		waiting = slices.DeleteFunc(waiting, func(mem store.Memory) bool {
			if _, ok := e.refused.Load(refusal{by, mem.ID}); ok {
				aside++
				return true
			}
			return false
		})
		if len(waiting) == 0 {
			// A window with nothing but what was set aside says nothing about
			// the memories behind them, so it is widened and read again. A
			// window the store did not fill has nothing behind them.
			if aside == 0 || read < window {
				return nil
			}
			continue
		}
		if err := e.embedStep(ctx, m, by, waiting[:min(embedBatch, len(waiting))]); err != nil {
			return err
		}
	}
}

// refusal is one memory the host would not take, under the model that was
// asked: another model is another question, and is asked it.
type refusal struct {
	by store.Embedded
	id store.MemoryID
}

// width is how wide the model's vectors came back, and zero until one has.
// What a model answers is the only thing that says how wide its vectors are,
// so it is read from an answer the way what a character costs is.
func (e *Engine) width(by store.Embedded) int {
	if n, ok := e.widths.Load(by); ok {
		return n.(int)
	}
	return 0
}

// answered takes the width of what came back. A model answering at another
// width than the memories were written at leaves them waiting: they are of
// another model as far as a vector goes, and are written again.
func (e *Engine) answered(by store.Embedded, vectors [][]float32) {
	if len(vectors) == 0 || len(vectors[0]) == 0 {
		return
	}
	e.widths.Store(by, len(vectors[0]))
}

// embedStep embeds a batch, and finds the one memory a host refuses to take.
// A batch is how many are asked for at once, not what has to go together, so
// one the host will not take is asked for in halves until it is alone — and
// then left aside, rather than left in front of every memory behind it.
//
// Only what the host said of the request itself splits a batch. Being asked to
// slow down, an outage and a connection that went are the host being away, and
// the same batch is asked for again when the work is tried again.
func (e *Engine) embedStep(ctx context.Context, m *model, by store.Embedded, waiting []store.Memory) error {
	err := e.embedOnce(ctx, m, by, waiting)
	if err == nil || !refused(err) {
		return err
	}
	if len(waiting) > 1 {
		half := len(waiting) / 2
		if err := e.embedStep(ctx, m, by, waiting[:half]); err != nil {
			return err
		}
		return e.embedStep(ctx, m, by, waiting[half:])
	}
	e.refused.Store(refusal{by, waiting[0].ID}, struct{}{})
	e.log.Warn("a memory was not turned into a vector",
		"memory", waiting[0].ID, "model", by.Model, "error", err)
	return nil
}

// refused reports whether a host turned a request down for what was in it:
// these are what both APIs answer a request they read and would not take. A
// key that is not taken, an account that owes, a model that is not there and a
// wait that is asked for are the host rather than the text, and so is anything
// else — a request nobody has seen refused is waited out rather than read as
// a hundred memories the host will not take.
func refused(err error) bool {
	var e *api.APIError
	if !errors.As(err, &e) {
		return false
	}
	switch e.Status {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return true
	}
	return false
}

// embedOnce embeds one batch, in an entry of its own, and stores the vectors
// together: a memory left without one would be searched for and never found.
func (e *Engine) embedOnce(ctx context.Context, m *model, by store.Embedded, waiting []store.Memory) error {
	said := make([]string, len(waiting))
	for i, mem := range waiting {
		said[i] = mem.Content
	}
	entry, err := e.inEntry(ctx, func(a *attempt) error {
		out, err := m.Runner.Embed(ctx, api.EmbedRequest{
			Model:    m.ID,
			Input:    said,
			Settings: m.settings,
			Recorder: e.recorder(a, store.PurposeEmbedding),
		})
		if err != nil {
			return err
		}
		e.answered(by, out.Vectors)
		if len(out.Vectors) != len(waiting) {
			return fmt.Errorf("%d memories were embedded as %d vectors", len(waiting), len(out.Vectors))
		}
		vectors := make(map[store.MemoryID][]float32, len(waiting))
		for i, mem := range waiting {
			vectors[mem.ID] = out.Vectors[i]
		}
		return e.store.Embed(context.WithoutCancel(ctx), by, vectors)
	})
	if err != nil {
		return err
	}
	e.log.Info("memories are embedded", "entry", entry.ID,
		"memories", len(waiting), "model", by.Model)
	return nil
}

// Summary is what she has been told of the conversation before the messages a
// prompt still carries, and nil while it has never been folded.
func (e *Engine) Summary(ctx context.Context) (*store.Summary, error) {
	return e.summary(ctx)
}

// Forget takes a memory away, and the ones it replaced with it. What was said
// stays: this is about what she carries, not about the conversation.
func (e *Engine) Forget(ctx context.Context, id store.MemoryID) ([]store.Memory, error) {
	return e.store.Forget(ctx, id)
}

// Memories are the memories closest in meaning to a query, or the newest ones
// when there is no query. They are what she has been told, whether or not a
// prompt had room to tell her.
func (e *Engine) Memories(ctx context.Context, query string, limit int) ([]store.Memory, error) {
	return e.memories(ctx, query, limit, nil)
}

// memories are what Memories answers, with the request that embeds the query
// kept by rec when a reply made it.
func (e *Engine) memories(ctx context.Context, query string, limit int, rec api.Recorder) ([]store.Memory, error) {
	if query == "" {
		return e.store.LatestMemories(ctx, limit)
	}
	m, by, err := e.embedded(ctx)
	if err != nil {
		return nil, err
	}
	// The query is embedded by the model the memories were, since a vector of
	// one model measures nothing against another's.
	out, err := m.Runner.Embed(ctx, api.EmbedRequest{
		Model: m.ID, Input: []string{query}, Settings: m.settings, Recorder: rec,
	})
	if err != nil {
		return nil, err
	}
	if len(out.Vectors) != 1 {
		return nil, fmt.Errorf("the query was embedded as %d vectors", len(out.Vectors))
	}
	// A search is an answer like any other: what the model answers here is what
	// says how wide its vectors are, and so which of the stored ones are of a
	// model that no longer answers that way.
	e.answered(by, out.Vectors)
	found, elsewhere, err := e.store.NearestMemories(ctx, by, out.Vectors[0], limit)
	if elsewhere > 0 {
		// The model answers at another width than it did when those were
		// written, so they are another model's as far as a vector goes. What
		// embeds writes them again, and until it has they are not searched.
		e.log.Warn("memories are embedded at another width and are not searched",
			"memories", elsewhere, "model", by.Model, "width", len(out.Vectors[0]))
	}
	return found, err
}
