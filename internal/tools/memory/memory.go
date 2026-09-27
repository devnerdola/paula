// Package memory is the tools that reach what she remembers: listing it,
// searching it, adding to it, and taking a memory away. Memories are never in
// the prompt, so these tools are the only way she has to them, and what each
// says of itself is what tells her how they work.
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

// Open reads the memory section and builds the four tools.
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
	return []api.Tool{list{h}, search{h: h, results: cfg.Results}, remember{h}, forget{h}}, nil
}

// arrangement is what every memory tool tells a model of where its memories
// are: nowhere in front of it, until it looks them up.
func arrangement(n api.Names) string {
	return "Your memories are not in your prompt. What you have in front of you is the summary of your " +
		"conversation with " + n.User + " so far and its most recent messages word for word; older messages " +
		"survive only as that summary, which keeps the story and loses the details. A memory is a lasting " +
		"fact, one sentence, that you kept with remember, and it is in front of you only when you list or " +
		"search your memories. Each memory has a number, which remember takes to replace it and " +
		"forget_memory to take it away."
}

// page is how many memories a list answers with at once. What a call answers
// goes into the prompt of the round after it, and a list of every memory of a
// long conversation would take more of it than the round has.
const page = 50

type list struct{ h api.Host }

func (t list) Definition() api.Definition {
	n := t.h.Names
	return api.Definition{
		Name: "list_memories",
		Description: fmt.Sprintf("List the memories you have kept about %s and about yourself, newest first, "+
			"%d at a time, each with its number and the day it was said. ", n.User, page) + arrangement(n) +
			" List them when you need to know what you know about " + n.User + ": when the conversation picks " +
			"up after a while, when something personal comes up that you may have been told before, and before " +
			"keeping a memory, to see whether it updates one you already have. A list with older memories " +
			"after it says so, and from lists them.",
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"from":{"type":"integer","description":"how many of the newest memories to pass over; leave it out for the newest"}}}`),
	}
}

type listArgs struct {
	From int `json:"from"`
}

func (list) Note(json.RawMessage) string { return "listing memories" }

func (list) LooksUp() {}

func (list) Call(ctx context.Context, env api.Env, args json.RawMessage) (string, error) {
	var a listArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	if a.From < 0 {
		return "", fmt.Errorf("from is %d, below zero", a.From)
	}
	// One past the page says whether there are older ones to list.
	memories, err := env.LatestMemories(ctx, a.From, page+1)
	if err != nil {
		return "", err
	}
	if len(memories) == 0 {
		if a.From > 0 {
			return fmt.Sprintf("no memories past the newest %d", a.From), nil
		}
		return "no memories yet", nil
	}
	older := len(memories) > page
	if older {
		memories = memories[:page]
	}
	out := lines(env, memories)
	if older {
		out += fmt.Sprintf("\nThere are older memories: list_memories with from %d lists them.", a.From+page)
	}
	return out, nil
}

type search struct {
	h       api.Host
	results int
}

func (t search) Definition() api.Definition {
	n := t.h.Names
	// Memories are written in the card's language, and a word in another one
	// finds none of them.
	query, _ := json.Marshal("the words to look for, in " + t.h.Language)
	return api.Definition{
		Name: "search_memories",
		Description: "Search the memories you have kept about " + n.User + " and about yourself by the words " +
			"they hold, the ones the words say most about first. " + arrangement(n) + " Search them when " +
			"something comes up that you may have been told before: a name, a place, a date, a plan, what " +
			n.User + " likes or does. A word finds its other forms, but not other words for the same thing, " +
			"so look for the words a memory would use, in " + t.h.Language + ".",
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

func (search) LooksUp() {}

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
		Description: "Keep a lasting fact about " + n.User + ", or one about yourself, as a memory. " +
			"Memories are the only thing that lasts word for word beyond the recent messages: nothing keeps a " +
			"memory for you, so a fact you do not keep is left to the summary, which loses the details. Keep " +
			"what you will want to know later: names and relationships, where and how " + n.User + " lives " +
			"and works, what " + n.User + " likes and dislikes, dates and plans that matter, and what you said " +
			"about yourself. Write it as one sentence in " + t.h.Language + ", in the third person, " +
			"naming who it is about: " + n.User + " or " + n.Character + ". " +
			"When it updates or contradicts memories you have, give their numbers in replaces: they are " +
			"replaced by this one. Your memories are not in your prompt; list_memories and search_memories " +
			"are how you see them.",
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
			" asks you to. The memories it replaced go with it. The number is the one list_memories and " +
			"search_memories give.",
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
