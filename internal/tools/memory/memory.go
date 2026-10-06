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

// defaultResults is how many memories a search answers with, and defaultPage
// how many a list does, when the file says nothing.
const (
	defaultResults = 10
	defaultPage    = 50
)

type settings struct {
	// Results is how many memories a search answers with, and Page how many a
	// list does, unless the call asks for another number.
	Results int `yaml:"results"`
	Page    int `yaml:"page"`
}

// Open reads the memory section and builds the four tools.
func Open(s config.Section, h api.Host) ([]api.Tool, error) {
	cfg := settings{Results: defaultResults, Page: defaultPage}
	if err := s.Decode(&cfg); err != nil {
		return nil, err
	}
	p := &config.Problems{Path: s.Path()}
	if cfg.Results < 1 {
		p.Addf("results: %d is below one", cfg.Results)
	}
	if cfg.Page < 1 {
		p.Addf("page: %d is below one", cfg.Page)
	}
	if err := p.Err(); err != nil {
		return nil, err
	}
	return []api.Tool{list{h: h, page: cfg.Page}, search{h: h, results: cfg.Results}, remember{h}, forget{h}}, nil
}

// counted is a number of memories, as a sentence says it.
func counted(n int) string {
	if n == 1 {
		return "1 memory"
	}
	return fmt.Sprintf("%d memories", n)
}

// arrangement is what every memory tool tells a model of where its memories
// are: nowhere in front of it, until it looks them up.
func arrangement(n api.Names) string {
	return "Your memories are not in your prompt. What you have in front of you is the summary of your " +
		"conversation with " + n.User + " so far and its most recent messages word for word; older messages " +
		"survive only as that summary, which keeps the story and loses the details. A memory is a lasting " +
		"fact that you kept with remember, and it is in front of you only when you list or search your " +
		"memories. Each memory has a number, which remember takes to replace it and forget_memory to take " +
		"it away."
}

type list struct {
	h    api.Host
	page int
}

func (t list) Definition() api.Definition {
	n := t.h.Names
	return api.Definition{
		Name: "list_memories",
		Description: fmt.Sprintf("List the memories you have kept about %s and about yourself, newest first, "+
			"each with its number and the day it was said: %d at a time, or as many as limit asks for. The "+
			"answer says how many memories there are in all. ", n.User, t.page) + arrangement(n) +
			" List them when you need to know what you know about " + n.User + ": when the conversation picks " +
			"up after a while, when something personal comes up that you may have been told before, and before " +
			"keeping a memory, to see whether it updates one you already have. A list with older memories " +
			"after it says so, and from lists them.",
		Parameters: json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{`+
			`"from":{"type":"integer","description":"how many of the newest memories to pass over; leave it out for the newest"},`+
			`"limit":{"type":"integer","description":"how many memories to list; leave it out for %d"}}}`, t.page)),
	}
}

type listArgs struct {
	From  int `json:"from"`
	Limit int `json:"limit"`
}

func (list) Note(json.RawMessage) string { return "listing memories" }

func (list) LooksUp() {}

func (t list) Call(ctx context.Context, env api.Env, args json.RawMessage) (string, error) {
	var a listArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	if a.From < 0 {
		return "", fmt.Errorf("from is %d, below zero", a.From)
	}
	limit, err := api.Count("limit", a.Limit, t.page)
	if err != nil {
		return "", err
	}
	memories, total, err := env.LatestMemories(ctx, a.From, limit)
	if err != nil {
		return "", err
	}
	if len(memories) == 0 {
		if total > 0 {
			return fmt.Sprintf("no memories past the newest %d: there are %d in all", a.From, total), nil
		}
		return "no memories yet", nil
	}
	shown := a.From + len(memories)
	out := fmt.Sprintf("%d to %d of %s, newest first:\n", a.From+1, shown, counted(total)) + lines(env, memories)
	if shown < total {
		out += fmt.Sprintf("\nThere are older memories: list_memories with from %d lists them.", shown)
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
			"they hold. Each word of the query is looked for on its own, so every memory that holds any one of " +
			"them is found, whatever it is about; the ones that hold more of the words, and rarer ones, come " +
			"first. A word finds its other forms, but not other words for the same thing, so look for the " +
			"words a memory would use, in " + t.h.Language + ". The answer says how many memories hold a word " +
			fmt.Sprintf("of the query, and lists the first %d of them, or as many as limit asks for. ", t.results) +
			arrangement(n) + " Search them when something comes up that you may have been told before: a " +
			"name, a place, a date, a plan, what " + n.User + " likes or does.",
		Parameters: json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{`+
			`"query":{"type":"string","description":%s},`+
			`"limit":{"type":"integer","description":"how many memories to list; leave it out for %d"}},`+
			`"required":["query"]}`, query, t.results)),
	}
}

type searchArgs struct {
	Query string `json:"query"`
	Limit int    `json:"limit"`
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
	limit, err := api.Count("limit", a.Limit, t.results)
	if err != nil {
		return "", err
	}
	memories, total, err := env.Memories(ctx, query, limit)
	if err != nil {
		return "", err
	}
	if len(memories) == 0 {
		return "no memory holds a word of this", nil
	}
	var head string
	switch {
	case total == 1:
		head = "1 memory holds a word of this:\n"
	case len(memories) == total:
		head = counted(total) + " hold a word of this:\n"
	case len(memories) == 1:
		head = counted(total) + " hold a word of this; this is the one that holds most of them:\n"
	default:
		head = fmt.Sprintf("%s hold a word of this; these are the %d that hold most of them:\n",
			counted(total), len(memories))
	}
	return head + lines(env, memories), nil
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
			"about yourself. Write it in " + t.h.Language + ", in the third person, " +
			"naming who it is about: " + n.User + " or " + n.Character + ". " +
			"When it updates or contradicts memories you have, holding the same fact as it, give their numbers " +
			"in replaces: they are replaced by this one. Your memories are not in your prompt; list_memories " +
			"and search_memories are how you see them.",
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"memory":{"type":"string","description":"the fact"},` +
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
		Description: "Forget a memory by its number: one that should not be kept at all, or that " +
			t.h.Names.User + " asks you to forget. The memories it replaced go with it, so a memory that is only " +
			"wrong is corrected with remember, replacing it, rather than forgotten. The number is the one " +
			"list_memories and search_memories give.",
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
