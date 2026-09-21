package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"nerdola.dev/x/paula/internal/store"
)

// openrouterCatalogue answers from the listings captured under the openrouter
// runner.
func openrouterCatalogue(t *testing.T) *httptest.Server {
	t.Helper()
	listing := &atomic.Bool{}
	listing.Store(true)
	return openrouterCatalogueWhile(t, listing)
}

// openrouterAnswer is one of the OpenRouter answers captured for the runner's
// own tests. Nothing a test here is held against is written by hand.
func openrouterAnswer(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "runners", "openrouter", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// openrouterCatalogueWhile is openrouterCatalogue with a switch: once it is
// turned off the listing refuses, while the key is still answered for.
func openrouterCatalogueWhile(t *testing.T, listing *atomic.Bool) *httptest.Server {
	t.Helper()
	read := func(name string) []byte { return openrouterAnswer(t, name) }
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			if !listing.Load() {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(`{"error":{"message":"listing is down","code":500}}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write(read("models.json"))
		case "/v1/embeddings/models":
			// The models that embed are listed apart from the ones that write,
			// and a catalogue is both.
			if !listing.Load() {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(`{"error":{"message":"listing is down","code":500}}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write(read("embedding_models.json"))
		case "/v1/key":
			// The answer to this one is the account's own spending, so there is
			// no fixture of it (runners/openrouter/testdata/SOURCES.md). Health
			// reads nothing out of the body, only that the key was taken.
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{}`))
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "text/event-stream")
			w.Write(read("stream_reply.sse"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

// card is one of the character cards beside this file. See testdata/SOURCES.md.
func card(t *testing.T, name string) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// configFile writes a configuration file. It carries the data directory of the
// test and the address standing in for the API, so it is written here rather
// than kept beside them. A body that names no card is given one.
func configFile(t *testing.T, body string) string {
	t.Helper()
	if !strings.Contains(body, "persona:") {
		body = "persona: " + card(t, "ada.yaml") + "\n" + body
	}
	path := filepath.Join(t.TempDir(), "paula.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestModels(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "test-token-abcdefgh")
	ts := openrouterCatalogue(t)
	path := configFile(t, `
runners:
  openrouter:
    type: openrouter
    url: `+ts.URL+`/v1
models:
  talk:
    runner: openrouter
    id: deepseek/deepseek-v4-pro-0813
    context: 65536
  look:
    runner: openrouter
    id: ~deepseek/deepseek-flash-latest
default_models:
  chat: talk
  vision: look
`)
	code, out, errOut := exec(t, "-config", path, "models")
	if code != 0 {
		t.Fatalf("code = %d, stdout %s, stderr %s", code, out, errOut)
	}

	for _, header := range [][]string{
		{"RUNNER", "TYPE", "URL", "STATUS"},
		{"MODEL", "RUNNER", "ID", "CONTEXT", "CAPABILITIES", "ROLES", "STATUS"},
		{"ROLE", "MODEL", "CHOICE", "TOKENS", "STATUS"},
	} {
		row := line(t, out, header[0]+" ")
		for _, cell := range header[1:] {
			if !strings.Contains(row, cell) {
				t.Errorf("the %s header has no %q: %s", header[0], cell, row)
			}
		}
	}
	if row := line(t, out, "openrouter "); !strings.Contains(row, ts.URL+"/v1") || !strings.Contains(row, "ok") {
		t.Errorf("the runner row = %s", row)
	}

	// The model row holds both contexts, what the catalogue says it does, and
	// the role it serves.
	row := line(t, out, "talk ")
	for _, want := range []string{"65536 of 1048576", "chat", "tools", "schema", "reasoning(", "ok"} {
		if !strings.Contains(row, want) {
			t.Errorf("the row of talk has no %q: %s", want, row)
		}
	}
	if !strings.Contains(line(t, out, "look "), "vision") {
		t.Errorf("the row of look does not say it sees images: %s", line(t, out, "look "))
	}

	// The role rows say the prompt each model is written within.
	if !strings.Contains(line(t, out, "chat "), "65536") {
		t.Errorf("the chat role does not say its prompt: %s", line(t, out, "chat "))
	}
	if got := field(t, line(t, out, "chat "), 2); got != "-" {
		t.Errorf("the chat role has a saved choice %q with no database", got)
	}
}

func line(t *testing.T, out, prefix string) string {
	t.Helper()
	for l := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
	t.Fatalf("no line starts with %q:\n%s", prefix, out)
	return ""
}

func TestModelsReportsProblems(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "test-token-abcdefgh")
	ts := openrouterCatalogue(t)
	path := configFile(t, `
runners:
  openrouter:
    type: openrouter
    url: `+ts.URL+`/v1
models:
  talk:
    runner: openrouter
    id: deepseek/deepseek-v4-pro-0813
  blind:
    runner: openrouter
    id: deepseek/deepseek-v4-pro-0813
default_models:
  chat: talk
  vision: blind
`)
	code, out, errOut := exec(t, "-config", path, "models")
	if code != 1 {
		t.Fatalf("code = %d, want 1:\n%s", code, out)
	}
	if !strings.Contains(errOut, "does not do vision") {
		t.Errorf("stderr does not say why: %s", errOut)
	}
	if !strings.Contains(errOut, "default_models.vision") {
		t.Errorf("stderr does not name the role: %s", errOut)
	}
	if !strings.Contains(line(t, out, "vision "), "problem") {
		t.Errorf("the vision role does not read as a problem: %s", line(t, out, "vision "))
	}
	if !strings.Contains(line(t, out, "chat "), "ok") {
		t.Errorf("the chat role does not read as ok: %s", line(t, out, "chat "))
	}
}

// Nothing of a listing is kept between runs, so however well the run before it
// went, there is no older answer to say ok from.
func TestModelsWhenTheListingIsDown(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "test-token-abcdefgh")
	listing := &atomic.Bool{}
	listing.Store(true)
	ts := openrouterCatalogueWhile(t, listing)
	path := configFile(t, `
data_dir: `+t.TempDir()+`
runners:
  openrouter:
    type: openrouter
    url: `+ts.URL+`/v1
models:
  talk:
    runner: openrouter
    id: deepseek/deepseek-v4-pro-0813
default_models:
  chat: talk
`)
	if code, out, errOut := exec(t, "-config", path, "models"); code != 0 {
		t.Fatalf("code = %d, stdout %s, stderr %s", code, out, errOut)
	}

	listing.Store(false)
	code, out, errOut := exec(t, "-config", path, "models")
	if code != 1 {
		t.Fatalf("code = %d, want the run to fail:\n%s", code, out)
	}
	if !strings.Contains(errOut, "listing is down") {
		t.Errorf("stderr does not say the listing could not be read: %s", errOut)
	}
}

func TestModelsWithARunnerThatDoesNotAnswer(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "test-token-abcdefgh")
	refusal, err := os.ReadFile(filepath.Join("..", "runners", "openrouter", "testdata", "error_unauthorized.json"))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write(refusal)
	}))
	t.Cleanup(ts.Close)
	path := configFile(t, "runners:\n  openrouter:\n    type: openrouter\n    url: "+ts.URL+
		"/v1\nmodels:\n  talk:\n    runner: openrouter\n    id: a\ndefault_models:\n  chat: talk\n")

	code, out, errOut := exec(t, "-config", path, "models")
	if code != 1 {
		t.Fatalf("code = %d, want 1:\n%s", code, out)
	}
	if !strings.Contains(errOut, "User not found.") {
		t.Errorf("stderr does not carry the API's own error: %s", errOut)
	}
	if !strings.Contains(line(t, out, "talk "), "not asked") {
		t.Errorf("the model row does not read as unasked: %s", line(t, out, "talk "))
	}
}

func TestModelsAvailable(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "test-token-abcdefgh")
	ts := openrouterCatalogue(t)
	path := configFile(t, "runners:\n  openrouter:\n    type: openrouter\n    url: "+ts.URL+
		"/v1\nmodels:\n  talk:\n    runner: openrouter\n    id: deepseek/deepseek-v4-pro-0813\ndefault_models:\n  chat: talk\n")

	_, plain, _ := exec(t, "-config", path, "models")
	code, out, _ := exec(t, "-config", path, "models", "-available")
	if code != 0 {
		t.Fatalf("code = %d:\n%s", code, out)
	}
	if !strings.Contains(out, "openrouter serves:") {
		t.Errorf("output has no catalogue:\n%s", out)
	}
	// A model the file does not name is only in the catalogue.
	const other = "inference-net/schematron-v2-turbo"
	if strings.Contains(plain, other) {
		t.Errorf("the plain tables hold a model the file does not name")
	}
	if !strings.Contains(out, other) {
		t.Errorf("the catalogue does not hold %s:\n%s", other, out)
	}
}

func TestModelsWithoutAConfigurationFile(t *testing.T) {
	code, _, errOut := exec(t, "-config", filepath.Join(t.TempDir(), "nope.yaml"), "models")
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(errOut, "nope.yaml") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestModelsTakesNoArguments(t *testing.T) {
	code, _, errOut := exec(t, "models", "extra")
	if code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
	if !strings.Contains(errOut, "no arguments are taken") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestModelsShowsTheChoiceThatWasSaved(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "test-token-abcdefgh")
	ts := openrouterCatalogue(t)
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	path := configFile(t, `
persona: `+card(t, "ada.yaml")+`
data_dir: `+data+`
runners:
  openrouter:
    type: openrouter
    url: `+ts.URL+`/v1
models:
  talk:
    runner: openrouter
    id: deepseek/deepseek-v4-pro-0813
  other:
    runner: openrouter
    id: ~deepseek/deepseek-flash-latest
default_models:
  chat: talk
`)

	// With no database, no choice is shown and none is invented.
	if got := field(t, line(t, output(t, path), "chat "), 2); got != "-" {
		t.Errorf("the chat role has the choice %q with no conversation", got)
	}

	s, err := store.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set(context.Background(), store.KeyModel("chat"), "other"); err != nil {
		t.Fatal(err)
	}
	s.Close()

	if got := field(t, line(t, output(t, path), "chat "), 2); got != "other" {
		t.Errorf("the chat role shows the choice %q, want the one that was saved", got)
	}
}

// A database the command cannot read is said out loud: every role would
// otherwise read as one nothing was ever chosen for.
func TestModelsSaysWhenTheChoicesCannotBeRead(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "test-token-abcdefgh")
	ts := openrouterCatalogue(t)
	data := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, store.File), []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := configFile(t, `
persona: `+card(t, "ada.yaml")+`
data_dir: `+data+`
runners:
  openrouter:
    type: openrouter
    url: `+ts.URL+`/v1
models:
  talk:
    runner: openrouter
    id: deepseek/deepseek-v4-pro-0813
default_models:
  chat: talk
`)

	code, out, errOut := exec(t, "-config", path, "models")
	if code != 0 {
		t.Fatalf("code = %d, stderr %s", code, errOut)
	}
	if !strings.Contains(errOut, "paula models:") {
		t.Errorf("stderr = %q, want why the choices are missing", errOut)
	}
	if got := field(t, line(t, out, "chat "), 2); got != "-" {
		t.Errorf("the chat role shows the choice %q", got)
	}
}

// field is one column of a row, since a row holds ids and names that carry
// whatever the cell being read might.
func field(t *testing.T, row string, n int) string {
	t.Helper()
	fields := strings.Fields(row)
	if n >= len(fields) {
		t.Fatalf("row %q has no column %d", row, n)
	}
	return fields[n]
}

func output(t *testing.T, config string) string {
	t.Helper()
	code, out, errOut := exec(t, "-config", config, "models")
	if code != 0 {
		t.Fatalf("code = %d, stderr %s", code, errOut)
	}
	return out
}
