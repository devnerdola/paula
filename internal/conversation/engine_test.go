package conversation

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/persona"
	"nerdola.dev/x/paula/internal/runners/api"
	"nerdola.dev/x/paula/internal/store"
)

// fakeClock lets a test move time on instead of waiting for it.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

// berlin is the zone the tests read times in, since a clock says what time it
// is and where.
var berlin = time.FixedZone("UTC+02:00", 2*60*60)

func newClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 9, 16, 20, 22, 0, 0, time.UTC).In(berlin)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) NewTimer(d time.Duration) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{clock: c, c: make(chan time.Time, 1), at: c.now.Add(d), on: true}
	c.timers = append(c.timers, t)
	return t
}

// Advance moves the clock on and fires every timer that comes due.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	now := c.now
	var due []*fakeTimer
	for _, t := range c.timers {
		if t.on && !t.at.After(now) {
			t.on = false
			due = append(due, t)
		}
	}
	c.mu.Unlock()

	for _, t := range due {
		select {
		case t.c <- now:
		default:
		}
	}
}

type fakeTimer struct {
	clock *fakeClock
	c     chan time.Time
	at    time.Time
	on    bool
}

func (t *fakeTimer) Chan() <-chan time.Time { return t.c }

func (t *fakeTimer) Reset(d time.Duration) {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	t.at = t.clock.now.Add(d)
	t.on = true
}

func (t *fakeTimer) Stop() {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	t.on = false
}

// chatFunc is what a runner answers with, which is where a test takes hold of
// a reply.
type chatFunc = func(ctx context.Context, req api.ChatRequest, fn func(api.Chunk) error) (*api.Result, error)

// replies is a runner a test drives: it says when a reply has reached it,
// holds the reply there until the test lets go, and answers with the text the
// test set.
type replies struct {
	mu    sync.Mutex
	calls int

	// start is closed by the first reply to reach the runner, and hold keeps
	// every reply there until a test closes it.
	start chan struct{}
	hold  chan struct{}
	// begun writes the text out before waiting, so the loop reads the reply as
	// one that has got somewhere.
	begun bool
	text  string
	// failures is how many replies come back as an error before one answers.
	failures int
}

func (a *replies) chat(ctx context.Context, _ api.ChatRequest, fn func(api.Chunk) error) (*api.Result, error) {
	a.mu.Lock()
	a.calls++
	failing := a.failures > 0
	if failing {
		a.failures--
	}
	start, hold, begun, text := a.start, a.hold, a.begun, a.text
	a.mu.Unlock()

	if failing {
		return nil, errHostAway
	}
	if text == "" {
		text = "hey you"
	}
	say := func() error { return fn(api.Chunk{Kind: api.ChunkText, Text: text}) }
	if begun {
		if err := say(); err != nil {
			return nil, err
		}
	}
	if start != nil {
		select {
		case <-start:
		default:
			close(start)
		}
	}
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !begun {
		if err := say(); err != nil {
			return nil, err
		}
	}
	return &api.Result{FinishReason: "stop"}, nil
}

func (a *replies) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

var errHostAway = errors.New("the host went away")

// replyTo is the message the latest reply answered, as the store holds it.
func replyTo(t *testing.T, st *store.Store) store.MessageID {
	t.Helper()
	m, err := st.LastMessage(context.Background(), store.RoleAssistant)
	if err != nil {
		t.Fatal(err)
	}
	return m.ReplyTo
}

// card is the character every test writes to.
func card() *persona.Card {
	return &persona.Card{
		ID:   "paula",
		Name: "Paula",
		User: persona.User{Name: "Caio"},
	}
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// open builds an engine that writes its replies through a runner answering
// with chat, and starts its loop. The loop's state is built before it runs,
// which is what the package's own constructor is for.
func open(t *testing.T, st *store.Store, c *fakeClock, chat chatFunc) *Engine {
	t.Helper()
	return openWith(t, st, c, config.DefaultEngine(), chat)
}

func openWith(t *testing.T, st *store.Store, c *fakeClock, cfg config.Engine, chat chatFunc) *Engine {
	t.Helper()
	if chat == nil {
		chat = (&replies{}).chat
	}
	e, l, err := newEngine(context.Background(), Options{
		Store:   st,
		Runners: setup(&fakeRunner{model: chatModel(), chat: chat}),
		Engine:  cfg,
		Persona: card(),
		Clock:   c,
	})
	if err != nil {
		t.Fatal(err)
	}
	go l.run()
	t.Cleanup(func() { e.Close() })
	return e
}

func post(t *testing.T, e *Engine, text string) {
	t.Helper()
	err := e.Post(context.Background(), NewMessage{Channel: "repl", Text: text})
	if err != nil {
		t.Fatal(err)
	}
}

// waitFor gives the loop a moment to reach a state a test is about to check.
func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for range 200 {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestABurstGetsOneReply(t *testing.T) {
	a := &replies{}
	c := newClock()
	st := newStore(t)
	e := open(t, st, c, a.chat)

	post(t, e, "one")
	post(t, e, "two")
	post(t, e, "three")
	if a.count() != 0 {
		t.Fatalf("a reply started before the wait was over")
	}

	c.Advance(config.DefaultEngine().Debounce.Duration())
	waitFor(t, "the reply", func() bool { return a.count() == 1 })

	if _, err := e.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a.count() != 1 {
		t.Errorf("replies = %d, want one for the three messages", a.count())
	}
	if got := replyTo(t, st); got != 3 {
		t.Errorf("answered up to %d, want the last message", got)
	}
}

func TestAMessageRestartsAReplyThatHasNotBegun(t *testing.T) {
	a := &replies{start: make(chan struct{}), hold: make(chan struct{})}
	c := newClock()
	st := newStore(t)
	e := open(t, st, c, a.chat)

	post(t, e, "one")
	c.Advance(config.DefaultEngine().Debounce.Duration())
	<-a.start

	post(t, e, "two")

	// The first reply is cancelled, and the wait starts again.
	waitFor(t, "the restart", func() bool { return a.count() == 1 && seen(e, ReplyRestarted) })
	c.Advance(config.DefaultEngine().Debounce.Duration())
	waitFor(t, "the second reply", func() bool { return a.count() == 2 })

	close(a.hold)
	waitFor(t, "the reply to be stored", func() bool { return seen(e, ReplyDone) })
	if got := replyTo(t, st); got != 2 {
		t.Errorf("answered up to %d, want both messages", got)
	}
}

func TestAMessageDoesNotRestartAReplyThatHasBegun(t *testing.T) {
	a := &replies{begun: true, start: make(chan struct{}), hold: make(chan struct{})}
	c := newClock()
	e := open(t, newStore(t), c, a.chat)

	post(t, e, "one")
	c.Advance(config.DefaultEngine().Debounce.Duration())
	<-a.start
	post(t, e, "two")

	close(a.hold)
	waitFor(t, "the first reply to end", func() bool { return seen(e, ReplyDone) })
	if seen(e, ReplyRestarted) {
		t.Error("a reply that had begun was restarted")
	}
}

func TestPrefillCancelOff(t *testing.T) {
	a := &replies{start: make(chan struct{}), hold: make(chan struct{})}
	c := newClock()
	cfg := config.DefaultEngine()
	cfg.PrefillCancel = false
	e := openWith(t, newStore(t), c, cfg, a.chat)

	post(t, e, "one")
	c.Advance(cfg.Debounce.Duration())
	<-a.start
	post(t, e, "two")
	close(a.hold)

	waitFor(t, "the reply to end", func() bool { return seen(e, ReplyDone) })
	if seen(e, ReplyRestarted) {
		t.Error("the reply was restarted with prefill_cancel off")
	}
}

func TestStopEndsTheReplyBeingWritten(t *testing.T) {
	a := &replies{start: make(chan struct{}), hold: make(chan struct{})}
	c := newClock()
	e := open(t, newStore(t), c, a.chat)

	post(t, e, "one")
	c.Advance(config.DefaultEngine().Debounce.Duration())
	<-a.start

	stopped, err := e.Stop(context.Background())
	if err != nil || !stopped {
		t.Fatalf("Stop = %v, %v", stopped, err)
	}
	waitFor(t, "the reply to stop", func() bool { return seen(e, ReplyStopped) })
}

func TestStopBeforeTheReplyStarts(t *testing.T) {
	a := &replies{}
	c := newClock()
	e := open(t, newStore(t), c, a.chat)

	post(t, e, "one")
	stopped, err := e.Stop(context.Background())
	if err != nil || !stopped {
		t.Fatalf("Stop = %v, %v", stopped, err)
	}

	c.Advance(config.DefaultEngine().Debounce.Duration() * 2)
	if _, err := e.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a.count() != 0 {
		t.Errorf("replies = %d, want none after stopping", a.count())
	}
	if !seen(e, ReplyStopped) {
		t.Error("nothing said the reply was stopped")
	}
}

func TestStopWithNothingToStop(t *testing.T) {
	e := open(t, newStore(t), newClock(), nil)
	stopped, err := e.Stop(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stopped {
		t.Error("Stop = true with nothing being written")
	}
}

func TestWaitReturnsWhenNothingIsPending(t *testing.T) {
	a := &replies{}
	c := newClock()
	e := open(t, newStore(t), c, a.chat)

	post(t, e, "one")
	done := make(chan Seq, 1)
	go func() {
		seq, err := e.Wait(context.Background())
		if err != nil {
			t.Error(err)
		}
		done <- seq
	}()

	select {
	case <-done:
		t.Fatal("Wait returned while a reply was pending")
	case <-time.After(50 * time.Millisecond):
	}

	c.Advance(config.DefaultEngine().Debounce.Duration())
	select {
	case seq := <-done:
		latest, _, err := e.Standing(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if seq != latest {
			t.Errorf("Wait = %d, want the latest event %d", seq, latest)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not return")
	}
}

func TestAFailedReply(t *testing.T) {
	c := newClock()
	e := open(t, newStore(t), c, (&replies{failures: 1}).chat)

	post(t, e, "one")
	c.Advance(config.DefaultEngine().Debounce.Duration())
	if _, err := e.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}

	var failed Event
	for _, ev := range published(e) {
		if ev.Kind == ReplyFailed {
			failed = ev
		}
	}
	if failed.Text != errHostAway.Error() {
		t.Errorf("failure = %q, want the error as it is", failed.Text)
	}
}

func TestAMessageLeftUnansweredIsAnsweredAtTheNextStart(t *testing.T) {
	st := newStore(t)
	c := newClock()
	a := &replies{}

	first := open(t, st, c, a.chat)
	post(t, first, "one")
	first.Close()
	if a.count() != 0 {
		t.Fatal("a reply was written before the run ended")
	}

	b := &replies{}
	second := open(t, st, c, b.chat)
	c.Advance(config.DefaultEngine().Debounce.Duration())
	waitFor(t, "the reply", func() bool { return b.count() == 1 })
	if _, err := second.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := replyTo(t, st); got != 1 {
		t.Errorf("answered up to %d, want the message left behind", got)
	}
}

// A new run picks the conversation up where the last one left it.
func TestAMessageAnsweredIsNotAnsweredAgain(t *testing.T) {
	st := newStore(t)
	c := newClock()
	a := &replies{}

	first := open(t, st, c, a.chat)
	post(t, first, "one")
	c.Advance(config.DefaultEngine().Debounce.Duration())
	waitFor(t, "the reply", func() bool { return a.count() == 1 })
	if _, err := first.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	first.Close()

	b := &replies{}
	second := open(t, st, c, b.chat)
	c.Advance(config.DefaultEngine().Debounce.Duration())
	if _, err := second.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b.count() != 0 {
		t.Errorf("replies = %d, want the answered message left alone", b.count())
	}
}

func TestAnEntryLeftRunningIsEnded(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	entry := &store.Entry{StartedAt: time.Now()}
	if err := st.StartEntry(ctx, entry); err != nil {
		t.Fatal(err)
	}

	open(t, st, newClock(), nil)

	got, err := st.Entry(ctx, entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.StatusFailed {
		t.Errorf("status = %q, want it ended", got.Status)
	}
	if got.Error == "" || got.EndedAt.IsZero() {
		t.Errorf("entry = %+v", got)
	}
}

func TestAMessageIsStoredAndPublished(t *testing.T) {
	st := newStore(t)
	e := open(t, st, newClock(), nil)
	post(t, e, "look at this")

	var stored Event
	for _, ev := range published(e) {
		if ev.Kind == MessageStored {
			stored = ev
		}
	}
	if stored.Message == nil || stored.Message.Text() != "look at this" {
		t.Fatalf("event = %+v", stored)
	}
	if stored.Channel != "repl" {
		t.Errorf("channel = %q", stored.Channel)
	}

	history, err := e.History(context.Background(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].ID != stored.Message.ID {
		t.Errorf("history = %+v", history)
	}
}

// events reads everything published so far.
func published(e *Engine) []Event {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out []Event
	last, _, err := e.Standing(ctx)
	if err != nil || last == 0 {
		return nil
	}
	for ev, err := range e.Events(ctx, 0) {
		if err != nil {
			continue
		}
		out = append(out, ev)
		if ev.Seq >= last {
			break
		}
	}
	return out
}

func seen(e *Engine, kind Kind) bool {
	for _, ev := range published(e) {
		if ev.Kind == kind {
			return true
		}
	}
	return false
}

// A message sent while a reply was being written is one nobody has answered
// when that reply fails: the failure answered nothing, and the message is not
// the failure's to leave behind.
func TestAMessageSentWhileAFailedReplyWasWrittenIsAnswered(t *testing.T) {
	st := newStore(t)
	c := newClock()
	started := make(chan struct{})
	hold := make(chan struct{})
	var tries atomic.Int64
	// The first reply writes something and then fails, so the message that
	// arrives meanwhile cannot restart it and is left with nothing.
	chat := func(_ context.Context, _ api.ChatRequest, fn func(api.Chunk) error) (*api.Result, error) {
		if tries.Add(1) == 1 {
			if err := fn(api.Chunk{Kind: api.ChunkText, Text: "one moment"}); err != nil {
				return nil, err
			}
			close(started)
			<-hold
			return nil, errHostAway
		}
		if err := fn(api.Chunk{Kind: api.ChunkText, Text: "here I am"}); err != nil {
			return nil, err
		}
		return &api.Result{FinishReason: "stop"}, nil
	}
	e := open(t, st, c, chat)

	post(t, e, "one")
	c.Advance(config.DefaultEngine().Debounce.Duration())
	<-started

	// This lands while the reply that is about to fail is being written.
	post(t, e, "two")
	close(hold)
	waitFor(t, "the failure", func() bool { return seen(e, ReplyFailed) })

	// Nothing else is said, and the message that was left over is answered.
	waitFor(t, "the reply to what was left over", func() bool {
		c.Advance(config.DefaultEngine().Debounce.Duration())
		return tries.Load() == 2
	})
	if _, err := e.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := replyTo(t, st); got != 2 {
		t.Errorf("answered up to %d, want the message that was sent meanwhile", got)
	}
}

// A message a failed reply could not answer is still unanswered, so the next
// reply covers it and the new message both.
func TestAFailedReplyIsTriedAgainWithTheNextMessage(t *testing.T) {
	st := newStore(t)
	c := newClock()
	a := &replies{failures: 1}
	e := open(t, st, c, a.chat)

	post(t, e, "one")
	c.Advance(config.DefaultEngine().Debounce.Duration())
	waitFor(t, "the failure", func() bool { return seen(e, ReplyFailed) })
	if _, err := e.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}

	answered, err := st.AnsweredUpto(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if answered != 0 {
		t.Errorf("answered up to %d, want the message left unanswered", answered)
	}
	if a.count() != 1 {
		t.Errorf("attempts = %d, want the failure left alone until he says something", a.count())
	}

	post(t, e, "two")
	c.Advance(config.DefaultEngine().Debounce.Duration())
	waitFor(t, "the second try", func() bool { return a.count() == 2 })
	if _, err := e.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := replyTo(t, st); got != 2 {
		t.Errorf("answered up to %d, want both messages", got)
	}
}

// The next run ends the entry with the reply a killed run had already stored,
// and answers nothing a second time.
func TestAReplyStoredByARunThatEndedIsNotWrittenAgain(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	c := newClock()

	// What a run that was killed between storing the reply and writing down
	// what it answered leaves behind.
	msg := &store.Message{Role: store.RoleUser, Channel: "repl",
		Parts: []store.Part{{Type: store.PartText, Text: "one"}}, CreatedAt: c.Now()}
	if err := st.AddMessage(ctx, msg); err != nil {
		t.Fatal(err)
	}
	entry := &store.Entry{Channel: "repl", UptoMessageID: msg.ID, StartedAt: c.Now()}
	if err := st.StartEntry(ctx, entry); err != nil {
		t.Fatal(err)
	}
	reply := &store.Message{Role: store.RoleAssistant, Channel: "repl",
		Parts:   []store.Part{{Type: store.PartText, Text: "hey you"}},
		ReplyTo: msg.ID, EntryID: entry.ID, CreatedAt: c.Now()}
	if err := st.AddMessage(ctx, reply); err != nil {
		t.Fatal(err)
	}

	a := &replies{}
	e := open(t, st, c, a.chat)
	c.Advance(config.DefaultEngine().Debounce.Duration())
	if _, err := e.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if a.count() != 0 {
		t.Errorf("replies = %d, want the message answered by the reply already stored", a.count())
	}

	ended, err := st.Entry(ctx, entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ended.Status != store.StatusDone {
		t.Errorf("status = %q, want the entry ended with the reply it stored", ended.Status)
	}
	if ended.UptoMessageID != msg.ID {
		t.Errorf("the entry answered up to %d, want the message %d", ended.UptoMessageID, msg.ID)
	}
}

// The run ended before the entry could be closed. It holds the part of a reply
// that a stop kept, so it ended stopped rather than as one that finished.
func TestAReplyStoppedByARunThatEndedReadsAsStopped(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	c := newClock()

	msg := &store.Message{Role: store.RoleUser, Channel: "repl",
		Parts: []store.Part{{Type: store.PartText, Text: "one"}}, CreatedAt: c.Now()}
	if err := st.AddMessage(ctx, msg); err != nil {
		t.Fatal(err)
	}
	entry := &store.Entry{Channel: "repl", UptoMessageID: msg.ID, StartedAt: c.Now()}
	if err := st.StartEntry(ctx, entry); err != nil {
		t.Fatal(err)
	}
	reply := &store.Message{Role: store.RoleAssistant, Channel: "repl",
		Parts: []store.Part{{Type: store.PartText, Text: "hey y"}}, Interrupted: true,
		ReplyTo: msg.ID, EntryID: entry.ID, CreatedAt: c.Now()}
	if err := st.AddMessage(ctx, reply); err != nil {
		t.Fatal(err)
	}

	open(t, st, c, (&replies{}).chat)

	ended, err := st.Entry(ctx, entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ended.Status != store.StatusStopped {
		t.Errorf("status = %q, want the entry ended as the stop it holds", ended.Status)
	}
	kept, err := st.ReplyOfEntry(ctx, entry.ID)
	if err != nil || kept.ID != reply.ID {
		t.Errorf("the entry holds %v, %v, want the reply %d", kept, err, reply.ID)
	}
}

// The stop lands while a message is still waiting for its reply. It answers
// that message with an entry of its own, so paula turns shows the turn happened
// and the next run does not answer it again.
func TestAStopBeforeAReplyStartsLeavesAnEntry(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	c := newClock()
	a := &replies{}
	e := open(t, st, c, a.chat)

	// The message is waiting for the debounce, so nothing is being written.
	post(t, e, "one")
	stopped, err := e.Stop(ctx)
	if err != nil || !stopped {
		t.Fatalf("Stop = %v, %v", stopped, err)
	}

	entries, err := st.Entries(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Status != store.StatusStopped {
		t.Fatalf("entries = %+v, want the one the stop left", entries)
	}
	if entries[0].UptoMessageID != 1 {
		t.Errorf("the entry answered up to %d, want the message it was asked on",
			entries[0].UptoMessageID)
	}
	if answered, err := st.AnsweredUpto(ctx); err != nil || answered != 1 {
		t.Errorf("answered up to %d, %v, want the message the stop was asked on", answered, err)
	}

	// The next run reads the same thing back and leaves the message alone.
	e.Close()
	second := open(t, st, c, a.chat)
	c.Advance(config.DefaultEngine().Debounce.Duration())
	if _, err := second.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if a.count() != 0 {
		t.Errorf("replies = %d, want the stopped message left alone", a.count())
	}
}

// The next line arrives before the stopped reply reports back, and the stop
// still keeps what was written and answers the messages it was for.
func TestAMessageLandingOnAStopLeavesItStopped(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	c := newClock()
	started := make(chan struct{})
	cancelled := make(chan struct{})
	release := make(chan struct{})
	e := open(t, st, c, func(ctx context.Context, _ api.ChatRequest, _ func(api.Chunk) error) (*api.Result, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		<-release
		return nil, ctx.Err()
	})

	post(t, e, "one")
	c.Advance(config.DefaultEngine().Debounce.Duration())
	<-started
	stopped, err := e.Stop(ctx)
	if err != nil || !stopped {
		t.Fatalf("Stop = %v, %v", stopped, err)
	}
	<-cancelled

	post(t, e, "two")
	close(release)
	waitFor(t, "the stop", func() bool { return seen(e, ReplyStopped) })
	if seen(e, ReplyRestarted) {
		t.Error("the stop was turned into a restart")
	}

	answered, err := st.AnsweredUpto(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if answered != 1 {
		t.Errorf("answered up to %d, want the message the stop was asked on", answered)
	}
	entries, err := st.Entries(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 || entries[len(entries)-1].Status != store.StatusStopped {
		t.Errorf("entries = %+v, want the first one stopped", entries)
	}
}

// A message arriving while a reply is being written is stored, and published,
// before the reply that did not wait for it.
func TestTheReplyIsStoredWhereTheEventsAreNumbered(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	c := newClock()
	a := &replies{begun: true, start: make(chan struct{}), hold: make(chan struct{}), text: "hey you"}
	e := open(t, st, c, a.chat)

	post(t, e, "one")
	c.Advance(config.DefaultEngine().Debounce.Duration())
	<-a.start

	// The next line arrives while she is writing, so it is stored before the
	// reply she is still finishing.
	post(t, e, "two")
	close(a.hold)
	waitFor(t, "the reply", func() bool { return seen(e, ReplyDone) })
	c.Advance(config.DefaultEngine().Debounce.Duration())
	if _, err := e.Wait(ctx); err != nil {
		t.Fatal(err)
	}

	var ids []store.MessageID
	for _, ev := range published(e) {
		if ev.Message != nil {
			ids = append(ids, ev.Message.ID)
		}
	}
	if !slices.IsSorted(ids) {
		t.Errorf("the messages were published in the order %v", ids)
	}

	messages, err := e.History(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) < 3 || messages[0].Text() != "one" || messages[1].Text() != "two" {
		t.Fatalf("the conversation holds %+v", messages)
	}
	if messages[2].Role != store.RoleAssistant || messages[2].Text() != "hey you" {
		t.Errorf("the reply is %+v", messages[2])
	}
	entries, err := st.Entries(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	first := entries[len(entries)-1]
	reply, err := st.ReplyOfEntry(ctx, first.ID)
	if err != nil || reply.ID != messages[2].ID {
		t.Errorf("the entry holds %v, %v, want the reply it stored, %d",
			reply, err, messages[2].ID)
	}
}

// The end of a run is not a stop: what she had written is dropped, and the
// message is answered by the run that follows.
func TestARunThatEndsLeavesTheReplyToTheNextOne(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	c := newClock()
	started := make(chan struct{})
	a := &replies{}
	e := open(t, st, c, func(ctx context.Context, _ api.ChatRequest, _ func(api.Chunk) error) (*api.Result, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})

	post(t, e, "one")
	c.Advance(config.DefaultEngine().Debounce.Duration())
	<-started
	e.Close()

	answered, err := st.AnsweredUpto(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if answered != 0 {
		t.Errorf("answered up to %d, want the message left to the next run", answered)
	}
	entries, err := st.Entries(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if entries[0].Status != store.StatusFailed || entries[0].Error != runEnded {
		t.Errorf("entry = %+v, want it ended as the run that stopped", entries[0])
	}

	// The next run answers it.
	second := open(t, st, c, a.chat)
	c.Advance(config.DefaultEngine().Debounce.Duration())
	waitFor(t, "the reply", func() bool { return a.count() == 1 })
	if _, err := second.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if got := replyTo(t, st); got != 1 {
		t.Errorf("answered up to %d, want the message the run left behind", got)
	}
}

// An entry a new message restarted ends as restarted, with no reply.
func TestTheEntryOfARestartedReply(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	c := newClock()
	a := &replies{start: make(chan struct{}), hold: make(chan struct{})}
	e := open(t, st, c, a.chat)

	post(t, e, "one")
	c.Advance(config.DefaultEngine().Debounce.Duration())
	<-a.start
	post(t, e, "two")
	waitFor(t, "the restart", func() bool { return seen(e, ReplyRestarted) })

	entries, err := st.Entries(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	first := entries[len(entries)-1]
	if first.Status != store.StatusRestarted {
		t.Errorf("status = %q, want the entry ended as restarted", first.Status)
	}
	if _, err := st.ReplyOfEntry(ctx, first.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the restarted entry holds a reply: %v", err)
	}
	if first.EndedAt.IsZero() {
		t.Error("the restarted entry was left running")
	}
}
