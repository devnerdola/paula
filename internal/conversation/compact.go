package conversation

import (
	"context"
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"

	"nerdola.dev/x/paula/internal/runners/api"
	"nerdola.dev/x/paula/internal/store"
)

// summaryPrompt is what the chat model is asked for each part of a compaction.
// CHAR, USER and LANGUAGE are filled in from the card, PART and PARTS say which
// part it is, and WORDS is how many words the part is written back in.
const summaryPrompt = `You keep the summary of a conversation between CHAR and USER, who text each other.
- Below is part PART of PARTS of it, in order: the summary so far, the messages added since, or some of each.
- Write it again as a summary: what they talked about and what happened, in order, with plans and feelings as they came up, and days and times as they were said.
- Compress older parts more than recent ones.
- Write about WORDS words, in LANGUAGE, in the third person and past tense.
- Reply with the summary only.`

// history is the conversation before the user input: the summary, and the
// messages after it that have been answered, each reply after what it answers.
// rest are the messages sent since, which the next turn answers.
func (e *Engine) history(ctx context.Context) (summary *store.Summary, said, rest []store.Message, err error) {
	summary, err = e.summary(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	messages, err := e.store.MessagesAfter(ctx, coveredUpto(summary))
	if err != nil {
		return nil, nil, nil, err
	}
	answered, err := e.store.AnsweredUpto(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, msg := range messages {
		if msg.Asked() && msg.ID > answered {
			rest = append(rest, msg)
			continue
		}
		said = append(said, msg)
	}
	return summary, ordered(said), rest, nil
}

// overflowed says the history has passed its reservation, counted the way a
// prompt carries it.
func (e *Engine) overflowed(ctx context.Context, m *model) (bool, error) {
	_, reserved := e.reservations(m)
	if reserved <= 0 {
		return false, nil
	}
	summary, said, _, err := e.history(ctx)
	if err != nil || len(said) == 0 {
		return false, err
	}
	weighing := e.weighing(ctx)
	if weighing.thought, err = e.pastThought(ctx, m, coveredUpto(summary)); err != nil {
		return false, err
	}
	msgs, _ := e.render(said, m.catalogue.Vision, 0, m.notes(), weighing)
	return size(msgs, e.costs.rate(m.Name), e.costs.image(m.Name)) > reserved, nil
}

// compact compresses the whole history, with the summary so far, into a new
// summary written to fill the summary's reservation. The history is empty
// after it, but for what is newer than a message still to be answered. It
// writes no memories.
func (e *Engine) compact(ctx context.Context, m *model) error {
	summary, said, rest, err := e.history(ctx)
	if err != nil {
		return err
	}
	room, _ := e.reservations(m)
	upto := coversUpto(said, rest)
	// What the summary does not cover stays in the history, so it is not written
	// into the summary as well.
	said = slices.DeleteFunc(said, func(msg store.Message) bool { return msg.ID > upto })
	if len(said) == 0 {
		return nil
	}
	entry, err := e.inEntry(ctx, func(a *attempt) error {
		written, err := e.summarise(ctx, a, m, summary, said, room)
		if err != nil {
			return err
		}
		// A model does not keep to the words it is asked for, and a summary
		// past its reservation takes from what the user input is held to, so
		// it is written again on its own.
		if size([]api.Message{api.Text(api.RoleSystem, written)}, e.costs.rate(m.Name), 0) > room {
			if written, err = e.summarise(ctx, a, m, &store.Summary{Content: written}, nil, room); err != nil {
				return err
			}
		}
		return e.store.Fold(context.WithoutCancel(ctx), &store.Summary{UptoMessageID: upto, Content: written})
	})
	if err != nil {
		return err
	}
	e.log.Info("the history is compacted into the summary", "entry", entry.ID, "upto", upto)
	return nil
}

// inEntry runs a compaction in an entry of its own. It answers no message, so
// the entry names none and what the conversation has been answered up to is
// untouched by it. What the work learned about itself outlives the context it
// was cut off in, so the entry is closed whichever way it went.
func (e *Engine) inEntry(ctx context.Context, work func(*attempt) error) (*store.Entry, error) {
	entry := &store.Entry{StartedAt: e.clock.Now()}
	if err := e.store.StartEntry(ctx, entry); err != nil {
		return nil, err
	}
	err := work(&attempt{entry: entry})
	entry.EndedAt = e.clock.Now()
	entry.Status = store.StatusDone
	if err != nil {
		entry.Status = store.StatusFailed
		entry.Error = err.Error()
	}
	if eerr := e.store.EndEntry(context.WithoutCancel(ctx), entry); eerr != nil && err == nil {
		err = eerr
	}
	return entry, err
}

// coversUpto is the message a summary of the history speaks for: the newest of
// it older than every message still to be answered. A reply written while a
// message arrived carries the higher id of the two, so the last of the history
// in the order it is read is not always the last of it by id, and a summary
// that took that one would cover a message nobody has answered.
func coversUpto(said, rest []store.Message) store.MessageID {
	oldest := store.MessageID(math.MaxInt64)
	for _, msg := range rest {
		oldest = min(oldest, msg.ID)
	}
	var upto store.MessageID
	for _, msg := range said {
		if msg.ID < oldest {
			upto = max(upto, msg.ID)
		}
	}
	return upto
}

// summarise asks the model for the summary written again with the messages
// added. Its reservation comes to as many words as it holds at what a word
// costs, and share is how much the text has to shrink to come to those words:
// a third when it is three times as long. The text is cut into parts, in
// order, and each part is asked back at that fraction of its words, rounded
// down, so the parts together fill the reservation and are never asked for
// more than it holds. The parts are sent at once, so the compaction takes as
// long as the slowest of them. A text shorter than the reservation is asked
// back at its own length, since a summary longer than what it is written from
// is made up. A history of pictures is one: it is measured by what a host
// bills for them, and carried by what they showed.
func (e *Engine) summarise(ctx context.Context, a *attempt, m *model, summary *store.Summary, said []store.Message, room int) (string, error) {
	pieces := e.pieces(ctx, summary, said)
	share := min(1, float64(room)/e.costs.rate(m.Name)/float64(wordsOf(pieces)))
	parts := e.cut(m, pieces, share)

	out := make([]string, len(parts))
	errs := make([]error, len(parts))
	var wg sync.WaitGroup
	for i, part := range parts {
		wg.Go(func() {
			words := int(share * float64(wordsOf(part)))
			got, err := e.answer(ctx, a, m, store.PurposeSummary, e.fill(i+1, len(parts), words), written(part))
			if err == nil && strings.TrimSpace(got) == "" {
				err = errors.New("the summary came back empty")
			}
			out[i], errs[i] = strings.TrimSpace(got), err
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return "", err
		}
	}
	return strings.Join(out, "\n\n"), nil
}

// fits says a part fits one request: the prompt, the part, and what the model
// writes back of it at share, all within the context, and what it writes back
// within what the model writes in one answer.
func (e *Engine) fits(m *model, part []piece, share float64) bool {
	rate := e.costs.rate(m.Name)
	prompt := size([]api.Message{api.Text(api.RoleSystem, e.fill(1, 1, 1))}, rate, 0)
	back := int(math.Ceil(share * float64(wordsOf(part)) * rate))
	if out := m.output(); out > 0 && back > out {
		return false
	}
	carried := int(math.Ceil(float64(wordsIn(written(part))) * rate))
	return prompt+carried+back <= m.limit()
}

// wordsOf is how many words pieces hold.
func wordsOf(pieces []piece) int {
	var n int
	for _, p := range pieces {
		n += wordsIn(p.text)
	}
	return n
}

// cut splits pieces, in order, into as few parts of about the same size as each
// fit a request. A piece is never split, so one larger than that is a part of
// its own.
func (e *Engine) cut(m *model, pieces []piece, share float64) [][]piece {
	for n := 1; ; n++ {
		parts := split(pieces, n)
		fit := !slices.ContainsFunc(parts, func(p []piece) bool { return !e.fits(m, p, share) })
		if fit || n >= len(pieces) {
			return parts
		}
	}
}

// piece is one paragraph of the summary so far, or one message of the history
// with the day it was sent: what a compaction is cut at.
type piece struct {
	summary bool
	day     string
	text    string
}

// pieces is what a compaction is written from: the summary so far by
// paragraph, and then the history by message, each with its time, who said it,
// and a line for every picture, by what it showed.
func (e *Engine) pieces(ctx context.Context, summary *store.Summary, said []store.Message) []piece {
	var out []piece
	if summary != nil {
		for p := range strings.SplitSeq(summary.Content, "\n\n") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, piece{summary: true, text: p})
			}
		}
	}
	loc := e.clock.Now().Location()
	for _, msg := range said {
		at := msg.CreatedAt.In(loc).Format("15:04 ")
		if msg.Role == store.RoleCallback {
			out = append(out, piece{day: dateText(loc, msg.CreatedAt), text: at + "(call back due: " + msg.Text() + ")"})
			continue
		}
		name := e.persona.User.Name
		if msg.Role == store.RoleAssistant {
			name = e.persona.Name
		}
		var lines []string
		if text := msg.Text(); text != "" {
			lines = append(lines, text)
		}
		for _, p := range msg.Images() {
			lines = append(lines, e.known(ctx, p.SHA256))
		}
		out = append(out, piece{
			day:  dateText(loc, msg.CreatedAt),
			text: at + name + ": " + strings.Join(lines, "\n"),
		})
	}
	return out
}

// written is pieces as a model reads them: the summary so far, and then the
// messages under the day they were sent. A day is named once, so the words of a
// message are what it said rather than when.
func written(pieces []piece) string {
	var summary, messages []string
	var day string
	for _, p := range pieces {
		if p.summary {
			summary = append(summary, p.text)
			continue
		}
		if p.day != day {
			messages = append(messages, p.day)
			day = p.day
		}
		messages = append(messages, p.text)
	}
	var sections []string
	if len(summary) > 0 {
		sections = append(sections, "Summary so far:\n"+strings.Join(summary, "\n\n"))
	}
	if len(messages) > 0 {
		sections = append(sections, "Messages to add:\n"+strings.Join(messages, "\n"))
	}
	return strings.Join(sections, "\n\n")
}

// split splits pieces, in order, into n parts of about the same size.
func split(pieces []piece, n int) [][]piece {
	whole := wordsOf(pieces)
	out := make([][]piece, 0, n)
	var part []piece
	var done int
	for _, p := range pieces {
		part = append(part, p)
		done += wordsIn(p.text)
		if len(out) < n-1 && done*n >= whole*(len(out)+1) {
			out = append(out, part)
			part = nil
		}
	}
	if len(part) > 0 {
		out = append(out, part)
	}
	return out
}

// fill puts the card's names and language, which part of how many a request is
// for, and the words it is written in, into the summary's prompt.
func (e *Engine) fill(part, parts, words int) string {
	return strings.NewReplacer(
		"CHAR", e.persona.Name,
		"USER", e.persona.User.Name,
		"LANGUAGE", e.persona.Language,
		"PARTS", strconv.Itoa(parts),
		"PART", strconv.Itoa(part),
		"WORDS", strconv.Itoa(words),
	).Replace(summaryPrompt)
}

// answer sends the request of a compaction and reads back what the model
// wrote. What the host counted it at is not read for what a word costs: the
// history is measured as a reply carries it, and a compaction carries it as a
// transcript. A part cut off at the most the model writes has lost the end of
// what it summarises, so it fails the compaction.
func (e *Engine) answer(ctx context.Context, a *attempt, m *model, purpose, system, said string) (string, error) {
	var text strings.Builder
	res, err := m.Runner.Chat(ctx, api.ChatRequest{
		Model:    m.ID,
		Settings: m.settings,
		CacheKey: e.cacheKey(purpose),
		Recorder: e.recorder(a, purpose),
		Messages: []api.Message{
			api.Text(api.RoleSystem, system),
			api.Text(api.RoleUser, said),
		},
	}, func(c api.Chunk) error {
		if c.Kind == api.ChunkText {
			text.WriteString(c.Text)
		}
		return nil
	})
	if err == nil && res.FinishReason == api.FinishLength {
		err = errors.New("a part of the summary was cut off at the most the model writes in one answer")
	}
	return text.String(), err
}
