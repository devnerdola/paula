package conversation

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/runners/api"
	"nerdola.dev/x/paula/internal/store"
)

// purpose is what a request was sent for, which its cache key carries.
func purpose(req api.ChatRequest) string {
	_, out, _ := strings.Cut(strings.TrimPrefix(req.CacheKey, "paula-"), "-")
	return out
}

// compacting answers a reply with reply, and a compaction with summary.
func compacting(reply, summary string) func(context.Context, api.ChatRequest, func(api.Chunk) error) (*api.Result, error) {
	return func(_ context.Context, req api.ChatRequest, fn func(api.Chunk) error) (*api.Result, error) {
		out := reply
		if purpose(req) == store.PurposeSummary {
			out = summary
		}
		if err := fn(api.Chunk{Kind: api.ChunkText, Text: out}); err != nil {
			return nil, err
		}
		return &api.Result{FinishReason: "stop"}, nil
	}
}

// long is one message of a conversation, long enough that some of them take
// the history of a 2,000-token context past its reservation, and short enough
// that the one that does takes it little past.
var long = strings.Repeat("a long thing to say ", 5)

// overflowing says the history is past its reservation, which is what sets a
// compaction off once a turn ends.
func overflowing(t *testing.T, e *Engine) bool {
	t.Helper()
	ctx := context.Background()
	m, err := e.roleModel(ctx, config.RoleChat)
	if err != nil {
		t.Fatal(err)
	}
	over, err := e.overflowed(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	return over
}

// settle waits for the compaction a turn set off, if it set one off. Wait
// answers once the turn is over, and the compaction goes on beside the
// conversation until the history is within its reservation, or until it fails
// and says so.
func settle(t *testing.T, r *replyEngine) {
	t.Helper()
	waitFor(t, "the compaction after the turn", func() bool {
		return !overflowing(t, r.Engine) || seen(r.Engine, ReplyFailed)
	})
}

// talkPast talks until the history has been compacted, and answers with the
// summary that stands then.
func talkPast(t *testing.T, r *replyEngine) *store.Summary {
	t.Helper()
	for i := range 30 {
		r.say(t, fmt.Sprintf("message %d: %s", i, long))
		settle(t, r)
		if s, err := r.store.LatestSummary(context.Background()); err == nil {
			return s
		}
	}
	t.Fatal("the history was never compacted")
	return nil
}

// Nothing is compacted while the history is within its reservation. The turn
// that takes it past goes out with the whole history, and the compaction
// follows that turn: the next one carries the summary and nothing it covers.
func TestAHistoryPastItsReservationIsCompactedAfterTheTurn(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: compacting("hm", "they said things")}
	r := openReplyWith(t, f, sized(f, 2000))
	ctx := context.Background()

	summary := talkPast(t, r)
	if summary.Content != "they said things" {
		t.Errorf("summary = %q", summary.Content)
	}

	requests := f.all()
	first := -1
	for i, req := range requests {
		if purpose(req) == store.PurposeSummary {
			first = i
			break
		}
	}
	// The turn before the compaction went out whole: every message said until
	// then, and nothing left out.
	before := requests[first-1]
	if purpose(before) != store.PurposeReply {
		t.Fatalf("the request before the compaction was for %s, want the reply that overflowed", purpose(before))
	}
	sent := said(before, api.RoleUser)
	if len(sent) == 0 || !strings.HasPrefix(sent[0], "message 0:") {
		t.Errorf("the turn that overflowed sent %d messages starting %q, want the whole history", len(sent), sent)
	}

	// The summary covers every message of that turn, so the next one carries
	// none of them: its history is empty.
	messages, err := r.store.Messages(ctx, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if last := messages[len(messages)-1].ID; summary.UptoMessageID != last {
		t.Errorf("the summary covers up to %d, want every message up to %d", summary.UptoMessageID, last)
	}
	r.say(t, "and then?")
	req := f.replied()
	if card := text(req.Messages[0]); !strings.Contains(card, "Earlier in your conversation with Caio:\nthey said things") {
		t.Errorf("the system message is %q, want the summary in it", card)
	}
	if got := said(req, api.RoleUser); !reflect.DeepEqual(got, []string{"and then?"}) {
		t.Errorf("the turn after the compaction sent %q, want only its own message", got)
	}
}

// The compaction carries the summary so far and the whole history, each
// message by who said it and what was said. It writes no memories.
func TestACompactionIsTheSummaryAndTheWholeHistory(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: compacting("hm", "they said things")}
	r := openReplyWith(t, f, sized(f, 2000))
	ctx := context.Background()

	talkPast(t, r)
	talked, err := r.store.Messages(ctx, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	talkPast2 := func() {
		for i := range 30 {
			r.say(t, fmt.Sprintf("again %d: %s", i, long))
			settle(t, r)
			if len(f.sentFor(store.PurposeSummary)) == 2 {
				return
			}
		}
		t.Fatal("the history was never compacted a second time")
	}
	talkPast2()

	compactions := f.sentFor(store.PurposeSummary)
	firstSaid := text(compactions[0].Messages[1])
	if strings.Contains(firstSaid, "Summary so far:") {
		t.Errorf("the first compaction said %q, want no summary before there was one", firstSaid)
	}
	for _, msg := range talked {
		name := r.persona.User.Name
		if msg.Role == store.RoleAssistant {
			name = r.persona.Name
		}
		if !strings.Contains(firstSaid, name+": "+msg.Text()) {
			t.Errorf("the first compaction was not given message %d", msg.ID)
		}
	}
	secondSaid := text(compactions[1].Messages[1])
	if !strings.Contains(secondSaid, "Summary so far:\nthey said things") {
		t.Errorf("the second compaction said %q, want the summary so far in it", secondSaid)
	}
	if strings.Contains(secondSaid, "message 0:") {
		t.Error("the second compaction was given messages the first had covered")
	}
	memories, err := r.store.Memories(ctx)
	if err != nil || len(memories) != 0 {
		t.Errorf("memories = %+v, %v, want none", memories, err)
	}
}

// wordsAsked is how a compaction says how many words a part is written back
// in.
var wordsAsked = regexp.MustCompile(`Write about (\d+) words`)

// The summary is written to fill its reservation, so the words a part is asked
// for grow with summary_ratio: three times the ratio is three times the words.
func TestTheSummaryIsAskedToFillItsReservation(t *testing.T) {
	asked := func(ratio float64) int {
		t.Helper()
		f := &fakeRunner{model: chatModel(), chat: compacting("hm", "they said things")}
		cfg := config.DefaultEngine()
		cfg.SummaryRatio = ratio
		r := openReplyOffering(t, f, sized(f, 2000), cfg)
		talkPast(t, r)
		prompt := text(f.sentFor(store.PurposeSummary)[0].Messages[0])
		found := wordsAsked.FindStringSubmatch(prompt)
		if found == nil {
			t.Fatalf("the compaction was asked %q, without a number of words", prompt)
		}
		n, _ := strconv.Atoi(found[1])
		return n
	}
	// Each reservation is rounded down to whole tokens, and each number of
	// words to a whole word, which leaves the two a little off three to one.
	small, large := asked(0.1), asked(0.3)
	if off := math.Abs(float64(large - 3*small)); off > max(3, float64(3*small)/100) {
		t.Errorf("the summary was asked for %d words at 0.1 and %d at 0.3, want three times as many", small, large)
	}
}

// manyWords is a text of n words.
func manyWords(n int) string { return strings.TrimSpace(strings.Repeat("word ", n)) }

// keeping answers a reply with "hm", and each part of a compaction with as many
// words as it asks for, the way a model that keeps to them does.
func keeping(_ context.Context, req api.ChatRequest, fn func(api.Chunk) error) (*api.Result, error) {
	out := "hm"
	if purpose(req) == store.PurposeSummary {
		found := wordsAsked.FindStringSubmatch(text(req.Messages[0]))
		if found == nil {
			return nil, fmt.Errorf("the compaction was asked %q, without a number of words", text(req.Messages[0]))
		}
		n, _ := strconv.Atoi(found[1])
		out = manyWords(n)
	}
	if err := fn(api.Chunk{Kind: api.ChunkText, Text: out}); err != nil {
		return nil, err
	}
	return &api.Result{FinishReason: "stop"}, nil
}

// A model does not keep to the words it is asked for. A summary written past
// its reservation is written again on its own, and what that writes is the
// summary.
func TestASummaryPastItsReservationIsWrittenAgainOnItsOwn(t *testing.T) {
	// The model writes twice the words it is asked for from messages, and
	// keeps to them from a summary on its own.
	f := &fakeRunner{model: chatModel(), chat: func(ctx context.Context, req api.ChatRequest, fn func(api.Chunk) error) (*api.Result, error) {
		if purpose(req) != store.PurposeSummary {
			return keeping(ctx, req, fn)
		}
		n, _ := strconv.Atoi(wordsAsked.FindStringSubmatch(text(req.Messages[0]))[1])
		if strings.Contains(text(req.Messages[1]), "Messages to add:") {
			n *= 2
		}
		if err := fn(api.Chunk{Kind: api.ChunkText, Text: manyWords(n)}); err != nil {
			return nil, err
		}
		return &api.Result{FinishReason: "stop"}, nil
	}}
	r := openReplyWith(t, f, sized(f, 2000))

	summary := talkPast(t, r)
	compactions := f.sentFor(store.PurposeSummary)
	last := compactions[len(compactions)-1]
	carried := text(last.Messages[1])
	if !strings.HasPrefix(carried, "Summary so far:\n") || strings.Contains(carried, "Messages to add:") {
		t.Fatalf("the last request of the compaction carried %q, want the summary on its own", carried)
	}
	written := strings.TrimPrefix(carried, "Summary so far:\n")
	n, _ := strconv.Atoi(wordsAsked.FindStringSubmatch(text(last.Messages[0]))[1])
	if got := wordsIn(summary.Content); got != n || n >= wordsIn(written) {
		t.Errorf("the summary is %d words, written again from %d in %d, want what it was written again in, and fewer",
			got, wordsIn(written), n)
	}
}

// partAsked is how a compaction says which part a request is, and of how many.
var partAsked = regexp.MustCompile(`part (\d+) of (\d+)`)

// partOf is which part of a compaction a request is, and of how many.
func partOf(req api.ChatRequest) (part, parts int) {
	found := partAsked.FindStringSubmatch(text(req.Messages[0]))
	if found == nil {
		return 0, 0
	}
	part, _ = strconv.Atoi(found[1])
	parts, _ = strconv.Atoi(found[2])
	return part, parts
}

// partsOfTheLast is the summary requests of the latest compaction, in the
// order of its parts. They are sent at once, so the order they came in says
// nothing.
func partsOfTheLast(f *fakeRunner) []api.ChatRequest {
	var out []api.ChatRequest
	for _, req := range f.all() {
		switch {
		case purpose(req) == store.PurposeSummary:
			out = append(out, req)
		case len(out) > 0 && purpose(req) == store.PurposeReply:
			out = nil
		}
	}
	slices.SortStableFunc(out, func(a, b api.ChatRequest) int {
		i, _ := partOf(a)
		j, _ := partOf(b)
		return i - j
	})
	return out
}

// A summary that fills its reservation and a history past its own are more than
// one request can hold with what it writes back, so the compaction is written in
// parts. Each carries the next of what is compacted, in order and once, and the
// summary is what the parts wrote, in order.
func TestACompactionTheContextCannotHoldIsWrittenInParts(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: keeping}
	r := openReplyWith(t, f, sized(f, 2000))
	ctx := context.Background()

	talkPast(t, r)
	for i := 0; len(f.sentFor(store.PurposeSummary)) < 2 || len(partsOfTheLast(f)) == len(f.sentFor(store.PurposeSummary)); i++ {
		if i == 30 {
			t.Fatal("the history was never compacted a second time")
		}
		r.say(t, fmt.Sprintf("again %d: %s", i, long))
		settle(t, r)
	}
	parts := partsOfTheLast(f)
	if len(parts) < 2 {
		t.Fatalf("the second compaction was %d request, want it in parts", len(parts))
	}

	var carried, wrote []string
	for i, req := range parts {
		prompt := text(req.Messages[0])
		if !strings.Contains(prompt, fmt.Sprintf("part %d of %d", i+1, len(parts))) {
			t.Errorf("part %d was asked %q, want it told which part it is", i+1, prompt)
		}
		carried = append(carried, text(req.Messages[1]))
		n, _ := strconv.Atoi(wordsAsked.FindStringSubmatch(prompt)[1])
		wrote = append(wrote, manyWords(n))
	}
	if !strings.HasPrefix(carried[0], "Summary so far:\n") {
		t.Errorf("the first part carried %q, want the summary so far first", carried[0])
	}
	all := strings.Join(carried, "\n")
	for i := range 30 {
		said := fmt.Sprintf("again %d: %s", i, long)
		if n := strings.Count(all, said); n > 1 {
			t.Errorf("%q was carried %d times, want once", said, n)
		}
	}
	last := parts[len(parts)-1]
	messages, err := r.store.Messages(ctx, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if said := messages[0].Text(); !strings.Contains(text(last.Messages[1]), said) {
		t.Errorf("the last part carried %q, want the last message of the history, %q", text(last.Messages[1]), said)
	}
	summary, err := r.store.LatestSummary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.Join(wrote, "\n\n"); summary.Content != want {
		t.Errorf("summary = %q, want what the parts wrote, in order", summary.Content)
	}
}

// A part is asked for no more than the model writes in one answer, as its
// catalogue says or as the file's max_tokens holds it to, whichever is less,
// however much the context would hold: the same compaction the context holds
// in one request is cut into parts for a model that writes little at once.
func TestAPartIsNoMoreThanTheModelWritesAtOnce(t *testing.T) {
	parts := func(output, maxTokens int) int {
		t.Helper()
		f := &fakeRunner{model: chatModel(), chat: keeping}
		f.model.Output = output
		set := sized(f, 2000)
		if maxTokens > 0 {
			set.Models[0].Settings.Output.MaxTokens = &maxTokens
		}
		r := openReplyWith(t, f, set)
		talkPast(t, r)
		return len(partsOfTheLast(f))
	}
	if whole := parts(0, 0); whole != 1 {
		t.Errorf("the compaction was %d requests for a model nothing says how much it writes at once, want one", whole)
	}
	for _, tc := range []struct {
		output, maxTokens int
		held              string
	}{
		{100, 0, "a catalogue that says 100 tokens"},
		{0, 100, "a max_tokens of 100"},
		{8000, 100, "a max_tokens of 100 under a catalogue that says 8,000"},
		{100, 8000, "a catalogue that says 100 under a max_tokens of 8,000"},
	} {
		if n := parts(tc.output, tc.maxTokens); n < 2 {
			t.Errorf("the compaction was %d request for a model held to %s, want parts", n, tc.held)
		}
	}
}

// The parts of a compaction are sent at once, so it takes as long as the
// slowest of them and not all of them one after another. The model answers a
// part only once every part has reached it, and a part sent on its own waits
// for others that never come.
func TestThePartsOfACompactionAreSentAtOnce(t *testing.T) {
	var (
		mu      sync.Mutex
		waiting []chan struct{}
	)
	together := func(ctx context.Context, req api.ChatRequest, fn func(api.Chunk) error) (*api.Result, error) {
		if purpose(req) == store.PurposeSummary {
			_, parts := partOf(req)
			here := make(chan struct{})
			mu.Lock()
			waiting = append(waiting, here)
			if len(waiting) == parts {
				for _, w := range waiting {
					close(w)
				}
				waiting = nil
			}
			mu.Unlock()
			select {
			case <-here:
			case <-time.After(time.Second):
				mu.Lock()
				waiting = slices.DeleteFunc(waiting, func(w chan struct{}) bool { return w == here })
				mu.Unlock()
				return nil, errors.New("the part was sent on its own")
			}
		}
		return keeping(ctx, req, fn)
	}
	f := &fakeRunner{model: chatModel(), chat: together}
	f.model.Output = 100
	r := openReplyWith(t, f, sized(f, 2000))

	for i := 0; len(f.sentFor(store.PurposeSummary)) == 0; i++ {
		if i == 30 {
			t.Fatal("the history was never compacted")
		}
		r.say(t, fmt.Sprintf("message %d: %s", i, long))
		settle(t, r)
	}
	if _, err := r.store.LatestSummary(context.Background()); err != nil {
		t.Fatalf("the compaction left no summary: %v", err)
	}
	if parts := partsOfTheLast(f); len(parts) < 2 {
		t.Errorf("the compaction was %d request, want it in parts", len(parts))
	}
}

// A compaction carries the conversation as a transcript, not the way a reply
// does, so what the host counted it at says nothing about what the history
// costs: the history is measured at the rate the replies left.
func TestACompactionsCountLeavesTheRateAsTheRepliesLeftIt(t *testing.T) {
	counted := func(ctx context.Context, req api.ChatRequest, fn func(api.Chunk) error) (*api.Result, error) {
		res, err := compacting("hm", "they said things")(ctx, req, fn)
		if res != nil && purpose(req) == store.PurposeSummary {
			res.Usage.PromptTokens = 1000000
		}
		return res, err
	}
	f := &fakeRunner{model: chatModel(), chat: counted}
	r := openReplyWith(t, f, sized(f, 2000))
	ctx := context.Background()
	m, err := r.roleModel(ctx, config.RoleChat)
	if err != nil {
		t.Fatal(err)
	}

	var before float64
	for i := 0; ; i++ {
		if i == 30 {
			t.Fatal("the history was never compacted")
		}
		before = r.costs.rate(m.Name)
		r.say(t, fmt.Sprintf("message %d: %s", i, long))
		settle(t, r)
		if _, err := r.store.LatestSummary(ctx); err == nil {
			break
		}
	}
	if after := r.costs.rate(m.Name); after != before {
		t.Errorf("a word costs %v after the compaction, want %v, what the replies left", after, before)
	}
}

// A compaction answers no message: its entry names none, and what the
// conversation has been answered up to is what the replies say.
func TestACompactionAnswersNoMessage(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: compacting("hm", "they said things")}
	r := openReplyWith(t, f, sized(f, 2000))
	ctx := context.Background()

	talkPast(t, r)
	entries, err := r.store.Entries(ctx, 1)
	if err != nil || len(entries) == 0 {
		t.Fatalf("entries = %+v, %v", entries, err)
	}
	if entry := entries[0]; entry.UptoMessageID != 0 || entry.AfterMessageID != 0 {
		t.Errorf("the compaction's entry = %+v, want one that answers nothing", entry)
	}
	answered, err := r.store.AnsweredUpto(ctx)
	if err != nil {
		t.Fatal(err)
	}
	last, err := r.store.LastMessage(ctx, store.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	if answered != last.ID {
		t.Errorf("answered up to %d, want the last message a reply answered, %d", answered, last.ID)
	}
}

// held answers a compaction only once the test lets it go, and says when it has
// been asked for one.
type held struct {
	asked   chan struct{}
	release chan struct{}
	once    atomic.Bool
}

func (h *held) chat(ctx context.Context, req api.ChatRequest, fn func(api.Chunk) error) (*api.Result, error) {
	if purpose(req) == store.PurposeSummary {
		if h.once.CompareAndSwap(false, true) {
			close(h.asked)
		}
		select {
		case <-h.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return compacting("hm", "they said things")(ctx, req, fn)
}

// wasAsked says a compaction has been asked for.
func (h *held) wasAsked() bool {
	select {
	case <-h.asked:
		return true
	default:
		return false
	}
}

// talkUntilAsked talks until a turn takes the history over and the compaction
// after it is asked for, which h holds. After each turn the history is
// measured beside the loop, so what follows a turn is either the compaction
// being asked for or the history within its reservation.
func talkUntilAsked(t *testing.T, r *replyEngine, h *held) {
	t.Helper()
	for i := 0; !h.wasAsked(); i++ {
		if i == 30 {
			t.Fatal("the history was never compacted")
		}
		post(t, r.Engine, fmt.Sprintf("message %d: %s", i, long))
		r.clock.Advance(config.DefaultEngine().Debounce.Duration())
		waitFor(t, "the turn and what follows it", func() bool {
			if h.wasAsked() {
				return true
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
			defer cancel()
			_, err := r.Wait(ctx)
			return err == nil && !overflowing(t, r.Engine)
		})
	}
}

// A message that arrives while the history is being compacted waits for it:
// its turn starts once the summary is stored, and carries it.
func TestAMessageDuringACompactionWaitsForIt(t *testing.T) {
	h := &held{asked: make(chan struct{}), release: make(chan struct{})}
	f := &fakeRunner{model: chatModel(), chat: h.chat}
	r := openReplyWith(t, f, sized(f, 2000))

	talkUntilAsked(t, r, h)
	replies := len(f.sentFor(store.PurposeReply))

	post(t, r.Engine, "are you there?")
	r.clock.Advance(config.DefaultEngine().Debounce.Duration())
	// Nothing to wait on shows the turn has not started; what shows it is that
	// it starts once the compaction is let go, and not before.
	if n := len(f.sentFor(store.PurposeReply)); n != replies {
		t.Fatalf("%d replies went out during the compaction, want none", n-replies)
	}
	close(h.release)
	if _, err := r.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	req := f.replied()
	if card := text(req.Messages[0]); !strings.Contains(card, "they said things") {
		t.Errorf("the turn that waited went out with %q, want the summary in it", card)
	}
	if got := said(req, api.RoleUser); !reflect.DeepEqual(got, []string{"are you there?"}) {
		t.Errorf("the turn that waited sent %q, want only its own message", got)
	}
}

// A message that arrives while a turn waits for a compaction has a wait of its
// own, and the turn starts once that wait is over: the two messages get one
// reply between them.
func TestAMessageWhileATurnWaitsForACompactionIsAnsweredByThatTurn(t *testing.T) {
	h := &held{asked: make(chan struct{}), release: make(chan struct{})}
	f := &fakeRunner{model: chatModel(), chat: h.chat}
	r := openReplyWith(t, f, sized(f, 2000))
	ctx := context.Background()
	for range 20 {
		asked := add(t, r, store.RoleUser, long)
		add(t, r, store.RoleAssistant, long)
		ended(t, r, asked)
	}
	debounce := config.DefaultEngine().Debounce.Duration()

	post(t, r.Engine, "are you there?")
	r.clock.Advance(debounce)
	waitFor(t, "the compaction the turn waits for", h.wasAsked)
	post(t, r.Engine, "hello?")
	close(h.release)
	waitFor(t, "the compaction", func() bool {
		_, err := r.store.LatestSummary(ctx)
		return err == nil
	})
	r.clock.Advance(debounce)
	if _, err := r.Wait(ctx); err != nil {
		t.Fatal(err)
	}

	replies := f.sentFor(store.PurposeReply)
	if len(replies) != 1 {
		t.Fatalf("%d replies went out, want one for both messages", len(replies))
	}
	if got := said(replies[0], api.RoleUser); !reflect.DeepEqual(got, []string{"are you there?", "hello?"}) {
		t.Errorf("the reply answered %q, want both messages", got)
	}
}

// A compaction no turn waits for keeps nothing waiting: once the reply that
// set it off is done the conversation is quiet, and a frontend asks for the
// next message while the compaction goes on.
func TestACompactionNoTurnWaitsForKeepsNothingWaiting(t *testing.T) {
	h := &held{asked: make(chan struct{}), release: make(chan struct{})}
	f := &fakeRunner{model: chatModel(), chat: h.chat}
	r := openReplyWith(t, f, sized(f, 2000))

	talkUntilAsked(t, r, h)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := r.Wait(ctx); err != nil {
		t.Errorf("waiting while the compaction went on = %v, want the conversation quiet", err)
	}
}

// A stop ends a turn that waits for a compaction: the conversation is quiet
// once it is stopped, and the compaction goes on, since the history still
// needs it. No reply goes out when it ends.
func TestAStopEndsATurnThatWaitsForACompaction(t *testing.T) {
	h := &held{asked: make(chan struct{}), release: make(chan struct{})}
	f := &fakeRunner{model: chatModel(), chat: h.chat}
	r := openReplyWith(t, f, sized(f, 2000))
	ctx := context.Background()

	talkUntilAsked(t, r, h)
	replies := len(f.sentFor(store.PurposeReply))
	post(t, r.Engine, "are you there?")
	r.clock.Advance(config.DefaultEngine().Debounce.Duration())
	if stopped, err := r.Stop(ctx); err != nil || !stopped {
		t.Fatalf("stopping the turn = %v, %v, want it stopped", stopped, err)
	}
	quiet, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if _, err := r.Wait(quiet); err != nil {
		t.Errorf("waiting after the stop = %v, want the conversation quiet while the compaction goes on", err)
	}

	close(h.release)
	waitFor(t, "the compaction", func() bool {
		_, err := r.store.LatestSummary(ctx)
		return err == nil
	})
	if _, err := r.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(f.sentFor(store.PurposeReply)); n != replies {
		t.Errorf("%d replies went out for the turn that was stopped, want none", n-replies)
	}
}

// A compaction that fails is said the way a failed reply is, and the message
// waiting on it is left unanswered. The next input has the history measured
// and compacted again before its turn, which answers both.
func TestACompactionThatFailsIsSaidAndTriedAgainOnTheNextInput(t *testing.T) {
	var failing atomic.Bool
	failing.Store(true)
	answer := compacting("hm", "they said things")
	f := &fakeRunner{model: chatModel(), chat: func(ctx context.Context, req api.ChatRequest, fn func(api.Chunk) error) (*api.Result, error) {
		if purpose(req) == store.PurposeSummary && failing.Load() {
			return nil, errors.New("the host is away")
		}
		return answer(ctx, req, fn)
	}}
	r := openReplyWith(t, f, sized(f, 2000))
	ctx := context.Background()

	for i := range 30 {
		r.say(t, fmt.Sprintf("message %d: %s", i, long))
		settle(t, r)
		if len(f.sentFor(store.PurposeSummary)) > 0 {
			break
		}
	}
	if len(f.sentFor(store.PurposeSummary)) == 0 {
		t.Fatal("the history was never compacted")
	}
	waitFor(t, "the failure", func() bool { return seen(r.Engine, ReplyFailed) })
	var failure string
	for _, ev := range published(r.Engine) {
		if ev.Kind == ReplyFailed {
			failure = ev.Text
		}
	}
	if !strings.Contains(failure, "the host is away") {
		t.Errorf("the failure was said as %q, want the host's error", failure)
	}

	// A message after the failure waits on the history being measured again,
	// and that fails too: it is left unanswered.
	replies := len(f.sentFor(store.PurposeReply))
	r.say(t, "hello?")
	if n := len(f.sentFor(store.PurposeReply)); n != replies {
		t.Errorf("%d replies went out on a history past its reservation, want none", n-replies)
	}
	if len(f.sentFor(store.PurposeSummary)) != 2 {
		t.Errorf("the compaction was tried %d times, want again on the input after it failed", len(f.sentFor(store.PurposeSummary)))
	}

	failing.Store(false)
	r.say(t, "hello again")
	if _, err := r.store.LatestSummary(ctx); err != nil {
		t.Fatalf("no summary after the compaction went through: %v", err)
	}
	if got := said(f.replied(), api.RoleUser); !reflect.DeepEqual(got, []string{"hello?", "hello again"}) {
		t.Errorf("the turn after it went through sent %q, want both messages that waited", got)
	}
}

// A part the model stopped writing at the most it writes in one answer has
// lost the end of what it summarises, so the compaction fails and no summary
// is written from it.
func TestACompactionWithAPartCutOffFails(t *testing.T) {
	answer := compacting("hm", "they said things")
	f := &fakeRunner{model: chatModel(), chat: func(ctx context.Context, req api.ChatRequest, fn func(api.Chunk) error) (*api.Result, error) {
		res, err := answer(ctx, req, fn)
		if res != nil && purpose(req) == store.PurposeSummary {
			// The finish reason both APIs document for an answer stopped by
			// max_tokens.
			res.FinishReason = "length"
		}
		return res, err
	}}
	r := openReplyWith(t, f, sized(f, 2000))

	for i := 0; len(f.sentFor(store.PurposeSummary)) == 0; i++ {
		if i == 30 {
			t.Fatal("the history was never compacted")
		}
		r.say(t, fmt.Sprintf("message %d: %s", i, long))
		settle(t, r)
	}
	waitFor(t, "the failure", func() bool { return seen(r.Engine, ReplyFailed) })
	if s, err := r.store.LatestSummary(context.Background()); err == nil {
		t.Errorf("a part cut off was written as the summary %q", s.Content)
	}
}

// A run picks the conversation up with its history as the last one left it,
// which may be past this run's reservation: the first turn has it measured,
// and compacted, before it starts.
func TestARunMeasuresTheHistoryBeforeItsFirstTurn(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: compacting("hm", "they said things")}
	r := openReplyWith(t, f, sized(f, 2000))
	ctx := context.Background()
	// A history written before this run's first turn, as a run before it left
	// one.
	for range 20 {
		asked := add(t, r, store.RoleUser, long)
		add(t, r, store.RoleAssistant, long)
		ended(t, r, asked)
	}
	r.say(t, "hello")
	if _, err := r.store.LatestSummary(ctx); err != nil {
		t.Fatalf("no summary before the first turn: %v", err)
	}
	if got := said(f.replied(), api.RoleUser); !reflect.DeepEqual(got, []string{"hello"}) {
		t.Errorf("the first turn sent %q, want its message after the compaction", got)
	}
}

// A picture is measured as what a host bills for it, so a history of short
// messages with pictures overflows as surely as one of long ones. The
// compaction is told what the pictures showed.
func TestAHistoryIsMeasuredWithItsPictures(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: compacting("hm", "they looked at pictures")}
	f.model.Vision = true
	eyes := &fakeRunner{model: visionModel(), chat: says("a red square")}
	set := withVision(f, eyes)
	set.Models[0].Context = 3000
	r := openReplyWith(t, f, set)
	ctx := context.Background()

	for _, said := range []string{"one", "two", "three"} {
		sendPhoto(t, r, said, photo(t))
	}
	summary, err := r.store.LatestSummary(ctx)
	if err != nil {
		t.Fatalf("three pictures did not take the history over: %v", err)
	}
	if summary.Content != "they looked at pictures" {
		t.Errorf("summary = %q", summary.Content)
	}
	shown := text(f.sentFor(store.PurposeSummary)[0].Messages[1])
	if !strings.Contains(shown, "one\n[photo: a red square]") {
		t.Errorf("the compaction was shown:\n%s\nwant each picture by what it showed", shown)
	}
}

// A summary is never asked for more words than it is written from. A history
// of pictures fills its reservation with what a host bills for them, and is
// written by what they showed, which is short.
func TestASummaryIsNeverAskedLongerThanWhatItIsWrittenFrom(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: compacting("hm", "they looked at pictures")}
	f.model.Vision = true
	eyes := &fakeRunner{model: visionModel(), chat: says("a red square")}
	set := withVision(f, eyes)
	set.Models[0].Context = 3000
	r := openReplyWith(t, f, set)

	for _, said := range []string{"one", "two", "three"} {
		sendPhoto(t, r, said, photo(t))
	}
	compactions := f.sentFor(store.PurposeSummary)
	if len(compactions) == 0 {
		t.Fatal("three pictures did not take the history over")
	}
	found := wordsAsked.FindStringSubmatch(text(compactions[0].Messages[0]))
	if found == nil {
		t.Fatalf("the compaction was asked %q, without a number of words", text(compactions[0].Messages[0]))
	}
	asked, _ := strconv.Atoi(found[1])
	if given := len(strings.Fields(text(compactions[0].Messages[1]))); asked > given {
		t.Errorf("the compaction was asked for %d words, written from %d", asked, given)
	}
}

// add stores one message of the conversation, and answers with its number.
func add(t *testing.T, r *replyEngine, role, text string) store.MessageID {
	t.Helper()
	m := &store.Message{Role: role, Channel: "repl", CreatedAt: r.clock.Now(),
		Parts: []store.Part{{Type: store.PartText, Text: text}}}
	if err := r.store.AddMessage(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	return m.ID
}

// ended writes down an entry that answered up to a message, which is what says
// the conversation has been answered that far.
func ended(t *testing.T, r *replyEngine, upto store.MessageID) {
	t.Helper()
	ctx := context.Background()
	entry := &store.Entry{Channel: "repl", UptoMessageID: upto, StartedAt: r.clock.Now()}
	if err := r.store.StartEntry(ctx, entry); err != nil {
		t.Fatal(err)
	}
	entry.Status, entry.EndedAt = store.StatusDone, r.clock.Now()
	if err := r.store.EndEntry(ctx, entry); err != nil {
		t.Fatal(err)
	}
}

// A summary covers the history up to the newest of it older than every message
// still to be answered: a message sent while a reply was being written is
// stored before that reply, and a summary that took the reply would cover a
// message nobody has answered.
func TestASummaryCoversNothingStillToBeAnswered(t *testing.T) {
	said := []store.Message{
		{ID: 1, Role: store.RoleUser},
		{ID: 3, Role: store.RoleAssistant, ReplyTo: 1},
	}
	if got := coversUpto(said, []store.Message{{ID: 2, Role: store.RoleUser}}); got != 1 {
		t.Errorf("covered up to %d, want the newest older than what is still to be answered", got)
	}
	// Nothing is still to be answered, so it covers the whole history.
	if got := coversUpto(said, nil); got != 3 {
		t.Errorf("covered up to %d, want all of it", got)
	}
}

// A reply written while the next message arrived is newer than that message,
// so the summary does not cover it. It stays in the history, and the
// compaction is not given it: nothing is both summarised and carried.
func TestACompactionIsGivenOnlyWhatTheSummaryCovers(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: compacting("hm", "they said things")}
	r := openReplyWith(t, f, sized(f, 2000))
	for range 20 {
		asked := add(t, r, store.RoleUser, long)
		add(t, r, store.RoleAssistant, long)
		ended(t, r, asked)
	}
	asked := add(t, r, store.RoleUser, "is it raining?")
	add(t, r, store.RoleUser, "never mind")
	add(t, r, store.RoleAssistant, "it is pouring")
	ended(t, r, asked)
	r.say(t, "hello")

	compactions := f.sentFor(store.PurposeSummary)
	if len(compactions) == 0 {
		t.Fatal("the history was never compacted")
	}
	for _, part := range compactions {
		if given := text(part.Messages[1]); strings.Contains(given, "it is pouring") {
			t.Errorf("the compaction was given the reply newer than a message still to be answered:\n%s", given)
		}
	}
	if carried := said(f.replied(), api.RoleAssistant); !slices.Contains(carried, "it is pouring") {
		t.Errorf("the turn after the compaction carried %q, want the reply the summary does not cover", carried)
	}
}
