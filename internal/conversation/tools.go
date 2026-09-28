package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"nerdola.dev/x/paula/internal/media"
	"nerdola.dev/x/paula/internal/runners/api"
	"nerdola.dev/x/paula/internal/store"
	toolsapi "nerdola.dev/x/paula/internal/tools/api"
)

// tools are the tools a conversation offers, what each is called, the text a
// model reads of them, and what they have her told in her system prompt.
type tools struct {
	defs   []api.ToolDef
	byName map[string]toolsapi.Tool
	text   string
	prompt string
}

func toolsOf(list []toolsapi.Tool) (tools, error) {
	t := tools{byName: make(map[string]toolsapi.Tool, len(list))}
	var prompt []string
	for _, tool := range list {
		// What a tool says of itself is what a runner offers a model.
		def := api.ToolDef(tool.Definition())
		if _, ok := t.byName[def.Name]; ok {
			return tools{}, fmt.Errorf("two tools are called %s", def.Name)
		}
		t.byName[def.Name] = tool
		t.defs = append(t.defs, def)
		if i, ok := tool.(toolsapi.Instructor); ok {
			prompt = append(prompt, i.Instructions())
		}
	}
	t.text = toolsText(t.defs)
	t.prompt = strings.Join(prompt, "\n\n")
	return t, nil
}

// looksUp says a tool of that name only looks something up.
func (t tools) looksUp(name string) bool {
	_, ok := t.byName[name].(toolsapi.Lookup)
	return ok
}

// A call a reply asked for is left unrun, and answered with why, once running
// it would come to nothing.
var (
	// errLookup is a call that only looks something up, or whose answer only a
	// later round could act on, asked for in the last round a reply may take
	// calls in: nothing it answered would be read before the reply is asked
	// for its answer.
	errLookup = errors.New("not run: nothing could act on what it answers, since the reply had taken every round of calls it may")
	// errNotRun is a call asked for in the round after the last, which was
	// asked for an answer with no call in it.
	errNotRun = errors.New("not run: the reply had taken every round of calls it may")
)

// call runs one tool a model asked for, and is what it answered, which is what
// the model is sent back unless it is too long for the room the round has. A
// call given why it is left is not run, and is answered with that. Whatever
// came of it is written down under the reply's entry and the request that
// asked, a call that could not run among them. The pictures it shows are
// added to shown, which is nil for a call that is not run.
func (e *Engine) call(ctx context.Context, a *attempt, request int64, c api.ToolCall, left error, shown *pictures) string {
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

	result, err := "", left
	if left == nil {
		result, err = e.run(ctx, a, c, rec.ID, shown)
	}
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

// run is a call itself: the tool it names, held to the arguments it gives. The
// requests the tool makes name the call, by the number it was written down
// under.
func (e *Engine) run(ctx context.Context, a *attempt, c api.ToolCall, call int64, shown *pictures) (string, error) {
	tool, ok := e.tools.byName[c.Name]
	if !ok {
		return "", fmt.Errorf("no tool is called %s", c.Name)
	}
	// A model asking for a tool that takes no parameters sends no arguments at
	// all on some hosts, which is asking for none, as an empty object is.
	args := json.RawMessage(c.Arguments)
	if strings.TrimSpace(c.Arguments) == "" {
		args = json.RawMessage(`{}`)
	}
	if !json.Valid(args) {
		return "", fmt.Errorf("the arguments are not JSON: %s", c.Arguments)
	}
	e.events.publish(Event{
		Kind: Note, Entry: a.entry.ID, Channel: a.entry.Channel, Text: tool.Note(args),
	})
	// From here the reply has done something, whatever the tool answers: a
	// call that named no tool, or gave no arguments one could read, did not.
	a.acted = true
	return tool.Call(ctx, env{e: e, a: a, call: call, shown: shown}, args)
}

// pictures are the pictures a call showed, which go to the model in its
// answer, and whether the model sees a picture there.
type pictures struct {
	sees   bool
	images [][]byte
}

// answered is what a call answered as the model is sent it: the text, and the
// pictures it showed.
func (e *Engine) answered(result string, p pictures) []api.Part {
	out := []api.Part{{Type: api.PartText, Text: result}}
	for _, data := range p.images {
		out = append(out, api.Part{Type: api.PartImage, MIME: media.MIMEJPEG, Data: data})
	}
	return out
}

// env is what a call reaches of the conversation: the engine, the reply that
// asked for it, the number the call was written down under, and the pictures
// the call shows.
type env struct {
	e     *Engine
	a     *attempt
	call  int64
	shown *pictures
}

func (v env) Date(t time.Time) string { return dateText(v.e.clock.Now().Location(), t) }

func (v env) Time(t time.Time) string { return timeText(v.e.clock.Now().Location(), t) }

func (v env) Memories(ctx context.Context, query string, limit int) ([]store.Memory, error) {
	return v.e.Memories(ctx, query, limit)
}

func (v env) LatestMemories(ctx context.Context, from, limit int) ([]store.Memory, error) {
	return v.e.store.LatestMemories(ctx, from, limit)
}

func (v env) Images(ctx context.Context, from, limit int) ([]store.Image, error) {
	return v.e.store.ImagesFrom(ctx, from, limit)
}

func (v env) Image(ctx context.Context, id int64) (*store.Image, error) {
	return v.e.store.Image(ctx, id)
}

// Show sends a picture in the call's answer, to a model shown a picture
// there: any other, and one whose file cannot be read, has what it showed in
// the answer's text.
func (v env) Show(img store.Image) bool {
	if v.shown == nil || !v.shown.sees {
		return false
	}
	data, err := v.e.media.Load(img.SHA256)
	if err != nil {
		v.e.log.Warn("reading an image", "sha256", img.SHA256, "error", err)
		return false
	}
	v.shown.images = append(v.shown.images, data)
	return true
}

// Remember keeps a memory as said in the newest message the reply answers,
// which is what she was told it in.
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

func (v env) Now() time.Time { return v.e.clock.Now() }

func (v env) Callbacks(ctx context.Context) ([]store.Callback, error) {
	return v.e.store.Callbacks(ctx)
}

// Schedule keeps a call back under the reply that asked for it, and has the
// loop look at when the soonest is due again.
func (v env) Schedule(ctx context.Context, at time.Time, reason string) (*store.Callback, error) {
	c := &store.Callback{DueAt: at, Reason: reason, Entry: v.a.entry.ID}
	if err := v.e.store.Schedule(ctx, c); err != nil {
		return nil, err
	}
	v.a.putOff = true
	v.e.rearm()
	return c, nil
}

func (v env) Move(ctx context.Context, id store.CallbackID, at time.Time) error {
	if err := v.e.store.MoveCallback(ctx, id, at); err != nil {
		return err
	}
	v.e.rearm()
	return nil
}

func (v env) Cancel(ctx context.Context, id store.CallbackID) error {
	if err := v.e.store.CancelCallback(ctx, id); err != nil {
		return err
	}
	v.e.rearm()
	return nil
}

// Recorder keeps a call's requests under the call, and apart from the rounds
// of the reply, whose recorder names the last of them as the round that asked
// for a call.
func (v env) Recorder() api.Recorder {
	rec := v.e.recorder(v.a, store.PurposeTool)
	rec.call = v.call
	return rec
}
