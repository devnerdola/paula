package conversation

import (
	"context"
	"errors"
	"strings"
	"time"

	"nerdola.dev/x/paula/internal/media"
	"nerdola.dev/x/paula/internal/runners/api"
	"nerdola.dev/x/paula/internal/store"
)

// dateText is how a day is written to a model, and timeText a time of one.
func dateText(loc *time.Location, t time.Time) string {
	return t.In(loc).Format("Monday, 2 January 2006")
}

func timeText(loc *time.Location, t time.Time) string {
	t = t.In(loc)
	return dateText(loc, t) + t.Format(", 15:04 ") + "UTC" + t.Format("-07:00")
}

// sentAt says when the message after it was sent, and now says what time it
// is. Each is told in a message of its own, in the role the model's family
// gives her notes, so there is no mark inside a message to explain and none
// for a model to copy.
func (e *Engine) sentAt(t time.Time) string {
	return "The next message was sent at " + timeText(e.clock.Now().Location(), t) + "."
}

func (e *Engine) now() string {
	at := e.clock.Now()
	return "It is now " + timeText(at.Location(), at) + "."
}

// cameDue says a call back she scheduled came due, when, and why: the whole
// of what she answers, told the way a time is, since none of it is from the
// user. It reads the same in every prompt after it, so what a host cached
// stands.
func (e *Engine) cameDue(msg store.Message) string {
	return "A call back you scheduled came due at " + timeText(e.clock.Now().Location(), msg.CreatedAt) +
		": " + msg.Text() + "."
}

// prompt builds the messages of a reply: the persona with the summary, the
// history with the time before every message she was sent, and then the user
// input, after the time it is now. standing is how many of its first messages
// the prompt of the next reply sends again as they are, which is everything
// before that time, and zero when that is not known.
func (e *Engine) prompt(ctx context.Context, a *attempt, m *model) (_ []api.Message, standing int, _ error) {
	summary, err := e.summary(ctx)
	if err != nil {
		return nil, 0, err
	}
	// What a compaction has written is told with the persona, so the messages
	// it covers are not carried one by one any more.
	messages, err := e.store.MessagesAfter(ctx, coveredUpto(summary))
	if err != nil {
		return nil, 0, err
	}
	var kept []store.Message
	for _, msg := range messages {
		if msg.ID <= a.entry.UptoMessageID || msg.Role == store.RoleAssistant {
			kept = append(kept, msg)
		}
	}
	kept = ordered(kept)
	sending := e.sending(ctx, a)
	if sending.thought, err = e.pastThought(ctx, m, coveredUpto(summary)); err != nil {
		return nil, 0, err
	}

	out := []api.Message{e.systemMessage(m, summary)}
	// What time it is now is told before the message she is answering, which
	// is the one the attempt is for. The last of what is carried is not always
	// that one: a reply written while the next message arrived carries the
	// higher id of the two, and one whose message the summary covers keeps the
	// place its id gives it.
	msgs, at := e.render(kept, m.catalogue.Vision, a.entry.UptoMessageID, m.notes(), sending)
	if at >= 0 {
		standing = len(out) + at
	}
	return append(out, msgs...), standing, nil
}

// systemMessage is what a reply is told outside the conversation: the card, and
// the summary of the messages it no longer carries, left out while there is
// none.
func (e *Engine) systemMessage(m *model, summary *store.Summary) api.Message {
	sections := []string{e.card(m)}
	if summary != nil && summary.Content != "" {
		sections = append(sections, "Earlier in your conversation with "+
			e.persona.User.Name+":\n"+summary.Content)
	}
	return api.Text(api.RoleSystem, strings.Join(sections, "\n\n"))
}

// notes is how her notes are told: as the model's family extension says, and
// as system messages, the role a model reads for what it is told rather than
// for what it is answering, for a model no extension serves.
func (m *model) notes() api.Notes {
	if m.settings.Extension == nil {
		return api.Notes{Role: api.RoleSystem}
	}
	return m.settings.Extension.Notes()
}

// card is the character card as a model reads it, with what the tools have
// her told after it. A model told her notes as user messages is told whose
// they are right after the card, where it stands as long as the card does,
// naming the ones it is told.
func (e *Engine) card(m *model) string {
	sections := []string{e.rendered}
	if notes := m.notes(); notes.Role != api.RoleSystem {
		user := e.persona.User.Name
		told := "the one before each of " + user + "'s messages saying when it was sent"
		if !notes.LastAsSent {
			told += ", and the one saying what time it is now"
		}
		sections = append(sections, "Some messages come from the app you and "+user+
			" text through, not from "+user+": "+told+". They are for you to know, never to answer.")
	}
	if e.tools.prompt != "" {
		sections = append(sections, e.tools.prompt)
	}
	return strings.Join(sections, "\n\n")
}

// summary is the summary that counts, and nil while the conversation has never
// been compacted.
func (e *Engine) summary(ctx context.Context) (*store.Summary, error) {
	out, err := e.store.LatestSummary(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	return out, err
}

// coveredUpto is the newest message a summary speaks for, and zero when there
// is none: the messages after it are the ones a prompt carries.
func coveredUpto(summary *store.Summary) store.MessageID {
	if summary == nil {
		return 0
	}
	return summary.UptoMessageID
}

// render is the messages as the model reads them. Every message she was sent
// is told the time before it: when an older one was sent, and what time it is
// now before the one she is answering, whose last line is then what was said
// rather than a time. Each of those stands once it is written, so a host that
// keeps a prompt keeps all of it but the time before the last message. at is
// where that time is, or -1 when the last message is not among them. The times
// are told the way notes says, which may tell the last one as when it was
// sent.
func (e *Engine) render(messages []store.Message, sees bool, last store.MessageID, notes api.Notes, r reading) (out []api.Message, at int) {
	out = make([]api.Message, 0, 2*len(messages))
	at = -1
	for _, msg := range messages {
		if msg.Role == store.RoleCallback {
			if msg.ID == last {
				at = len(out)
			}
			out = append(out, api.Text(notes.Role, e.cameDue(msg)))
			continue
		}
		if msg.Role == store.RoleUser {
			when := e.sentAt(msg.CreatedAt)
			if msg.ID == last {
				at = len(out)
				if !notes.LastAsSent {
					when = e.now()
				}
			}
			out = append(out, api.Text(notes.Role, when))
		}
		out = append(out, e.message(msg, sees, r))
	}
	return out, at
}

// reading is how messages are built: what a picture that is not sent as one is
// described as, and whether a picture that is sent as one carries its bytes.
//
// A prompt reads messages to send them, so it asks for what it does not know
// and carries what it sends. Measuring the history reads the same messages to
// weigh them, so it asks nothing and loads nothing: what a picture costs is
// counted, and pictures are counted whether or not the bytes are there.
type reading struct {
	describe func(sha256 string) string
	load     bool
	// thought is what a reply goes back with of what it thought, and nil for
	// a model that is sent none.
	thought func(store.Message) *api.Reasoning
}

// pastThought is what earlier replies go back to a model with of what they
// thought, and nil for a model whose family extension does not ask for it:
// such a model reads each reply as what it said. A reply goes back to the
// model that wrote it, on the runner it wrote it on, with what it thought as
// it came. What a model thought is signed or encrypted for that model and the
// host it came from, and another model's host refuses it, so to any other
// model it goes back as its text alone.
func (e *Engine) pastThought(ctx context.Context, m *model, after store.MessageID) (func(store.Message) *api.Reasoning, error) {
	if m.settings.Extension == nil || !m.settings.Extension.PastThought() {
		return nil, nil
	}
	writers, err := e.store.Writers(ctx, after)
	if err != nil {
		return nil, err
	}
	return func(msg store.Message) *api.Reasoning {
		if w, ok := writers[msg.ID]; ok && w.Runner == m.Runner.Name() && w.Model == m.ID &&
			(msg.Reasoning != "" || len(msg.ReasoningDetails) > 0) {
			return &api.Reasoning{Text: msg.Reasoning, Details: msg.ReasoningDetails}
		}
		if msg.Reasoning != "" {
			return &api.Reasoning{Text: msg.Reasoning}
		}
		return nil
	}, nil
}

// sending is how a prompt reads messages, under the attempt it is for.
func (e *Engine) sending(ctx context.Context, a *attempt) reading {
	return reading{
		describe: func(sha256 string) string { return e.described(ctx, a, sha256) },
		load:     true,
	}
}

// weighing is how the history is read to be measured against its reservation.
func (e *Engine) weighing(ctx context.Context) reading {
	return reading{
		describe: func(sha256 string) string { return e.known(ctx, sha256) },
	}
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

// message is one message as a model reads it. A model that sees images is sent
// every picture as one; any other is sent a line with what the picture showed.
func (e *Engine) message(msg store.Message, sees bool, r reading) api.Message {
	if msg.Role == store.RoleAssistant {
		out := api.Text(api.RoleAssistant, msg.Text())
		if r.thought != nil {
			out.Reasoning = r.thought(msg)
		}
		return out
	}

	// What was said, and a line for every picture that is not sent as one.
	var lines []string
	if text := msg.Text(); text != "" {
		lines = append(lines, text)
	}

	var images []api.Part
	for _, p := range msg.Images() {
		// Every image is described when it is first seen, whether or not the
		// model is shown the image, so any model chosen later can be served it.
		described := r.describe(p.SHA256)
		if sees {
			// What a picture weighs is that it is one, not what its bytes are,
			// so weighing an exchange counts a picture without reading it.
			if !r.load {
				images = append(images, api.Part{Type: api.PartImage, MIME: media.MIMEJPEG})
				continue
			}
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
	// A picture sent with nothing said about it is the picture: a text part
	// with nothing in it is a message a host refuses.
	if len(lines) > 0 {
		out.Parts = append(out.Parts, api.Part{Type: api.PartText, Text: strings.Join(lines, "\n")})
	}
	out.Parts = append(out.Parts, images...)
	return out
}
