package store

import (
	"context"
	"errors"
	"testing"
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

// A fold stores the summary as it reads, and the one a later fold writes is
// the one that counts. Her memories are not a fold's to write: what she kept
// stands as it was through every fold.
func TestAFoldStoresItsSummaryAndLeavesHerMemoriesAlone(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	first := said(t, s, "my sister Ana lives in Porto")
	second := said(t, s, "she moved to Lisbon last month")
	kept := &Memory{Content: "Ana lives in Porto", Source: first.ID}
	if err := s.Remember(ctx, kept); err != nil {
		t.Fatal(err)
	}

	summary := &Summary{UptoMessageID: first.ID, Content: "they talked about Ana"}
	if err := s.Fold(ctx, summary); err != nil {
		t.Fatal(err)
	}
	if summary.ID == 0 {
		t.Fatalf("no id was filled in: %+v", summary)
	}

	got, err := s.LatestSummary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "they talked about Ana" || got.UptoMessageID != first.ID {
		t.Errorf("summary = %+v", got)
	}
	if !got.CoversUpto.Equal(first.CreatedAt) {
		t.Errorf("summary covers up to %v, want the time of the message it covers", got.CoversUpto)
	}

	if err := s.Fold(ctx, &Summary{UptoMessageID: second.ID, Content: "Ana moved"}); err != nil {
		t.Fatal(err)
	}
	got, err = s.LatestSummary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "Ana moved" || got.UptoMessageID != second.ID {
		t.Errorf("summary = %+v, want the one the later fold wrote", got)
	}
	standing, err := s.Memories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(standing) != 1 || standing[0].ID != kept.ID || standing[0].ReplacedBy != 0 {
		t.Errorf("memories = %+v, want the one she kept, as it was", standing)
	}
}

// A memory she keeps herself is dated by the message it was said in. One
// said in no message is refused, since the day it was said is read from that
// message.
func TestAMemoryKeptStandsWithTheMessageItWasSaidIn(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	if err := s.Remember(ctx, &Memory{Content: "Ana lives in Lisbon", Source: 99}); err == nil {
		t.Error("a memory said in no message was kept")
	}

	first := said(t, s, "my sister Ana lives in Lisbon")
	kept := &Memory{Content: "Ana lives in Lisbon", Source: first.ID}
	if err := s.Remember(ctx, kept); err != nil {
		t.Fatal(err)
	}
	if kept.ID == 0 {
		t.Fatal("no id was filled in")
	}
	standing, err := s.Memories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(standing) != 1 || standing[0].ID != kept.ID || standing[0].Content != "Ana lives in Lisbon" {
		t.Fatalf("memories = %+v, want the one kept", standing)
	}
	if standing[0].Source != first.ID || !standing[0].SaidAt.Equal(first.CreatedAt) {
		t.Errorf("memory = %+v, want it said in message %d", standing[0], first.ID)
	}
}

// A memory she keeps replaces only memories that stand. A number that is not
// one keeps nothing at all, since she chose it and is told so. The next number
// is the one the memory would be given, and names nothing yet: a number is
// given again once the row that held it is forgotten, and a memory replaced by
// itself would stand for nothing.
func TestAMemoryKeptReplacesOnlyWhatStands(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	first := said(t, s, "my sister Ana lives in Porto")
	porto := &Memory{Content: "Ana lives in Porto", Source: first.ID}
	if err := s.Remember(ctx, porto); err != nil {
		t.Fatal(err)
	}

	for _, replaces := range []MemoryID{porto.ID + 1, porto.ID + 5} {
		err := s.Remember(ctx, &Memory{Content: "Ana lives in Lisbon", Source: first.ID, Replaces: []MemoryID{replaces}})
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("replacing #%d = %v, want it refused", replaces, err)
		}
	}
	if got, _ := s.Memories(ctx); len(got) != 1 || got[0].ID != porto.ID {
		t.Fatalf("memories = %+v, want only the one there was", got)
	}

	// A number named twice is the one memory it names.
	lisbon := &Memory{Content: "Ana lives in Lisbon", Source: first.ID, Replaces: []MemoryID{porto.ID, porto.ID}}
	if err := s.Remember(ctx, lisbon); err != nil {
		t.Fatal(err)
	}
	standing, err := s.Memories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(standing) != 1 || standing[0].ID != lisbon.ID {
		t.Errorf("memories = %+v, want the one that replaced it", standing)
	}

	// What was replaced once is not replaced again.
	err = s.Remember(ctx, &Memory{Content: "Ana lives in Faro", Source: first.ID, Replaces: []MemoryID{porto.ID}})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("replacing a memory already replaced = %v, want it refused", err)
	}
}

func TestASummaryCoversTheMessageItWasWrittenUpTo(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	first := said(t, s, "my sister Ana lives in Porto")

	// The time a summary carries is the conversation's rather than its own: the
	// message it covers up to is what says how far it goes.
	if err := s.Fold(ctx, &Summary{UptoMessageID: first.ID, Content: "they talked about Ana"}); err != nil {
		t.Fatal(err)
	}

	got, err := s.LatestSummary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !got.CoversUpto.Equal(now) {
		t.Errorf("summary covers up to %v, want the message at %v", got.CoversUpto, now)
	}
}
