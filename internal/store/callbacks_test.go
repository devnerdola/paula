package store

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

// scheduled keeps a call back under an entry of its own, due so long after
// now.
func scheduled(t *testing.T, s *Store, in time.Duration, reason string) *Callback {
	t.Helper()
	ctx := context.Background()
	entry := &Entry{StartedAt: now}
	if err := s.StartEntry(ctx, entry); err != nil {
		t.Fatal(err)
	}
	c := &Callback{DueAt: now.Add(in), Reason: reason, Entry: entry.ID}
	if err := s.Schedule(ctx, c); err != nil {
		t.Fatal(err)
	}
	return c
}

func numbers(callbacks []Callback) []CallbackID {
	out := make([]CallbackID, len(callbacks))
	for i, c := range callbacks {
		out[i] = c.ID
	}
	return out
}

// A conversation with nothing scheduled lists nothing, and a number that
// names no pending call back is not found by either of the two.
func TestNothingScheduled(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	pending, err := s.Callbacks(ctx)
	if err != nil || len(pending) != 0 {
		t.Errorf("callbacks = %v, %v, want none", pending, err)
	}
	if err := s.MoveCallback(ctx, 1, now); !errors.Is(err, ErrNotFound) {
		t.Errorf("MoveCallback = %v, want not found", err)
	}
	if err := s.CancelCallback(ctx, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("CancelCallback = %v, want not found", err)
	}
}

// The pending call backs are listed soonest first, whatever order they were
// scheduled in, and one that is moved takes its new place.
func TestPendingCallbacksComeSoonestFirst(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	later := scheduled(t, s, 2*time.Hour, "ask about the interview")
	soon := scheduled(t, s, 10*time.Minute, "say good night")
	pending, err := s.Callbacks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := numbers(pending); !slices.Equal(got, []CallbackID{soon.ID, later.ID}) {
		t.Errorf("pending = %v, want the soonest first", got)
	}
	if c := pending[0]; c.Reason != "say good night" || !c.DueAt.Equal(now.Add(10*time.Minute)) || c.Entry == 0 || c.Message != 0 {
		t.Errorf("the soonest is %+v, want it whole and pending", c)
	}

	if err := s.MoveCallback(ctx, soon.ID, now.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	pending, err = s.Callbacks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := numbers(pending); !slices.Equal(got, []CallbackID{later.ID, soon.ID}) {
		t.Errorf("pending after the move = %v, want the moved one last", got)
	}
}

// A cancelled call back is gone: it is not listed, and cannot be cancelled
// again.
func TestACancelledCallbackIsGone(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	c := scheduled(t, s, time.Hour, "check on the cold")
	if err := s.CancelCallback(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.CancelCallback(ctx, c.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("cancelling it again = %v, want not found", err)
	}
	pending, err := s.Callbacks(ctx)
	if err != nil || len(pending) != 0 {
		t.Errorf("callbacks = %v, %v, want none", pending, err)
	}
}

// A call back fires as a message she answers, which names it as fired: it is
// no longer pending, so nothing moves, cancels or fires it again, and the
// message is the newest she answers.
func TestAFiredCallbackIsTheMessageSheAnswers(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	said(t, s, "good night")
	c := scheduled(t, s, time.Hour, "say good morning")

	m := &Message{Role: RoleCallback, Channel: "repl",
		Parts: []Part{{Type: PartText, Text: c.Reason}}, CreatedAt: now.Add(time.Hour)}
	if err := s.Fire(ctx, c.ID, m); err != nil {
		t.Fatal(err)
	}
	if m.ID == 0 {
		t.Fatal("the message it fired as has no number")
	}
	if pending, err := s.Callbacks(ctx); err != nil || len(pending) != 0 {
		t.Errorf("callbacks = %v, %v, want none pending", pending, err)
	}
	if err := s.Fire(ctx, c.ID, &Message{Role: RoleCallback, Parts: m.Parts, CreatedAt: now}); !errors.Is(err, ErrNotFound) {
		t.Errorf("firing it again = %v, want not found", err)
	}
	if err := s.CancelCallback(ctx, c.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("cancelling it = %v, want not found", err)
	}
	last, err := s.LastAsked(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if last.ID != m.ID || last.Role != RoleCallback || !last.Asked() {
		t.Errorf("the last message she answers is %+v, want the call back", last)
	}
	// Firing again stored no second message.
	newest, err := s.LastMessage(ctx, "")
	if err != nil || newest.ID != m.ID {
		t.Errorf("the newest message is %+v, %v, want the one it fired as", newest, err)
	}
	// A call back that came due is the app's, not something either of them
	// said: the newest of what was said passes over it, and it is not counted.
	history, n, err := s.Messages(ctx, 0, 1)
	if err != nil || len(history) != 1 || history[0].Role != RoleUser || n != 1 {
		t.Errorf("the newest said = %+v of %d, %v, want the one message the user sent", history, n, err)
	}
}

// The messages an entry answered include a call back that fired.
func TestAnEntryAnswersTheCallbackItWasStartedFor(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	c := scheduled(t, s, time.Minute, "check in")
	m := &Message{Role: RoleCallback, Parts: []Part{{Type: PartText, Text: c.Reason}}, CreatedAt: now}
	if err := s.Fire(ctx, c.ID, m); err != nil {
		t.Fatal(err)
	}
	entry := &Entry{UptoMessageID: m.ID, StartedAt: now}
	if err := s.StartEntry(ctx, entry); err != nil {
		t.Fatal(err)
	}
	answered, err := s.MessagesAnsweredBy(ctx, entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(answered) != 1 || answered[0].ID != m.ID {
		t.Errorf("the entry answered %+v, want the call back", answered)
	}
}
