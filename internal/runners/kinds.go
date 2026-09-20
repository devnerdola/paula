package runners

import (
	"fmt"
	"slices"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/runners/openrouter"
	"nerdola.dev/x/paula/internal/runners/venice"
)

// Factory builds a runner of one kind from its settings.
type Factory func(name string, s config.Section, h Host) (Runner, error)

// kinds is the table of runner kinds: the only place naming an implementation,
// so nothing else has to know one exists.
var kinds = map[string]Factory{
	openrouter.Kind: func(name string, s config.Section, h Host) (Runner, error) {
		return openrouter.Open(name, s, h)
	},
	venice.Kind: func(name string, s config.Section, h Host) (Runner, error) {
		return venice.Open(name, s, h)
	},
}

// Kinds are the runner kinds a configuration file may name.
func Kinds() []string {
	out := make([]string, 0, len(kinds))
	for k := range kinds {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// Open builds the runner a section describes.
func Open(name, kind string, s config.Section, h Host) (Runner, error) {
	open, ok := kinds[kind]
	if !ok {
		return nil, fmt.Errorf("%s: no runner is of type %q, want one of %v", s.Path(), kind, Kinds())
	}
	return open(name, s, h)
}
