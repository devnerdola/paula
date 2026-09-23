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

func TestAFoldStoresItsSummaryAndItsMemories(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	first := said(t, s, "my sister Ana lives in Porto")
	second := said(t, s, "she moved to Lisbon last month")

	summary := &Summary{UptoMessageID: first.ID, Content: "they talked about Ana"}
	memories := []Memory{{Content: "Ana lives in Porto", Source: first.ID}}
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
	if !got.CoversUpto.Equal(first.CreatedAt) {
		t.Errorf("summary covers up to %v, want the time of the message it covers", got.CoversUpto)
	}

	// A later fold replaces what it contradicts, and the summary it writes is
	// the one that counts.
	later := &Summary{UptoMessageID: second.ID, Content: "Ana moved"}
	moved := []Memory{{
		Content:  "Ana lives in Lisbon",
		Source:   second.ID,
		Replaces: []MemoryID{memories[0].ID},
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

// A memory she keeps herself stands beside the ones a fold wrote, dated by the
// message it was said in. One said in no message is refused, since the day it
// was said is read from that message.
func TestAMemoryKeptOnItsOwnStandsWithTheMessageItWasSaidIn(t *testing.T) {
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

// A memory she keeps herself replaces only memories that stand. A number that
// is not one keeps nothing at all, since she chose it and is told so.
func TestAMemoryKeptOnItsOwnReplacesOnlyWhatStands(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	first := said(t, s, "my sister Ana lives in Porto")
	porto := Memory{Content: "Ana lives in Porto", Source: first.ID}
	if err := s.Fold(ctx, &Summary{UptoMessageID: first.ID, Content: "Ana"}, []Memory{porto}); err != nil {
		t.Fatal(err)
	}
	standing, err := s.Memories(ctx)
	if err != nil || len(standing) != 1 {
		t.Fatalf("memories = %+v, %v", standing, err)
	}
	porto = standing[0]

	// The next number is the one the memory would be given, and it names
	// nothing yet.
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
	standing, err = s.Memories(ctx)
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
	err := s.Fold(ctx, &Summary{
		UptoMessageID: first.ID, Content: "they talked about Ana",
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
}

// A number is given again once the row that held it is forgotten, so a fold
// that read its memories before its model answered can name what has since
// become its own row. A memory replaced by itself would stand for nothing and
// be told to nobody.
func TestAFoldCannotReplaceTheMemoryItIsWriting(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	first := said(t, s, "Ana lives in Porto")
	was := []Memory{{Content: "Ana lives in Porto.", Source: first.ID}}
	err := s.Fold(ctx,
		&Summary{UptoMessageID: first.ID, Content: "they talked"}, was)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Forget(ctx, was[0].ID); err != nil {
		t.Fatal(err)
	}

	second := said(t, s, "Ana moved to Lisbon")
	next := []Memory{{
		Content: "Ana lives in Lisbon.", Source: second.ID,
		Replaces: []MemoryID{was[0].ID},
	}}
	err = s.Fold(ctx,
		&Summary{UptoMessageID: second.ID, Content: "they talked"}, next)
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
		&Summary{UptoMessageID: first.ID, Content: "they said hey"},
		[]Memory{{Content: "nothing real", Source: first.ID + 404}})
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
		&Summary{UptoMessageID: first.ID, Content: "they said hey"},
		[]Memory{{Content: "nothing real"}})
	if err == nil {
		t.Fatal("a memory of no message was stored")
	}
}
