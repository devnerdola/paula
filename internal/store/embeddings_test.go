package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

var bge = Embedded{Runner: "venice", Model: "text-embedding-bge-m3"}

// remembers stores a fold of one memory about a message, and returns it.
func remembers(t *testing.T, s *Store, text, memory string, replaces ...MemoryID) Memory {
	t.Helper()
	m := said(t, s, text)
	stored := []Memory{{Content: memory, Source: m.ID, Replaces: replaces}}
	err := s.Fold(context.Background(),
		&Summary{UptoMessageID: m.ID, Content: "they talked"}, stored)
	if err != nil {
		t.Fatal(err)
	}
	return stored[0]
}

func TestAMemoryIsEmbeddedOncePerModel(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	first := remembers(t, s, "Ana lives in Lisbon", "Caio's sister Ana lives in Lisbon.")
	second := remembers(t, s, "I cook on saturdays", "Caio cooks for friends on Saturdays.")

	// Nothing is embedded yet, so both are waiting.
	waiting, err := s.MemoriesToEmbed(ctx, bge, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(waiting) != 2 || waiting[0].ID != first.ID {
		t.Fatalf("waiting = %+v, want both, oldest first", waiting)
	}

	err = s.Embed(ctx, bge, map[MemoryID][]float32{first.ID: {1, 0, 0}})
	if err != nil {
		t.Fatal(err)
	}
	waiting, err = s.MemoriesToEmbed(ctx, bge, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(waiting) != 1 || waiting[0].ID != second.ID {
		t.Errorf("waiting = %+v, want the one that has no vector", waiting)
	}

	// A vector of one model says nothing about another, so under another model
	// both are waiting again.
	other := Embedded{Runner: "openrouter", Model: "openai/text-embedding-3-small"}
	if waiting, err := s.MemoriesToEmbed(ctx, other, 10); err != nil || len(waiting) != 2 {
		t.Errorf("waiting under another model = %+v, %v", waiting, err)
	}
}

func TestMemoriesAreFoundByWhatTheyMean(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	lisbon := remembers(t, s, "Ana lives in Lisbon", "Caio's sister Ana lives in Lisbon.")
	cooking := remembers(t, s, "I cook on saturdays", "Caio cooks for friends on Saturdays.")
	bike := remembers(t, s, "the bike is fixed", "Caio fixed the bicycle.")

	// Three vectors pointing three ways, so what is closest is not in doubt.
	err := s.Embed(ctx, bge, map[MemoryID][]float32{
		lisbon.ID:  {1, 0, 0},
		cooking.ID: {0, 1, 0},
		bike.ID:    {0, 0, 1},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Nearly the first, and the second next: how alike they point is what
	// orders them, not how long they are.
	found, err := s.NearestMemories(ctx, bge, []float32{9, 3, 0}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 2 || found[0].ID != lisbon.ID || found[1].ID != cooking.ID {
		t.Fatalf("found %+v, want the memory about Lisbon and then the cooking", found)
	}
	if found[0].Content != "Caio's sister Ana lives in Lisbon." || found[0].SaidAt.IsZero() {
		t.Errorf("found = %+v, want the whole memory", found[0])
	}
	if found, err := s.NearestMemories(ctx, bge, []float32{0, 0, 1}, 1); err != nil ||
		len(found) != 1 || found[0].ID != bike.ID {
		t.Errorf("found %+v, %v, want the memory about the bicycle", found, err)
	}
}

func TestWhatIsSearchedIsWhatStandsAndWhatThisModelEmbedded(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	was := remembers(t, s, "Ana lives in Porto", "Caio's sister Ana lives in Porto.")
	now2 := remembers(t, s, "Ana moved to Lisbon", "Caio's sister Ana lives in Lisbon.", was.ID)

	err := s.Embed(ctx, bge, map[MemoryID][]float32{was.ID: {1, 0, 0}, now2.ID: {1, 0, 0}})
	if err != nil {
		t.Fatal(err)
	}

	// The one that was replaced is kept, so that forgetting can take it too,
	// but nothing searches it.
	found, err := s.NearestMemories(ctx, bge, []float32{1, 0, 0}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].ID != now2.ID {
		t.Errorf("found %+v, want only the memory that stands", found)
	}
	// Nothing was embedded by another model, so nothing is near anything.
	other := Embedded{Runner: "openrouter", Model: "openai/text-embedding-3-small"}
	if found, err := s.NearestMemories(ctx, other, []float32{1, 0, 0}, 10); err != nil || len(found) != 0 {
		t.Errorf("found %+v, %v under a model that embedded nothing", found, err)
	}
}

func TestForgettingTakesWhatTheMemoryReplaced(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	first := remembers(t, s, "Ana lives in Porto", "Caio's sister Ana lives in Porto.")
	second := remembers(t, s, "Ana moved to Lisbon", "Caio's sister Ana lives in Lisbon.", first.ID)
	kept := remembers(t, s, "I fixed the bike", "Caio fixed the bicycle.")
	err := s.Embed(ctx, bge, map[MemoryID][]float32{
		first.ID: {1, 0, 0}, second.ID: {1, 0, 0}, kept.ID: {0, 0, 1},
	})
	if err != nil {
		t.Fatal(err)
	}

	gone, err := s.Forget(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(gone) != 2 || gone[0].ID != first.ID || gone[1].ID != second.ID {
		t.Fatalf("forgot %+v, want the memory and the one it replaced, oldest first", gone)
	}

	left, err := s.LatestMemories(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0].ID != kept.ID {
		t.Errorf("what is left is %+v, want the memory about the bicycle", left)
	}
	// The vectors go with them: nothing is near a memory that is not there.
	near, err := s.NearestMemories(ctx, bge, []float32{1, 0, 0}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(near) != 1 || near[0].ID != kept.ID {
		t.Errorf("found %+v, want only the memory that is left", near)
	}
	// What was said stays, and so does the summary.
	if _, err := s.LatestSummary(ctx); err != nil {
		t.Errorf("the summary went with the memory: %v", err)
	}
	if _, err := s.Message(ctx, second.Source); err != nil {
		t.Errorf("the message went with the memory: %v", err)
	}
	if _, err := s.Forget(ctx, second.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("forgetting it again = %v, want not found", err)
	}
}

func TestAMemoryForgottenWhileItIsEmbeddedLeavesTheRestEmbedded(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	going := remembers(t, s, "Ana lives in Lisbon", "Caio's sister Ana lives in Lisbon.")
	kept := remembers(t, s, "I fixed the bike", "Caio fixed the bicycle.")

	// A batch is read, and one of its memories is forgotten before the vectors
	// come back. What is left of the batch is stored, and the memory that went
	// takes its vector with it.
	if _, err := s.Forget(ctx, going.ID); err != nil {
		t.Fatal(err)
	}
	err := s.Embed(ctx, bge, map[MemoryID][]float32{
		going.ID: {1, 0, 0}, kept.ID: {0, 0, 1},
	})
	if err != nil {
		t.Fatalf("a memory that went took the batch with it: %v", err)
	}

	if waiting, err := s.MemoriesToEmbed(ctx, bge, 10); err != nil || len(waiting) != 0 {
		t.Errorf("waiting = %+v, %v, want the batch stored", waiting, err)
	}
	found, err := s.NearestMemories(ctx, bge, []float32{0, 0, 1}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].ID != kept.ID {
		t.Errorf("found %+v, want the memory that is still there", found)
	}
}

func TestAQueryOfAnotherWidthIsRefused(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	m := remembers(t, s, "Ana lives in Lisbon", "Caio's sister Ana lives in Lisbon.")
	if err := s.Embed(ctx, bge, map[MemoryID][]float32{m.ID: {1, 0, 0}}); err != nil {
		t.Fatal(err)
	}

	// A model answers at one width. Another width is another model, whatever
	// it was stored under, and measuring the two against each other would
	// answer with numbers that mean nothing.
	_, err := s.NearestMemories(ctx, bge, []float32{1, 0}, 10)
	if err == nil || !strings.Contains(err.Error(), "wide") {
		t.Errorf("error = %v, want it to say the widths differ", err)
	}
}

func TestAVectorIsStoredAsItWas(t *testing.T) {
	// Every number survives the round trip, in order, including the ones that
	// do not land on a round binary fraction.
	want := []float32{0, 1, -1, 0.1, -0.000123, 3.4028235e+38, 1.1754944e-38}
	got, err := vectorOf(vectorBytes(want))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, want) {
		t.Errorf("vector = %v, want %v", got, want)
	}
	// Bytes that are not a whole number of numbers are not a vector.
	if _, err := vectorOf([]byte{1, 2, 3}); err == nil {
		t.Error("three bytes read as a vector")
	}
	if _, err := vectorOf(nil); err == nil {
		t.Error("no bytes read as a vector")
	}
}
