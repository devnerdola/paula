// Package tools is what she can do in the middle of a reply. It declares which
// kinds of tool there are. What a tool is, and what it reaches of the
// conversation, is internal/tools/api, which a caller imports beside this
// package.
package tools

import (
	"fmt"
	"slices"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/tools/api"
	"nerdola.dev/x/paula/internal/tools/memory"
)

// What the program around a tool gives it, which is what Open takes, and the
// tools it answers with.
type (
	Host  = api.Host
	Names = api.Names
	Tool  = api.Tool
)

// Factory builds the tools of one kind from its settings.
type Factory func(s config.Section, h Host) ([]Tool, error)

// kinds is the table of tool kinds: the only place naming an implementation, so
// nothing else has to know one exists.
var kinds = map[string]Factory{
	memory.Kind: memory.Open,
}

// Kinds are the tool kinds a configuration file may name.
func Kinds() []string {
	out := make([]string, 0, len(kinds))
	for k := range kinds {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// Open builds the tools a section describes.
func Open(kind string, s config.Section, h Host) ([]Tool, error) {
	open, ok := kinds[kind]
	if !ok {
		return nil, fmt.Errorf("%s: no kind of tool is called %q, want one of %v", s.Path(), kind, Kinds())
	}
	return open(s, h)
}
