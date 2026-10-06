package conversation

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/runners/api"
	"nerdola.dev/x/paula/internal/store"
	toolsapi "nerdola.dev/x/paula/internal/tools/api"
	"nerdola.dev/x/paula/internal/tools/callbacks"
)

// scheduling keeps a call back due so long from now, under an entry of its
// own, and has the loop look at it, the way the tool does.
func scheduling(t *testing.T, r *replyEngine, in time.Duration, reason string) *store.Callback {
	t.Helper()
	ctx := context.Background()
	entry := &store.Entry{StartedAt: r.clock.Now()}
	if err := r.store.StartEntry(ctx, entry); err != nil {
		t.Fatal(err)
	}
	entry.Status, entry.EndedAt = store.StatusDone, r.clock.Now()
	if err := r.store.EndEntry(ctx, entry); err != nil {
		t.Fatal(err)
	}
	c := &store.Callback{DueAt: r.clock.Now().Add(in), Reason: reason, Entry: entry.ID}
	if err := r.store.Schedule(ctx, c); err != nil {
		t.Fatal(err)
	}
	r.rearm()
	return c
}

// armed waits until a timer is set for that time.
func armed(t *testing.T, c *fakeClock, at time.Time) {
	t.Helper()
	waitFor(t, fmt.Sprintf("a timer set for %s", at.Format(time.TimeOnly)), func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		for _, timer := range c.timers {
			if timer.on && timer.at.Equal(at) {
				return true
			}
		}
		return false
	})
}

// nextTimer waits until a timer is set, and says when the soonest one is set
// for. While nothing is being written, the only timer set is the loop's for
// the soonest call back.
func nextTimer(t *testing.T, c *fakeClock) time.Time {
	t.Helper()
	var at time.Time
	waitFor(t, "a timer to be set", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		at = time.Time{}
		for _, timer := range c.timers {
			if timer.on && (at.IsZero() || timer.at.Before(at)) {
				at = timer.at
			}
		}
		return !at.IsZero()
	})
	return at
}

// pass moves the clock on to a time, a step at a time: each step is what the
// loop set its timer for, which is a minute at most, the way it looks at the
// soonest call back again every minute.
func pass(t *testing.T, r *replyEngine, until time.Time) {
	t.Helper()
	for {
		at := nextTimer(t, r.clock)
		if at.After(until) {
			t.Fatalf("the timer is set for %s, past %s", at.Format(time.TimeOnly), until.Format(time.TimeOnly))
		}
		r.clock.Advance(at.Sub(r.clock.Now()))
		if !at.Before(until) {
			return
		}
	}
}

// dones counts the replies that ended done.
func dones(e *Engine) int {
	n := 0
	for _, ev := range published(e) {
		if ev.Kind == ReplyDone {
			n++
		}
	}
	return n
}

// callbacksAsked are the call back messages of the conversation, oldest
// first.
func callbacksAsked(t *testing.T, r *replyEngine) []store.Message {
	t.Helper()
	all, err := r.store.MessagesAfter(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.Message
	for _, m := range all {
		if m.Role == store.RoleCallback {
			out = append(out, m)
		}
	}
	return out
}

// A call back comes due while nothing is being written: it is stored as a
// message she answers, on the channel of the latest message, and a turn
// starts at once. The prompt tells it as a note in its own message, with
// nothing after it, and her reply answers it.
func TestACallbackThatComesDueIsAMessageSheAnswers(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: says("so, how did it go?")}
	r := openReply(t, f)
	ctx := context.Background()
	r.say(t, "off to the interview")
	c := scheduling(t, r, 90*time.Minute, "ask how the interview went")

	pass(t, r, c.DueAt.Add(-time.Minute))
	if got := callbacksAsked(t, r); len(got) != 0 {
		t.Fatalf("a call back fired a minute early: %+v", got)
	}
	pass(t, r, c.DueAt)
	waitFor(t, "the reply to the call back", func() bool { return dones(r.Engine) == 2 })

	fired := callbacksAsked(t, r)
	if len(fired) != 1 {
		t.Fatalf("call backs fired: %+v, want one", fired)
	}
	m := fired[0]
	if m.Text() != "ask how the interview went" || m.Channel != "repl" || !m.CreatedAt.Equal(r.clock.Now()) {
		t.Errorf("the call back fired as %+v, want its reason, on the latest channel, now", m)
	}
	if pending, _ := r.store.Callbacks(ctx); len(pending) != 0 {
		t.Errorf("still pending: %+v", pending)
	}

	req := f.replied()
	msgs := req.Messages
	last := msgs[len(msgs)-1]
	want := "A call back you scheduled came due at Wednesday, 16 September 2026, 23:52 UTC+02:00: ask how the interview went."
	if last.Role != api.RoleSystem || text(last) != want {
		t.Errorf("the prompt ends with %s: %q, want the note %q", last.Role, text(last), want)
	}
	// A reason that ends with a stop of its own is not given a second one.
	sentence := store.Message{CreatedAt: newClock().Now(), Parts: []store.Part{{Type: store.PartText, Text: "Ask how the interview went."}}}
	if got := r.cameDue(sentence); !strings.HasSuffix(got, ": Ask how the interview went.") {
		t.Errorf("a reason that is a sentence is told as %q, want its one stop", got)
	}
	if got := said(req, api.RoleUser); len(got) != 1 || got[0] != "off to the interview" {
		t.Errorf("the prompt carries the user messages %q, want only the one sent before", got)
	}
	reply, _ := stored(t, r)
	if reply.ReplyTo != m.ID || reply.Text() != "so, how did it go?" {
		t.Errorf("the reply is %+v, want one answering the call back", reply)
	}
	entries, err := r.store.Entries(ctx, 1)
	if err != nil || entries[0].UptoMessageID != m.ID {
		t.Errorf("the turn's entry is %+v, %v, want it to answer the call back", entries, err)
	}
}

// A call back that comes due while a reply is being written fires once that
// turn is over, and the turn it starts follows it.
func TestACallbackDueWhileAReplyIsWrittenFiresAfterIt(t *testing.T) {
	hold := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	f := &fakeRunner{model: chatModel()}
	f.chat = func(ctx context.Context, req api.ChatRequest, fn func(api.Chunk) error) (*api.Result, error) {
		first := false
		once.Do(func() { first = true })
		if first {
			fn(api.Chunk{Kind: api.ChunkText, Text: "one moment"})
			close(started)
			<-hold
		} else {
			fn(api.Chunk{Kind: api.ChunkText, Text: "and now"})
		}
		return &api.Result{FinishReason: "stop"}, nil
	}
	r := openReply(t, f)
	post(t, r.Engine, "hey")
	r.clock.Advance(config.DefaultEngine().Debounce.Duration())
	<-started

	c := scheduling(t, r, 30*time.Second, "say good night")
	pass(t, r, c.DueAt)
	if got := callbacksAsked(t, r); len(got) != 0 {
		t.Fatalf("a call back fired while a reply was being written: %+v", got)
	}
	close(hold)
	if _, err := r.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The turn ended with the call back due, so the loop looks at it at once.
	armed(t, r.clock, r.clock.Now())
	r.clock.Advance(0)
	waitFor(t, "the reply to the call back", func() bool { return dones(r.Engine) == 2 })

	fired := callbacksAsked(t, r)
	first, _ := r.store.ReplyOfEntry(context.Background(), 1)
	if len(fired) != 1 || first == nil || fired[0].ID < first.ID {
		t.Errorf("the call back fired as %+v, want it after the reply %+v", fired, first)
	}
}

// A call back that comes due while a message waits its two seconds joins that
// message's turn: one reply answers both, with the note after the message.
func TestACallbackDueWhileAMessageWaitsJoinsItsTurn(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: says("hi, and good night")}
	r := openReply(t, f)
	ctx := context.Background()
	r.say(t, "hey")
	c := scheduling(t, r, time.Second, "say good night")
	post(t, r.Engine, "one more thing")
	armed(t, r.clock, c.DueAt)
	r.clock.Advance(time.Second)
	waitFor(t, "the call back to fire", func() bool { return len(callbacksAsked(t, r)) == 1 })
	if dones(r.Engine) != 1 {
		t.Fatalf("%d replies done, want the call back to wait for the message's turn", dones(r.Engine))
	}
	r.clock.Advance(config.DefaultEngine().Debounce.Duration())
	if _, err := r.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if dones(r.Engine) != 2 {
		t.Fatalf("%d replies done, want one turn for the message and the call back", dones(r.Engine))
	}
	msgs := f.replied().Messages
	if got := said(f.replied(), api.RoleUser); len(got) != 2 || got[1] != "one more thing" {
		t.Errorf("the turn sent the messages %q, want the one that waited", got)
	}
	if last := msgs[len(msgs)-1]; !strings.HasPrefix(text(last), "A call back you scheduled came due at ") {
		t.Errorf("the prompt ends with %q, want the note after the message", text(last))
	}
	entries, _ := r.store.Entries(ctx, 1)
	if fired := callbacksAsked(t, r); len(entries) != 1 || entries[0].UptoMessageID != fired[0].ID {
		t.Errorf("the turn's entry is %+v, want it to answer up to the call back", entries)
	}
}

// A stop during a call back's turn ends it the way it ends any reply: what
// she had written is kept, and the call back counts as answered.
func TestAStopDuringACallbacksTurnEndsIt(t *testing.T) {
	hold := make(chan struct{})
	started := make(chan struct{})
	var calls atomic.Int32
	f := &fakeRunner{model: chatModel()}
	f.chat = func(ctx context.Context, req api.ChatRequest, fn func(api.Chunk) error) (*api.Result, error) {
		if calls.Add(1) == 1 {
			fn(api.Chunk{Kind: api.ChunkText, Text: "hi"})
			return &api.Result{FinishReason: "stop"}, nil
		}
		fn(api.Chunk{Kind: api.ChunkText, Text: "good night, I was about to say"})
		close(started)
		select {
		case <-hold:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &api.Result{FinishReason: "stop"}, nil
	}
	r := openReply(t, f)
	ctx := context.Background()
	r.say(t, "hey")
	c := scheduling(t, r, time.Minute, "say good night")
	pass(t, r, c.DueAt)
	<-started

	if stopped, err := r.Stop(ctx); err != nil || !stopped {
		t.Fatalf("stopping the call back's turn = %v, %v, want it stopped", stopped, err)
	}
	if _, err := r.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	fired := callbacksAsked(t, r)
	entries, _ := r.store.Entries(ctx, 1)
	if len(fired) != 1 || len(entries) != 1 || entries[0].Status != store.StatusStopped || entries[0].UptoMessageID != fired[0].ID {
		t.Errorf("the turn's entry is %+v, want it stopped, answering the call back %+v", entries, fired)
	}
	if reply, _ := stored(t, r); !reply.Interrupted || reply.Text() != "good night, I was about to say" {
		t.Errorf("the reply kept is %+v, want what she had written, interrupted", reply)
	}
	if answered, _ := r.store.AnsweredUpto(ctx); len(fired) == 1 && answered != fired[0].ID {
		t.Errorf("answered up to %d, want the call back", answered)
	}
}

// Call backs that came due while no run was on fire when one starts, soonest
// first, one turn each.
func TestOverdueCallbacksFireAtStartInDueOrder(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: says("hello?")}
	r := openReply(t, f)
	ctx := context.Background()
	r.say(t, "talk later")
	later := scheduling(t, r, 2*time.Minute, "ask about dinner")
	sooner := scheduling(t, r, time.Minute, "say the movie starts")
	r.Engine.Close()
	r.clock.Advance(time.Hour)

	e, err := Open(ctx, Options{
		Store: r.store, Runners: setup(f), Persona: fullCard(), Engine: config.DefaultEngine(),
		Clock: r.clock, Log: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	// The run starts with both due, so its timer is set for now.
	armed(t, r.clock, r.clock.Now())
	r.clock.Advance(0)
	waitFor(t, "the first call back's reply", func() bool { return dones(e) == 1 })
	armed(t, r.clock, r.clock.Now())
	r.clock.Advance(0)
	waitFor(t, "the second call back's reply", func() bool { return dones(e) == 2 })

	fired := callbacksAsked(t, r)
	if len(fired) != 2 || fired[0].Text() != sooner.Reason || fired[1].Text() != later.Reason {
		t.Errorf("the call backs fired as %+v, want the sooner first", fired)
	}
	if pending, _ := r.store.Callbacks(ctx); len(pending) != 0 {
		t.Errorf("still pending: %+v", pending)
	}
}

// A cancelled call back never fires, and a moved one fires at its new time.
func TestACancelledCallbackDoesNotFireAndAMovedOneFiresLater(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: says("hi")}
	r := openReply(t, f)
	ctx := context.Background()
	r.say(t, "hey")

	gone := scheduling(t, r, 10*time.Minute, "never")
	if err := r.store.CancelCallback(ctx, gone.ID); err != nil {
		t.Fatal(err)
	}
	r.rearm()
	moved := scheduling(t, r, 10*time.Minute, "later")
	movedTo := r.clock.Now().Add(20 * time.Minute)
	if err := r.store.MoveCallback(ctx, moved.ID, movedTo); err != nil {
		t.Fatal(err)
	}
	r.rearm()

	pass(t, r, moved.DueAt)
	if got := callbacksAsked(t, r); len(got) != 0 {
		t.Fatalf("fired at the old time: %+v", got)
	}
	pass(t, r, movedTo)
	waitFor(t, "the moved call back's reply", func() bool { return dones(r.Engine) == 2 })
	if got := callbacksAsked(t, r); len(got) != 1 || got[0].Text() != "later" {
		t.Errorf("fired: %+v, want the moved one alone", got)
	}
}

// A call back she schedules with the tool is kept under the reply that asked
// for it, and fires when it is due.
func TestACallbackSheSchedulesFires(t *testing.T) {
	tools, err := callbacks.Open(config.Section{}, toolsapi.Host{Names: toolsapi.Names{Character: "Paula", User: "Caio"}})
	if err != nil {
		t.Fatal(err)
	}
	due := newClock().Now().Add(time.Hour)
	f := &fakeRunner{model: chatModel(), chat: answering(
		round{calls: []api.ToolCall{{ID: "call_1", Name: "schedule_callback",
			Arguments: fmt.Sprintf(`{"at":%q,"reason":"ask how the interview went"}`, due.Format(time.RFC3339))}}},
		round{text: "good luck, I'll check in after"},
		round{text: "so, how did it go?"},
	)}
	r := openReplyOffering(t, f, setup(f), config.DefaultEngine(), tools...)
	r.say(t, "interview in an hour")

	pending, err := r.store.Callbacks(context.Background())
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %+v, %v, want the one she scheduled", pending, err)
	}
	reply, calls := stored(t, r)
	if pending[0].Entry != reply.EntryID || len(calls) != 1 || !strings.HasPrefix(calls[0].Result, "call back #1 set for ") {
		t.Errorf("the call back %+v was kept by %+v, want it under the reply's entry with the tool told its number", pending[0], calls)
	}

	pass(t, r, due)
	waitFor(t, "the reply to the call back", func() bool { return dones(r.Engine) == 2 })
	if got := callbacksAsked(t, r); len(got) != 1 || got[0].Text() != "ask how the interview went" {
		t.Errorf("fired: %+v", got)
	}
}

// A reply that schedules a call back and writes nothing is her putting the
// answer off: the turn ends done with no reply, the message counts as
// answered, and it stays in the history, so when the call back comes due she
// is shown it again, right before the note, and writes then.
func TestAReplyOfNothingAfterSchedulingACallbackPutsTheAnswerOff(t *testing.T) {
	tools, err := callbacks.Open(config.Section{}, toolsapi.Host{Names: toolsapi.Names{Character: "Paula", User: "Caio"}})
	if err != nil {
		t.Fatal(err)
	}
	due := newClock().Now().Add(6 * time.Hour)
	f := &fakeRunner{model: chatModel(), chat: answering(
		round{calls: []api.ToolCall{{ID: "call_1", Name: "schedule_callback",
			Arguments: fmt.Sprintf(`{"at":%q,"reason":"answer Caio, who asked whether I was up"}`, due.Format(time.RFC3339))}}},
		round{},
		round{text: "morning! sorry, I was fast asleep"},
	)}
	r := openReplyOffering(t, f, setup(f), config.DefaultEngine(), tools...)
	ctx := context.Background()
	r.say(t, "are you up?")

	entries, err := r.store.Entries(ctx, 1)
	if err != nil || len(entries) != 1 || entries[0].Status != store.StatusDone {
		t.Fatalf("entries = %+v, %v, want the turn ended done", entries, err)
	}
	if reply, err := r.store.ReplyOfEntry(ctx, entries[0].ID); err == nil {
		t.Errorf("the turn stored the reply %+v, want none", reply)
	}
	if answered, _ := r.store.AnsweredUpto(ctx); answered != entries[0].UptoMessageID || answered == 0 {
		t.Errorf("answered up to %d, want the message she put off, %d", answered, entries[0].UptoMessageID)
	}
	var done []Event
	for _, ev := range published(r.Engine) {
		if ev.Kind == ReplyDone {
			done = append(done, ev)
		}
	}
	if len(done) != 1 || done[0].Message != nil {
		t.Errorf("reply done events = %+v, want one carrying no message", done)
	}
	if seen(r.Engine, ReplyFailed) {
		t.Error("the turn was said to have failed")
	}

	pass(t, r, due)
	waitFor(t, "the reply to the call back", func() bool { return dones(r.Engine) == 2 })
	msgs := f.replied().Messages
	last, before := msgs[len(msgs)-1], msgs[len(msgs)-2]
	if !strings.HasPrefix(text(last), "A call back you scheduled came due at ") || before.Role != api.RoleUser || text(before) != "are you up?" {
		t.Errorf("the prompt ends with %s: %q after %s: %q, want the note right after the message she put off",
			last.Role, text(last), before.Role, text(before))
	}
	if reply, _ := stored(t, r); reply.Text() != "morning! sorry, I was fast asleep" {
		t.Errorf("the reply is %q, want the one written when the call back came due", reply.Text())
	}
}

// A model whose family reads her notes as user messages is told a call back
// came due in that role, with no message of the user's after it.
func TestACallbackIsToldInTheRoleTheFamilyGivesHerNotes(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: says("hello")}
	set := setup(f)
	set.Models[0].Settings.Extension = notesAsUser{}
	r := openReplyWith(t, f, set)
	r.say(t, "hey")
	c := scheduling(t, r, time.Minute, "say good night")
	pass(t, r, c.DueAt)
	waitFor(t, "the reply to the call back", func() bool { return dones(r.Engine) == 2 })

	msgs := f.replied().Messages
	last := msgs[len(msgs)-1]
	if last.Role != api.RoleUser || !strings.HasPrefix(text(last), "A call back you scheduled came due at ") {
		t.Errorf("the prompt ends with %s: %q, want the note as a user message", last.Role, text(last))
	}
	if before := msgs[len(msgs)-2]; before.Role != api.RoleAssistant {
		t.Errorf("before the note comes %s: %q, want her reply to the message before", before.Role, text(before))
	}
}

// A compaction's transcript carries a call back that came due as what it
// was, at its time, with no one saying it.
func TestTheTranscriptCarriesACallbackThatCameDue(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: says("hi")}
	r := openReply(t, f)
	at := time.Date(2026, 9, 16, 21, 30, 0, 0, berlin)
	pieces := r.pieces(context.Background(), nil, []store.Message{
		{ID: 1, Role: store.RoleUser, CreatedAt: at.Add(-time.Hour), Parts: []store.Part{{Type: store.PartText, Text: "talk later"}}},
		{ID: 2, Role: store.RoleCallback, CreatedAt: at, Parts: []store.Part{{Type: store.PartText, Text: "say good night"}}},
		{ID: 3, Role: store.RoleAssistant, ReplyTo: 2, CreatedAt: at, Parts: []store.Part{{Type: store.PartText, Text: "good night, love"}}},
	})
	want := "Messages to add:\nWednesday, 16 September 2026\n20:30 Caio: talk later\n" +
		"21:30 (call back due: say good night)\n21:30 Paula: good night, love"
	if got := written(pieces); got != want {
		t.Errorf("the transcript is\n%s\nwant\n%s", got, want)
	}
}
