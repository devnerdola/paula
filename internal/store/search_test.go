package store

import (
	"context"
	"errors"
	"slices"
	"testing"
)

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

// kept are memories a model wrote with the remember tool in conversations run
// on 23 and 24 September 2026, as they were stored.
var kept = []string{
	"Caio tried learning guitar this week but gave up, saying it wasn't for him.",
	"Caio's sister Ana is arriving from Recife on Saturday and Caio is picking her up at the airport.",
	"Caio has a sister named Ana who lives in Recife.",
	"Caio started learning the guitar this week.",
	`In September 2026 Caio built a web chat screen for his texting-companion app, with blue chat bubbles, command chips and a "notify me" button, and the character in it was named Ada.`,
	"On 23 September 2026 Paula spent the day inking a children's book commission and burned her risotto while practicing piano.",
	"Caio works in software and in September 2026 was reviewing the README for a work project: a Go texting-companion app called Paula, whose example persona is named Paula and whose user is named Caio.",
	"Caio is working on a Go project called Paula, a terminal/Telegram texting companion app, and wrote its README himself.",
}

func keepAll(t *testing.T, s *Store) []MemoryID {
	t.Helper()
	ids := make([]MemoryID, len(kept))
	for i, memory := range kept {
		ids[i] = remembers(t, s, "they said something", memory).ID
	}
	return ids
}

func ids(found []Memory) []MemoryID {
	out := make([]MemoryID, len(found))
	for i, m := range found {
		out[i] = m.ID
	}
	return out
}

// Nothing to look for finds nothing, and a word that is an operator of the
// search is looked for as the word it is.
func TestASearchForNothingFindsNothing(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	keepAll(t, s)
	for _, words := range [][]string{nil, {"NOT"}, {"NEAR"}, {`"`}, {"*"}} {
		found, err := s.SearchMemories(ctx, words, 10)
		if err != nil {
			t.Errorf("searching %q: %v", words, err)
		}
		if len(found) != 0 {
			t.Errorf("searching %q found %v", words, ids(found))
		}
	}
}

// Words a model looked for with. What holds none of them is not found, however
// much else it shares with the query, and a topic nothing holds finds nothing.
func TestAMemoryIsFoundByTheWordsItHolds(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	m := keepAll(t, s)
	for _, c := range []struct {
		words []string
		want  []MemoryID
	}{
		{[]string{"sister", "Ana", "Recife"}, []MemoryID{m[1], m[2]}},
		{[]string{"guitar"}, []MemoryID{m[0], m[3]}},
		{[]string{"sisters"}, []MemoryID{m[1], m[2]}},
		{[]string{"piano", "practice"}, []MemoryID{m[5]}},
		{[]string{"favourite", "food"}, nil},
		{[]string{"coriander"}, nil},
	} {
		found, err := s.SearchMemories(ctx, c.words, 10)
		if err != nil {
			t.Fatal(err)
		}
		got := ids(found)
		slices.Sort(got)
		if !slices.Equal(got, c.want) {
			t.Errorf("searching %q found %v, want %v", c.words, got, c.want)
		}
	}
}

// What holds more of the words, and rarer ones, comes first, and no more than
// the limit come back.
func TestWhatTheWordsSayMostAboutComesFirst(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	m := keepAll(t, s)
	found, err := s.SearchMemories(ctx, []string{"sister", "Recife", "airport"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(found); !slices.Equal(got, []MemoryID{m[1]}) {
		t.Errorf("found %v, want the memory that holds all three words", got)
	}
	if found[0].Content != kept[1] || found[0].SaidAt.IsZero() {
		t.Errorf("found %+v, want the whole memory", found[0])
	}
}

// A memory another took the place of is kept, so that forgetting can take it
// too, but nothing finds it.
func TestWhatIsSearchedIsWhatStands(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	was := remembers(t, s, "Ana lives in Porto", "Caio's sister Ana lives in Porto.")
	now := remembers(t, s, "Ana moved to Lisbon", "Caio's sister Ana lives in Lisbon.", was.ID)
	found, err := s.SearchMemories(ctx, []string{"sister"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(found); !slices.Equal(got, []MemoryID{now.ID}) {
		t.Errorf("found %v, want only the memory that stands", got)
	}
}

func TestForgettingTakesWhatTheMemoryReplaced(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	first := remembers(t, s, "Ana lives in Porto", "Caio's sister Ana lives in Porto.")
	second := remembers(t, s, "Ana moved to Lisbon", "Caio's sister Ana lives in Lisbon.", first.ID)
	left := remembers(t, s, "I fixed the bike", "Caio fixed the bicycle.")

	gone, err := s.Forget(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(gone) != 2 || gone[0].ID != first.ID || gone[1].ID != second.ID {
		t.Fatalf("forgot %+v, want the memory and the one it replaced, oldest first", gone)
	}

	latest, err := s.LatestMemories(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(latest) != 1 || latest[0].ID != left.ID {
		t.Errorf("what is left is %+v, want the memory about the bicycle", latest)
	}
	// Their words go with them: nothing finds a memory that is not there.
	found, err := s.SearchMemories(ctx, []string{"sister", "bicycle"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(found); !slices.Equal(got, []MemoryID{left.ID}) {
		t.Errorf("found %v, want only the memory that is left", got)
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

// The number of the newest memory is given again once it is forgotten, and the
// memory that takes it answers to its own words only.
func TestANumberGivenAgainTakesNoWordsWithIt(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	going := remembers(t, s, "Ana lives in Lisbon", "Caio's sister Ana lives in Lisbon.")
	if _, err := s.Forget(ctx, going.ID); err != nil {
		t.Fatal(err)
	}
	now := remembers(t, s, "I fixed the bike", "Caio fixed the bicycle.")
	if now.ID != going.ID {
		t.Fatalf("the new memory is %d, want the number %d given again", now.ID, going.ID)
	}
	found, err := s.SearchMemories(ctx, []string{"sister"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Errorf("searching for the sister found %+v", found)
	}
}

// A database that searched memories by their vectors has its memories found
// by their words once it is brought up to date, and loses the vectors and the
// log of the requests that made them. A reply that searched keeps its entry.
func TestADatabaseThatSearchedByVectorsIsSearchedByWords(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := openStore(dir, true, migrations[:3])
	if err != nil {
		t.Fatal(err)
	}
	sister := remembers(t, s, "Ana lives in Recife", kept[2])
	for _, stmt := range []string{
		`INSERT INTO memory_embeddings (memory_id, runner, model, vector) VALUES (1, 'venice', 'bge', x'0000803f')`,
		`INSERT INTO entries (id, status, started_at) VALUES (1, 'done', 1), (2, 'done', 2)`,
		`INSERT INTO requests (entry_id, purpose, runner, method, url, started_at) VALUES
			(1, 'embedding', 'venice', 'POST', 'u', 1),
			(2, 'reply', 'venice', 'POST', 'u', 2),
			(2, 'memory-search', 'venice', 'POST', 'u', 2)`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()

	s, err = openStore(dir, true, migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	found, err := s.SearchMemories(ctx, []string{"sister"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(found); !slices.Equal(got, []MemoryID{sister.ID}) {
		t.Errorf("found %v, want the memory that was there before", got)
	}

	var tables, entries int
	var purposes string
	if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name = 'memory_embeddings'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM entries`).Scan(&entries); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT group_concat(purpose) FROM requests`).Scan(&purposes); err != nil {
		t.Fatal(err)
	}
	if tables != 0 || entries != 1 || purposes != "reply" {
		t.Errorf("vectors table %d, entries %d, requests %q; want no vectors, the reply's entry and its reply",
			tables, entries, purposes)
	}
}

// A model the conversation was given for a role that is gone goes with the
// role, even in a database whose memories were already searched by words.
func TestAModelGivenForTheEmbedRoleIsForgotten(t *testing.T) {
	dir := t.TempDir()
	s, err := openStore(dir, true, migrations[:4])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO kv (key, value) VALUES ('model.chat', 'pro'), ('model.embed', 'vectors')`); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = openStore(dir, true, migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var keys string
	if err := s.db.QueryRow(`SELECT group_concat(key) FROM kv`).Scan(&keys); err != nil {
		t.Fatal(err)
	}
	if keys != "model.chat" {
		t.Errorf("kv holds %q, want the chat model alone", keys)
	}
}
