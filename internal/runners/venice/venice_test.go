package venice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"nerdola.dev/x/paula/internal/config"
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

// answers serves one captured file for every path.
func answers(t *testing.T, kind, file string) *httptest.Server {
	t.Helper()
	body := read(t, file)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			w.Write(read(t, "models.json"))
			return
		}
		w.Header().Set("Content-Type", kind)
		w.Write(body)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func runner(t *testing.T, url string, body string) *Runner {
	t.Helper()
	t.Setenv("VENICE_API_KEY", "test-token-abcdefgh")
	path := filepath.Join(t.TempDir(), "paula.yaml")
	file := "persona: paula.yaml\nrunners:\n  venice:\n    type: venice\n    url: " + url + "/v1\n"
	for _, line := range strings.Split(body, "\n") {
		if line != "" {
			file += "    " + line + "\n"
		}
	}
	file += "models:\n  chat:\n    runner: venice\n    id: x\ndefault_models:\n  chat: chat\n"
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Open("venice", cfg.Runners[0].Section, api.Host{})
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
	r := runner(t, answers(t, "application/json", "models.json").URL, "")

	// A model that reasons at a chosen effort, sees images, takes tools and
	// answers a schema.
	m := model(t, r, "z-ai-glm-5-3-flash")
	if !m.Chat || !m.Vision || !m.Tools || !m.Reasoning || !m.StructuredOutputs {
		t.Errorf("%s = %+v", m.ID, m)
	}
	if m.Context != 1048576 {
		t.Errorf("%s = %+v, want the context the listing gives", m.ID, m)
	}
	if len(m.Efforts) == 0 {
		t.Errorf("%s lists no efforts", m.ID)
	}
	if slices.Contains(m.Efforts, "none") {
		t.Errorf("%s efforts = %v, want none left out", m.ID, m.Efforts)
	}

	// A model that reasons but takes no effort.
	if m := model(t, r, "z-ai-glm-5-3"); !m.Reasoning || len(m.Efforts) != 0 {
		t.Errorf("%s = %+v, want reasoning with no efforts", m.ID, m)
	}

	// A model that does not reason.
	if m := model(t, r, "venice-uncensored-1-2"); m.Reasoning || len(m.Efforts) != 0 {
		t.Errorf("%s = %+v, want no reasoning", m.ID, m)
	}

	// A model that embeds. It writes nothing, so the catalogue says that and
	// no more of it.
	m = model(t, r, "text-embedding-bge-m3")
	if !m.Embeddings || m.Chat || m.Vision || m.Tools || m.Reasoning {
		t.Errorf("%s = %+v, want a model that only embeds", m.ID, m)
	}
	if m.Context != 8192 {
		t.Errorf("%s has context %d, want what it takes in", m.ID, m.Context)
	}

	if _, err := r.Model(context.Background(), "nope"); err == nil {
		t.Error("Model of an unknown id succeeded")
	}
}

func TestCatalogueIsReadOnce(t *testing.T) {
	var reads int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads++
		w.Header().Set("Content-Type", "application/json")
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
		mu.Lock()
		reads++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
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

// The answer is the one a request of two strings came back with.
func TestEmbeddingOfACapturedAnswer(t *testing.T) {
	r := runner(t, answers(t, "application/json", "embedding.json").URL, "")

	said := []string{"Caio's sister Ana lives in Lisbon", "Caio cooks for friends on saturdays"}
	out, err := r.Embed(context.Background(), api.EmbedRequest{Model: "m", Input: said})
	if err != nil {
		t.Fatal(err)
	}
	// One vector for each string, in the order they were sent, all of the
	// width the model embeds at.
	if len(out.Vectors) != len(said) {
		t.Fatalf("vectors = %d, want one for each of the %d strings", len(out.Vectors), len(said))
	}
	for i, v := range out.Vectors {
		if len(v) != len(out.Vectors[0]) {
			t.Errorf("vector %d is %d wide, want %d", i, len(v), len(out.Vectors[0]))
		}
	}
	if slices.Equal(out.Vectors[0], out.Vectors[1]) {
		t.Error("two different strings embedded the same")
	}
	// An embedding writes nothing, so what it cost is what it read.
	if out.Usage.PromptTokens == 0 || out.Usage.CompletionTokens != 0 {
		t.Errorf("usage = %+v", out.Usage)
	}
}

// The stream is the one a reply with reasoning came back in, and its last chunk
// carries the usage with no choice beside it.
func TestStreamOfACapturedReply(t *testing.T) {
	r := runner(t, answers(t, "text/event-stream", "stream_reply.sse").URL, "")

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
	if res.Reasoning == "" || reasoning.String() != res.Reasoning {
		t.Errorf("reasoning = %q, chunks = %q", res.Reasoning, reasoning.String())
	}
	if res.FinishReason != "stop" {
		t.Errorf("finish reason = %q", res.FinishReason)
	}
	if res.Usage.PromptTokens == 0 || res.Usage.CompletionTokens == 0 || res.Usage.ReasoningTokens == 0 {
		t.Errorf("usage = %+v", res.Usage)
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
		want   string
	}{
		{"a model that does not exist", "error_bad_model.json", 404, "Specified model not found"},
		{"a key that does not exist", "error_unauthorized.json", 401, "Authentication failed"},
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
			if e.Status != tc.status {
				t.Errorf("status = %d", e.Status)
			}
			if !strings.Contains(e.Message, tc.want) {
				t.Errorf("message = %q, want %q in it", e.Message, tc.want)
			}
		})
	}
}

func TestRequestBody(t *testing.T) {
	var sent []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write(read(t, "stream_reply.sse"))
	}))
	t.Cleanup(ts.Close)

	r := runner(t, ts.URL, "provider:\n  character: some-slug\n  output:\n    verbosity: low")
	_, err := r.Chat(context.Background(), api.ChatRequest{
		Model:    "m",
		Messages: []api.Message{api.Text(api.RoleUser, "hey")},
		Settings: api.Settings{
			Reasoning: api.ReasoningSettings{Mode: api.ReasoningOn, Effort: "high"},
			Provider:  r.Settings().Provider,
		},
	}, func(api.Chunk) error { return nil })
	if err != nil {
		t.Fatal(err)
	}

	var body map[string]any
	if err := json.Unmarshal(sent, &body); err != nil {
		t.Fatal(err)
	}
	options, _ := body["stream_options"].(map[string]any)
	if options == nil || options["include_usage"] != true {
		t.Errorf("stream_options = %v, want the usage asked for", body["stream_options"])
	}
	params, _ := body["venice_parameters"].(map[string]any)
	if params == nil {
		t.Fatalf("venice_parameters = %v", body["venice_parameters"])
	}
	if params["include_venice_system_prompt"] != false {
		t.Errorf("include_venice_system_prompt = %v, want it off", params["include_venice_system_prompt"])
	}
	if params["character_slug"] != "some-slug" {
		t.Errorf("character_slug = %v", params["character_slug"])
	}
	if body["verbosity"] != "low" {
		t.Errorf("verbosity = %v", body["verbosity"])
	}
	reasoning, _ := body["reasoning"].(map[string]any)
	if reasoning == nil || reasoning["enabled"] != true || reasoning["effort"] != "high" {
		t.Errorf("reasoning = %v", body["reasoning"])
	}
}

// check is what the runner found wrong with a model, as one error to read.
func check(ctx context.Context, r *Runner, m api.Checked) error {
	return errors.Join(r.Check(ctx, m)...)
}

func TestSystemPromptStaysOffUnlessAskedFor(t *testing.T) {
	// A runner section that names no provider keys.
	p, errs := decodeProvider(config.Section{})
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	if p.object()["include_venice_system_prompt"] != false {
		t.Errorf("object = %v, want the system prompt off", p.object())
	}
}

func TestCheckHoldsTheSettingsAgainstTheCatalogue(t *testing.T) {
	r := runner(t, answers(t, "application/json", "models.json").URL, "")
	ctx := context.Background()

	for _, tc := range []struct {
		name  string
		id    string
		s     api.Settings
		needs api.Needs
		want  string
	}{
		{
			"reasoning on a model that does not reason",
			"venice-uncensored-1-2",
			api.Settings{Reasoning: api.ReasoningSettings{Mode: api.ReasoningOn}},
			api.Needs{}, "the model does not reason",
		},
		{
			"an effort on a model that takes none",
			"z-ai-glm-5-3",
			api.Settings{Reasoning: api.ReasoningSettings{Effort: "high"}},
			api.Needs{}, "lists no efforts",
		},
		{
			"an effort the model does not list",
			"z-ai-glm-5-3-flash",
			api.Settings{Reasoning: api.ReasoningSettings{Effort: "max"}},
			api.Needs{}, "is not one of",
		},
		{
			"a seed at zero",
			"z-ai-glm-5-3-flash",
			api.Settings{Sampling: api.SamplingSettings{Seed: new(int64(0))}},
			api.Needs{}, "sampling.seed",
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
		Sampling:  api.SamplingSettings{Temperature: new(0.8), Seed: new(int64(7))},
	}
	needs := api.Needs{Chat: true, Vision: true, Tools: true}
	if err := check(ctx, r, api.Checked{ID: "z-ai-glm-5-3-flash", Settings: s, Needs: needs}); err != nil {
		t.Errorf("Check = %v, want it to pass", err)
	}
}

func TestFallbacksMustBeServed(t *testing.T) {
	r := runner(t, answers(t, "application/json", "models.json").URL,
		"provider:\n  fallbacks: [z-ai-glm-5-3, nope]")
	s := api.Settings{Provider: r.Settings().Provider}
	err := check(context.Background(), r, api.Checked{ID: "z-ai-glm-5-3-flash", Settings: s})
	if err == nil {
		t.Fatal("Check passed")
	}
	if !strings.Contains(err.Error(), `provider.fallbacks`) || !strings.Contains(err.Error(), "nope") {
		t.Errorf("error = %v", err)
	}
}

func TestOpenWithoutAToken(t *testing.T) {
	t.Setenv("VENICE_API_KEY", "")
	path := filepath.Join(t.TempDir(), "paula.yaml")
	os.WriteFile(path, []byte("persona: paula.yaml\nrunners:\n  venice:\n    type: venice\nmodels:\n  chat:\n    runner: venice\n    id: x\ndefault_models:\n  chat: chat\n"), 0o600)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open("venice", cfg.Runners[0].Section, api.Host{}); err == nil {
		t.Fatal("Open succeeded")
	} else if !strings.Contains(err.Error(), "VENICE_API_KEY is not set") {
		t.Errorf("error = %v", err)
	}
}

func TestOpenRejectsUnknownProviderKeys(t *testing.T) {
	t.Setenv("VENICE_API_KEY", "test-token-abcdefgh")
	path := filepath.Join(t.TempDir(), "paula.yaml")
	os.WriteFile(path, []byte("persona: paula.yaml\nrunners:\n  venice:\n    type: venice\n    provider:\n      enable_web_search: true\nmodels:\n  chat:\n    runner: venice\n    id: x\ndefault_models:\n  chat: chat\n"), 0o600)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open("venice", cfg.Runners[0].Section, api.Host{}); err == nil {
		t.Fatal("Open succeeded")
	} else if !strings.Contains(err.Error(), "enable_web_search: unknown key") {
		t.Errorf("error = %v", err)
	}
}

func TestACatalogueThatFailedIsAskedAgain(t *testing.T) {
	var reads int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads++
		if reads == 1 {
			http.Error(w, `{"error":"the host is away"}`, http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
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

func TestTheResetHeaderIsCountedFromTheGivenTime(t *testing.T) {
	var hook hooks
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	h := http.Header{}
	h.Set(resetHeader, strconv.FormatInt(now.Add(90*time.Second).Unix(), 10))

	d, ok := hook.Retry(now, http.StatusTooManyRequests, h)
	if !ok || d != 90*time.Second {
		t.Errorf("retry = %s, %v, want a minute and a half", d, ok)
	}
	// A reset that has already passed is no delay at all.
	if d, ok := hook.Retry(now.Add(2*time.Minute), http.StatusTooManyRequests, h); !ok || d != 0 {
		t.Errorf("retry = %s, %v, want no delay", d, ok)
	}
	if _, ok := hook.Retry(now, http.StatusBadGateway, h); ok {
		t.Error("a status Venice does not name is sent again")
	}
}

func TestHealthAsksWhatTheKeyMayDo(t *testing.T) {
	var permitted bool
	var asked string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":{"accessPermitted":%t}}`, permitted)
	}))
	t.Cleanup(ts.Close)
	r := runner(t, ts.URL, "")
	ctx := context.Background()

	if err := r.Health(ctx); err == nil {
		t.Error("a key that may not make requests passed")
	}
	if asked != "/v1/api_keys/rate_limits" {
		t.Errorf("asked %q", asked)
	}
	permitted = true
	if err := r.Health(ctx); err != nil {
		t.Errorf("Health = %v", err)
	}
}

func TestTheRunnerSaysWhereItSends(t *testing.T) {
	ts := answers(t, "application/json", "models.json")
	r := runner(t, ts.URL, "")
	if got := r.URL(); got != ts.URL+"/v1" {
		t.Errorf("URL = %q", got)
	}
	if r.Name() != "venice" || r.Kind() != Kind {
		t.Errorf("runner = %s, %s", r.Name(), r.Kind())
	}
}

// A timeout that is no wait at all would give up on a request before it was
// sent, so it is refused where it is written.
func TestOpenRejectsATimeoutThatIsNoWait(t *testing.T) {
	for _, key := range []string{"idle_timeout", "request_timeout"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv("VENICE_API_KEY", "test-token-abcdefgh")
			path := filepath.Join(t.TempDir(), "paula.yaml")
			os.WriteFile(path, []byte("persona: paula.yaml\nrunners:\n  venice:\n    type: venice\n    "+
				key+": 0s\nmodels:\n  chat:\n    runner: venice\n    id: x\ndefault_models:\n  chat: chat\n"), 0o600)
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Open("venice", cfg.Runners[0].Section, api.Host{}); err == nil {
				t.Fatal("Open succeeded")
			} else if !strings.Contains(err.Error(), key+": 0s is not above zero") {
				t.Errorf("error = %v", err)
			}
		})
	}
}

// The whole a listing may take is the runner's to set, since a catalogue that
// takes longer than the client's own default is not one that failed.
func TestTheRequestTimeoutAsItIsWritten(t *testing.T) {
	r := runner(t, answers(t, "application/json", "models.json").URL, "request_timeout: 3m")
	if got := r.client.RequestTimeout; got != 3*time.Minute {
		t.Errorf("request timeout = %s, want the one the file writes", got)
	}
}
