package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"nerdola.dev/x/paula/internal/config"
	runnersapi "nerdola.dev/x/paula/internal/runners/api"
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

func (f *fakeEnv) Time(t time.Time) string { return t.UTC().Format("2 Jan 15:04") }

func (f *fakeEnv) Memories(_ context.Context, query string, limit int) ([]store.Memory, error) {
	f.searched, f.query, f.limit = true, query, limit
	return f.memories, nil
}

// LatestMemories are the test's memories, which it gives oldest first, newest
// first from the one at from on.
func (f *fakeEnv) LatestMemories(_ context.Context, from, limit int) ([]store.Memory, error) {
	newest := slices.Clone(f.memories)
	slices.Reverse(newest)
	if from >= len(newest) {
		return nil, nil
	}
	return newest[from:min(from+limit, len(newest))], nil
}

func (f *fakeEnv) Images(context.Context, int, int) ([]store.Image, error) { return nil, nil }

func (f *fakeEnv) Image(context.Context, int64) (*store.Image, error) { return nil, store.ErrNotFound }

func (f *fakeEnv) Show(store.Image) bool { return false }

func (f *fakeEnv) Photo(context.Context, string, string, []byte) (*store.Image, error) {
	return nil, nil
}

func (f *fakeEnv) SendPhoto(context.Context, int64) error { return nil }

func (f *fakeEnv) Now() time.Time { return time.Time{} }

func (f *fakeEnv) Callbacks(context.Context) ([]store.Callback, error) { return nil, nil }

func (f *fakeEnv) Schedule(context.Context, time.Time, string) (*store.Callback, error) {
	return nil, nil
}

func (f *fakeEnv) Move(context.Context, store.CallbackID, time.Time) error { return nil }

func (f *fakeEnv) Cancel(context.Context, store.CallbackID) error { return nil }

func (f *fakeEnv) Recorder() runnersapi.Recorder { return nil }

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

// The memories are listed newest first, with the number each is forgotten by
// and the day it was said, and a conversation that has none says so.
func TestAListIsTheNewestMemories(t *testing.T) {
	e := env()
	e.memories = nil
	if got, err := call(t, "list_memories", e, `{}`); err != nil || got != "no memories yet" {
		t.Errorf("listing no memories answered %q, %v", got, err)
	}

	e = env()
	got, err := call(t, "list_memories", e, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	want := "#7 (said on 20 Sep) Ana visits in December.\n" +
		"#3 (said on 19 Sep) Caio's sister Ana lives in Lisbon."
	if got != want {
		t.Errorf("the list answered\n%s\nwant\n%s", got, want)
	}
	if e.searched {
		t.Error("listing searched the memories")
	}
}

// What a call answers goes into the prompt of the round after it, so a list of
// many memories answers fifty at a time, saying where the older ones are.
func TestAListOfManyMemoriesAnswersAPageAtATime(t *testing.T) {
	e := env()
	e.memories = nil
	for i := range 120 {
		e.memories = append(e.memories, store.Memory{ID: store.MemoryID(i + 1), Content: fmt.Sprintf("Memory %d.", i+1),
			SaidAt: time.Date(2026, 9, 19, 1, 0, 0, 0, time.UTC)})
	}
	for _, tc := range []struct {
		args        string
		first, last string
		lines       int
		older       string
	}{
		{`{}`, "#120 ", "#71 ", 50, "list_memories with from 50"},
		{`{"from":50}`, "#70 ", "#21 ", 50, "list_memories with from 100"},
		{`{"from":100}`, "#20 ", "#1 ", 20, ""},
	} {
		got, err := call(t, "list_memories", e, tc.args)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(got, "\n")
		memories := lines
		if tc.older != "" {
			memories = lines[:len(lines)-1]
			if !strings.Contains(lines[len(lines)-1], tc.older) {
				t.Errorf("%s: the list ends with %q, want it to say %q", tc.args, lines[len(lines)-1], tc.older)
			}
		}
		if len(memories) != tc.lines || !strings.HasPrefix(memories[0], tc.first) ||
			!strings.HasPrefix(memories[len(memories)-1], tc.last) {
			t.Errorf("%s: the list is %d memories from %q to %q, want %d from %q to %q", tc.args, len(memories),
				memories[0], memories[len(memories)-1], tc.lines, tc.first, tc.last)
		}
	}
	if got, err := call(t, "list_memories", e, `{"from":200}`); err != nil || got != "no memories past the newest 200" {
		t.Errorf("listing past every memory answered %q, %v", got, err)
	}
	if _, err := call(t, "list_memories", e, `{"from":-1}`); err == nil {
		t.Error("listing from below zero was answered")
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
		{"list_memories", `{}`, "listing memories"},
		{"search_memories", `{"query":"Ana"}`, "searching memories for Ana"},
		{"remember", `{"memory":"Ana lives in Lisbon."}`, "remembering: Ana lives in Lisbon."},
		{"forget_memory", `{"id":3}`, "forgetting memory #3"},
	} {
		if got := tool(t, tc.name).Note(json.RawMessage(tc.args)); got != tc.want {
			t.Errorf("%s says %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A model is told who a memory is about and what it is written in by the card.
// Memories are never in its prompt, so the tools that reach them say so, and
// name the tools it has them by.
func TestAToolIsDescribedByTheCard(t *testing.T) {
	d := tool(t, "remember").Definition().Description
	for _, want := range []string{"Caio", "Paula", "Portuguese", "not in your prompt", "list_memories", "search_memories"} {
		if !strings.Contains(d, want) {
			t.Errorf("remember is described as %q, without %q", d, want)
		}
	}
	for _, name := range []string{"list_memories", "search_memories"} {
		d := tool(t, name).Definition().Description
		for _, want := range []string{"Caio", "not in your prompt", "remember", "forget_memory"} {
			if !strings.Contains(d, want) {
				t.Errorf("%s is described as %q, without %q", name, d, want)
			}
		}
	}
	if d := tool(t, "forget_memory").Definition().Description; !strings.Contains(d, "Caio") {
		t.Errorf("forget_memory is described as %q, without who it is about", d)
	}
	// A search looks for words, and the memories hold them in that language.
	var params struct {
		Properties struct {
			Query struct {
				Description string `json:"description"`
			} `json:"query"`
		} `json:"properties"`
	}
	raw := tool(t, "search_memories").Definition().Parameters
	if err := json.Unmarshal(raw, &params); err != nil {
		t.Fatalf("the parameters are %s: %v", raw, err)
	}
	if !strings.Contains(params.Properties.Query.Description, "Portuguese") {
		t.Errorf("the query is described as %q, without the language", params.Properties.Query.Description)
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
	i := slices.IndexFunc(tools, func(t api.Tool) bool { return t.Definition().Name == "search_memories" })
	e := env()
	if _, err := tools[i].Call(context.Background(), e, json.RawMessage(`{"query":"Ana"}`)); err != nil {
		t.Fatal(err)
	}
	if e.limit != 3 {
		t.Errorf("the conversation was asked for %d, want the 3 the file says", e.limit)
	}
}
