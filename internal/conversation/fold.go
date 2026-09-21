package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/runners/api"
	"nerdola.dev/x/paula/internal/store"
)

// memoryPrompt and summaryPrompt are what the chat model is asked of the
// messages a fold takes. CHAR, USER, LANGUAGE and WORDS are filled in from the
// card and the room the summary has.
const memoryPrompt = `You keep the long-term memory of a conversation between CHAR and USER, who text each other.
- From the new messages, write down the lasting facts: facts about USER, and what CHAR said about themselves.
- Leave out plans, appointments and passing moments, which the summary keeps.
- Write each memory as one sentence in the third person, naming who it is about, in LANGUAGE, and give the number of the message it was said in.
- When a new memory updates or contradicts stored memories, list their numbers in ` + "`replaces`" + `.
- Reply only with the JSON object {"memories": [{"content": "...", "message": 3, "replaces": [2]}]}, with an empty list when there is nothing to remember.`

const summaryPrompt = `You keep the summary of a conversation between CHAR and USER, who text each other.
- Write the summary again from the summary so far and the messages to add: what they talked about and what happened, in order, with plans and feelings as they came up, and days and times as they were said.
- Compress older parts more than recent ones.
- Use at most WORDS words, in LANGUAGE, in the third person and past tense.
- Reply with the summary only.`

// compactionPrompt is what a summary that has outgrown its room is written
// again by. It is given the summary and nothing else, so it says what to let
// go of rather than what to add, and says not to add anything: a rewrite is
// where a model is most tempted to fill a gap it cannot see.
const compactionPrompt = `You keep the summary of a conversation between CHAR and USER, who text each other.
- The summary below has outgrown the room it has. Write it again, shorter, from the summary alone.
- Keep what still bears on them: what they did and decided, what is still ahead of them, how things stand between them, and the days and times already written down.
- Let go of the rest: passing remarks, detail that changes nothing now, and anything said twice.
- Take the most from the oldest parts and the least from the newest.
- Add nothing that is not there already, and leave nothing in that the words you have do not cover.
- Use at most WORDS words, in LANGUAGE, in the third person and past tense.
- Reply with the summary only.`

// foldWait is how long the next fold waits after one failed, doubling up to
// foldMost. Rate limits and outages usually clear within minutes, and replies
// go on meanwhile.
const (
	foldWait = 30 * time.Second
	foldMost = 10 * time.Minute
)

// worked is one piece of the work behind a reply reporting back to the loop.
// Keeping the prompt inside the context and embedding the memories are told
// apart: they are asked of different models, and a host that is away for one
// has nothing to say about the other.
type worked struct {
	embedding bool
	err       error
}

// fold folds the oldest of the conversation away, a step at a time, until what
// is left fits the share of the context the messages have. It runs beside the
// loop, so a reply is never held up by one.
func (e *Engine) fold(ctx context.Context) error {
	for {
		more, err := e.foldStep(ctx)
		if err != nil || !more {
			return err
		}
	}
}

// compact writes the summary again from itself when it has outgrown the room
// it has of the system message. A fold is what makes it longer, so this
// follows one.
func (e *Engine) compact(ctx context.Context) error {
	m, err := e.roleModel(ctx, config.RoleChat)
	if err != nil {
		return err
	}
	summary, err := e.summary(ctx)
	if err != nil || summary == nil {
		return err
	}
	memories, err := e.store.Memories(ctx)
	if err != nil {
		return err
	}
	ratio := e.costs.ratio(m.Name)
	room := e.summaryRoom(m, memories, ratio)
	was := size([]api.Message{api.Text(api.RoleSystem, summary.Content)}, ratio, 0)
	if room <= 0 || was <= room {
		// Nothing bounds the system message, or the summary is inside what it
		// has. Either way there is nothing to write again.
		return nil
	}

	now := was
	entry, err := e.inEntry(ctx, func(a *attempt) error {
		written, err := e.summarise(ctx, a, m, summary, nil, room)
		if err != nil {
			return err
		}
		now = size([]api.Message{api.Text(api.RoleSystem, written)}, ratio, 0)
		if now >= was {
			// It kept its length, so the one it was written from stands rather
			// than being replaced by something no shorter. Writing it again
			// straight away would ask the same thing of the same model and get
			// the same answer; the wait a failure earns is what stops that.
			return fmt.Errorf("the summary is %d tokens, no shorter than it was, and its room is %d", now, room)
		}
		// It covers the same messages as the one it was written from, since
		// nothing was added to it.
		return e.store.Fold(context.WithoutCancel(ctx), &store.Summary{
			UptoMessageID: summary.UptoMessageID,
			Content:       written,
			EntryID:       a.entry.ID,
			CreatedAt:     e.clock.Now(),
		}, nil)
	})
	if err != nil {
		return err
	}

	e.log.Info("the summary is written again", "entry", entry.ID,
		"tokens", now, "was", was, "room", room)
	return nil
}

// inEntry runs one piece of the work behind a reply in an entry of its own: a
// fold, a summary written again, or a batch of memories turned into vectors.
// None of them answers a message, so the entry names none and what the
// conversation has been answered up to is untouched by it. What the work
// learned about itself outlives the context it was cut off in, so the entry is
// closed whichever way it went.
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

// foldStep folds one chunk: the oldest whole exchanges the messages can spare
// become memories and a summary, stored together. It reports whether the
// messages that are left still take more than their share.
func (e *Engine) foldStep(ctx context.Context) (bool, error) {
	m, err := e.roleModel(ctx, config.RoleChat)
	if err != nil {
		return false, err
	}
	_, history := e.split(m)
	if history <= 0 {
		// Nothing says what the model holds, so nothing says the conversation
		// has outgrown it.
		return false, nil
	}
	summary, err := e.summary(ctx)
	if err != nil {
		return false, err
	}
	messages, err := e.store.MessagesAfter(ctx, coveredUpto(summary))
	if err != nil {
		return false, err
	}

	// Every exchange is measured as a prompt carries it, which is what a fold
	// is deciding about: the text of what was said, and what a picture sent as
	// a picture costs. Nothing here loads one or asks what one shows — how many
	// there are is enough, and an exchange heavy with pictures is one a prompt
	// cannot carry however short its words are.
	ratio, image := e.costs.ratio(m.Name), e.costs.image(m.Name)
	said := ordered(messages)
	inline := e.inlineFrom(said, m)
	groups := exchanges(said)
	sizes := make([]int, len(groups))
	for i, group := range groups {
		sizes[i] = size([]api.Message{api.Text(api.RoleUser, e.chunkText(group))}, ratio, 0) +
			inlineImages(group, inline)*image
	}
	chunk := chunkOf(sizes, share(history, e.cfg.HistoryKeep), history)
	if chunk == 0 {
		// Nothing older than the exchange she is answering, or what is there
		// is already what a fold leaves behind. There is no work here, so
		// there is no entry for it either.
		return false, nil
	}

	var more bool
	_, err = e.inEntry(ctx, func(a *attempt) error {
		var err error
		more, err = e.foldOnce(ctx, a, m, summary, groups, sizes, chunk, ratio)
		return err
	})
	return more, err
}

// foldOnce is the work of a step, once it has an entry to record under.
func (e *Engine) foldOnce(ctx context.Context, a *attempt, m *model, summary *store.Summary, groups [][]store.Message, sizes []int, chunk int, ratio float64) (bool, error) {
	taken := slices.Concat(groups[:chunk]...)
	upto := coversUpto(taken, slices.Concat(groups[chunk:]...))
	stored, err := e.store.Memories(ctx)
	if err != nil {
		return false, err
	}

	learned, err := e.remember(ctx, a, m, stored, taken)
	if err != nil {
		return false, err
	}
	written, err := e.summarise(ctx, a, m, summary, taken, e.summaryRoom(m, stored, ratio))
	if err != nil {
		return false, err
	}

	now := e.clock.Now()
	for i := range learned {
		learned[i].EntryID = a.entry.ID
		learned[i].CreatedAt = now
	}
	next := &store.Summary{
		UptoMessageID: upto,
		Content:       written,
		EntryID:       a.entry.ID,
		CreatedAt:     now,
	}
	if err := e.store.Fold(context.WithoutCancel(ctx), next, learned); err != nil {
		return false, err
	}
	e.log.Info("the oldest of the conversation is folded into the summary",
		"entry", a.entry.ID, "exchanges", chunk, "upto", upto, "memories", len(learned))

	// Another step is due while what is left is more than a fold keeps.
	var left int
	for _, n := range sizes[chunk:] {
		left += n
	}
	_, history := e.split(m)
	return left > share(history, e.cfg.HistoryKeep), nil
}

// coversUpto is the message a summary of the chunk speaks for: the newest of
// it older than everything left behind. A reply written while a message
// arrived carries the higher id of the two, so the last of the chunk in the
// order it is read is not always the last of it by id, and a summary that took
// that one would cover a message it never folded.
func coversUpto(taken, left []store.Message) store.MessageID {
	oldest := left[0].ID
	for _, msg := range left {
		oldest = min(oldest, msg.ID)
	}
	var upto store.MessageID
	for _, msg := range taken {
		if msg.ID < oldest {
			upto = max(upto, msg.ID)
		}
	}
	return upto
}

// chunkOf is how many of the oldest exchanges one step folds away: as many as
// it takes to leave keep behind, while the chunk itself stays inside one
// prompt's worth. The newest exchange is never one of them, since a fold that
// took everything would leave a reply nothing to answer.
//
// The first one it takes is taken whatever its size. An exchange longer than a
// step may carry would otherwise stop every fold that followed it, for good
// and without a word, while every prompt dropped the oldest of the
// conversation instead; a step too long for the model is refused by the host,
// which says so and is tried again.
func chunkOf(sizes []int, keep, most int) int {
	var left int
	for _, n := range sizes {
		left += n
	}
	var chunk, taken int
	for i := 0; i < len(sizes)-1 && left > keep; i++ {
		if chunk > 0 && taken+sizes[i] > most {
			break
		}
		taken += sizes[i]
		left -= sizes[i]
		chunk++
	}
	return chunk
}

// summaryRoom is what the summary has of the system message: what is left of
// it once the card and the memories are written.
func (e *Engine) summaryRoom(m *model, stored []store.Memory, ratio float64) int {
	system, _ := e.split(m)
	if system <= 0 {
		return 0
	}
	left := max(0, system-size([]api.Message{api.Text(api.RoleSystem, e.rendered)}, ratio, 0))
	loc := e.clock.Now().Location()
	told := remembered(stored, share(left, e.cfg.MemoryRatio), ratio, loc)
	for _, mem := range told {
		left -= size([]api.Message{api.Text(api.RoleSystem, memoryLine(mem, loc))}, ratio, 0)
	}
	return max(0, left)
}

// memoryReply is what the model answers the memory request with.
type memoryReply struct {
	Memories []struct {
		Content  string  `json:"content"`
		Message  int64   `json:"message"`
		Replaces []int64 `json:"replaces"`
	} `json:"memories"`
}

// remember asks the model what lasting facts the chunk holds.
func (e *Engine) remember(ctx context.Context, a *attempt, m *model, stored []store.Memory, chunk []store.Message) ([]store.Memory, error) {
	var b strings.Builder
	if len(stored) > 0 {
		b.WriteString("Stored memories:\n")
		for _, mem := range stored {
			fmt.Fprintf(&b, "[%d] (said on %s) %s\n",
				mem.ID, dateText(e.clock.Now().Location(), mem.SaidAt), mem.Content)
		}
		b.WriteString("\n")
	}
	b.WriteString("New messages:\n")
	b.WriteString(e.chunkText(chunk))

	text, err := e.answer(ctx, a, m, store.PurposeMemories,
		e.fill(memoryPrompt, 0), b.String())
	if err != nil {
		return nil, err
	}

	var reply memoryReply
	if err := json.Unmarshal([]byte(strings.TrimSpace(text)), &reply); err != nil {
		return nil, fmt.Errorf("the memories came back as something other than the JSON object asked for: %w", err)
	}

	here := make(map[store.MessageID]bool, len(chunk))
	for _, msg := range chunk {
		here[msg.ID] = true
	}
	shown := make(map[store.MemoryID]bool, len(stored))
	for _, mem := range stored {
		shown[mem.ID] = true
	}

	out := make([]store.Memory, 0, len(reply.Memories))
	for _, mem := range reply.Memories {
		if strings.TrimSpace(mem.Content) == "" {
			continue
		}
		// A memory of a message that was not shown, or replacing one that was
		// not, is a model that lost its place: the step fails rather than
		// storing it.
		if !here[store.MessageID(mem.Message)] {
			return nil, fmt.Errorf("a memory names message %d, which was not among the messages", mem.Message)
		}
		next := store.Memory{Content: strings.TrimSpace(mem.Content), Source: store.MessageID(mem.Message)}
		for _, id := range mem.Replaces {
			if !shown[store.MemoryID(id)] {
				return nil, fmt.Errorf("a memory replaces memory %d, which was not among the stored ones", id)
			}
			next.Replaces = append(next.Replaces, store.MemoryID(id))
		}
		out = append(out, next)
	}
	return out, nil
}

// summarise asks the model to write the summary again, with the chunk added,
// or to write it again from itself when there is no chunk.
func (e *Engine) summarise(ctx context.Context, a *attempt, m *model, summary *store.Summary, chunk []store.Message, room int) (string, error) {
	var b strings.Builder
	if summary != nil && summary.Content != "" {
		b.WriteString("Summary so far:\n")
		b.WriteString(summary.Content)
		b.WriteString("\n\n")
	}
	// A compaction adds nothing, so it leaves out the section that would list
	// what to add, and asks by a prompt of its own: the one a fold uses names
	// messages it would not be given.
	prompt, purpose := compactionPrompt, store.PurposeCompaction
	if len(chunk) > 0 {
		prompt, purpose = summaryPrompt, store.PurposeSummary
		b.WriteString("Messages to add:\n")
		b.WriteString(e.chunkText(chunk))
	}

	text, err := e.answer(ctx, a, m, purpose,
		e.fill(prompt, e.words(b.String(), room, e.costs.ratio(m.Name))), b.String())
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(text) == "" {
		return "", errors.New("the summary came back empty")
	}
	return strings.TrimSpace(text), nil
}

// words is how many words the summary may take: the characters its room comes
// to, at what a word of this conversation has been costing. A summary nothing
// bounds is held to the words its own input takes.
func (e *Engine) words(input string, room int, ratio float64) int {
	count := len(strings.Fields(input))
	if count == 0 {
		count = 1
	}
	perWord := max(1, utf8.RuneCountInString(input)/count)
	if room <= 0 {
		return count
	}
	return max(1, int(float64(room)/ratio)/perWord)
}

// chunkText is how the messages of a chunk are written to a model: the number
// they are named by, when they were sent, and who said what.
func (e *Engine) chunkText(chunk []store.Message) string {
	loc := e.clock.Now().Location()
	var b strings.Builder
	for _, msg := range chunk {
		name := e.persona.User.Name
		if msg.Role == store.RoleAssistant {
			name = e.persona.Name
		}
		fmt.Fprintf(&b, "#%d %s %s: %s\n",
			msg.ID, timeText(loc, msg.CreatedAt), name, msg.Text())
	}
	return b.String()
}

// fill puts the card's names and language, and the words a summary may take,
// into a prompt.
func (e *Engine) fill(prompt string, words int) string {
	out := strings.NewReplacer(
		"CHAR", e.persona.Name,
		"USER", e.persona.User.Name,
		"LANGUAGE", e.persona.Language,
	).Replace(prompt)
	if words > 0 {
		out = strings.Replace(out, "WORDS", strconv.Itoa(words), 1)
	}
	return out
}

// answer sends one request of a fold and reads back what the model wrote.
func (e *Engine) answer(ctx context.Context, a *attempt, m *model, purpose, system, said string) (string, error) {
	var text strings.Builder
	_, err := m.Runner.Chat(ctx, api.ChatRequest{
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
	return text.String(), err
}
