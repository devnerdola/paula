package conversation

import (
	"context"
	"errors"
	"fmt"

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
		return nil
	}
	if err != nil {
		return err
	}
	for {
		waiting, err := e.store.MemoriesToEmbed(ctx, by, embedBatch)
		if err != nil || len(waiting) == 0 {
			return err
		}
		if err := e.embedStep(ctx, m, by, waiting); err != nil {
			return err
		}
	}
}

// embedStep embeds one batch, in an entry of its own, and stores the vectors
// together: a memory left without one would be searched for and never found.
func (e *Engine) embedStep(ctx context.Context, m *model, by store.Embedded, waiting []store.Memory) error {
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

// Memories are the memories closest in meaning to a query, or the newest ones
// when there is no query. They are what she has been told, whether or not a
// prompt had room to tell her.
func (e *Engine) Memories(ctx context.Context, query string, limit int) ([]store.Memory, error) {
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
		Model: m.ID, Input: []string{query}, Settings: m.settings,
	})
	if err != nil {
		return nil, err
	}
	if len(out.Vectors) != 1 {
		return nil, fmt.Errorf("the query was embedded as %d vectors", len(out.Vectors))
	}
	return e.store.NearestMemories(ctx, by, out.Vectors[0], limit)
}
