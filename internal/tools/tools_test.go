package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"nerdola.dev/x/paula/internal/config"
)

// load reads a configuration file with the tools section given.
func load(t *testing.T, tools string) *config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "paula.yaml")
	file := "persona: paula.yaml\nrunners:\n  r:\n    type: venice\nmodels:\n  chat:\n    runner: r\n    id: x\n" +
		"default_models:\n  chat: chat\ntools:\n" + tools
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
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
	if !slices.Contains(got, "memory") {
		t.Errorf("Kinds = %v, want %q in it", got, "memory")
	}
}

// Every kind opens as it is written with nothing under it, and what a model is
// offered of it is a tool of a name of its own with parameters that are JSON.
func TestEveryKindOpens(t *testing.T) {
	for _, kind := range Kinds() {
		t.Run(kind, func(t *testing.T) {
			cfg := load(t, "  "+kind+":\n")
			tools, err := Open(kind, cfg.Tools[0].Section, Host{})
			if err != nil {
				t.Fatal(err)
			}
			if len(tools) == 0 {
				t.Fatal("the kind opened no tool")
			}
			var names []string
			for _, tool := range tools {
				def := tool.Definition()
				if slices.Contains(names, def.Name) {
					t.Errorf("two tools are called %s", def.Name)
				}
				names = append(names, def.Name)
				if !json.Valid(def.Parameters) {
					t.Errorf("%s takes parameters that are not JSON: %s", def.Name, def.Parameters)
				}
			}
		})
	}
}

func TestUnknownKind(t *testing.T) {
	cfg := load(t, "  diary:\n")
	_, err := Open("diary", cfg.Tools[0].Section, Host{})
	if err == nil {
		t.Fatal("Open succeeded")
	}
	if !strings.Contains(err.Error(), `no kind of tool is called "diary"`) {
		t.Errorf("error = %v", err)
	}
	for _, kind := range Kinds() {
		if !strings.Contains(err.Error(), kind) {
			t.Errorf("error = %v, want %q named", err, kind)
		}
	}
}
