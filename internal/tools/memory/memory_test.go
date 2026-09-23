package memory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/store"
	"nerdola.dev/x/paula/internal/tools/api"
)

// fakeEnv is a conversation holding the memories a test gives it, which
// writes a day in a way of its own.
type fakeEnv struct {
	memories []store.Memory

	query    string
	limit    int
	kept     string
	replaces []store.MemoryID
	forgot   store.MemoryID
	searched bool
}

func (f *fakeEnv) Date(t time.Time) string { return t.UTC().Format("2 Jan") }

func (f *fakeEnv) Memories(_ context.Context, query string, limit int) ([]store.Memory, error) {
	f.searched, f.query, f.limit = true, query, limit
	return f.memories, nil
}

func (f *fakeEnv) Remember(_ context.Context, content string, replaces []store.MemoryID) (*store.Memory, error) {
	f.kept, f.replaces = content, replaces
	return &store.Memory{ID: 12, Content: content}, nil
}

func (f *fakeEnv) Forget(_ context.Context, id store.MemoryID) ([]store.Memory, error) {
	f.forgot = id
	for _, m := range f.memories {
		if m.ID == id {
			return []store.Memory{m}, nil
		}
	}
	return nil, store.ErrNotFound
}

func env() *fakeEnv {
	return &fakeEnv{
		memories: []store.Memory{
			{ID: 3, Content: "Caio's sister Ana lives in Lisbon.", SaidAt: time.Date(2026, 9, 19, 1, 0, 0, 0, time.UTC)},
			{ID: 7, Content: "Ana visits in December.", SaidAt: time.Date(2026, 9, 20, 18, 0, 0, 0, time.UTC)},
		},
	}
}

var host = api.Host{Names: api.Names{Character: "Paula", User: "Caio"}, Language: "Portuguese"}

func tool(t *testing.T, name string) api.Tool {
	t.Helper()
	tools, err := Open(config.Section{}, host)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools {
		if tool.Definition().Name == name {
			return tool
		}
	}
	t.Fatalf("no tool is called %s", name)
	return nil
}

func call(t *testing.T, name string, e *fakeEnv, args string) (string, error) {
	t.Helper()
	return tool(t, name).Call(context.Background(), e, json.RawMessage(args))
}

func TestASearchForNothingIsNotMade(t *testing.T) {
	for _, args := range []string{`{}`, `{"query":"  "}`} {
		e := env()
		if _, err := call(t, "search_memories", e, args); err == nil {
			t.Errorf("a search with %s succeeded", args)
		}
		if e.searched {
			t.Errorf("a search with %s was made", args)
		}
	}
}

// Each memory is found with the number it is forgotten by and the day it was
// said, written the way the conversation writes a day.
func TestASearchAnswersWithTheNumberAndTheDayOfEachMemory(t *testing.T) {
	e := env()
	got, err := call(t, "search_memories", e, `{"query":" where does Ana live "}`)
	if err != nil {
		t.Fatal(err)
	}
	want := "#3 (said on 19 Sep) Caio's sister Ana lives in Lisbon.\n" +
		"#7 (said on 20 Sep) Ana visits in December."
	if got != want {
		t.Errorf("the search answered\n%s\nwant\n%s", got, want)
	}
	if e.query != "where does Ana live" || e.limit != 10 {
		t.Errorf("the conversation was asked %q for %d", e.query, e.limit)
	}

	e.memories = nil
	if got, err := call(t, "search_memories", e, `{"query":"bicycle"}`); err != nil || got != "no memories match" {
		t.Errorf("a search that found nothing answered %q, %v", got, err)
	}
}

func TestNothingToRememberIsNotKept(t *testing.T) {
	e := env()
	if _, err := call(t, "remember", e, `{"memory":" "}`); err == nil {
		t.Error("an empty memory was kept")
	}
	if e.kept != "" {
		t.Errorf("the conversation was asked to keep %q", e.kept)
	}
}

func TestAMemoryIsKeptAsItWasWritten(t *testing.T) {
	e := env()
	got, err := call(t, "remember", e, `{"memory":" Caio's sister Ana lives in Lisbon. "}`)
	if err != nil {
		t.Fatal(err)
	}
	if got != "remembered #12" {
		t.Errorf("remember answered %q", got)
	}
	if e.kept != "Caio's sister Ana lives in Lisbon." || e.replaces != nil {
		t.Errorf("the conversation was asked to keep %q in place of %v", e.kept, e.replaces)
	}

	// A fact that updates what she found takes the place of it.
	if _, err := call(t, "remember", e, `{"memory":"Ana lives in Porto.","replaces":[3,7]}`); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(e.replaces, []store.MemoryID{3, 7}) {
		t.Errorf("the memory was kept in place of %v, want #3 and #7", e.replaces)
	}
}

func TestForgettingAMemoryThatIsNotThereSaysSo(t *testing.T) {
	e := env()
	_, err := call(t, "forget_memory", e, `{"id":9}`)
	if err == nil || err.Error() != "no memory is numbered 9" {
		t.Errorf("forgetting a memory that is not there = %v", err)
	}
}

func TestForgettingAMemorySaysWhatWent(t *testing.T) {
	e := env()
	got, err := call(t, "forget_memory", e, `{"id":3}`)
	if err != nil {
		t.Fatal(err)
	}
	if got != "forgot:\n#3 (said on 19 Sep) Caio's sister Ana lives in Lisbon." {
		t.Errorf("forgetting answered %q", got)
	}
	if e.forgot != 3 {
		t.Errorf("the conversation was asked to forget #%d", e.forgot)
	}
}

// What is shown while a call runs says what it was asked.
func TestEachCallSaysWhatItIsDoing(t *testing.T) {
	for _, tc := range []struct{ name, args, want string }{
		{"search_memories", `{"query":"Ana"}`, "searching memories for Ana"},
		{"remember", `{"memory":"Ana lives in Lisbon."}`, "remembering: Ana lives in Lisbon."},
		{"forget_memory", `{"id":3}`, "forgetting memory #3"},
	} {
		if got := tool(t, tc.name).Note(json.RawMessage(tc.args)); got != tc.want {
			t.Errorf("%s says %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A model is told who a memory is about and what it is written in by the card,
// the same way a fold is.
func TestAToolIsDescribedByTheCard(t *testing.T) {
	d := tool(t, "remember").Definition().Description
	for _, want := range []string{"Caio", "Paula", "Portuguese"} {
		if !strings.Contains(d, want) {
			t.Errorf("remember is described as %q, without %q", d, want)
		}
	}
	for _, name := range []string{"search_memories", "forget_memory"} {
		if d := tool(t, name).Definition().Description; !strings.Contains(d, "Caio") {
			t.Errorf("%s is described as %q, without who it is about", name, d)
		}
	}
}

// section is the memory section of a configuration file, written as given.
func section(t *testing.T, memory string) config.Section {
	t.Helper()
	path := filepath.Join(t.TempDir(), "paula.yaml")
	file := "persona: paula.yaml\nrunners:\n  r:\n    type: venice\nmodels:\n  chat:\n    runner: r\n    id: x\n" +
		"default_models:\n  chat: chat\ntools:\n  memory:\n" + memory
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Tools[0].Section
}

// A problem of the section is named where the file has it.
func TestTheSettingsAreHeldToWhatTheyCanBe(t *testing.T) {
	for _, tc := range []struct{ memory, want string }{
		{"    results: 0\n", "tools.memory: results: 0 is below one"},
		{"    size: 5\n", "size"},
	} {
		_, err := Open(section(t, tc.memory), host)
		if err == nil || !strings.Contains(err.Error(), "tools.memory") || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q = %v, want %q", tc.memory, err, tc.want)
		}
	}
}

func TestASearchAnswersWithAsManyAsTheFileSays(t *testing.T) {
	tools, err := Open(section(t, "    results: 3\n"), host)
	if err != nil {
		t.Fatal(err)
	}
	e := env()
	if _, err := tools[0].Call(context.Background(), e, json.RawMessage(`{"query":"Ana"}`)); err != nil {
		t.Fatal(err)
	}
	if e.limit != 3 {
		t.Errorf("the conversation was asked for %d, want the 3 the file says", e.limit)
	}
}
