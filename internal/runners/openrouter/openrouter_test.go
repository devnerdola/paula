package openrouter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/logs"
	"nerdola.dev/x/paula/internal/runners/api"
)

func read(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// catalogue answers the listings from the captured ones: the models that
// write, and the ones that embed, which the API lists apart from them.
func catalogue(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			w.Write(read(t, "models.json"))
		case "/v1/embeddings/models":
			w.Write(read(t, "embedding_models.json"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

func runner(t *testing.T, url string, body string) *Runner {
	t.Helper()
	return runnerFor(t, url, body, api.Host{})
}

// runnerFor builds a runner of a configuration under a host of its own, for a
// test that watches what the runner tells the host.
func runnerFor(t *testing.T, url string, body string, h api.Host) *Runner {
	t.Helper()
	t.Setenv("OPENROUTER_API_KEY", "test-token-abcdefgh")
	dir := t.TempDir()
	path := filepath.Join(dir, "paula.yaml")
	file := "persona: paula.yaml\nrunners:\n  openrouter:\n    type: openrouter\n    url: " + url + "/v1\n"
	for line := range strings.SplitSeq(body, "\n") {
		if line != "" {
			file += "    " + line + "\n"
		}
	}
	file += "models:\n  chat:\n    runner: openrouter\n    id: x\ndefault_models:\n  chat: chat\n"
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Open("openrouter", cfg.Runners[0].Section, h)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func model(t *testing.T, r *Runner, id string) api.Model {
	t.Helper()
	m, err := r.Model(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return *m
}

// What Paula makes of each entry of the captured listing.
func TestCatalogue(t *testing.T) {
	r := runner(t, catalogue(t).URL, "")

	// A model that reasons, sees images, takes tools and answers a schema.
	m := model(t, r, "~deepseek/deepseek-flash-latest")
	if !m.Chat || !m.Vision || !m.Tools || !m.Reasoning || !m.StructuredOutputs {
		t.Errorf("%s = %+v", m.ID, m)
	}
	if m.Mandatory {
		t.Errorf("%s reads as always reasoning", m.ID)
	}
	if m.Context != 1048576 {
		t.Errorf("%s has context %d, want the listing's", m.ID, m.Context)
	}
	if !slices.Equal(m.Efforts, []string{"low", "high", "max"}) {
		t.Errorf("%s efforts = %v, want the listing's in the documented order", m.ID, m.Efforts)
	}
	if slices.Contains(m.Efforts, "none") {
		t.Errorf("%s efforts = %v, want none left out", m.ID, m.Efforts)
	}

	// A model whose reasoning cannot be turned off.
	if m := model(t, r, "~openai/gpt-astra-latest"); !m.Mandatory || !m.Reasoning {
		t.Errorf("%s = %+v, want it always reasoning", m.ID, m)
	}

	// A model that does not reason.
	m = model(t, r, "inference-net/schematron-v2-turbo")
	if m.Reasoning || m.Mandatory || len(m.Efforts) != 0 {
		t.Errorf("%s = %+v, want no reasoning", m.ID, m)
	}

	// A model that reasons without listing efforts.
	if m := model(t, r, "inclusionai/ling-3.0-flash-vl"); !m.Reasoning || len(m.Efforts) != 0 {
		t.Errorf("%s = %+v, want reasoning with no efforts", m.ID, m)
	}

	// A model that embeds, which the API lists apart from the ones that write.
	// It writes nothing, and the catalogue says so.
	m = model(t, r, "openai/text-embedding-3-small")
	if !m.Embeddings || m.Chat || m.Vision || m.Tools || m.Reasoning {
		t.Errorf("%s = %+v, want a model that only embeds", m.ID, m)
	}
	if m.Context != 8192 {
		t.Errorf("%s has context %d, want the listing's", m.ID, m.Context)
	}

	models, err := r.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The catalogue is both listings: the models she can talk to, and the ones
	// she can embed with.
	if len(models) != 8 {
		t.Errorf("models = %d, want the five that write and the three that embed", len(models))
	}
}

// Half a listing is not a catalogue: the models that embed are asked for apart
// from the ones that write, and a run that took what came back would hold every
// model of the listing it missed to be one the API does not serve.
func TestAListingOfTheModelsThatEmbedThatCannotBeRead(t *testing.T) {
	var asked int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/v1/embeddings/models" {
			w.Write(read(t, "models.json"))
			return
		}
		asked++
		if asked == 1 {
			http.Error(w, `{"error":{"message":"upstream is away"}}`, http.StatusBadGateway)
			return
		}
		w.Write(read(t, "embedding_models.json"))
	}))
	t.Cleanup(ts.Close)

	r := runner(t, ts.URL, "retries: 0")
	ctx := context.Background()
	if _, err := r.Models(ctx); err == nil {
		t.Fatal("the models that write were taken as the whole catalogue")
	}
	// Nothing of a read that failed is held onto, so the next one asks again
	// and the catalogue is both listings.
	models, err := r.Models(ctx)
	if err != nil {
		t.Fatalf("the catalogue was never read again: %v", err)
	}
	if len(models) != 8 {
		t.Errorf("models = %d, want the five that write and the three that embed", len(models))
	}
}

func TestCatalogueIsReadOnce(t *testing.T) {
	var reads int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/embeddings/models" {
			w.Write(read(t, "embedding_models.json"))
			return
		}
		reads++
		w.Write(read(t, "models.json"))
	}))
	t.Cleanup(ts.Close)

	r := runner(t, ts.URL, "")
	for range 3 {
		if _, err := r.Models(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if reads != 1 {
		t.Errorf("reads = %d, want the listing read once", reads)
	}
}

// The answer is served no-store, and the models it names are what every
// configured model is held to, so a second runner over the same data directory
// asks the API again.
func TestEveryRunReadsTheListing(t *testing.T) {
	var (
		mu    sync.Mutex
		reads int
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/embeddings/models" {
			w.Write(read(t, "embedding_models.json"))
			return
		}
		mu.Lock()
		reads++
		mu.Unlock()
		w.Write(read(t, "models.json"))
	}))
	t.Cleanup(ts.Close)

	for range 2 {
		r := runner(t, ts.URL, "")
		models, err := r.Models(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(models) == 0 {
			t.Fatal("the run read an empty catalogue")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if reads != 2 {
		t.Errorf("the listing was read %d times, want it read by each run", reads)
	}
}

// The stream is the one a reply that asked for two tools came back in, as
// OpenRouter sent it: each call opened by its id and name and then built from
// fragments of its arguments, some of them empty.
func TestTheCallsOfACapturedStreamComeBackWhole(t *testing.T) {
	res := streamed(t, "stream_tool_calls.sse")
	want := []api.ToolCall{
		{ID: "call_99c9ff3a556f41f49700d812", Name: "search_memories", Arguments: `{"query": "Caio's sister name"}`},
		{ID: "call_7169b5d7266d44e1b8cc2058", Name: "search_memories", Arguments: `{"query": "sister"}`},
	}
	if !slices.Equal(res.ToolCalls, want) {
		t.Errorf("calls = %+v, want %+v", res.ToolCalls, want)
	}
	if res.FinishReason != "tool_calls" {
		t.Errorf("finish reason = %q", res.FinishReason)
	}
}

// A stream sends each item of what a model thought a fragment at a time under
// the index it has, so a reply comes back with the one item its fragments
// make: the text OpenRouter sent beside them as the reasoning, and every other
// field as it came.
func TestTheReasoningOfAReplyComesBackAsTheItemItMakes(t *testing.T) {
	res := streamed(t, "stream_tool_calls.sse")
	if len(res.ReasoningDetails) != 1 {
		t.Fatalf("reasoning_details = %s, want the one item the fragments make", res.ReasoningDetails)
	}
	var item map[string]any
	if err := json.Unmarshal(res.ReasoningDetails[0], &item); err != nil {
		t.Fatal(err)
	}
	if item["text"] != res.Reasoning {
		t.Errorf("the item says %q, want the reasoning OpenRouter sent beside it, %q", item["text"], res.Reasoning)
	}
	if item["type"] != "reasoning.text" || item["format"] != "unknown" || item["index"] != 0.0 {
		t.Errorf("the item = %+v, want every other field as it came", item)
	}
}

// The reasoning documentation asks for the details of what a model thought to
// go back with what it wrote, as they came. A reply whose reasoning came as
// text alone, as a runner that sends no details keeps it, goes back as the
// text.
func TestTheReasoningOfAReplyGoesBackAsItCame(t *testing.T) {
	// A message that thought nothing hands nothing back.
	none := map[string]any{}
	hooks{}.Message(none, api.Text(api.RoleAssistant, "hey"))
	if len(none) != 0 {
		t.Errorf("a message with no reasoning carries %+v", none)
	}

	res := streamed(t, "stream_tool_calls.sse")
	out := map[string]any{}
	hooks{}.Message(out, api.Message{
		Role:      api.RoleAssistant,
		Reasoning: &api.Reasoning{Text: res.Reasoning, Details: res.ReasoningDetails},
	})
	sent, err := json.Marshal(out["reasoning_details"])
	if err != nil {
		t.Fatal(err)
	}
	came, err := json.Marshal(res.ReasoningDetails)
	if err != nil {
		t.Fatal(err)
	}
	if string(sent) != string(came) || out["reasoning"] != nil {
		t.Errorf("the message carries %s and %v, want the details as they came, %s", sent, out["reasoning"], came)
	}

	text := map[string]any{}
	hooks{}.Message(text, api.Message{Role: api.RoleAssistant, Reasoning: &api.Reasoning{Text: res.Reasoning}})
	if text["reasoning"] != res.Reasoning || text["reasoning_details"] != nil {
		t.Errorf("a reply that thought in text alone carries %+v, want the text", text)
	}
}

// streamed is what the runner makes of a captured stream.
func streamed(t *testing.T, name string) *api.Result {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write(read(t, name))
	}))
	t.Cleanup(ts.Close)
	res, err := runner(t, ts.URL, "").Chat(context.Background(), api.ChatRequest{Model: "m"},
		func(api.Chunk) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestUnknownModel(t *testing.T) {
	r := runner(t, catalogue(t).URL, "")
	if _, err := r.Model(context.Background(), "nope/nope"); err == nil {
		t.Fatal("Model succeeded")
	}
}

// The stream is the one a reply with reasoning came back in, as OpenRouter sent
// it.
func TestStreamOfACapturedReply(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write(read(t, "stream_reply.sse"))
	}))
	t.Cleanup(ts.Close)
	r := runner(t, ts.URL, "")

	var text, reasoning strings.Builder
	res, err := r.Chat(context.Background(), api.ChatRequest{Model: "m"}, func(c api.Chunk) error {
		switch c.Kind {
		case api.ChunkText:
			text.WriteString(c.Text)
		case api.ChunkReasoning:
			reasoning.WriteString(c.Text)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if text.Len() == 0 {
		t.Error("no text came out of the stream")
	}
	if reasoning.String() != res.Reasoning {
		t.Errorf("the reasoning chunks and the result disagree")
	}
	if res.Reasoning == "" {
		t.Error("no reasoning came out of the stream")
	}
	if res.FinishReason != "stop" {
		t.Errorf("finish reason = %q", res.FinishReason)
	}
	if res.Provider == "" {
		t.Error("the serving provider was not read")
	}
	if res.Usage.PromptTokens == 0 || res.Usage.CompletionTokens == 0 {
		t.Errorf("usage = %+v", res.Usage)
	}
	if res.Usage.ReasoningTokens == 0 {
		t.Errorf("usage = %+v, want the reasoning tokens", res.Usage)
	}
	if res.Usage.Cost == 0 {
		t.Errorf("usage = %+v, want the cost", res.Usage)
	}
}

func TestCapturedErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		file   string
		status int
		code   string
		want   string
	}{
		{"a model that does not exist", "error_bad_model.json", 400, "", "is not a valid model ID"},
		{"a key that does not exist", "error_unauthorized.json", 401, "", "User not found."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := read(t, tc.file)
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				w.Write(body)
			}))
			t.Cleanup(ts.Close)
			r := runner(t, ts.URL, "")

			_, err := r.Chat(context.Background(), api.ChatRequest{Model: "m"}, func(api.Chunk) error { return nil })
			var e *api.APIError
			if !errors.As(err, &e) {
				t.Fatalf("error = %v, want an APIError", err)
			}
			if e.Status != tc.status || e.Code != tc.code {
				t.Errorf("error = %+v", e)
			}
			if !strings.Contains(e.Message, tc.want) {
				t.Errorf("message = %q, want %q in it", e.Message, tc.want)
			}
		})
	}
}

func TestEndpointsCheck(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/models":
			w.Write(read(t, "models.json"))
		case r.URL.Path == "/v1/embeddings/models":
			w.Write(read(t, "embedding_models.json"))
		case strings.HasSuffix(r.URL.Path, "/endpoints"):
			w.Write(read(t, "endpoints.json"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)

	const id = "deepseek/deepseek-v4-pro-0813"
	needs := api.Needs{Chat: true, Tools: true}

	// A host the listing holds, which takes everything.
	r := runner(t, ts.URL, "provider:\n  routing:\n    only: [ionstream]")
	s := api.Settings{Reasoning: api.ReasoningSettings{Mode: api.ReasoningOn}, Provider: providerOf(t, r)}
	if err := check(context.Background(), r, api.Checked{ID: id, Settings: s, Needs: needs}); err != nil {
		t.Errorf("Check = %v, want it to pass", err)
	}

	// A base slug covers its variants, so novita matches novita/fp8, which
	// takes everything but a minimum probability.
	r = runner(t, ts.URL, "provider:\n  routing:\n    only: [novita]")
	s.Provider = providerOf(t, r)
	if err := check(context.Background(), r, api.Checked{ID: id, Settings: s, Needs: needs}); err != nil {
		t.Errorf("Check = %v, want novita/fp8 to match novita", err)
	}
	picky := s
	picky.Sampling.MinP = new(0.1)
	err := check(context.Background(), r, api.Checked{ID: id, Settings: picky, Needs: needs})
	if err == nil {
		t.Fatal("Check passed")
	}
	if !strings.Contains(err.Error(), "novita/fp8 takes no min_p") {
		t.Errorf("error = %v", err)
	}

	// A host that serves the model with a smaller context than the file asks
	// for is a problem, however it was found to take every parameter.
	r = runner(t, ts.URL, "provider:\n  routing:\n    only: [streamlake]")
	s.Provider = providerOf(t, r)
	err = check(context.Background(), r, api.Checked{ID: id, Settings: s, Needs: api.Needs{Chat: true, Tools: true, Context: 1048576}})
	if err == nil {
		t.Fatal("Check passed")
	}
	if !strings.Contains(err.Error(), "holding 1024000") {
		t.Errorf("error = %v, want the context the host holds", err)
	}

	// A host the listing does not hold.
	r = runner(t, ts.URL, "provider:\n  routing:\n    only: [nowhere]")
	s.Provider = providerOf(t, r)
	err = check(context.Background(), r, api.Checked{ID: id, Settings: s, Needs: needs})
	if err == nil {
		t.Fatal("Check passed")
	}
	if !strings.Contains(err.Error(), "no endpoint of the model") {
		t.Errorf("error = %v", err)
	}
}

// check is what the runner found wrong with a model, as one error to read.
func check(ctx context.Context, r *Runner, m api.Checked) error {
	return errors.Join(r.Check(ctx, m)...)
}

func providerOf(t *testing.T, r *Runner) config.Section {
	t.Helper()
	return r.Settings().Provider
}

func TestCheckHoldsTheSettingsAgainstTheCatalogue(t *testing.T) {
	r := runner(t, catalogue(t).URL, "")
	ctx := context.Background()

	for _, tc := range []struct {
		name  string
		id    string
		s     api.Settings
		needs api.Needs
		want  string
	}{
		{
			"reasoning off on a model that always reasons",
			"~openai/gpt-astra-latest",
			api.Settings{Reasoning: api.ReasoningSettings{Mode: api.ReasoningOff}},
			api.Needs{}, "the model always reasons",
		},
		{
			"reasoning on a model that does not reason",
			"inference-net/schematron-v2-turbo",
			api.Settings{Reasoning: api.ReasoningSettings{Mode: api.ReasoningOn}},
			api.Needs{}, "the model does not reason",
		},
		{
			"an effort the model does not list",
			"deepseek/deepseek-v4-pro-0813",
			api.Settings{Reasoning: api.ReasoningSettings{Effort: "medium"}},
			api.Needs{}, "is not one of",
		},
		{
			"a sampling setting the model does not take",
			"~openai/gpt-astra-latest",
			api.Settings{Sampling: api.SamplingSettings{TopK: new(40)}},
			api.Needs{}, "does not take top_k",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := check(ctx, r, api.Checked{ID: tc.id, Settings: tc.s, Needs: tc.needs})
			if err == nil {
				t.Fatalf("Check passed, want %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want %q in it", err, tc.want)
			}
		})
	}

	s := api.Settings{
		Reasoning: api.ReasoningSettings{Mode: api.ReasoningOn, Effort: "high"},
		Sampling:  api.SamplingSettings{Temperature: new(0.8)},
		Output:    api.OutputSettings{MaxTokens: new(2048)},
	}
	needs := api.Needs{Chat: true, Tools: true}
	if err := check(ctx, r, api.Checked{ID: "deepseek/deepseek-v4-pro-0813", Settings: s, Needs: needs}); err != nil {
		t.Errorf("Check = %v, want it to pass", err)
	}
}

func TestOpenWithoutAToken(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "")
	dir := t.TempDir()
	path := filepath.Join(dir, "paula.yaml")
	os.WriteFile(path, []byte("persona: paula.yaml\nrunners:\n  openrouter:\n    type: openrouter\nmodels:\n  chat:\n    runner: openrouter\n    id: x\ndefault_models:\n  chat: chat\n"), 0o600)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Open("openrouter", cfg.Runners[0].Section, api.Host{})
	if err == nil {
		t.Fatal("Open succeeded")
	}
	if !strings.Contains(err.Error(), "OPENROUTER_API_KEY is not set") {
		t.Errorf("error = %v", err)
	}
}

// Redacting a value that short would mangle ordinary text, so it would reach
// the log as it is: it is refused rather than used.
func TestOpenWithAKeyTooShortToBeOne(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "sk-1")
	dir := t.TempDir()
	path := filepath.Join(dir, "paula.yaml")
	os.WriteFile(path, []byte("persona: paula.yaml\nrunners:\n  openrouter:\n    type: openrouter\nmodels:\n  chat:\n    runner: openrouter\n    id: x\ndefault_models:\n  chat: chat\n"), 0o600)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open("openrouter", cfg.Runners[0].Section, api.Host{}); err == nil {
		t.Fatal("Open succeeded")
	} else if !strings.Contains(err.Error(), "too short to be a key") {
		t.Errorf("error = %v, want it to say the value is no key", err)
	}
}

// A key written with nothing after it is a key the file says nothing about:
// the value it was given is taken away rather than left as it was, so it is
// reported under its own name like any other value a runner cannot take.
func TestOpenWithARetriesKeyThatSaysNothing(t *testing.T) {
	for _, written := range []string{"retries:", "retries: ~", "retries: null"} {
		t.Setenv("OPENROUTER_API_KEY", "test-token-abcdefgh")
		path := filepath.Join(t.TempDir(), "paula.yaml")
		file := "persona: paula.yaml\nrunners:\n  openrouter:\n    type: openrouter\n    " + written +
			"\nmodels:\n  chat:\n    runner: openrouter\n    id: x\ndefault_models:\n  chat: chat\n"
		if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		_, err = Open("openrouter", cfg.Runners[0].Section, api.Host{})
		if err == nil {
			t.Errorf("%q was taken", written)
			continue
		}
		if !strings.Contains(err.Error(), "retries: no number is written") {
			t.Errorf("%q = %v, want it reported under its key", written, err)
		}
	}
}

func TestOpenRejectsUnknownKeys(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "test-token-abcdefgh")
	dir := t.TempDir()
	path := filepath.Join(dir, "paula.yaml")
	os.WriteFile(path, []byte("persona: paula.yaml\nrunners:\n  openrouter:\n    type: openrouter\n    family: deepseek\nmodels:\n  chat:\n    runner: openrouter\n    id: x\ndefault_models:\n  chat: chat\n"), 0o600)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Open("openrouter", cfg.Runners[0].Section, api.Host{})
	if err == nil {
		t.Fatal("Open succeeded")
	}
	if !strings.Contains(err.Error(), "family: unknown key") {
		t.Errorf("error = %v", err)
	}
	if !strings.HasPrefix(err.Error(), "runners.openrouter: ") {
		t.Errorf("error = %q, want the key path first", err)
	}
}

func TestACatalogueThatFailedIsAskedAgain(t *testing.T) {
	var asked int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/embeddings/models" {
			w.Write(read(t, "embedding_models.json"))
			return
		}
		asked++
		if asked == 1 {
			http.Error(w, `{"error":{"message":"upstream is away"}}`, http.StatusBadGateway)
			return
		}
		w.Write(read(t, "models.json"))
	}))
	t.Cleanup(ts.Close)

	r := runner(t, ts.URL, "retries: 0")
	ctx := context.Background()
	if _, err := r.Models(ctx); err == nil {
		t.Fatal("the first read succeeded")
	}
	models, err := r.Models(ctx)
	if err != nil {
		t.Fatalf("the catalogue was never read again: %v", err)
	}
	if len(models) == 0 {
		t.Error("the catalogue is empty")
	}
}

// An API that says the key back in an error is recorded without it: the token a
// runner opens with is a secret of the run, whatever writes it.
func TestAKeyAnAPISaysBackIsNotRecorded(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"no such key: test-token-abcdefgh","code":401}}`)
	}))
	t.Cleanup(ts.Close)
	secrets := new(logs.Secrets)
	r := runnerFor(t, ts.URL, "", api.Host{Secrets: secrets})

	kept := &records{}
	_, err := r.Chat(context.Background(), api.ChatRequest{Model: "m", Recorder: kept},
		func(api.Chunk) error { return nil })
	if err == nil {
		t.Fatal("the API answered a key it refused")
	}
	if len(kept.ended) != 1 {
		t.Fatalf("records = %d, want one", len(kept.ended))
	}
	rec := kept.ended[0]
	if strings.Contains(rec.Error, "test-token-abcdefgh") {
		t.Errorf("the record holds the key: %q", rec.Error)
	}
	if !strings.Contains(rec.Error, logs.Mask) {
		t.Errorf("error = %q, want the key replaced", rec.Error)
	}
	for _, a := range rec.Attempts {
		if strings.Contains(a.Error, "test-token-abcdefgh") {
			t.Errorf("an attempt holds the key: %q", a.Error)
		}
	}
}

// records keeps what a runner reported, the way the conversation does.
type records struct{ ended []*api.Record }

func (r *records) StartRequest(context.Context, *api.Record) error { return nil }

func (r *records) EndRequest(_ context.Context, rec *api.Record) error {
	r.ended = append(r.ended, rec)
	return nil
}

// The host that served a reply, how it ended and what it cost are what paula
// turns reads back.
func TestWhatAnAnswerSaysOfItselfIsRecorded(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write(read(t, "stream_reply.sse"))
	}))
	t.Cleanup(ts.Close)
	r := runner(t, ts.URL, "")

	kept := &records{}
	res, err := r.Chat(context.Background(), api.ChatRequest{Model: "m", Recorder: kept},
		func(api.Chunk) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(kept.ended) != 1 {
		t.Fatalf("records = %d, want one", len(kept.ended))
	}

	rec := kept.ended[0]
	if rec.Provider != res.Provider || rec.Provider == "" {
		t.Errorf("provider = %q, want the one the answer named", rec.Provider)
	}
	if rec.FinishReason != res.FinishReason || rec.FinishReason == "" {
		t.Errorf("finish = %q", rec.FinishReason)
	}
	if rec.Usage == nil || *rec.Usage != res.Usage {
		t.Fatalf("usage = %+v, want what the answer reported", rec.Usage)
	}
	if rec.Usage.PromptTokens == 0 || rec.Usage.CompletionTokens == 0 {
		t.Errorf("usage = %+v", rec.Usage)
	}
	if rec.Usage.Cost == 0 || rec.Usage.Cost != res.Usage.Cost {
		t.Errorf("cost = %v, want what it cost", rec.Usage.Cost)
	}
}

// Many readers share one read, and each gets a copy of its own.
func TestTheCatalogueIsReadOnceUnderManyReaders(t *testing.T) {
	var reads atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			reads.Add(1)
			w.Write(read(t, "models.json"))
		case "/v1/embeddings/models":
			w.Write(read(t, "embedding_models.json"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	r := runner(t, ts.URL, "")

	var wg sync.WaitGroup
	lists := make([][]api.Model, 8)
	errs := make([]error, 8)
	for i := range lists {
		wg.Go(func() {
			lists[i], errs[i] = r.Models(context.Background())
		})
	}
	wg.Wait()

	for i := range lists {
		if errs[i] != nil {
			t.Fatalf("read %d: %v", i, errs[i])
		}
		if len(lists[i]) != len(lists[0]) {
			t.Fatalf("read %d gave %d models, want %d", i, len(lists[i]), len(lists[0]))
		}
	}
	if reads.Load() != 1 {
		t.Errorf("the catalogue was read %d times", reads.Load())
	}

	// What one reader does with its own list is not what the next one gets.
	slices.Reverse(lists[0])
	again, err := r.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if again[0].ID == lists[0][0].ID && len(again) > 1 {
		t.Error("a reader that sorted its list sorted the runner's")
	}
}

// A model may take the block its runner writes, so the key path has to be given
// in full.
func TestAProblemInsideTheProviderIsNamedInFull(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "test-token-abcdefgh")
	path := filepath.Join(t.TempDir(), "paula.yaml")
	file := "persona: paula.yaml\nrunners:\n  r:\n    type: openrouter\n    provider:\n      nope: 1\n" +
		"models:\n  a:\n    runner: r\n    id: x\ndefault_models:\n  chat: a\n"
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Open("r", cfg.Runners[0].Section, api.Host{})
	if err == nil {
		t.Fatal("Open succeeded")
	}
	if !strings.Contains(err.Error(), "runners.r.provider") {
		t.Errorf("error = %v, want the key it is written under", err)
	}
}

// The settings this API names differently go where it documents them, not where
// an OpenAI body would carry them.
// The requests that share a prompt are kept on the host that has read it by
// the session id the prompt caching documentation names; a request that shares
// its prompt with nothing names none.
func TestARequestNamesTheSessionItsPromptBelongsTo(t *testing.T) {
	var bodies []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write(read(t, "stream_reply.sse"))
	}))
	t.Cleanup(ts.Close)

	r := runner(t, ts.URL, "")
	for _, key := range []string{"", "paula-paula-reply"} {
		_, err := r.Chat(context.Background(), api.ChatRequest{
			Model:    "some/model",
			Messages: []api.Message{api.Text(api.RoleUser, "hey")},
			CacheKey: key,
		}, func(api.Chunk) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
	}
	type named struct {
		Session *string `json:"session_id"`
	}
	var sent []named
	for _, b := range bodies {
		var s named
		if err := json.Unmarshal([]byte(b), &s); err != nil {
			t.Fatal(err)
		}
		sent = append(sent, s)
	}
	if len(sent) != 2 {
		t.Fatalf("%d requests were sent, want two", len(sent))
	}
	if sent[0].Session != nil {
		t.Errorf("a request with no cache key names the session %q", *sent[0].Session)
	}
	if sent[1].Session == nil || *sent[1].Session != "paula-paula-reply" {
		t.Errorf("the session is %v, want the cache key", sent[1].Session)
	}
}

func TestTheRequestBodyCarriesWhatOnlyOpenRouterDocuments(t *testing.T) {
	var body []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write(read(t, "stream_reply.sse"))
	}))
	t.Cleanup(ts.Close)

	r := runner(t, ts.URL, `
provider:
  sampling:
    top_a: 0.2
    logit_bias:
      "123": -5
  cache:
    control:
      type: ephemeral
      ttl: 5m
  service_tier: flex
  reasoning:
    max_tokens: 2048
  routing:
    only: [ionstream]
    zdr: true
`)
	s := r.Settings()
	s.Reasoning = api.ReasoningSettings{Mode: api.ReasoningOn, Effort: "high"}
	_, err := r.Chat(context.Background(), api.ChatRequest{
		Model:    "some/model",
		Messages: []api.Message{api.Text(api.RoleUser, "hey")},
		Settings: s,
	}, func(api.Chunk) error { return nil })
	if err != nil {
		t.Fatal(err)
	}

	var sent struct {
		TopA      float64        `json:"top_a"`
		LogitBias map[string]int `json:"logit_bias"`
		Cache     struct {
			Type string `json:"type"`
			TTL  string `json:"ttl"`
		} `json:"cache_control"`
		Tier      string `json:"service_tier"`
		Reasoning struct {
			Enabled   *bool  `json:"enabled"`
			Effort    string `json:"effort"`
			MaxTokens int    `json:"max_tokens"`
		} `json:"reasoning"`
		Provider struct {
			Only []string `json:"only"`
			ZDR  bool     `json:"zdr"`
		} `json:"provider"`
	}
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("the body was not read back: %v (%s)", err, body)
	}
	if sent.TopA != 0.2 || sent.LogitBias["123"] != -5 {
		t.Errorf("sampling = %+v", sent)
	}
	if sent.Cache.Type != "ephemeral" || sent.Cache.TTL != "5m" || sent.Tier != "flex" {
		t.Errorf("cache and tier = %+v, %q", sent.Cache, sent.Tier)
	}
	if sent.Reasoning.Enabled == nil || !*sent.Reasoning.Enabled ||
		sent.Reasoning.Effort != "high" || sent.Reasoning.MaxTokens != 2048 {
		t.Errorf("reasoning = %+v", sent.Reasoning)
	}
	if !slices.Equal(sent.Provider.Only, []string{"ionstream"}) || !sent.Provider.ZDR {
		t.Errorf("provider = %+v", sent.Provider)
	}
}

// A cache is written only where a request marks it, and the end of a prompt is
// what the next reply changes. So the last message the next reply sends again is
// marked beside the end, and only on a model that is told to cache.
func TestTheLastMessageThatStandsIsMarkedForTheCache(t *testing.T) {
	var bodies [][]byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, b)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write(read(t, "stream_reply.sse"))
	}))
	t.Cleanup(ts.Close)

	messages := []api.Message{
		api.Text(api.RoleSystem, "You are Paula."),
		api.Text(api.RoleUser, "hey"),
		api.Text(api.RoleAssistant, "hi love"),
		api.Text(api.RoleSystem, "It is now Wednesday."),
		api.Text(api.RoleUser, "you there?"),
	}
	plain := runner(t, ts.URL, "")
	cached := runner(t, ts.URL, "provider:\n  cache:\n    control:\n      type: ephemeral\n      ttl: 1h\n")
	for _, c := range []struct {
		r        *Runner
		standing int
	}{{plain, 3}, {cached, 0}, {cached, 3}} {
		_, err := c.r.Chat(context.Background(), api.ChatRequest{
			Model: "some/model", Messages: messages, Settings: c.r.Settings(), Standing: c.standing,
		}, func(api.Chunk) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
	}

	type sent struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	var got []sent
	for _, b := range bodies {
		var s sent
		if err := json.Unmarshal(b, &s); err != nil {
			t.Fatalf("the body was not read back: %v (%s)", err, b)
		}
		got = append(got, s)
	}
	if len(got) != 3 {
		t.Fatalf("%d requests were sent, want three", len(got))
	}
	for i, s := range got {
		for j, m := range s.Messages {
			marked := i == 2 && j == 2
			if !marked && bytes.Contains(m.Content, []byte("cache_control")) {
				t.Errorf("request %d marks message %d: %s", i+1, j, m.Content)
			}
			if !marked && m.Content[0] != '"' {
				t.Errorf("request %d sends message %d as %s, want its text", i+1, j, m.Content)
			}
		}
	}
	var parts []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Control struct {
			Type string `json:"type"`
			TTL  string `json:"ttl"`
		} `json:"cache_control"`
	}
	if err := json.Unmarshal(got[2].Messages[2].Content, &parts); err != nil {
		t.Fatalf("the marked message is %s: %v", got[2].Messages[2].Content, err)
	}
	if len(parts) != 1 || parts[0].Type != "text" || parts[0].Text != "hi love" ||
		parts[0].Control.Type != "ephemeral" || parts[0].Control.TTL != "1h" {
		t.Errorf("the marked message is %+v, want its text as one part carrying the marker", parts)
	}
}

// The memories are the conversation in the words a fold left it in, so where a
// request may be routed, and what a host may keep of it, holds for them as it
// does for a reply. What is about writing one does not.
func TestEmbeddingCarriesTheRoutingAndNothingAboutWriting(t *testing.T) {
	var body []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"object":"list","data":[{"index":0,"object":"embedding",`+
			`"embedding":[1,0]}],"usage":{"prompt_tokens":4}}`)
	}))
	t.Cleanup(ts.Close)

	r := runner(t, ts.URL, `
provider:
  sampling:
    top_a: 0.2
  service_tier: flex
  routing:
    only: [ionstream]
    zdr: true
    data_collection: deny
`)
	s := r.Settings()
	s.Reasoning = api.ReasoningSettings{Mode: api.ReasoningOn, Effort: "high"}
	_, err := r.Embed(context.Background(), api.EmbedRequest{
		Model: "some/vectors", Input: []string{"Caio's sister lives in Lisbon."}, Settings: s,
	})
	if err != nil {
		t.Fatal(err)
	}

	var sent struct {
		TopA      float64 `json:"top_a"`
		Tier      string  `json:"service_tier"`
		Reasoning any     `json:"reasoning"`
		Provider  struct {
			Only           []string `json:"only"`
			ZDR            bool     `json:"zdr"`
			DataCollection string   `json:"data_collection"`
		} `json:"provider"`
	}
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("the body was not read back: %v (%s)", err, body)
	}
	if !slices.Equal(sent.Provider.Only, []string{"ionstream"}) || !sent.Provider.ZDR ||
		sent.Provider.DataCollection != "deny" {
		t.Errorf("provider = %+v, want the routing the file sets", sent.Provider)
	}
	if sent.TopA != 0 || sent.Tier != "" || sent.Reasoning != nil {
		t.Errorf("the request carries %+v, want nothing about writing a reply", sent)
	}
}

// Nothing else means no effort and no summary.
func TestReasoningOffCarriesNothingElse(t *testing.T) {
	var body []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write(read(t, "stream_reply.sse"))
	}))
	t.Cleanup(ts.Close)

	r := runner(t, ts.URL, "")
	s := r.Settings()
	s.Reasoning = api.ReasoningSettings{Mode: api.ReasoningOff, Effort: "high"}
	if _, err := r.Chat(context.Background(), api.ChatRequest{
		Model:    "some/model",
		Messages: []api.Message{api.Text(api.RoleUser, "hey")},
		Settings: s,
	}, func(api.Chunk) error { return nil }); err != nil {
		t.Fatal(err)
	}
	var sent struct {
		Reasoning map[string]any `json:"reasoning"`
	}
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatal(err)
	}
	if len(sent.Reasoning) != 1 || sent.Reasoning["enabled"] != false {
		t.Errorf("reasoning = %+v, want it off and nothing else", sent.Reasoning)
	}
}

func TestRetryAfterAsADateAndAsZero(t *testing.T) {
	var hook hooks
	now := time.Date(2026, 9, 17, 6, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		header string
		want   time.Duration
	}{
		{"a date", now.Add(90 * time.Second).UTC().Format(http.TimeFormat), 90 * time.Second},
		{"no wait at all", "0", 0},
		{"nothing sensible", "soon", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{"Retry-After": {tc.header}}
			got, again := hook.Retry(now, http.StatusTooManyRequests, h)
			if !again {
				t.Fatal("the request is not sent again")
			}
			if got != tc.want {
				t.Errorf("wait = %s, want %s", got, tc.want)
			}
		})
	}
	if _, again := hook.Retry(now, http.StatusNotFound, http.Header{}); again {
		t.Error("a status the API does not ask to retry is sent again")
	}
}

// An entry that says little is not a model that can do nothing.
func TestAListingThatSaysLittle(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/embeddings/models" {
			io.WriteString(w, `{"data":[]}`)
			return
		}
		io.WriteString(w, `{"data":[{"id":"quiet/model","architecture":`+
			`{"input_modalities":["text"],"output_modalities":["text"]}}]}`)
	}))
	t.Cleanup(ts.Close)
	r := runner(t, ts.URL, "")

	m, err := r.Model(context.Background(), "quiet/model")
	if err != nil {
		t.Fatal(err)
	}
	if m.Context != 0 {
		t.Errorf("context = %d, want none given", m.Context)
	}
	if m.Parameters != nil {
		t.Errorf("parameters = %v, want none listed", m.Parameters)
	}

	// A setting is not held against a model whose listing names none.
	s := api.Settings{Sampling: api.SamplingSettings{Temperature: new(0.8)}}
	if err := check(context.Background(), r, api.Checked{ID: "quiet/model", Settings: s, Needs: api.Needs{Chat: true}}); err != nil {
		t.Errorf("Check = %v, want it to pass", err)
	}
}
