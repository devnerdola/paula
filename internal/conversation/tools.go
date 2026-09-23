package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"nerdola.dev/x/paula/internal/runners/api"
	"nerdola.dev/x/paula/internal/store"
	toolsapi "nerdola.dev/x/paula/internal/tools/api"
)

// tools are the tools a conversation offers, what each is called, and the text
// a model reads of them.
type tools struct {
	defs   []api.ToolDef
	byName map[string]toolsapi.Tool
	text   string
}

func toolsOf(list []toolsapi.Tool) (tools, error) {
	t := tools{byName: make(map[string]toolsapi.Tool, len(list))}
	for _, tool := range list {
		// What a tool says of itself is what a runner offers a model.
		def := api.ToolDef(tool.Definition())
		if _, ok := t.byName[def.Name]; ok {
			return tools{}, fmt.Errorf("two tools are called %s", def.Name)
		}
		t.byName[def.Name] = tool
		t.defs = append(t.defs, def)
	}
	t.text = toolsText(t.defs)
	return t, nil
}

// errNotRun is a call a reply asked for after it had taken every round it may.
// The round it came in was asked for an answer with no call in it.
var errNotRun = errors.New("not run: the reply had taken every round of calls it may")

// call runs one tool a model asked for, and is what the model is sent back.
// Whatever came of it is written down under the reply's entry and the request
// that asked, a call that could not run among them.
func (e *Engine) call(ctx context.Context, a *attempt, request int64, c api.ToolCall, run bool) string {
	rec := &store.ToolCall{
		EntryID:   a.entry.ID,
		RequestID: request,
		CallID:    c.ID,
		Name:      c.Name,
		Arguments: c.Arguments,
		StartedAt: e.clock.Now(),
	}
	// A call is written down as it starts, so a run that ends in the middle of
	// one leaves a record that it was asked for.
	keep := context.WithoutCancel(ctx)
	if err := e.store.StartToolCall(keep, rec); err != nil {
		e.log.Error("keeping a tool call", "entry", a.entry.ID, "error", err)
	}

	result, err := e.run(ctx, a, c, run)
	if err != nil {
		rec.Error = err.Error()
		result = "error: " + rec.Error
	}
	rec.Result = result
	rec.EndedAt = e.clock.Now()
	if rec.ID != 0 {
		if err := e.store.EndToolCall(keep, rec); err != nil {
			e.log.Error("keeping what came of a tool call", "entry", a.entry.ID, "error", err)
		}
	}
	return result
}

// run is a call itself: the tool it names, held to the arguments it gives.
func (e *Engine) run(ctx context.Context, a *attempt, c api.ToolCall, run bool) (string, error) {
	if !run {
		return "", errNotRun
	}
	tool, ok := e.tools.byName[c.Name]
	if !ok {
		return "", fmt.Errorf("no tool is called %s", c.Name)
	}
	args := json.RawMessage(c.Arguments)
	if !json.Valid(args) {
		return "", fmt.Errorf("the arguments are not JSON: %s", c.Arguments)
	}
	e.events.publish(Event{
		Kind: Note, Entry: a.entry.ID, Channel: a.entry.Channel, Text: tool.Note(args),
	})
	return tool.Call(ctx, env{e: e, a: a}, args)
}

// env is what a call reaches of the conversation: the engine, and the reply
// that asked for it.
type env struct {
	e *Engine
	a *attempt
}

func (v env) Date(t time.Time) string { return dateText(v.e.clock.Now().Location(), t) }

// Memories searches the memories as Engine.Memories does, and keeps the
// request that embeds the query under the reply that asked, as a request of
// that turn.
func (v env) Memories(ctx context.Context, query string, limit int) ([]store.Memory, error) {
	return v.e.memories(ctx, query, limit, v.e.recorder(v.a, store.PurposeMemorySearch))
}

// Remember keeps a memory as said in the newest message the reply answers,
// which is what she was told it in. It is embedded with the rest once the
// reply is done.
func (v env) Remember(ctx context.Context, content string, replaces []store.MemoryID) (*store.Memory, error) {
	m := &store.Memory{Content: content, Source: v.a.entry.UptoMessageID, Replaces: replaces}
	if err := v.e.store.Remember(ctx, m); err != nil {
		return nil, err
	}
	return m, nil
}

func (v env) Forget(ctx context.Context, id store.MemoryID) ([]store.Memory, error) {
	return v.e.Forget(ctx, id)
}
