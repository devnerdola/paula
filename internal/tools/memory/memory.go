// Package memory is the tools that reach what she remembers: searching it,
// adding to it, and taking a memory away.
package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/store"
	"nerdola.dev/x/paula/internal/tools/api"
)

// Kind is the key written in the configuration file.
const Kind = "memory"

// defaultResults is how many memories a search answers with when the file
// says nothing.
const defaultResults = 10

type settings struct {
	// Results is the most memories a search answers with.
	Results int `yaml:"results"`
}

// Open reads the memory section and builds the three tools.
func Open(s config.Section, h api.Host) ([]api.Tool, error) {
	cfg := settings{Results: defaultResults}
	if err := s.Decode(&cfg); err != nil {
		return nil, err
	}
	p := &config.Problems{Path: s.Path()}
	if cfg.Results < 1 {
		p.Addf("results: %d is below one", cfg.Results)
	}
	if err := p.Err(); err != nil {
		return nil, err
	}
	return []api.Tool{search{h: h, results: cfg.Results}, remember{h}, forget{h}}, nil
}

type search struct {
	h       api.Host
	results int
}

func (t search) Definition() api.Definition {
	// Memories are written in the card's language, and a word in another one
	// finds none of them.
	query, _ := json.Marshal("the words to look for, in " + t.h.Language)
	return api.Definition{
		Name: "search_memories",
		Description: "Search what you remember of " + t.h.Names.User +
			" and of yourself, by the words a memory holds. Each memory comes with its number.",
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"query":{"type":"string","description":` + string(query) + `}},` +
			`"required":["query"]}`),
	}
}

type searchArgs struct {
	Query string `json:"query"`
}

func (search) Note(args json.RawMessage) string {
	var a searchArgs
	json.Unmarshal(args, &a)
	return strings.TrimSpace("searching memories for " + a.Query)
}

func (t search) Call(ctx context.Context, env api.Env, args json.RawMessage) (string, error) {
	var a searchArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	query := strings.TrimSpace(a.Query)
	if query == "" {
		return "", errors.New("no query was given")
	}
	memories, err := env.Memories(ctx, query, t.results)
	if err != nil {
		return "", err
	}
	if len(memories) == 0 {
		return "no memories match", nil
	}
	return lines(env, memories), nil
}

type remember struct{ h api.Host }

func (t remember) Definition() api.Definition {
	n := t.h.Names
	return api.Definition{
		Name: "remember",
		// A memory kept here is read beside the ones a fold writes, so it is
		// asked for in the words a fold is.
		Description: "Remember a lasting fact about " + n.User + ", or one about yourself. " +
			"Write it as one sentence in " + t.h.Language + ", in the third person, " +
			"naming who it is about: " + n.User + " or " + n.Character + ". " +
			"When it updates or contradicts memories you found, give their numbers in replaces.",
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"memory":{"type":"string","description":"the fact, as one sentence"},` +
			`"replaces":{"type":"array","items":{"type":"integer"},` +
			`"description":"the numbers of the memories it takes the place of"}},` +
			`"required":["memory"]}`),
	}
}

type rememberArgs struct {
	Memory   string           `json:"memory"`
	Replaces []store.MemoryID `json:"replaces"`
}

func (remember) Note(args json.RawMessage) string {
	var a rememberArgs
	json.Unmarshal(args, &a)
	return "remembering: " + a.Memory
}

func (remember) Call(ctx context.Context, env api.Env, args json.RawMessage) (string, error) {
	var a rememberArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	content := strings.TrimSpace(a.Memory)
	if content == "" {
		return "", errors.New("there is nothing to remember")
	}
	m, err := env.Remember(ctx, content, a.Replaces)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("remembered #%d", m.ID), nil
}

type forget struct{ h api.Host }

func (t forget) Definition() api.Definition {
	return api.Definition{
		Name: "forget_memory",
		Description: "Forget a memory by its number, only when " + t.h.Names.User +
			" asks you to. The memories it replaced go with it.",
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"id":{"type":"integer","description":"the number of the memory"}},` +
			`"required":["id"]}`),
	}
}

type forgetArgs struct {
	ID int64 `json:"id"`
}

func (forget) Note(args json.RawMessage) string {
	var a forgetArgs
	json.Unmarshal(args, &a)
	return fmt.Sprintf("forgetting memory #%d", a.ID)
}

func (forget) Call(ctx context.Context, env api.Env, args json.RawMessage) (string, error) {
	var a forgetArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	gone, err := env.Forget(ctx, store.MemoryID(a.ID))
	if errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("no memory is numbered %d", a.ID)
	}
	if err != nil {
		return "", err
	}
	return "forgot:\n" + lines(env, gone), nil
}

// lines are memories as a model reads them: the number each is forgotten by,
// the day it was said, and what it says.
func lines(env api.Env, memories []store.Memory) string {
	out := make([]string, len(memories))
	for i, m := range memories {
		out[i] = fmt.Sprintf("#%d (said on %s) %s", m.ID, env.Date(m.SaidAt), m.Content)
	}
	return strings.Join(out, "\n")
}
