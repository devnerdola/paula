package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// said stores a message to hang a summary and a memory on.
func said(t *testing.T, s *Store, text string) *Message {
	t.Helper()
	m := &Message{Role: RoleUser, Parts: []Part{{Type: PartText, Text: text}}, CreatedAt: now}
	if err := s.AddMessage(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestAConversationWithNoFoldYet(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	if _, err := s.LatestSummary(ctx); !errors.Is(err, ErrNotFound) {
		t.Errorf("summary error = %v, want not found", err)
	}
	memories, err := s.Memories(ctx)
	if err != nil || len(memories) != 0 {
		t.Errorf("memories = %+v, %v", memories, err)
	}
}

func TestAFoldStoresItsSummaryAndItsMemories(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	first := said(t, s, "my sister Ana lives in Porto")
	second := said(t, s, "she moved to Lisbon last month")

	summary := &Summary{UptoMessageID: first.ID, Content: "they talked about Ana", CreatedAt: now}
	memories := []Memory{{Content: "Ana lives in Porto", Source: first.ID, CreatedAt: now}}
	if err := s.Fold(ctx, summary, memories); err != nil {
		t.Fatal(err)
	}
	if summary.ID == 0 || memories[0].ID == 0 {
		t.Fatalf("no ids were filled in: %+v %+v", summary, memories[0])
	}

	got, err := s.LatestSummary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "they talked about Ana" || got.UptoMessageID != first.ID {
		t.Errorf("summary = %+v", got)
	}
	if !got.CreatedAt.Equal(now) {
		t.Errorf("summary was written at %v, want %v", got.CreatedAt, now)
	}

	// A later fold replaces what it contradicts, and the summary it writes is
	// the one that counts.
	later := &Summary{UptoMessageID: second.ID, Content: "Ana moved", CreatedAt: now}
	moved := []Memory{{
		Content:   "Ana lives in Lisbon",
		Source:    second.ID,
		Replaces:  []MemoryID{memories[0].ID},
		CreatedAt: now,
	}}
	if err := s.Fold(ctx, later, moved); err != nil {
		t.Fatal(err)
	}

	got, err = s.LatestSummary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "Ana moved" || got.UptoMessageID != second.ID {
		t.Errorf("summary = %+v, want the one the later fold wrote", got)
	}

	// Only what still stands is told to a model, and what it replaced says
	// which memory took its place.
	standing, err := s.Memories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(standing) != 1 || standing[0].Content != "Ana lives in Lisbon" {
		t.Fatalf("memories = %+v, want the one that stands", standing)
	}
	if standing[0].Source != second.ID || standing[0].ReplacedBy != 0 {
		t.Errorf("memory = %+v", standing[0])
	}
}

func TestASummaryCoversTheMessageItWasWrittenUpTo(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	first := said(t, s, "my sister Ana lives in Porto")

	// A summary written again covers what it covered before, however much later
	// it was written: the time it carries is the conversation's, not its own.
	written := now.Add(3 * time.Hour)
	err := s.Fold(ctx, &Summary{
		UptoMessageID: first.ID, Content: "they talked about Ana", CreatedAt: written,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.LatestSummary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !got.CoversUpto.Equal(now) {
		t.Errorf("summary covers up to %v, want the message at %v", got.CoversUpto, now)
	}
	if !got.CreatedAt.Equal(written) {
		t.Errorf("summary was written at %v, want %v", got.CreatedAt, written)
	}
}

// A number is given again once the row that held it is forgotten, so a fold
// that read its memories before its model answered can name what has since
// become its own row. A memory replaced by itself would stand for nothing and
// be told to nobody.
func TestAFoldCannotReplaceTheMemoryItIsWriting(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	first := said(t, s, "Ana lives in Porto")
	was := []Memory{{Content: "Ana lives in Porto.", Source: first.ID, CreatedAt: now}}
	err := s.Fold(ctx,
		&Summary{UptoMessageID: first.ID, Content: "they talked", CreatedAt: now}, was)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Forget(ctx, was[0].ID); err != nil {
		t.Fatal(err)
	}

	second := said(t, s, "Ana moved to Lisbon")
	next := []Memory{{
		Content: "Ana lives in Lisbon.", Source: second.ID,
		Replaces: []MemoryID{was[0].ID}, CreatedAt: now,
	}}
	err = s.Fold(ctx,
		&Summary{UptoMessageID: second.ID, Content: "they talked", CreatedAt: now}, next)
	if err != nil {
		t.Fatal(err)
	}
	if next[0].ID != was[0].ID {
		t.Fatalf("the memory was written as %d, want the number %d that was given again",
			next[0].ID, was[0].ID)
	}

	standing, err := s.Memories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(standing) != 1 || standing[0].ID != next[0].ID {
		t.Fatalf("memories = %+v, want the one the fold just wrote", standing)
	}
	if standing[0].ReplacedBy != 0 {
		t.Errorf("the memory is replaced by %d, and it is itself", standing[0].ReplacedBy)
	}
}

func TestAFoldThatCannotBeStoredLeavesNothingBehind(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	first := said(t, s, "hey")

	// The memory names a message that is not there, which the foreign key
	// refuses. The summary of the same step must not be left standing alone.
	err := s.Fold(ctx,
		&Summary{UptoMessageID: first.ID, Content: "they said hey", CreatedAt: now},
		[]Memory{{Content: "nothing real", Source: first.ID + 404, CreatedAt: now}})
	if err == nil {
		t.Fatal("a memory of a message that is not there was stored")
	}
	if _, err := s.LatestSummary(ctx); !errors.Is(err, ErrNotFound) {
		t.Errorf("summary error = %v, want the step to have left nothing", err)
	}

	// A memory of no message at all is the same refusal: it is read back beside
	// the day it was said, which is the message's, so one naming none would be
	// listed by nothing and searched by nothing.
	err = s.Fold(ctx,
		&Summary{UptoMessageID: first.ID, Content: "they said hey", CreatedAt: now},
		[]Memory{{Content: "nothing real", CreatedAt: now}})
	if err == nil {
		t.Fatal("a memory of no message was stored")
	}
}
