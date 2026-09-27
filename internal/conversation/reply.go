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

// errNoRoom says a turn's prompt is past the context while the history can
// be compacted to make room for it, which the turn waits for.
var errNoRoom = errors.New("the prompt is past the context, and the history is compacted to make room")

// tooLong is what a model is sent in place of an answer that would take the
// round past the context. The call ran, which the model is told so it does
// not run it again.
const tooLong = "error: the call ran, but what it answered is too long for the room left in the context, so it was not sent"

// lastRound is what a model is told after the answers of the last round of
// calls a reply may take, so it answers with what it has.
const lastRound = "This reply has taken every round of calls it may, so write your answer now. " +
	"A call that only looks something up is not run any more; one that changes something still runs."

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
	// A prompt past the context is one the host refuses. A history that can
	// be compacted makes room for the messages to answer, and the turn waits
	// for that; a prompt still past it fails, saying why.
	if over := e.excess(m, messages); over > 0 {
		room, _ := e.reservations(m)
		_, said, rest, err := e.history(ctx)
		if err != nil {
			return nil, err
		}
		if room > 0 && coversUpto(said, rest) > 0 {
			return nil, errNoRoom
		}
		return nil, fmt.Errorf("the prompt is about %d tokens past the %d the model's context holds", over, m.limit())
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
	// past ends a reply whose next round is past the context, which no host
	// takes. One that has run a tool keeps what it wrote, as it does however
	// else it fails.
	past := func(over int) (*store.Message, error) {
		err := fmt.Errorf("the next round is about %d tokens past the %d the model's context holds", over, m.limit())
		if a.acted {
			return kept(true), err
		}
		return nil, err
	}
	for round := 1; ; round++ {
		// The round after the last a reply may take calls in is sent as the
		// ones before it, so a host reads from its cache all it kept of them,
		// with a note in the last answer saying it is the last. A model that
		// asks to look something up there all the same is asked once more,
		// for an answer with no call in it.
		last := round == e.cfg.ToolRounds+1
		choice := ""
		if round > e.cfg.ToolRounds+1 {
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

		// What the host counted the prompt as is what a word costs on this
		// model, whatever became of the reply. The rounds after the first
		// carry calls and what they answered, which the history does not.
		if res != nil && round == 1 {
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
				e.call(ctx, a, rec.last, c, errNotRun, nil)
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
		// A round past the context before any answer is in it runs none of
		// its calls: what they did would never reach the model.
		if over := e.excess(m, messages); over > 0 {
			return past(over)
		}
		// In the last round, a call that only looks something up is left:
		// what it answered would not be read before the reply is asked for
		// its answer. One that changes something runs, such as putting the
		// answer off or keeping a memory, which is as good there as in any
		// round.
		lookedUp := false
		for i, c := range res.ToolCalls {
			var answer api.Message
			if last && e.tools.looksUp(c.Name) {
				lookedUp = true
				result := e.call(ctx, a, rec.last, c, errLookup, nil)
				answer = api.Message{Role: api.RoleTool, ToolCallID: c.ID, Parts: e.answered(result, pictures{})}
			} else {
				shown := pictures{sees: m.catalogue.Vision}
				result := e.call(ctx, a, rec.last, c, nil, &shown)
				answer = api.Message{Role: api.RoleTool, ToolCallID: c.ID, Parts: e.answered(result, shown)}
			}
			// The last answer of the last round that may run every call says
			// the next round is the last. It is part of that answer rather than
			// a message of its own, which a model reads as a new turn of the
			// user's, and a host as the end of the calls it keeps the thinking of.
			var note []api.Part
			if round == e.cfg.ToolRounds && i == len(res.ToolCalls)-1 {
				note = []api.Part{{Type: api.PartText, Text: lastRound}}
			}
			answer.Parts = append(answer.Parts, note...)
			// An answer that takes the next round past the context would have
			// the host refuse the round, so the model is told so in its place.
			if e.excess(m, append(messages, answer)) > 0 {
				answer.Parts = append([]api.Part{{Type: api.PartText, Text: tooLong}}, note...)
			}
			messages = append(messages, answer)
		}
		// A stop while a tool ran keeps what she had written before it.
		if a.was(stopped) {
			return kept(true), nil
		}
		// The notes that take the place of answers are short, so only a round
		// that was all but full before its answers is still past the context.
		if over := e.excess(m, messages); over > 0 {
			return past(over)
		}
		// The last round is her answer when it wrote one and left nothing she
		// asked to look up, or when it put the answer off. Otherwise she is
		// asked once more, with what its calls answered.
		if last && !lookedUp && (strings.TrimSpace(said.String()) != "" || a.putOff) {
			break
		}
	}

	// What she wrote in any round is what she said: a model often puts the
	// whole of its answer beside the call it makes, and has nothing to add
	// once the call is answered. Nothing at all is her putting the answer off
	// when she scheduled a call back for it: the messages are answered, and
	// stay in the history for when it comes due.
	if strings.TrimSpace(written.String()) == "" {
		if a.putOff {
			a.finished()
			return nil, nil
		}
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
