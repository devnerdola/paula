package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const minimal = `
persona: personas/paula.yaml
runners:
  openrouter:
    type: openrouter
models:
  chat:
    runner: openrouter
    id: some/model
default_models:
  chat: chat
`

func write(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "paula.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func load(t *testing.T, body string) (*Config, error) {
	t.Helper()
	return Load(write(t, body))
}

func mustLoad(t *testing.T, body string) *Config {
	t.Helper()
	c, err := load(t, body)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return c
}

// The values are written out here, not read from the code that fills them in.
func TestDefaults(t *testing.T) {
	c := mustLoad(t, minimal)
	want := Engine{
		Debounce:      Duration(2 * time.Second),
		PrefillCancel: true,
		ImageTurns:    2,
		ImageMaxPx:    1024,
		ImageTokens:   1000,
		LogKeep:       500,
	}
	if c.Engine != want {
		t.Errorf("engine = %+v, want %+v", c.Engine, want)
	}
	if want := filepath.Join(c.Dir, "data"); c.DataDir != want {
		t.Errorf("data_dir = %q, want %q", c.DataDir, want)
	}
	if want := filepath.Join(c.Dir, "personas/paula.yaml"); c.Persona != want {
		t.Errorf("persona = %q, want %q", c.Persona, want)
	}
	if len(c.Frontends) != 0 {
		t.Errorf("frontends = %v, want none", c.Frontends)
	}
}

func TestEngineOverrides(t *testing.T) {
	c := mustLoad(t, minimal+`
engine:
  debounce: 1500ms
  prefill_cancel: false
  image_max_px: 0
`)
	if c.Engine.Debounce.Duration() != 1500*time.Millisecond {
		t.Errorf("debounce = %s", c.Engine.Debounce)
	}
	if c.Engine.PrefillCancel {
		t.Error("prefill_cancel = true, want false")
	}
	if c.Engine.ImageMaxPx != 0 {
		t.Errorf("image_max_px = %d, want 0", c.Engine.ImageMaxPx)
	}
	// A key the file leaves out keeps the default the README gives.
	if c.Engine.LogKeep != 500 {
		t.Errorf("log_keep = %d, want the default of 500", c.Engine.LogKeep)
	}
}

func TestOrderAndLookup(t *testing.T) {
	c := mustLoad(t, `
persona: paula.yaml
runners:
  b:
    type: venice
  a:
    type: openrouter
models:
  zeta:
    runner: a
    id: z
  alpha:
    runner: b
    id: a
    context: 4096
default_models:
  chat: alpha
  vision: zeta
`)
	var names []string
	for _, m := range c.Models {
		names = append(names, m.Name)
	}
	if strings.Join(names, ",") != "zeta,alpha" {
		t.Errorf("models = %v, want the order written", names)
	}
	if c.Model("alpha").Context != 4096 {
		t.Errorf("alpha context = %d", c.Model("alpha").Context)
	}
	if runner(t, c, "b").Type != "venice" {
		t.Errorf("runner b type = %q", runner(t, c, "b").Type)
	}
	if c.Model("nope") != nil {
		t.Error("lookup of an unknown name found something")
	}
	if c.DefaultModels.Get(RoleVision) != "zeta" || c.DefaultModels.Get(RoleChat) != "alpha" {
		t.Errorf("default models = %+v", c.DefaultModels)
	}
}

func TestPathsResolve(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	c := mustLoad(t, minimal+`
data_dir: ~/paula-data
`)
	if want := filepath.Join(home, "paula-data"); c.DataDir != want {
		t.Errorf("data_dir = %q, want %q", c.DataDir, want)
	}

	abs := filepath.Join(t.TempDir(), "elsewhere")
	c = mustLoad(t, minimal+"\ndata_dir: "+abs+"\n")
	if c.DataDir != abs {
		t.Errorf("data_dir = %q, want %q", c.DataDir, abs)
	}
}

func TestValidationErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"unknown top level key", minimal + "\nnope: 1\n", "nope: unknown key"},
		{"no runners", "models:\n  a:\n    runner: r\n    id: i\ndefault_models:\n  chat: a\n", "at least one runner"},
		{"runner without type", "runners:\n  r: {}\nmodels:\n  a:\n    runner: r\n    id: i\ndefault_models:\n  chat: a\n", "runners.r.type: no type is set"},
		{"model without runner", "runners:\n  r:\n    type: openrouter\nmodels:\n  a:\n    id: i\ndefault_models:\n  chat: a\n", "models.a.runner: no runner is set"},
		{"model with an unknown runner", "runners:\n  r:\n    type: openrouter\nmodels:\n  a:\n    runner: q\n    id: i\ndefault_models:\n  chat: a\n", `models.a.runner: no runner is called "q"`},
		{"model without id", "runners:\n  r:\n    type: openrouter\nmodels:\n  a:\n    runner: r\ndefault_models:\n  chat: a\n", "models.a.id: no id is set"},
		{"bad model name", "runners:\n  r:\n    type: openrouter\nmodels:\n  _a:\n    runner: r\n    id: i\ndefault_models:\n  chat: _a\n", "models._a: a name holds"},
		{"no chat model", "runners:\n  r:\n    type: openrouter\nmodels:\n  a:\n    runner: r\n    id: i\n", "default_models.chat: no model is set"},
		{"unknown chat model", "runners:\n  r:\n    type: openrouter\nmodels:\n  a:\n    runner: r\n    id: i\ndefault_models:\n  chat: b\n", `default_models.chat: no model is called "b"`},
		{"unknown role", strings.Replace(minimal, "  chat: chat\n", "  chat: chat\n  audio: chat\n", 1), "default_models.audio: unknown key"},
		{"runners is not a mapping", "runners: [a, b]\ndefault_models:\n  chat: a\n", "runners: want a mapping"},
		{"image max px", minimal + "\nengine:\n  image_max_px: -1\n", "engine.image_max_px: -1 is below zero"},
		{"image turns", minimal + "\nengine:\n  image_turns: -1\n", "engine.image_turns: -1 is below zero"},
		{"image tokens", minimal + "\nengine:\n  image_tokens: -1\n", "engine.image_tokens: -1 is below zero"},
		{"log keep", minimal + "\nengine:\n  log_keep: -1\n", "engine.log_keep: -1 is below zero"},
		{"bad duration", minimal + "\nengine:\n  debounce: soon\n", "engine.debounce: \"soon\" is not a duration"},
		{"duration that is not text", minimal + "\nengine:\n  debounce: [1, 2]\n", "engine.debounce: want a duration"},
		{"unknown engine key", minimal + "\nengine:\n  reply_reserve: 100\n", "engine.reply_reserve: unknown key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := load(t, tc.body)
			if err == nil {
				t.Fatalf("Load succeeded, want %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want %q in it", err, tc.want)
			}
		})
	}
}

func TestEveryProblemIsReported(t *testing.T) {
	_, err := load(t, `
runners:
  r:
    type: openrouter
models:
  a:
    runner: q
default_models:
  chat: b
engine:
  image_turns: -1
`)
	if err == nil {
		t.Fatal("Load succeeded")
	}
	for _, want := range []string{"models.a.runner", "models.a.id", "default_models.chat", "engine.image_turns"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want %q in it", err, want)
		}
	}
}

func TestMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("Load succeeded")
	}
}

func TestFind(t *testing.T) {
	t.Setenv("PAULA_CONFIG", "env.yaml")
	if got := Find("given.yaml"); got != "given.yaml" {
		t.Errorf("Find = %q", got)
	}
	if got := Find(""); got != "env.yaml" {
		t.Errorf("Find = %q", got)
	}
	t.Setenv("PAULA_CONFIG", "")
	if got := Find(""); got != "paula.yaml" {
		t.Errorf("Find = %q", got)
	}
}

func TestSectionRejectsUnknownKeys(t *testing.T) {
	c := mustLoad(t, `
persona: paula.yaml
runners:
  openrouter:
    type: openrouter
    idle_timeout: 30s
    nope: 1
models:
  chat:
    runner: openrouter
    id: some/model
default_models:
  chat: chat
`)
	var v struct {
		Type        string   `yaml:"type"`
		IdleTimeout Duration `yaml:"idle_timeout"`
	}
	err := runner(t, c, "openrouter").Section.Decode(&v)
	if err == nil {
		t.Fatal("Decode succeeded")
	}
	if !strings.HasPrefix(err.Error(), "runners.openrouter: ") {
		t.Errorf("error = %q, want the key path first", err)
	}
	if !strings.Contains(err.Error(), "nope: unknown key") {
		t.Errorf("error = %q", err)
	}
}

func TestSectionDecode(t *testing.T) {
	c := mustLoad(t, minimal+`
frontends:
  repl:
    socket: paula.sock
`)
	if len(c.Frontends) != 1 || c.Frontends[0].Name != "repl" {
		t.Fatalf("frontends = %+v", c.Frontends)
	}
	var v struct {
		Socket string `yaml:"socket"`
	}
	if err := c.Frontends[0].Section.Decode(&v); err != nil {
		t.Fatal(err)
	}
	if v.Socket != "paula.sock" {
		t.Errorf("socket = %q", v.Socket)
	}
	if got := c.Frontends[0].Section.Path(); got != "frontends.repl" {
		t.Errorf("path = %q", got)
	}
	if !c.Frontends[0].Section.Set() {
		t.Error("Set = false")
	}
}

func TestSectionNotSet(t *testing.T) {
	var s Section
	if s.Set() {
		t.Error("Set = true")
	}
	var v struct {
		A int `yaml:"a"`
	}
	if err := s.Decode(&v); err != nil {
		t.Errorf("Decode = %v", err)
	}
}

func TestMergeSections(t *testing.T) {
	c := mustLoad(t, `
persona: paula.yaml
runners:
  r:
    type: openrouter
    provider:
      routing:
        only: [a]
        zdr: true
      service_tier: flex
models:
  a:
    runner: r
    id: i
    provider:
      routing:
        only: [b]
default_models:
  chat: a
`)
	var runner, model struct {
		Provider Section `yaml:"provider"`
		Type     string  `yaml:"type"`
		Runner   string  `yaml:"runner"`
		ID       string  `yaml:"id"`
	}
	if err := section(t, c, "r").Decode(&runner); err != nil {
		t.Fatal(err)
	}
	if err := c.Model("a").Section.Decode(&model); err != nil {
		t.Fatal(err)
	}

	var got struct {
		Routing struct {
			Only []string `yaml:"only"`
			ZDR  bool     `yaml:"zdr"`
		} `yaml:"routing"`
		ServiceTier string `yaml:"service_tier"`
	}
	if err := MergeSections(runner.Provider, model.Provider).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.Routing.Only, ",") != "b" {
		t.Errorf("only = %v, want the model's", got.Routing.Only)
	}
	if !got.Routing.ZDR {
		t.Error("zdr = false, want the runner's")
	}
	if got.ServiceTier != "flex" {
		t.Errorf("service_tier = %q, want the runner's", got.ServiceTier)
	}
}

func TestMergeSectionsWithOneSide(t *testing.T) {
	c := mustLoad(t, minimal)
	base := section(t, c, "openrouter")
	if got := MergeSections(base, Section{}); got.Path() != base.Path() {
		t.Errorf("merge with nothing = %q, want %q", got.Path(), base.Path())
	}
	if got := MergeSections(Section{}, base); got.Path() != base.Path() {
		t.Errorf("merge onto nothing = %q, want %q", got.Path(), base.Path())
	}
}

// runner is the configured runner of that name.
func runner(t *testing.T, c *Config, name string) *Runner {
	t.Helper()
	for i := range c.Runners {
		if c.Runners[i].Name == name {
			return &c.Runners[i]
		}
	}
	t.Fatalf("no runner is called %q", name)
	return nil
}

func section(t *testing.T, c *Config, name string) Section {
	t.Helper()
	return runner(t, c, name).Section
}

// An anchor is resolved when the section is decoded, wherever it was written.
func TestSettingsSharedByAnAnchor(t *testing.T) {
	c := mustLoad(t, `
persona: paula.yaml
runners:
  r:
    type: openrouter
models:
  a:
    runner: r
    id: x
    sampling: &sampling
      temperature: 0.8
      top_p: 0.9
  b:
    runner: r
    id: y
    sampling: *sampling
default_models:
  chat: a
`)
	var v struct {
		Runner   string `yaml:"runner"`
		ID       string `yaml:"id"`
		Sampling struct {
			Temperature float64 `yaml:"temperature"`
			TopP        float64 `yaml:"top_p"`
		} `yaml:"sampling"`
	}
	for _, m := range c.Models {
		v.Sampling.Temperature, v.Sampling.TopP = 0, 0
		if err := m.Section.Decode(&v); err != nil {
			t.Fatalf("%s: %v", m.Name, err)
		}
		if v.Sampling.Temperature != 0.8 || v.Sampling.TopP != 0.9 {
			t.Errorf("%s: sampling = %+v", m.Name, v.Sampling)
		}
	}
}

// The keys of an inlined struct are keys of the one that inlines it, so a
// section written under one of them is labelled through the struct it is in
// rather than by the place it has inside it.
func TestASectionInsideAnInlinedStruct(t *testing.T) {
	root, err := Root([]byte("name: talk\nprovider:\n  routing:\n    only: [novita]\n"))
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Name   string `yaml:"name"`
		Inline struct {
			Provider Section `yaml:"provider"`
		} `yaml:",inline"`
	}
	if err := (Section{path: "models.talk", node: root.node}).Decode(&v); err != nil {
		t.Fatal(err)
	}
	if got := v.Inline.Provider.Path(); got != "models.talk.provider" {
		t.Errorf("path = %q, want the key it is written at", got)
	}
	if !v.Inline.Provider.Set() {
		t.Error("the section was not decoded")
	}
	if v.Name != "talk" {
		t.Errorf("name = %q, want the key beside it left as it is", v.Name)
	}
}

// A context that is not a number is reported under its own key, rather than
// cascading into what the rest of the file then reads as.
func TestAValueThatCannotBeReadIsSaidWhereItIs(t *testing.T) {
	_, err := load(t, `
persona: paula.yaml
runners:
  r:
    type: openrouter
models:
  a:
    runner: r
    id: x
    context: lots
default_models:
  chat: a
`)
	if err == nil {
		t.Fatal("Load succeeded")
	}
	if !strings.Contains(err.Error(), "context") {
		t.Errorf("error = %q, want the key it is about", err)
	}
}

// The rest of the file is still read, so a name written twice does not cascade
// into a file that names no runner and models that name one.
func TestANameWrittenTwiceIsReportedOnItsOwn(t *testing.T) {
	_, err := load(t, `
persona: paula.yaml
runners:
  r:
    type: openrouter
  r:
    type: venice
models:
  a:
    runner: r
    id: x
default_models:
  chat: a
`)
	if err == nil {
		t.Fatal("Load succeeded")
	}
	if !strings.Contains(err.Error(), "runners.r: written twice") {
		t.Errorf("error = %q, want the name that is written twice", err)
	}
	for _, not := range []string{"at least one runner", "no runner is called"} {
		if strings.Contains(err.Error(), not) {
			t.Errorf("error = %q, want no %q in it", err, not)
		}
	}
}

// A file shares settings the other way too: the keys of an anchored mapping are
// laid into the one that merges it.
func TestSettingsSharedByAMergeKey(t *testing.T) {
	c := mustLoad(t, `
persona: paula.yaml
runners:
  r:
    type: openrouter
models:
  a: &base
    runner: r
    id: x
    sampling:
      temperature: 0.8
  b:
    <<: *base
    id: y
default_models:
  chat: a
`)
	var v struct {
		Runner   string `yaml:"runner"`
		ID       string `yaml:"id"`
		Sampling struct {
			Temperature float64 `yaml:"temperature"`
		} `yaml:"sampling"`
	}
	b := c.Model("b")
	if b == nil {
		t.Fatal("the file names no model b")
	}
	if err := b.Section.Decode(&v); err != nil {
		t.Fatal(err)
	}
	if v.Runner != "r" || v.ID != "y" || v.Sampling.Temperature != 0.8 {
		t.Errorf("b = %+v, want the merged keys with its own id", v)
	}
}
