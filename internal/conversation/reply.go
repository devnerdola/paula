package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"

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

// reply writes one reply and stores it. A reply offered tools goes in rounds:
// the model asks for tools, they run, what they answered goes back to it, and
// it goes on, until it answers without asking for any.
func (e *Engine) reply(ctx context.Context, a *attempt) (*store.Message, error) {
	m, err := e.roleModel(ctx, config.RoleChat)
	if err != nil {
		return nil, err
	}
	messages, standing, err := e.prompt(ctx, a, m)
	if err != nil {
		return nil, err
	}

	// written is the text of every round, as the frontends were sent it: they
	// are given the whole of it each time and show what is new, so it only
	// ever grows. thought is the reasoning of every round, and details what
	// its runner sent of it to be handed back.
	var written strings.Builder
	var thought []string
	var details []json.RawMessage
	var res *api.Result
	kept := func(interrupted bool) *store.Message {
		return e.replyMessage(a, written.String(), strings.Join(thought, "\n\n"), details, interrupted)
	}
	for round := 1; ; round++ {
		// The round after the last a reply may take calls in is asked for an
		// answer with none, so a model that keeps asking still answers.
		choice := ""
		if round > e.cfg.ToolRounds {
			choice = api.ToolChoiceNone
		}
		rec := e.recorder(a, store.PurposeReply)
		var said strings.Builder
		res, err = m.Runner.Chat(ctx, api.ChatRequest{
			Model:      m.ID,
			Messages:   messages,
			Settings:   m.settings,
			Tools:      e.tools.defs,
			ToolChoice: choice,
			CacheKey:   e.cacheKey(store.PurposeReply),
			Standing:   standing,
			Recorder:   rec,
		}, func(c api.Chunk) error {
			if c.Kind != api.ChunkText || c.Text == "" {
				return nil
			}
			// What a round writes starts at its first word: one that sends a
			// blank line before the calls it asks for has written nothing, and
			// has not started the reply.
			text := c.Text
			if said.Len() == 0 {
				text = strings.TrimLeftFunc(text, unicode.IsSpace)
				if text == "" {
					return nil
				}
			}
			// A reply a new message restarted shows nothing, not even what
			// landed while it was being cancelled.
			if !a.started() {
				return nil
			}
			// What a round writes after another has is a text of its own: what
			// came before it is what she said before a call.
			if said.Len() == 0 {
				written.WriteString(gap(written.String()))
			}
			said.WriteString(text)
			written.WriteString(text)
			e.events.publish(Event{
				Kind: ReplyText, Entry: a.entry.ID, Channel: a.entry.Channel, Text: written.String(),
			})
			return nil
		})

		// What the host counted this prompt as is what a character costs on
		// this model, whatever became of the reply.
		if res != nil {
			e.costs.correct(m.Name, res.Usage.PromptTokens, messages, e.tools.text)
		}

		// A restart keeps nothing, even when the reply finished as it landed.
		if a.was(restarted) {
			return nil, context.Canceled
		}
		if err != nil {
			// A reply that was stopped keeps what it had written, and so does
			// one that has run a tool, however it failed.
			if a.was(stopped) {
				return kept(true), nil
			}
			if a.acted {
				return kept(true), err
			}
			return nil, err
		}
		if res.Reasoning != "" {
			thought = append(thought, res.Reasoning)
		}
		// A reply goes back as one message, and what that message was in the
		// answer is its last round: the details that go back with it are that
		// round's, exactly as they came, since a host holds a signed thought
		// to the response it was in. What the rounds before it thought went
		// back with their calls.
		details = res.ReasoningDetails
		if len(res.ToolCalls) == 0 {
			break
		}

		if choice == api.ToolChoiceNone {
			// A model asked for an answer with no call in it that asks anyway
			// has its calls written down and left: what it wrote beside them
			// is its answer.
			for _, c := range res.ToolCalls {
				e.call(ctx, a, rec.last, c, false)
			}
			break
		}

		// A reply that runs a tool has done something, so a message that
		// arrives while it does no longer takes its place.
		if !a.started() {
			return nil, context.Canceled
		}
		// A stop that lands as a round asks for its calls runs none of them.
		if a.was(stopped) {
			return kept(true), nil
		}
		messages = append(messages, asked(said.String(), res))
		for _, c := range res.ToolCalls {
			result := e.call(ctx, a, rec.last, c, true)
			messages = append(messages, api.Message{
				Role:       api.RoleTool,
				ToolCallID: c.ID,
				Parts:      []api.Part{{Type: api.PartText, Text: result}},
			})
		}
		// A stop while a tool ran keeps what she had written before it.
		if a.was(stopped) {
			return kept(true), nil
		}
	}

	// What she wrote in any round is what she said: a model often puts the
	// whole of its answer beside the call it makes, and has nothing to add
	// once the call is answered.
	if strings.TrimSpace(written.String()) == "" {
		return nil, fmt.Errorf("the model returned no text (finish reason %s)", reason(res))
	}
	a.finished()
	return kept(false), nil
}

// asked is a round that asked for tools, as the rounds after it are told it:
// what it wrote, what it asked for, and the reasoning it came with, which a
// model that reasons across rounds reads again.
func asked(said string, res *api.Result) api.Message {
	m := api.Message{Role: api.RoleAssistant, ToolCalls: res.ToolCalls}
	if said != "" {
		m.Parts = []api.Part{{Type: api.PartText, Text: said}}
	}
	if res.Reasoning != "" || len(res.ReasoningDetails) > 0 {
		m.Reasoning = &api.Reasoning{Text: res.Reasoning, Details: res.ReasoningDetails}
	}
	return m
}

// gap is what goes between what a reply has written and what a new round of it
// writes: a blank line, less whatever of one the text already ends with. A
// blank line is where one text ends and the next begins.
func gap(written string) string {
	switch {
	case written == "", strings.HasSuffix(written, "\n\n"):
		return ""
	case strings.HasSuffix(written, "\n"):
		return "\n"
	}
	return "\n\n"
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
func (e *Engine) replyMessage(a *attempt, text, reasoning string, details []json.RawMessage, interrupted bool) *store.Message {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	return &store.Message{
		Role:             store.RoleAssistant,
		Channel:          a.entry.Channel,
		Parts:            []store.Part{{Type: store.PartText, Text: text}},
		Reasoning:        reasoning,
		ReasoningDetails: details,
		Interrupted:      interrupted,
		ReplyTo:          a.entry.UptoMessageID,
		EntryID:          a.entry.ID,
		CreatedAt:        e.clock.Now(),
	}
}
