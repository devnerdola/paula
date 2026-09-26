// Package families is the table of what each family of models takes on each
// provider: the role her notes are told in, and what caching it needs. It is
// the only place that names an extension, and the only code that names a model
// family or a host.
package families

import (
	"fmt"
	"maps"
	"path"
	"slices"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/families/api"
	openrouterclaude "nerdola.dev/x/paula/internal/families/openrouter/claude"
	openrouterdeepseek "nerdola.dev/x/paula/internal/families/openrouter/deepseek"
	openroutergpt "nerdola.dev/x/paula/internal/families/openrouter/gpt"
	veniceclaude "nerdola.dev/x/paula/internal/families/venice/claude"
	venicedeepseek "nerdola.dev/x/paula/internal/families/venice/deepseek"
	venicegpt "nerdola.dev/x/paula/internal/families/venice/gpt"
)

// entry is one extension: the runner kind it is for, the ids of the models it
// serves as path.Match reads them, and what opens it.
type entry struct {
	runner string
	models []string
	open   api.Open
}

// table is every extension under the name it is known by. No two entries serve
// the same model, so an entry changes what no other model is sent.
var table = map[string]entry{
	"openrouter/claude": {"openrouter", []string{"anthropic/*", "~anthropic/*"}, openrouterclaude.Open},
	"openrouter/gpt": {"openrouter", []string{
		"openai/gpt-5.6*", "openai/gpt-6*",
		"~openai/gpt-astra-latest", "~openai/gpt-sol-latest", "~openai/gpt-terra-latest", "~openai/gpt-luna-latest",
	}, openroutergpt.Open},
	"openrouter/deepseek": {"openrouter", []string{"deepseek/*", "~deepseek/*"}, openrouterdeepseek.Open},
	"venice/claude":       {"venice", []string{"claude-*"}, veniceclaude.Open},
	"venice/gpt":          {"venice", []string{"openai-gpt-56-*", "openai-gpt-6-*"}, venicegpt.Open},
	"venice/deepseek":     {"venice", []string{"deepseek-*"}, venicedeepseek.Open},
}

// Match names the extensions that serve a model of a runner kind, which is one
// or none.
func Match(runner, model string) []string {
	var out []string
	for _, name := range slices.Sorted(maps.Keys(table)) {
		e := table[name]
		if e.runner != runner {
			continue
		}
		for _, pattern := range e.models {
			if ok, _ := path.Match(pattern, model); ok {
				out = append(out, name)
				break
			}
		}
	}
	return out
}

// For opens the extension that serves a model of a runner kind. The model's
// cache key is true unless the file writes false, which turns caching off. A
// model no extension serves is given none, and a cache key written for one is
// reported.
func For(runner, model string, s config.Section) (api.Extension, error) {
	names := Match(runner, model)
	if len(names) == 0 {
		if s.Set() {
			return nil, fmt.Errorf("%s: no family extension serves the %s model %q", s.Path(), runner, model)
		}
		return nil, nil
	}
	cache := true
	if err := s.Decode(&cache); err != nil {
		return nil, err
	}
	e, err := table[names[0]].open(cache)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", s.Path(), err)
	}
	return e, nil
}
