package frontend

import (
	"context"
	"fmt"
	"slices"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/frontend/api"
	"nerdola.dev/x/paula/internal/frontend/repl"
	"nerdola.dev/x/paula/internal/frontend/telegram"
)

// What the program around a frontend gives it, which is what Open takes.
type (
	Host  = api.Host
	Names = api.Names
)

// Factory builds a frontend of one kind from its settings.
type Factory func(s config.Section, h Host) (api.Frontend, error)

// kinds is the table of frontend kinds: the only place naming an
// implementation, so nothing else has to know one exists.
var kinds = map[string]Factory{
	repl.Kind: func(s config.Section, h Host) (api.Frontend, error) {
		return repl.Open(s, h)
	},
	telegram.Kind: func(s config.Section, h Host) (api.Frontend, error) {
		return telegram.Open(s, h)
	},
}

// Kinds are the frontend kinds a configuration file may name.
func Kinds() []string {
	out := make([]string, 0, len(kinds))
	for k := range kinds {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// Open builds the frontend a section describes.
func Open(kind string, s config.Section, h Host) (api.Frontend, error) {
	open, ok := kinds[kind]
	if !ok {
		return nil, fmt.Errorf("%s: no frontend is called %q, want one of %v", s.Path(), kind, Kinds())
	}
	return open(s, h)
}

// Run keeps a session for every adapter a frontend yields, until the context
// ends.
func Run(ctx context.Context, f api.Frontend, o Options) error {
	return f.Run(ctx, func(ctx context.Context, a api.Adapter) error {
		return New(a, o).Run(ctx)
	})
}
