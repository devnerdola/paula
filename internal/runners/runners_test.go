package runners

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/runners/api"
)

// openrouterCatalogue answers from the listings captured under the openrouter
// runner, so both packages hold the models against the same answers.
func openrouterCatalogue(t *testing.T) *httptest.Server {
	t.Helper()
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join("openrouter", "testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			w.Write(read("models.json"))
		case "/v1/embeddings/models":
			// The models that embed are listed apart from the ones that write,
			// and a catalogue is both.
			w.Write(read("embedding_models.json"))
		case "/v1/key":
			// The answer to this one is the account's own spending, so there is
			// no fixture of it (openrouter/testdata/SOURCES.md). Health reads
			// nothing out of the body, only that the key was taken.
			w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

func load(t *testing.T, body string) *config.Config {
	t.Helper()
	t.Setenv("OPENROUTER_API_KEY", "test-token-abcdefgh")
	t.Setenv("VENICE_API_KEY", "test-token-abcdefgh")
	path := filepath.Join(t.TempDir(), "paula.yaml")
	if err := os.WriteFile(path, []byte("persona: paula.yaml\n"+body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestKinds(t *testing.T) {
	got := Kinds()
	if !slices.IsSorted(got) {
		t.Errorf("Kinds = %v, want them sorted", got)
	}
	for _, want := range []string{"openrouter", "venice"} {
		if !slices.Contains(got, want) {
			t.Errorf("Kinds = %v, want %q in it", got, want)
		}
	}
}

func TestEveryKindOpens(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "test-token-abcdefgh")
	t.Setenv("VENICE_API_KEY", "test-token-abcdefgh")
	for _, kind := range Kinds() {
		t.Run(kind, func(t *testing.T) {
			cfg := load(t, "runners:\n  r:\n    type: "+kind+
				"\nmodels:\n  chat:\n    runner: r\n    id: x\ndefault_models:\n  chat: chat\n")
			r, err := Open("r", kind, cfg.Runners[0].Section, Host{})
			if err != nil {
				t.Fatal(err)
			}
			if r.Name() != "r" || r.Kind() != kind {
				t.Errorf("runner = %q of kind %q", r.Name(), r.Kind())
			}
		})
	}
}

func TestUnknownKind(t *testing.T) {
	cfg := load(t, "runners:\n  r:\n    type: ollama\nmodels:\n  chat:\n    runner: r\n    id: x\ndefault_models:\n  chat: chat\n")
	_, err := Open("r", "ollama", cfg.Runners[0].Section, Host{})
	if err == nil {
		t.Fatal("Open succeeded")
	}
	if !strings.Contains(err.Error(), `no runner is of type "ollama"`) {
		t.Errorf("error = %v", err)
	}
	for _, kind := range Kinds() {
		if !strings.Contains(err.Error(), kind) {
			t.Errorf("error = %v, want %q named", err, kind)
		}
	}
}

func TestModelSettingsGoOverTheRunners(t *testing.T) {
	cfg := load(t, `
runners:
  openrouter:
    type: openrouter
    sampling:
      temperature: 0.7
      top_p: 0.9
    reasoning:
      mode: "on"
      effort: high
    provider:
      routing:
        only: [ionstream]
        zdr: true
      service_tier: flex
models:
  chat:
    runner: openrouter
    id: deepseek/deepseek-v4-pro-0813
    context: 4096
    sampling:
      temperature: 1.2
    provider:
      routing:
        only: [novita]
default_models:
  chat: chat
`)
	s, err := Configure(cfg, Host{})
	if err != nil {
		t.Fatal(err)
	}
	m := s.Model("chat")
	if m == nil {
		t.Fatal("the model is missing")
	}
	if *m.Settings.Sampling.Temperature != 1.2 {
		t.Errorf("temperature = %v, want the model's", *m.Settings.Sampling.Temperature)
	}
	if *m.Settings.Sampling.TopP != 0.9 {
		t.Errorf("top_p = %v, want the runner's", *m.Settings.Sampling.TopP)
	}
	if m.Settings.Reasoning.Effort != "high" || m.Settings.Reasoning.Mode != api.ReasoningOn {
		t.Errorf("reasoning = %+v, want the runner's", m.Settings.Reasoning)
	}
	if m.Context != 4096 {
		t.Errorf("context = %d", m.Context)
	}

	var prov struct {
		Routing struct {
			Only []string `yaml:"only"`
			ZDR  bool     `yaml:"zdr"`
		} `yaml:"routing"`
		ServiceTier string `yaml:"service_tier"`
	}
	if err := m.Settings.Provider.Decode(&prov); err != nil {
		t.Fatal(err)
	}
	if strings.Join(prov.Routing.Only, ",") != "novita" {
		t.Errorf("routing.only = %v, want the model's", prov.Routing.Only)
	}
	if !prov.Routing.ZDR || prov.ServiceTier != "flex" {
		t.Errorf("provider = %+v, want the runner's kept key by key", prov)
	}
}

func TestDefaultsAndNeeds(t *testing.T) {
	cfg := load(t, `
runners:
  openrouter:
    type: openrouter
models:
  talk:
    runner: openrouter
    id: a
  look:
    runner: openrouter
    id: b
default_models:
  chat: talk
  vision: look
`)
	s, err := Configure(cfg, Host{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Defaults[config.RoleChat].Name != "talk" || s.Defaults[config.RoleVision].Name != "look" {
		t.Errorf("defaults = %+v", s.Defaults)
	}
	if _, ok := s.Defaults[config.RoleVision]; !ok {
		t.Error("no model was chosen for vision")
	}

	if chat := RoleNeeds(config.RoleChat); chat != (api.Needs{Chat: true, Tools: true}) {
		t.Errorf("chat needs = %+v", chat)
	}
	if vision := RoleNeeds(config.RoleVision); vision != (api.Needs{Vision: true}) {
		t.Errorf("vision needs = %+v", vision)
	}
}

func TestCheckPasses(t *testing.T) {
	ts := openrouterCatalogue(t)
	cfg := load(t, `
runners:
  openrouter:
    type: openrouter
    url: `+ts.URL+`/v1
models:
  talk:
    runner: openrouter
    id: deepseek/deepseek-v4-pro-0813
  look:
    runner: openrouter
    id: ~deepseek/deepseek-flash-latest
default_models:
  chat: talk
  vision: look
`)
	s, err := Configure(cfg, Host{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Check(context.Background()); err != nil {
		t.Errorf("Check = %v, want it to pass", err)
	}
}

func TestCheckReportsEveryProblem(t *testing.T) {
	ts := openrouterCatalogue(t)
	cfg := load(t, `
runners:
  openrouter:
    type: openrouter
    url: `+ts.URL+`/v1
models:
  talk:
    runner: openrouter
    id: deepseek/deepseek-v4-pro-0813
    context: 99999999
  blind:
    runner: openrouter
    id: deepseek/deepseek-v4-pro-0813
  gone:
    runner: openrouter
    id: nope/nope
default_models:
  chat: talk
  vision: blind
`)
	s, err := Configure(cfg, Host{})
	if err != nil {
		t.Fatal(err)
	}
	err = s.Check(context.Background())
	if err == nil {
		t.Fatal("Check passed")
	}
	for _, want := range []string{
		"models.talk: context: 99999999 is above the 1048576",
		"default_models.vision",
		"does not do vision",
		"models.gone",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want %q in it", err, want)
		}
	}
}

// The runner names what it finds under the model, and a provider block names
// itself, since a model may take the one its runner writes.
func TestEveryProblemNamesItsKeyOnce(t *testing.T) {
	ts := openrouterCatalogue(t)
	cfg := load(t, `
runners:
  openrouter:
    type: openrouter
    url: `+ts.URL+`/v1
models:
  talk:
    runner: openrouter
    id: deepseek/deepseek-v4-pro-0813
    reasoning:
      effort: enormous
    provider:
      routing:
        nope: true
default_models:
  chat: talk
`)
	s, err := Configure(cfg, Host{})
	if err != nil {
		t.Fatal(err)
	}
	err = s.Check(context.Background())
	if err == nil {
		t.Fatal("Check passed")
	}
	for _, want := range []string{
		"models.talk:",
		"reasoning.effort:",
		"models.talk.provider: routing.nope: unknown key",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want %q in it", err, want)
		}
	}
	if n := strings.Count(err.Error(), "reasoning.effort"); n != 1 {
		t.Errorf("error = %v, want reasoning.effort reported once, got %d", err, n)
	}
	if strings.Contains(err.Error(), "models.talk: models.talk") {
		t.Errorf("error = %v, want the model named once", err)
	}
	if strings.Contains(err.Error(), "deepseek/deepseek-v4-pro-0813:") {
		t.Errorf("error = %v, want the model id left out of the key path", err)
	}
}

func TestCheckReportsARunnerThatDoesNotAnswer(t *testing.T) {
	refusal, err := os.ReadFile(filepath.Join("openrouter", "testdata", "error_unauthorized.json"))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write(refusal)
	}))
	t.Cleanup(ts.Close)
	cfg := load(t, "runners:\n  openrouter:\n    type: openrouter\n    url: "+ts.URL+
		"/v1\nmodels:\n  talk:\n    runner: openrouter\n    id: a\ndefault_models:\n  chat: talk\n")
	s, err := Configure(cfg, Host{})
	if err != nil {
		t.Fatal(err)
	}
	err = s.Check(context.Background())
	if err == nil {
		t.Fatal("Check passed")
	}
	if !strings.Contains(err.Error(), "runners.openrouter") {
		t.Errorf("error = %v", err)
	}
	if strings.Contains(err.Error(), "models.talk") {
		t.Errorf("error = %v, want the models left alone when the runner does not answer", err)
	}
}

func TestBudget(t *testing.T) {
	ts := openrouterCatalogue(t)
	cfg := load(t, "runners:\n  openrouter:\n    type: openrouter\n    url: "+ts.URL+
		"/v1\nmodels:\n  set:\n    runner: openrouter\n    id: deepseek/deepseek-v4-pro-0813\n    context: 4096\n"+
		"  unset:\n    runner: openrouter\n    id: deepseek/deepseek-v4-pro-0813\ndefault_models:\n  chat: set\n")
	s, err := Configure(cfg, Host{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if n, err := s.ContextSize(ctx, s.Model("set")); err != nil || n != 4096 {
		t.Errorf("ContextSize = %d, %v, want what the file says", n, err)
	}
	n, err := s.ContextSize(ctx, s.Model("unset"))
	if err != nil {
		t.Fatal(err)
	}
	if n <= 4096 {
		t.Errorf("ContextSize = %d, want what the catalogue reports", n)
	}
}

func TestARoleIsNotAskedWhenItsModelIsNotServed(t *testing.T) {
	ts := openrouterCatalogue(t)
	cfg := load(t, "runners:\n  openrouter:\n    type: openrouter\n    url: "+ts.URL+
		"/v1\nmodels:\n  gone:\n    runner: openrouter\n    id: nope/nope\ndefault_models:\n  chat: gone\n")
	s, err := Configure(cfg, Host{})
	if err != nil {
		t.Fatal(err)
	}
	r := s.Report(context.Background())
	chat := roleRow(t, r, config.RoleChat)
	if !chat.Skipped {
		t.Errorf("the chat role = %+v, want it left unasked", chat)
	}
	if chat.Err != nil {
		t.Errorf("the role carries %v, want the model's row to say it", chat.Err)
	}
	if r.Models[0].Err == nil {
		t.Error("the model row says nothing")
	}
}

// A role serving is not done without is listed whether or not the file names a
// model for it, so a setup that cannot serve says so wherever it is shown
// rather than at the first serve.
func TestARoleWithNoModelIsStillListed(t *testing.T) {
	ts := openrouterCatalogue(t)
	cfg := load(t, "runners:\n  openrouter:\n    type: openrouter\n    url: "+ts.URL+
		"/v1\nmodels:\n  talk:\n    runner: openrouter\n    id: deepseek/deepseek-v4-pro-0813\ndefault_models:\n  chat: talk\n")
	s, err := Configure(cfg, Host{})
	if err != nil {
		t.Fatal(err)
	}

	r := s.Report(context.Background())
	embed := roleRow(t, r, config.RoleEmbed)
	if !embed.Missing || embed.Model != nil {
		t.Errorf("the embed role = %+v, want it named with no model", embed)
	}
	// Seeing a picture is a choice, so a role nothing is named for is not one
	// to answer for.
	for _, st := range r.Roles {
		if st.Role == config.RoleVision {
			t.Errorf("the vision role is listed as %+v, want it left out", st)
		}
	}
	// What serving would refuse is the run's to say, since a conversation may
	// have a model of its own saved: the role carries no error of its own.
	if embed.Err != nil {
		t.Errorf("the role carries %v, want what reads the conversation to say it", embed.Err)
	}
}

func roleRow(t *testing.T, r *Report, role config.Role) RoleStatus {
	t.Helper()
	for _, st := range r.Roles {
		if st.Role == role {
			return st
		}
	}
	t.Fatalf("roles = %+v, want one for %s", r.Roles, role)
	return RoleStatus{}
}

// Every model that can do the work serves the role, and the one the catalogue
// says cannot does not.
func TestServingIsWhatTheCatalogueAllows(t *testing.T) {
	ts := openrouterCatalogue(t)
	cfg := load(t, "runners:\n  openrouter:\n    type: openrouter\n    url: "+ts.URL+`/v1
models:
  talk:
    runner: openrouter
    id: deepseek/deepseek-v4-pro-0813
  look:
    runner: openrouter
    id: ~deepseek/deepseek-flash-latest
default_models:
  chat: talk
  vision: look
`)
	s, err := Configure(cfg, Host{})
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	for _, tc := range []struct {
		role config.Role
		want []string
	}{
		// A model that cannot read an image serves the chat role alone, and one
		// that can serves both.
		{config.RoleChat, []string{"talk", "look"}},
		{config.RoleVision, []string{"look"}},
	} {
		serving, err := s.Serving(ctx, tc.role)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, m := range serving {
			got = append(got, m.Name)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s is served by %v, want %v", tc.role, got, tc.want)
		}
	}
}

func TestReasoningDefaultsToWhatTheCatalogueAllows(t *testing.T) {
	ts := openrouterCatalogue(t)
	cfg := load(t, "runners:\n  openrouter:\n    type: openrouter\n    url: "+ts.URL+`/v1
models:
  reasons:
    runner: openrouter
    id: deepseek/deepseek-v4-pro-0813
  chosen:
    runner: openrouter
    id: deepseek/deepseek-v4-pro-0813
    reasoning:
      effort: max
  quiet:
    runner: openrouter
    id: inference-net/schematron-v2-turbo
  noefforts:
    runner: openrouter
    id: inclusionai/ling-3.0-flash-vl
default_models:
  chat: reasons
`)
	s, err := Configure(cfg, Host{})
	if err != nil {
		t.Fatal(err)
	}
	reasoning := func(name string) api.ReasoningSettings {
		t.Helper()
		m := s.Model(name)
		catalogue, err := s.Lookup(context.Background(), m)
		if err != nil {
			t.Fatal(err)
		}
		return WithDefaults(m.Settings, catalogue).Reasoning
	}

	// The catalogue lists low, high and max, so the middle one is high.
	if got := reasoning("reasons"); got.Mode != api.ReasoningOn || got.Effort != "high" {
		t.Errorf("reasons = %+v, want it on at the middle effort", got)
	}
	if got := reasoning("chosen"); got.Effort != "max" {
		t.Errorf("chosen = %+v, want the effort the file sets", got)
	}
	if got := reasoning("quiet"); got.Mode != api.ReasoningOff || got.Effort != "" {
		t.Errorf("quiet = %+v, want it off on a model that does not reason", got)
	}
	if got := reasoning("noefforts"); got.Mode != api.ReasoningOn || got.Effort != "" {
		t.Errorf("noefforts = %+v, want it on with no effort", got)
	}

	// What the file says is left as it was written, so asking twice answers the
	// same.
	if got := s.Model("reasons").Settings.Reasoning; got != (api.ReasoningSettings{}) {
		t.Errorf("the file's settings were changed to %+v", got)
	}
}
