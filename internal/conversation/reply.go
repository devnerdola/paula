package conversation

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/runners"
	"nerdola.dev/x/paula/internal/runners/api"
	"nerdola.dev/x/paula/internal/store"
)

// errNoModel says the configuration names no model for a role, which is a
// choice and not a failure.
var errNoModel = errors.New("no model is set")

// model is a configured model and what its runner says it can do.
type model struct {
	*runners.Configured
	catalogue *api.Model
	settings  api.Settings
}

// roleModel is the model a role is served by: the one saved for it, or the one
// the configuration file makes the default.
func (e *Engine) roleModel(ctx context.Context, role config.Role) (*model, error) {
	if e.runners == nil {
		return nil, errors.New("no runner is set up")
	}
	chosen, err := RoleModel(ctx, e.store, e.runners, role)
	if err != nil {
		return nil, err
	}
	if chosen == nil {
		return nil, fmt.Errorf("%w for %s", errNoModel, role)
	}
	catalogue, err := e.runners.Lookup(ctx, chosen)
	if err != nil {
		return nil, err
	}
	// What a catalogue says can change under a name that was saved, so the
	// model is held against the role every time it is used.
	if missing := catalogue.Missing(runners.RoleNeeds(role)); len(missing) > 0 {
		return nil, fmt.Errorf("%s cannot be the %s model: %w",
			chosen.Name, role, errors.Join(missing...))
	}
	return &model{
		Configured: chosen,
		catalogue:  catalogue,
		settings:   runners.WithDefaults(chosen.Settings, catalogue),
	}, nil
}

// cacheKey groups the requests that share a prompt prefix, so what a host keeps
// of one is there for the next.
func (e *Engine) cacheKey(purpose string) string {
	return "paula-" + e.persona.ID + "-" + purpose
}

// reply writes one reply and stores it.
func (e *Engine) reply(ctx context.Context, a *attempt) (*store.Message, error) {
	m, err := e.roleModel(ctx, config.RoleChat)
	if err != nil {
		return nil, err
	}
	messages, err := e.prompt(ctx, a, m)
	if err != nil {
		return nil, err
	}

	var text strings.Builder
	res, err := m.Runner.Chat(ctx, api.ChatRequest{
		Model:    m.ID,
		Messages: messages,
		Settings: m.settings,
		CacheKey: e.cacheKey(store.PurposeReply),
		Recorder: e.recorder(a, store.PurposeReply),
	}, func(c api.Chunk) error {
		if c.Kind != api.ChunkText || c.Text == "" {
			return nil
		}
		// A reply a new message restarted shows nothing, not even what landed
		// while it was being cancelled.
		if !a.started() {
			return nil
		}
		text.WriteString(c.Text)
		e.events.publish(Event{
			Kind: ReplyText, Entry: a.entry.ID, Channel: a.entry.Channel, Text: text.String(),
		})
		return nil
	})

	// What the host counted this prompt as is what a character costs on this
	// model, whatever became of the reply.
	if res != nil {
		e.costs.correct(m.Name, res.Usage.PromptTokens, messages)
	}

	// A restart keeps nothing, even when the reply finished as it landed.
	if a.was(restarted) {
		return nil, context.Canceled
	}
	if err != nil {
		// A reply that was stopped keeps what it had written.
		if a.was(stopped) {
			return e.replyMessage(a, text.String(), "", true), nil
		}
		return nil, err
	}
	if strings.TrimSpace(text.String()) == "" {
		return nil, fmt.Errorf("the model returned no text (finish reason %s)", reason(res))
	}
	a.finished()
	return e.replyMessage(a, text.String(), res.Reasoning, false), nil
}

func reason(res *api.Result) string {
	if res == nil || res.FinishReason == "" {
		return "none"
	}
	return res.FinishReason
}

// replyMessage is the reply as a message of its own, for the loop to store. How
// the model finished is kept with the request that asked, which paula turns
// shows, so it is not kept here a second time.
func (e *Engine) replyMessage(a *attempt, text, reasoning string, interrupted bool) *store.Message {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	return &store.Message{
		Role:        store.RoleAssistant,
		Channel:     a.entry.Channel,
		Parts:       []store.Part{{Type: store.PartText, Text: text}},
		Reasoning:   reasoning,
		Interrupted: interrupted,
		ReplyTo:     a.entry.UptoMessageID,
		EntryID:     a.entry.ID,
		CreatedAt:   e.clock.Now(),
	}
}
