package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"nerdola.dev/x/paula/internal/store"
)

// embedding is a runner that answers the catalogue and turns text into a
// vector pointing one way for each thing the conversation is about, so a test
// can say which memory a question is nearest without a model.
func embedding(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			w.Write(openrouterAnswer(t, "models.json"))
		case "/v1/embeddings/models":
			w.Write(openrouterAnswer(t, "embedding_models.json"))
		case "/v1/embeddings":
			var req struct {
				Input []string `json:"input"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			var out struct {
				Data []struct {
					Index     int       `json:"index"`
					Embedding []float32 `json:"embedding"`
				} `json:"data"`
			}
			for i, said := range req.Input {
				v := []float32{0, 0}
				for word, at := range map[string]int{"porto": 0, "lisbon": 0, "sister": 0, "bicycle": 1, "bike": 1} {
					if strings.Contains(strings.ToLower(said), word) {
						v[at]++
					}
				}
				out.Data = append(out.Data, struct {
					Index     int       `json:"index"`
					Embedding []float32 `json:"embedding"`
				}{Index: i, Embedding: v})
			}
			json.NewEncoder(w).Encode(out)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

// remembered is a data directory holding two memories, the newer of which took
// the place of one that is no longer told, and a third about something else.
func remembered(t *testing.T) (dir string, newest, replaced, other store.MemoryID) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "data")
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx := context.Background()
	var said int
	add := func(text, memory string, replaces ...store.MemoryID) store.Memory {
		said++
		m := &store.Message{
			Role:  store.RoleUser,
			Parts: []store.Part{{Type: store.PartText, Text: text}},
			// A day apart, so the days they are listed by differ.
			CreatedAt: when.AddDate(0, 0, said),
		}
		if err := s.AddMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
		stored := []store.Memory{{
			Content: memory, Source: m.ID, Replaces: replaces, CreatedAt: when,
		}}
		err := s.Fold(ctx, &store.Summary{
			UptoMessageID: m.ID, Content: "they talked", CreatedAt: when,
		}, stored)
		if err != nil {
			t.Fatal(err)
		}
		return stored[0]
	}

	was := add("Ana lives in Porto", "Caio's sister Ana lives in Porto.")
	now := add("Ana moved to Lisbon", "Caio's sister Ana lives in Lisbon.", was.ID)
	bike := add("the bike is fixed", "Caio fixed the bicycle.")

	// The memories that stand are embedded, the way the conversation embeds
	// what a fold wrote.
	by := store.Embedded{Runner: "r", Model: "openai/text-embedding-3-small"}
	err = s.Embed(ctx, by, map[store.MemoryID][]float32{
		now.ID: {1, 0}, bike.ID: {0, 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	return dir, now.ID, was.ID, bike.ID
}

// memoryConfig is a file naming a model that writes and one that embeds, both
// served by the test's own runner.
func memoryConfig(t *testing.T, dir, url string) string {
	t.Helper()
	t.Setenv("OPENROUTER_API_KEY", "test-token-abcdefgh")
	return configFile(t, "data_dir: "+dir+
		"\nrunners:\n  r:\n    type: openrouter\n    url: "+url+"/v1"+
		"\nmodels:\n  talk:\n    runner: r\n    id: ~deepseek/deepseek-flash-latest"+
		"\n  vectors:\n    runner: r\n    id: openai/text-embedding-3-small"+
		"\ndefault_models:\n  chat: talk\n  embed: vectors\n")
}

func TestMemoryList(t *testing.T) {
	dir, newest, _, _ := remembered(t)
	cfg := memoryConfig(t, dir, embedding(t).URL)
	code, out, errOut := exec(t, "-config", cfg, "memory", "list")
	if code != 0 {
		t.Fatalf("code = %d, stderr %s", code, errOut)
	}

	header := line(t, out, "ID ")
	for _, want := range []string{"SAID", "MEMORY"} {
		if !strings.Contains(header, want) {
			t.Errorf("header = %q, want %q in it", header, want)
		}
	}
	// Only what still stands is listed, by the number it is forgotten by.
	if !strings.Contains(out, "Lisbon") {
		t.Errorf("out = %q, want the memory that stands", out)
	}
	if strings.Contains(out, "Porto") {
		t.Errorf("out = %q, want nothing of the memory it replaced", out)
	}
	if !strings.Contains(out, strconv.FormatInt(int64(newest), 10)) {
		t.Errorf("out = %q, want the number it is forgotten by", out)
	}
}

func TestMemorySearch(t *testing.T) {
	dir, _, _, other := remembered(t)
	cfg := memoryConfig(t, dir, embedding(t).URL)

	// A question about Caio's sister is nearest the memory about her, though it
	// shares not one word with it.
	code, out, errOut := exec(t, "-config", cfg, "memory", "search", "-n", "1", "where is Caio's sister")
	if code != 0 {
		t.Fatalf("code = %d, stderr %s", code, errOut)
	}
	if !strings.Contains(out, "Lisbon") || strings.Contains(out, "bicycle") {
		t.Errorf("out = %q, want the memory about Caio's sister", out)
	}
	// And one about the bike is nearest the other.
	if _, out, _ := exec(t, "-config", cfg, "memory", "search", "-n", "1", "was the bike ever mended"); !strings.Contains(out, "bicycle") {
		t.Errorf("out = %q, want the memory about the bicycle", out)
	} else if !strings.Contains(out, strconv.FormatInt(int64(other), 10)) {
		t.Errorf("out = %q, want it listed by its number", out)
	}
	// What to search for is not optional.
	if code, _, _ := exec(t, "-config", cfg, "memory", "search"); code != 2 {
		t.Errorf("searching for nothing = %d, want a usage error", code)
	}
}

// Searching asks a model, so it says which key is missing rather than
// answering with nothing.
func TestMemorySearchWithoutAModelThatEmbeds(t *testing.T) {
	dir, _, _, _ := remembered(t)
	t.Setenv("OPENROUTER_API_KEY", "test-token-abcdefgh")
	cfg := configFile(t, "data_dir: "+dir+
		"\nrunners:\n  r:\n    type: openrouter\nmodels:\n  talk:\n    runner: r\n    id: talk"+
		"\ndefault_models:\n  chat: talk\n")

	code, _, errOut := exec(t, "-config", cfg, "memory", "search", "lisbon")
	if code != 1 || !strings.Contains(errOut, "default_models.embed: no model is set") {
		t.Errorf("code = %d, stderr %q", code, errOut)
	}
	// Listing one asks no model, so it answers on the same file.
	if code, _, errOut := exec(t, "-config", cfg, "memory", "list"); code != 0 {
		t.Errorf("list = %d, stderr %s", code, errOut)
	}
}

func TestMemoryForget(t *testing.T) {
	dir, newest, replaced, _ := remembered(t)
	cfg := memoryConfig(t, dir, embedding(t).URL)

	code, out, errOut := exec(t, "-config", cfg, "memory", "forget", strconv.FormatInt(int64(newest), 10))
	if code != 0 {
		t.Fatalf("code = %d, stderr %s", code, errOut)
	}
	// What went is what it took: the memory, and the one it replaced.
	if !strings.HasPrefix(out, "forgot:") {
		t.Errorf("out = %q, want what was forgotten", out)
	}
	for _, want := range []string{"Lisbon", "Porto"} {
		if !strings.Contains(out, want) {
			t.Errorf("out = %q, want %q in it", out, want)
		}
	}

	if _, out, _ := exec(t, "-config", cfg, "memory", "list"); strings.Contains(out, "Lisbon") {
		t.Errorf("out = %q, want the memory gone", out)
	}
	// It is gone from what is searched as well as from what is listed.
	if _, out, _ := exec(t, "-config", cfg, "memory", "search", "where is Caio's sister"); strings.Contains(out, "Lisbon") {
		t.Errorf("out = %q, want nothing of a memory that is gone", out)
	}
	if code, _, _ := exec(t, "-config", cfg, "memory", "forget", strconv.FormatInt(int64(replaced), 10)); code == 0 {
		t.Error("a memory that is gone was forgotten again")
	}
	if code, _, _ := exec(t, "-config", cfg, "memory", "forget", "none"); code != 2 {
		t.Error("forgetting something that is not a number was not a usage error")
	}
}

// Forgetting writes, but it does not start a conversation: a data_dir with a
// typo in it says so rather than being left holding an empty database.
func TestForgettingWhereThereIsNoConversation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	cfg := memoryConfig(t, dir, embedding(t).URL)

	code, _, errOut := exec(t, "-config", cfg, "memory", "forget", "1")
	if code != 1 || !strings.Contains(errOut, "holds no conversation yet") {
		t.Errorf("code = %d, stderr %q", code, errOut)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s was left behind: %v", dir, err)
	}
}

func TestMemoryTakesOneOfItsCommands(t *testing.T) {
	dir, _, _, _ := remembered(t)
	cfg := memoryConfig(t, dir, embedding(t).URL)
	code, _, errOut := exec(t, "-config", cfg, "memory")
	if code != 2 {
		t.Errorf("code = %d, want a usage error", code)
	}
	for _, want := range []string{"list", "search", "forget"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr = %q, want %q among the commands", errOut, want)
		}
	}

	// A word that is none of them is a command that was misspelt, and saying
	// which leaves nothing to guess at.
	code, _, errOut = exec(t, "-config", cfg, "memory", "forgot", "7")
	if code != 2 || !strings.Contains(errOut, `unknown command "forgot"`) {
		t.Errorf("code = %d, stderr = %q, want it to name what was asked for", code, errOut)
	}
}
