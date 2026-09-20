package conversation

import (
	"context"
	"strings"
	"time"

	"nerdola.dev/x/paula/internal/media"
	"nerdola.dev/x/paula/internal/runners/api"
	"nerdola.dev/x/paula/internal/store"
)

// timeText is how a time is written to a model.
func timeText(loc *time.Location, t time.Time) string {
	t = t.In(loc)
	return t.Format("Monday, 2 January 2006, 15:04 ") + "UTC" + t.Format("-07:00")
}

// sentAt says when the message after it was sent, and now says what time it
// is. Each is told in a message of its own, in the role a model reads for what
// it is told rather than for what it is answering, so there is no mark inside
// a message to explain and none for a model to copy.
func (e *Engine) sentAt(t time.Time) string {
	return "The next message was sent at " + timeText(e.clock.Now().Location(), t) + "."
}

func (e *Engine) now() string {
	at := e.clock.Now()
	return "It is now " + timeText(at.Location(), at) + "."
}

// prompt builds the messages of a reply: the card, then the conversation up to
// the message being answered, with the time before every message she was sent.
func (e *Engine) prompt(ctx context.Context, a *attempt, m *model) ([]api.Message, error) {
	// The whole conversation: from the newest message, with no limit.
	messages, err := e.store.Messages(ctx, 0, 0)
	if err != nil {
		return nil, err
	}
	var kept []store.Message
	for _, msg := range messages {
		if msg.ID <= a.upto || msg.Role == store.RoleAssistant {
			kept = append(kept, msg)
		}
	}
	kept = ordered(kept)

	// The card is the whole of what a reply is told about her, and stands for
	// every reply of the run.
	out := []api.Message{api.Text(api.RoleSystem, e.rendered)}
	inline := e.inlineFrom(kept, m)
	for i, msg := range kept {
		// Every message she was sent is told the time before it: when an older
		// one was sent, and what time it is now before the one she is
		// answering, whose last line is then what was said rather than a time.
		// Each of those stands once it is written, so a host that keeps a
		// prompt keeps all of it but the time before the last message.
		if msg.Role == store.RoleUser {
			var when string
			if i == len(kept)-1 {
				when = e.now()
			} else {
				when = e.sentAt(msg.CreatedAt)
			}
			out = append(out, api.Text(api.RoleSystem, when))
		}
		out = append(out, e.message(ctx, a, msg, inline))
	}
	return out, nil
}

// ordered puts a reply right after the messages it answers, which is not where
// it sits by id when something was said while it was being written.
func ordered(messages []store.Message) []store.Message {
	here := make(map[store.MessageID]bool, len(messages))
	for _, m := range messages {
		here[m.ID] = true
	}
	answering := map[store.MessageID][]store.Message{}
	for _, m := range messages {
		// A reply whose message is not here has nowhere to move to, and keeps
		// the place its id gives it.
		if m.Role == store.RoleAssistant && here[m.ReplyTo] {
			answering[m.ReplyTo] = append(answering[m.ReplyTo], m)
		}
	}

	out := make([]store.Message, 0, len(messages))
	for _, m := range messages {
		if m.Role == store.RoleAssistant && here[m.ReplyTo] {
			continue
		}
		out = append(out, m)
		out = append(out, answering[m.ID]...)
	}
	return out
}

// inlineFrom is the oldest message whose images go to the model as images,
// counting back engine.image_turns of the messages that carry any. Zero sends
// none, which is what a model without vision is sent.
func (e *Engine) inlineFrom(messages []store.Message, m *model) store.MessageID {
	if !m.catalogue.Vision || e.cfg.ImageTurns <= 0 || len(messages) == 0 {
		return 0
	}
	var seen int
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Role != store.RoleUser || len(msg.Images()) == 0 {
			continue
		}
		seen++
		if seen == e.cfg.ImageTurns {
			return msg.ID
		}
	}
	// Fewer messages carry images than the setting allows, so every one of
	// them is sent.
	return messages[0].ID
}

func (e *Engine) message(ctx context.Context, a *attempt, msg store.Message, inline store.MessageID) api.Message {
	if msg.Role == store.RoleAssistant {
		return api.Text(api.RoleAssistant, msg.Text())
	}

	// What was said, and a line for every picture that is not sent as one.
	var lines []string
	if text := msg.Text(); text != "" {
		lines = append(lines, text)
	}

	var images []api.Part
	for _, p := range msg.Images() {
		// Every image is described when it is first seen, whether or not the
		// model is also shown the image.
		described := e.described(ctx, a, p.SHA256)
		if inline > 0 && msg.ID >= inline {
			data, err := e.media.Load(p.SHA256)
			if err == nil {
				images = append(images, api.Part{
					Type: api.PartImage, MIME: media.MIMEJPEG, Data: data,
				})
				continue
			}
			e.log.Warn("reading an image", "sha256", p.SHA256, "error", err)
		}
		lines = append(lines, described)
	}

	out := api.Message{Role: api.RoleUser}
	out.Parts = append([]api.Part{{Type: api.PartText, Text: strings.Join(lines, "\n")}}, images...)
	return out
}
