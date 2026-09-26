package conversation_test

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/conversation"
	"nerdola.dev/x/paula/internal/logs"
	"nerdola.dev/x/paula/internal/persona"
	"nerdola.dev/x/paula/internal/runners"
	"nerdola.dev/x/paula/internal/store"
	"nerdola.dev/x/paula/internal/tools"
)

// texts are what the live test sends, one a turn, the way a person texts
// through a day.
var texts = []string{
	"hey, you around?",
	"the train was packed again, I stood the whole way",
	"what are you up to?",
	"I keep thinking about that pasta place we talked about",
	"they moved my meeting to friday, so tonight is free after all",
	"did you sleep well?",
	"making coffee, it's going to be terrible",
	"ok it's actually decent",
	"lunch was a sad sandwich at my desk",
	"what's the best thing that happened to you today?",
	"heading home now",
	"finally on the couch",
}

// TestLiveTheCacheIsReadAndRecovers holds a model's cache to what the
// conversation needs of it: a reply after the first reads what the replies
// before it wrote, and from that one on reading never drops and grows with the
// conversation. After a wait past the cache's lifetime, reading comes back and
// goes past what it read before.
//
// It asks a real model, so it runs only when PAULA_LIVE names a configuration
// file. The file's data_dir is left alone, and every token comes from the
// environment as the file's token_env says:
//
//	PAULA_LIVE             the configuration file
//	PAULA_LIVE_MODEL       the model of the file to ask, the chat default otherwise
//	PAULA_LIVE_TURNS       how many messages are sent, 4 unless set
//	PAULA_LIVE_CACHE_WAIT  how long to wait after them before sending as many
//	                       again, nothing unless set
//
// A wait longer than go test's own ten minutes needs a -timeout above it.
func TestLiveTheCacheIsReadAndRecovers(t *testing.T) {
	file := os.Getenv("PAULA_LIVE")
	if file == "" {
		t.Skip("PAULA_LIVE names no configuration file")
	}
	turns := 4
	if s := os.Getenv("PAULA_LIVE_TURNS"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 3 {
			t.Fatalf("PAULA_LIVE_TURNS = %q, want a count of 3 or more", s)
		}
		turns = n
	}
	var wait time.Duration
	if s := os.Getenv("PAULA_LIVE_CACHE_WAIT"); s != "" {
		var err error
		if wait, err = time.ParseDuration(s); err != nil {
			t.Fatalf("PAULA_LIVE_CACHE_WAIT: %v", err)
		}
	}

	cfg, err := config.Load(file)
	if err != nil {
		t.Fatal(err)
	}
	card, err := persona.Load(cfg.Persona)
	if err != nil {
		t.Fatal(err)
	}
	set, err := runners.Configure(cfg, runners.Host{Secrets: new(logs.Secrets)})
	if err != nil {
		t.Fatal(err)
	}
	var offered []tools.Tool
	for _, tc := range cfg.Tools {
		opened, err := tools.Open(tc.Name, tc.Section, tools.Host{
			Names:    tools.Names{Character: card.Name, User: card.User.Name},
			Language: card.Language,
		})
		if err != nil {
			t.Fatal(err)
		}
		offered = append(offered, opened...)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	ctx := t.Context()
	if err := set.Check(ctx); err != nil {
		t.Fatal(err)
	}
	e, err := conversation.Open(ctx, conversation.Options{
		Store:   st,
		Runners: set,
		Persona: card,
		Engine:  cfg.Engine,
		Tools:   offered,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	if name := os.Getenv("PAULA_LIVE_MODEL"); name != "" {
		if err := e.SetModel(ctx, config.RoleChat, name); err != nil {
			t.Fatal(err)
		}
	}

	sent := 0
	phase := func(name string) []int {
		var reads []int
		for turn := 1; turn <= turns; turn++ {
			if err := e.Post(ctx, conversation.NewMessage{Channel: "repl", Text: texts[sent%len(texts)]}); err != nil {
				t.Fatal(err)
			}
			sent++
			if _, err := e.Wait(ctx); err != nil {
				t.Fatal(err)
			}
			rounds := replyRounds(t, st)
			// The provider says whether a round that read less than the one
			// before it went somewhere else.
			for i, r := range rounds {
				t.Logf("%s, turn %d, round %d, from %s: prompt %d, read %d, written %d",
					name, turn, i+1, r.Provider, r.Usage.PromptTokens, r.Usage.CachedTokens, r.Usage.CacheWriteTokens)
			}
			// What she wrote shows whether the model read the times as times.
			if reply, err := st.LastMessage(ctx, store.RoleAssistant); err == nil {
				t.Logf("%s, turn %d, she wrote: %s", name, turn, reply.Text())
			}
			reads = append(reads, rounds[0].Usage.CachedTokens)
		}
		readingHolds(t, name, reads)
		return reads
	}

	before := phase("before the wait")
	if wait == 0 {
		return
	}
	t.Logf("waiting %s", wait)
	select {
	case <-time.After(wait):
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	after := phase("after the wait")
	if last, was := after[len(after)-1], before[len(before)-1]; last <= was {
		t.Errorf("the last reply read %d, no more than the %d the last one before the wait read", last, was)
	}
}

// replyRounds is every round of the latest reply as it was recorded, with its
// usage, which has to have ended with the reply written.
func replyRounds(t *testing.T, st *store.Store) []store.Request {
	t.Helper()
	ctx := t.Context()
	entries, err := st.Entries(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(entries, func(e store.Entry) bool { return e.UptoMessageID != 0 })
	if i < 0 {
		t.Fatal("no entry answered a message")
	}
	entry := entries[i]
	if entry.Status != store.StatusDone {
		t.Fatalf("the reply ended %s: %s", entry.Status, entry.Error)
	}
	requests, err := st.Requests(ctx, entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.Request
	for _, r := range requests {
		if r.Purpose != store.PurposeReply {
			continue
		}
		if r.Usage == nil {
			r.Usage = &store.Usage{}
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		t.Fatalf("entry %d sent no reply request", entry.ID)
	}
	return out
}

// readingHolds checks what the first round of each reply of a phase read. The
// first reply may find nothing, so some reply after it has to read; from the
// first that does, none reads less than the one before it; and the last reads
// more than that first, since the conversation it resends grew.
func readingHolds(t *testing.T, phase string, reads []int) {
	t.Helper()
	first := slices.IndexFunc(reads[1:], func(n int) bool { return n > 0 }) + 1
	if first == 0 {
		t.Errorf("%s: no reply after the first read the cache", phase)
		return
	}
	for k := first + 1; k < len(reads); k++ {
		if reads[k] < reads[k-1] {
			t.Errorf("%s: turn %d read %d, less than the %d turn %d read", phase, k+1, reads[k], reads[k-1], k)
		}
	}
	if last := reads[len(reads)-1]; last <= reads[first] {
		t.Errorf("%s: reading stayed at %d from turn %d on", phase, last, first+1)
	}
}
