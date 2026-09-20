package conversation

import (
	"context"
	"errors"
	"testing"
	"time"

	"nerdola.dev/x/paula/internal/store"
)

// collect reads events until it has n of them, or the test times out.
func collect(t *testing.T, l *events, after Seq, n int) []Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var out []Event
	for e, err := range l.Events(ctx, after) {
		if err != nil {
			t.Fatalf("Events: %v", err)
		}
		out = append(out, e)
		if len(out) == n {
			return out
		}
	}
	t.Fatalf("read %d events, want %d", len(out), n)
	return nil
}

func TestEventsAreNumbered(t *testing.T) {
	l := newEvents()
	if l.Seq() != 0 {
		t.Errorf("Seq = %d, want 0", l.Seq())
	}
	l.publish(Event{Kind: ReplyStarted, Entry: 1})
	if l.Seq() != 1 {
		t.Errorf("Seq = %d, want the first event numbered 1", l.Seq())
	}
	l.publish(Event{Kind: ReplyDone, Entry: 1})
	if l.Seq() != 2 {
		t.Errorf("Seq = %d, want the second numbered 2", l.Seq())
	}
}

func TestEventsAfterANumber(t *testing.T) {
	l := newEvents()
	l.publish(Event{Kind: ReplyStarted, Entry: 1})
	l.publish(Event{Kind: ReplyText, Entry: 1, Text: "hey"})
	l.publish(Event{Kind: ReplyDone, Entry: 1})

	got := collect(t, l, 1, 2)
	if got[0].Kind != ReplyText || got[1].Kind != ReplyDone {
		t.Errorf("events = %+v", got)
	}
	if got[0].Text != "hey" {
		t.Errorf("text = %q", got[0].Text)
	}
}

func TestReplyTextReplacesTheTextBeforeIt(t *testing.T) {
	l := newEvents()
	l.publish(Event{Kind: ReplyStarted, Entry: 1})
	l.publish(Event{Kind: ReplyText, Entry: 1, Text: "he"})
	l.publish(Event{Kind: ReplyText, Entry: 1, Text: "hell"})
	l.publish(Event{Kind: ReplyText, Entry: 1, Text: "hello"})
	last := l.Seq()

	got := collect(t, l, 0, 2)
	if len(got) != 2 || got[1].Text != "hello" || got[1].Seq != last {
		t.Errorf("events = %+v, want the reply's text once", got)
	}
	if last != 4 {
		t.Errorf("seq = %d, want a number of its own", last)
	}
}

func TestReplyTextOfAnotherEntryIsKept(t *testing.T) {
	l := newEvents()
	l.publish(Event{Kind: ReplyText, Entry: 1, Text: "one"})
	l.publish(Event{Kind: ReplyText, Entry: 2, Text: "two"})
	got := collect(t, l, 0, 2)
	if got[0].Entry != 1 || got[1].Entry != 2 {
		t.Errorf("events = %+v", got)
	}
}

func TestReplyTextAfterAnotherKindIsKept(t *testing.T) {
	l := newEvents()
	l.publish(Event{Kind: ReplyText, Entry: 1, Text: "one"})
	l.publish(Event{Kind: ReplyStarted, Entry: 2})
	l.publish(Event{Kind: ReplyText, Entry: 1, Text: "two"})
	if got := collect(t, l, 0, 3); len(got) != 3 {
		t.Errorf("events = %+v", got)
	}
}

func TestEventsWait(t *testing.T) {
	l := newEvents()
	done := make(chan Event, 1)
	reading := make(chan struct{})
	go func() {
		close(reading)
		done <- collect(t, l, 0, 1)[0]
	}()

	// The reader is waiting for something to happen before anything does.
	<-reading
	l.publish(Event{Kind: MessageStored, Message: &store.Message{ID: 7}})

	select {
	case e := <-done:
		if e.Message == nil || e.Message.ID != 7 {
			t.Errorf("event = %+v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the reader was not woken")
	}
}

func TestEventsEndWithTheContext(t *testing.T) {
	l := newEvents()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range l.Events(ctx, 0) {
		}
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the reader did not end")
	}
}

func TestAReaderTooFarBehind(t *testing.T) {
	l := newEvents()
	for range keptEvents + 10 {
		l.publish(Event{Kind: ReplyStarted, Entry: 1})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var told bool
	var first Event
	for e, err := range l.Events(ctx, 1) {
		if err != nil {
			if !errors.Is(err, ErrBehind) {
				t.Fatalf("error = %v", err)
			}
			told = true
			continue
		}
		first = e
		break
	}
	if !told {
		t.Error("the reader was not told it had fallen behind")
	}
	if first.Seq != 11 {
		t.Errorf("first event = %d, want the oldest kept", first.Seq)
	}
}

func TestAReaderThatIsOnlyAtTheOldestIsNotBehind(t *testing.T) {
	l := newEvents()
	for range keptEvents {
		l.publish(Event{Kind: ReplyStarted, Entry: 1})
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, err := range l.Events(ctx, 0) {
		if err != nil {
			t.Fatalf("error = %v, want none with nothing dropped", err)
		}
		break
	}
}

func TestTheLogKeepsItsSize(t *testing.T) {
	l := newEvents()
	for range keptEvents * 2 {
		l.publish(Event{Kind: ReplyStarted, Entry: 1})
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.kept) != keptEvents {
		t.Errorf("kept = %d, want %d", len(l.kept), keptEvents)
	}
	if l.kept[0].Seq != keptEvents+1 {
		t.Errorf("oldest = %d", l.kept[0].Seq)
	}
}

// A reply's text replaces the text before it, and the gaps that leaves in the
// numbering are not events a reader missed.
func TestAReaderIsNotBehindOverTextThatWasReplaced(t *testing.T) {
	l := newEvents()
	l.publish(Event{Kind: ReplyStarted, Entry: 1})
	for _, text := range []string{"one", "one two", "one two three"} {
		l.publish(Event{Kind: ReplyText, Entry: 1, Text: text})
	}
	l.mu.Lock()
	kept := len(l.kept)
	l.mu.Unlock()
	if kept != 2 {
		t.Fatalf("kept = %d, want the reply's text collapsed into one", kept)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for e, err := range l.Events(ctx, 2) {
		if err != nil {
			t.Fatalf("error = %v, want none with nothing dropped", err)
		}
		if e.Text != "one two three" {
			t.Errorf("event = %+v, want the latest text", e)
		}
		break
	}
}

// A reader changing the event it was given does not change what the log holds.
func TestAReaderKeepsWhatItWasGiven(t *testing.T) {
	l := newEvents()
	l.publish(Event{Kind: MessageStored, Message: &store.Message{
		ID: 1, Role: store.RoleUser, Parts: []store.Part{{Type: store.PartText, Text: "hey"}},
	}})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for e, err := range l.Events(ctx, 0) {
		if err != nil {
			t.Fatal(err)
		}
		e.Message.Parts[0].Text = "something else"
		break
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if got := l.kept[0].Message.Text(); got != "hey" {
		t.Errorf("the log holds %q", got)
	}
}
