package conversation

import (
	"context"
	"errors"
	"slices"
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

// prompt builds the messages of a reply: what she is told outside the
// conversation, then the messages the summary does not cover, with the time
// before every message she was sent.
func (e *Engine) prompt(ctx context.Context, a *attempt, m *model) ([]api.Message, error) {
	summary, err := e.summary(ctx)
	if err != nil {
		return nil, err
	}
	memories, err := e.store.Memories(ctx)
	if err != nil {
		return nil, err
	}
	// What a fold has written is told in the system message, so the messages
	// it covers are not carried one by one any more.
	messages, err := e.store.MessagesAfter(ctx, coveredUpto(summary))
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

	ratio := e.costs.ratio(m.Name)
	limit := m.limit()
	system, history := e.split(m)
	card := e.systemMessage(memories, summary, system, ratio)
	// The summary is told whole, so nothing else notices when it has outgrown
	// what is left of the system message once the card and the memories are
	// written. This is where both numbers are known.
	if room := e.summaryRoom(m, memories, ratio); room > 0 && summary != nil {
		a.compactDue = size([]api.Message{api.Text(api.RoleSystem, summary.Content)}, ratio, 0) > room
	}
	if len(kept) == 0 {
		return []api.Message{card}, nil
	}

	inline := e.inlineFrom(kept, m)
	last := kept[len(kept)-1].ID
	image := e.costs.image(m.Name)
	head := size([]api.Message{card}, ratio, image)
	taken := head

	// The newest exchange is built first and the older ones are added while
	// they fit, so nothing older than the first that does not fit is built: no
	// picture of one is loaded, and none is sent to be described.
	groups := exchanges(kept)
	built := make([][]api.Message, 0, len(groups))
	dropped := 0
	for i, group := range slices.Backward(groups) {
		msgs := e.exchange(ctx, a, group, inline, last)
		// The exchange being answered goes whatever it takes, since leaving it
		// out would answer nothing.
		if n := size(msgs, ratio, image); i == len(groups)-1 || limit <= 0 || taken+n <= limit {
			taken += n
			built = append(built, msgs)
			continue
		}
		dropped = i + 1
		break
	}
	if dropped > 0 {
		e.log.Warn("the oldest of the conversation is left out of the prompt",
			"entry", a.entry.ID, "exchanges", dropped, "context", limit, "tokens", taken)
	}
	// What the messages took is what a fold is due on, and this is where it is
	// known exactly: these are the messages, rendered as the model reads them.
	// One that left something out says it outright, since what it carried is
	// held to the context and would sit under the share for ever while the
	// conversation grew past it — unless the system message is what is over,
	// and then folding would take messages the prompt could still carry and
	// make the summary that is over even longer.
	if history > 0 {
		a.foldDue = taken-head > history || (dropped > 0 && head <= system)
	}

	out := []api.Message{card}
	for _, b := range slices.Backward(built) {
		out = append(out, b...)
	}
	return out, nil
}

// systemMessage is the whole of what a reply is told outside the conversation:
// the card, the memories that still stand, and the summary of the messages it
// no longer carries. Each section is left out when it holds nothing, and the
// card is the one that always stands, so a run with no fold behind it reads
// exactly as it did before there were folds.
func (e *Engine) systemMessage(memories []store.Memory, summary *store.Summary, room int, ratio float64) api.Message {
	card := api.Text(api.RoleSystem, e.rendered)
	sections := []string{e.rendered}

	// What is left of the system message once the card is written is divided
	// between the memories and the summary. Nothing bounding the prompt leaves
	// both of them whole.
	loc := e.clock.Now().Location()
	told := memories
	if room > 0 {
		left := max(0, room-size([]api.Message{card}, ratio, 0))
		told = remembered(memories, share(left, e.cfg.MemoryRatio), ratio, loc)
	}
	if len(told) > 0 {
		lines := []string{"What you remember from your conversations with " +
			e.persona.User.Name + ", oldest first:"}
		for _, m := range told {
			lines = append(lines, memoryLine(m, loc))
		}
		sections = append(sections, strings.Join(lines, "\n"))
	}
	if summary != nil && summary.Content != "" {
		sections = append(sections, "Earlier in your conversation with "+
			e.persona.User.Name+":\n"+summary.Content)
	}
	return api.Text(api.RoleSystem, strings.Join(sections, "\n\n"))
}

// memoryLine is one memory as a model reads it, dated by the day it was said
// rather than the day a fold wrote it down.
func memoryLine(m store.Memory, loc *time.Location) string {
	return "- (said on " + dateText(loc, m.SaidAt) + ") " + m.Content
}

// remembered is the memories a prompt tells: the newest that fit the room
// memories have, in the order they were said.
func remembered(all []store.Memory, room int, ratio float64, loc *time.Location) []store.Memory {
	var taken int
	for i, a := range slices.Backward(all) {
		taken += size([]api.Message{api.Text(api.RoleSystem, memoryLine(a, loc))}, ratio, 0)
		if taken > room {
			return all[i+1:]
		}
	}
	return all
}

// summary is the summary that counts, and nil while the conversation has never
// been folded.
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

// exchange is the messages of one exchange as the model reads them. Every
// message she was sent is told the time before it: when an older one was sent,
// and what time it is now before the one she is answering, whose last line is
// then what was said rather than a time. Each of those stands once it is
// written, so a host that keeps a prompt keeps all of it but the time before
// the last message.
func (e *Engine) exchange(ctx context.Context, a *attempt, group []store.Message, inline, last store.MessageID) []api.Message {
	out := make([]api.Message, 0, 2*len(group))
	for _, msg := range group {
		if msg.Role == store.RoleUser {
			var when string
			if msg.ID == last {
				when = e.now()
			} else {
				when = e.sentAt(msg.CreatedAt)
			}
			out = append(out, api.Text(api.RoleSystem, when))
		}
		out = append(out, e.message(ctx, a, msg, inline))
	}
	return out
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
	for _, msg := range slices.Backward(messages) {
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

// inlineImages is how many pictures of an exchange go to the model as
// pictures rather than as the line that describes them, which is what they
// weigh in a prompt.
func inlineImages(group []store.Message, inline store.MessageID) int {
	if inline == 0 {
		return 0
	}
	var n int
	for _, msg := range group {
		if msg.Role == store.RoleUser && msg.ID >= inline {
			n += len(msg.Images())
		}
	}
	return n
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
