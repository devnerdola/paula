package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/runners"
	"nerdola.dev/x/paula/internal/runners/api"
	"nerdola.dev/x/paula/internal/store"
	toolsapi "nerdola.dev/x/paula/internal/tools/api"
)

// fakeTool is a tool a test offers. It answers what the test gives it, keeps
// what it was asked with, and holds a call until the test lets it go when it
// is given something to hold on.
type fakeTool struct {
	name   string
	answer string
	fail   error

	mu    sync.Mutex
	asked []string
	// started is closed as a call starts, and hold keeps it running until a
	// test closes it.
	started chan struct{}
	hold    chan struct{}
}

func (t *fakeTool) Definition() toolsapi.Definition {
	return toolsapi.Definition{
		Name:        t.name,
		Description: "Looks something up.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`),
	}
}

func (t *fakeTool) Note(args json.RawMessage) string {
	var a struct{ Query string }
	json.Unmarshal(args, &a)
	return "looking up " + a.Query
}

func (t *fakeTool) Call(ctx context.Context, _ toolsapi.Env, args json.RawMessage) (string, error) {
	t.mu.Lock()
	t.asked = append(t.asked, string(args))
	started, hold := t.started, t.hold
	t.mu.Unlock()
	if started != nil {
		close(started)
	}
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return t.answer, t.fail
}

func (t *fakeTool) calls() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.asked...)
}

// round is what a model answers one round of a reply with.
type round struct {
	text      string
	calls     []api.ToolCall
	reasoning string
	details   []json.RawMessage
}

// answering answers the rounds of a reply in turn, and every round after them
// with the last.
func answering(rounds ...round) func(context.Context, api.ChatRequest, func(api.Chunk) error) (*api.Result, error) {
	var mu sync.Mutex
	next := 0
	return func(_ context.Context, _ api.ChatRequest, fn func(api.Chunk) error) (*api.Result, error) {
		mu.Lock()
		r := rounds[min(next, len(rounds)-1)]
		next++
		mu.Unlock()
		if r.text != "" {
			if err := fn(api.Chunk{Kind: api.ChunkText, Text: r.text}); err != nil {
				return nil, err
			}
		}
		return &api.Result{
			FinishReason: "stop", Reasoning: r.reasoning, ReasoningDetails: r.details, ToolCalls: r.calls,
		}, nil
	}
}

func lookup(id, args string) api.ToolCall {
	return api.ToolCall{ID: id, Name: "search_memories", Arguments: args}
}

// requests are every request the runner was sent, in order.
func (f *fakeRunner) all() []api.ChatRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]api.ChatRequest(nil), f.requests...)
}

// reply is the reply the engine stored last, and the calls its entry made.
func stored(t *testing.T, r *replyEngine) (*store.Message, []store.ToolCall) {
	t.Helper()
	m, err := r.store.LastMessage(context.Background(), store.RoleAssistant)
	if err != nil {
		t.Fatal(err)
	}
	calls, err := r.store.ToolCalls(context.Background(), m.EntryID)
	if err != nil {
		t.Fatal(err)
	}
	return m, calls
}

// A reply offered tools goes in rounds: the model asks for one, it runs, and
// what it answered goes back to the model, which answers with it.
func TestAToolIsRunAndWhatItAnsweredGoesBack(t *testing.T) {
	look := &fakeTool{name: "search_memories", answer: "Ana lives in Lisbon"}
	f := &fakeRunner{model: chatModel(), chat: answering(
		round{calls: []api.ToolCall{lookup("call_1", `{"query":"Ana"}`)}},
		round{text: "Ana lives in Lisbon, you told me"},
	)}
	r := openReplyOffering(t, f, setup(f), config.DefaultEngine(), look)
	r.say(t, "where does Ana live?")

	if got := look.calls(); len(got) != 1 || got[0] != `{"query":"Ana"}` {
		t.Errorf("the tool was asked %q", got)
	}

	requests := f.all()
	if len(requests) != 2 {
		t.Fatalf("%d rounds, want the one that asked and the one that answered", len(requests))
	}
	for i, req := range requests {
		if len(req.Tools) != 1 || req.Tools[0].Name != "search_memories" {
			t.Errorf("round %d was offered %+v", i+1, req.Tools)
		}
	}
	// The second round carries what the first asked for, and what it got.
	msgs := requests[1].Messages
	asked, answered := msgs[len(msgs)-2], msgs[len(msgs)-1]
	if asked.Role != api.RoleAssistant || len(asked.ToolCalls) != 1 || asked.ToolCalls[0].ID != "call_1" {
		t.Errorf("the round after the call opens with %+v, want the call it made", asked)
	}
	if answered.Role != api.RoleTool || answered.ToolCallID != "call_1" || text(answered) != "Ana lives in Lisbon" {
		t.Errorf("the tool's answer went back as %+v", answered)
	}

	reply, calls := stored(t, r)
	if reply.Text() != "Ana lives in Lisbon, you told me" {
		t.Errorf("the reply is %q", reply.Text())
	}
	if len(calls) != 1 {
		t.Fatalf("%d calls were written down, want the one that ran", len(calls))
	}
	c := calls[0]
	if c.CallID != "call_1" || c.Name != "search_memories" || c.Arguments != `{"query":"Ana"}` ||
		c.Result != "Ana lives in Lisbon" || c.Error != "" || c.EndedAt.IsZero() {
		t.Errorf("the call was written down as %+v", c)
	}
	// It names the request of the round that asked for it, which is the
	// reply's first.
	requestsOf, err := r.store.Requests(context.Background(), reply.EntryID)
	if err != nil {
		t.Fatal(err)
	}
	if len(requestsOf) != 2 || c.RequestID != requestsOf[0].ID {
		t.Errorf("the call names request %d, want the first of %+v", c.RequestID, requestsOf)
	}
}

// A reply offered nothing is one round, and a request that carries no tools
// is the request it always was.
func TestAReplyOfferedNothingIsOneRound(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: says("hi there")}
	r := openReply(t, f)
	r.say(t, "hey")

	requests := f.all()
	if len(requests) != 1 {
		t.Fatalf("%d rounds, want one", len(requests))
	}
	if requests[0].Tools != nil || requests[0].ToolChoice != "" {
		t.Errorf("the request carries tools %+v and choice %q", requests[0].Tools, requests[0].ToolChoice)
	}
}

// A reply may take so many rounds of calls. The round after them is asked for
// an answer with none in it, and what a model asks for there anyway is written
// down and left, so a model that keeps asking still answers.
func TestTheRoundAfterTheLastIsAskedForNoCall(t *testing.T) {
	look := &fakeTool{name: "search_memories", answer: "nothing"}
	f := &fakeRunner{model: chatModel(), chat: answering(
		round{calls: []api.ToolCall{lookup("call_1", `{"query":"a"}`)}},
		round{text: "I could not find it", calls: []api.ToolCall{lookup("call_2", `{"query":"b"}`)}},
	)}
	cfg := config.DefaultEngine()
	cfg.ToolRounds = 1
	r := openReplyOffering(t, f, setup(f), cfg, look)
	r.say(t, "do you remember?")

	requests := f.all()
	if len(requests) != 2 {
		t.Fatalf("%d rounds, want the round of calls and the one after it", len(requests))
	}
	if requests[0].ToolChoice != "" || requests[1].ToolChoice != api.ToolChoiceNone {
		t.Errorf("the rounds were asked with %q and %q", requests[0].ToolChoice, requests[1].ToolChoice)
	}
	if got := look.calls(); len(got) != 1 {
		t.Errorf("the tool ran %d times, want only in the round calls were taken in", len(got))
	}

	reply, calls := stored(t, r)
	if reply.Text() != "I could not find it" {
		t.Errorf("the reply is %q, want what was written beside the call that was left", reply.Text())
	}
	if len(calls) != 2 || !strings.Contains(calls[1].Error, "not run") {
		t.Errorf("the calls were written down as %+v, want the second left unrun", calls)
	}
}

// A call that cannot run is still answered, with why: a model told nothing
// would ask again, or answer as if it had been told something.
func TestACallThatCannotRunIsAnsweredWithWhy(t *testing.T) {
	broken := &fakeTool{name: "search_memories", fail: errors.New("the index is gone")}
	f := &fakeRunner{model: chatModel(), chat: answering(
		round{calls: []api.ToolCall{
			{ID: "call_1", Name: "read_minds", Arguments: `{}`},
			lookup("call_2", `{"query":`),
			lookup("call_3", `{"query":"Ana"}`),
		}},
		round{text: "sorry, I cannot look that up"},
	)}
	r := openReplyOffering(t, f, setup(f), config.DefaultEngine(), broken)
	r.say(t, "look it up")

	msgs := f.all()[1].Messages
	answers := msgs[len(msgs)-3:]
	for i, want := range []string{
		"error: no tool is called read_minds",
		"error: the arguments are not JSON",
		"error: the index is gone",
	} {
		if !strings.HasPrefix(text(answers[i]), want) {
			t.Errorf("call %d was answered %q, want %q", i+1, text(answers[i]), want)
		}
	}
	if got := broken.calls(); len(got) != 1 {
		t.Errorf("the tool ran %d times, want only for the call it could take", len(got))
	}
	_, calls := stored(t, r)
	for i, c := range calls {
		if c.Error == "" {
			t.Errorf("call %d was written down with no error: %+v", i+1, c)
		}
	}
}

// What she wrote in any round is what she said. Text written beside a call is
// on the screen already, and a model often puts its whole answer there and
// has nothing to add once the call is answered.
func TestAReplyIsWhatEveryRoundWrote(t *testing.T) {
	look := &fakeTool{name: "search_memories", answer: "found it"}
	f := &fakeRunner{model: chatModel(), chat: answering(
		round{text: "let me check", calls: []api.ToolCall{lookup("call_1", `{"query":"Ana"}`)}},
		round{text: "she lives in Lisbon"},
	)}
	r := openReplyOffering(t, f, setup(f), config.DefaultEngine(), look)
	r.say(t, "where does Ana live?")

	reply, _ := stored(t, r)
	if reply.Text() != "let me check\n\nshe lives in Lisbon" {
		t.Errorf("the reply is %q, want each round a text of its own", reply.Text())
	}
	// What the frontends are sent only ever grows, since they show what is new
	// of it.
	var was string
	for _, ev := range published(r.Engine) {
		if ev.Kind != ReplyText {
			continue
		}
		if !strings.HasPrefix(ev.Text, was) {
			t.Errorf("the reply went from %q to %q", was, ev.Text)
		}
		was = ev.Text
	}
}

func TestARoundWithNothingAfterItsCallIsStillAReply(t *testing.T) {
	look := &fakeTool{name: "search_memories", answer: "remembered"}
	f := &fakeRunner{model: chatModel(), chat: answering(
		round{text: "got it, I will remember that", calls: []api.ToolCall{lookup("call_1", `{"query":"x"}`)}},
		round{},
	)}
	r := openReplyOffering(t, f, setup(f), config.DefaultEngine(), look)
	r.say(t, "remember my sister is Ana")

	reply, _ := stored(t, r)
	if reply.Text() != "got it, I will remember that" {
		t.Errorf("the reply is %q, want what she wrote beside the call", reply.Text())
	}
}

// An answer that would take the next round past the context would have the
// host refuse the round, so the model is told in its place that the call ran
// and its answer did not fit. An answer that fits goes as it is.
func TestAnAnswerTooLongForTheContextIsNotSent(t *testing.T) {
	short := &fakeTool{name: "search_memories", answer: "Ana lives in Lisbon"}
	long := &fakeTool{name: "list_memories", answer: manyWords(1000)}
	f := &fakeRunner{model: chatModel(), chat: countedAtThree(answering(
		round{calls: []api.ToolCall{
			{ID: "call_1", Name: "search_memories", Arguments: `{"query":"Ana"}`},
			{ID: "call_2", Name: "list_memories", Arguments: `{}`},
		}},
		round{text: "Ana is in Lisbon"},
	))}
	r := openReplyOffering(t, f, sized(f, 2000), config.DefaultEngine(), short, long)
	r.say(t, "where does Ana live?")

	requests := f.all()
	if len(requests) != 2 {
		t.Fatalf("%d rounds, want the one that asked and the one after it", len(requests))
	}
	answers := map[string]string{}
	for _, m := range requests[1].Messages {
		if m.Role == api.RoleTool {
			answers[m.ToolCallID] = text(m)
		}
	}
	if got := answers["call_1"]; got != "Ana lives in Lisbon" {
		t.Errorf("the answer that fits went as %q", got)
	}
	if got := answers["call_2"]; !strings.HasPrefix(got, "error: ") || strings.Contains(got, manyWords(10)) {
		t.Errorf("the answer too long for the context went as %.60q…, want an error in its place", got)
	}
	if reply, _ := stored(t, r); reply.Text() != "Ana is in Lisbon" {
		t.Errorf("the reply is %q, want the round after the answers", reply.Text())
	}
}

// failure is what the latest failed reply said.
func failure(r *replyEngine) string {
	var out string
	for _, ev := range published(r.Engine) {
		if ev.Kind == ReplyFailed {
			out = ev.Text
		}
	}
	return out
}

// A round past the context before any answer is in it is one no host takes,
// and its calls run nothing: what they did would never reach the model. A
// reply that has done nothing fails and keeps nothing, saying why.
func TestARoundPastTheContextRunsNoneOfItsCalls(t *testing.T) {
	look := &fakeTool{name: "search_memories", answer: "Ana lives in Lisbon"}
	f := &fakeRunner{model: chatModel(), chat: countedAtThree(answering(
		round{text: manyWords(1000), calls: []api.ToolCall{lookup("call_1", `{"query":"Ana"}`)}},
		round{text: "never sent"},
	))}
	r := openReplyOffering(t, f, sized(f, 2000), config.DefaultEngine(), look)
	ctx := context.Background()
	r.say(t, "tell me about Ana")

	if got := look.calls(); len(got) != 0 {
		t.Errorf("the tool ran with %q, want none of the round's calls run", got)
	}
	if n := len(f.sentFor(store.PurposeReply)); n != 1 {
		t.Errorf("%d rounds went out, want the one before the round past the context", n)
	}
	if got := failure(r); !strings.Contains(got, "context") {
		t.Errorf("the reply ended saying %q, want it to say the round is past the context", got)
	}
	entries, err := r.store.Entries(ctx, 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries = %+v, %v", entries, err)
	}
	if kept, err := r.store.ReplyOfEntry(ctx, entries[0].ID); err == nil {
		t.Errorf("the reply kept %.60q…, want nothing of a reply that did nothing", kept.Text())
	}
}

// A reply whose earlier round ran a tool has done something, so a round past
// the context after it ends the reply with what she had written, the way any
// failure after a tool ran does.
func TestARoundPastTheContextAfterACallThatRanKeepsWhatItWrote(t *testing.T) {
	look := &fakeTool{name: "search_memories", answer: "Ana lives in Lisbon"}
	f := &fakeRunner{model: chatModel(), chat: countedAtThree(answering(
		round{text: "let me look", calls: []api.ToolCall{lookup("call_1", `{"query":"Ana"}`)}},
		round{text: manyWords(1000), calls: []api.ToolCall{lookup("call_2", `{"query":"Lisbon"}`)}},
		round{text: "never sent"},
	))}
	r := openReplyOffering(t, f, sized(f, 2000), config.DefaultEngine(), look)
	r.say(t, "tell me about Ana")

	if got := look.calls(); len(got) != 1 {
		t.Errorf("the tool ran %d times, want only in the round that fit", len(got))
	}
	if got := failure(r); !strings.Contains(got, "context") {
		t.Errorf("the reply ended saying %q, want it to say the round is past the context", got)
	}
	if reply, _ := stored(t, r); reply.Text() != "let me look\n\n"+manyWords(1000) {
		t.Errorf("the reply kept %.60q…, want what she had written", reply.Text())
	}
}

// A round whose answers were all too long is still past the context when the
// notes in their places take it there, and the reply ends with what she had
// written.
func TestARoundPastTheContextWithNotesInPlaceOfItsAnswersEndsTheReply(t *testing.T) {
	look := &fakeTool{name: "search_memories", answer: manyWords(1000)}
	var calls []api.ToolCall
	for i := range 50 {
		calls = append(calls, lookup(fmt.Sprintf("call_%d", i), `{"query":"Ana"}`))
	}
	f := &fakeRunner{model: chatModel(), chat: countedAtThree(answering(
		round{text: "let me look", calls: calls},
		round{text: "never sent"},
	))}
	r := openReplyOffering(t, f, sized(f, 2000), config.DefaultEngine(), look)
	r.say(t, "tell me about Ana")

	if n := len(f.sentFor(store.PurposeReply)); n != 1 {
		t.Errorf("%d rounds went out, want the one before the round past the context", n)
	}
	if got := failure(r); !strings.Contains(got, "context") {
		t.Errorf("the reply ended saying %q, want it to say the round is past the context", got)
	}
	if reply, _ := stored(t, r); reply.Text() != "let me look" {
		t.Errorf("the reply kept %q, want what she had written", reply.Text())
	}
}

// A model that reasons across the rounds of a reply reads its own thinking
// again, so the reasoning a round's calls came with goes back with them.
func TestTheReasoningOfARoundGoesBackWithItsCalls(t *testing.T) {
	look := &fakeTool{name: "search_memories", answer: "found"}
	details := []json.RawMessage{json.RawMessage(`{"type":"reasoning.text","text":"I should look"}`)}
	f := &fakeRunner{model: chatModel(), chat: answering(
		round{reasoning: "I should look", details: details, calls: []api.ToolCall{lookup("call_1", `{"query":"a"}`)}},
		round{text: "here"},
	)}
	r := openReplyOffering(t, f, setup(f), config.DefaultEngine(), look)
	r.say(t, "look")

	msgs := f.all()[1].Messages
	asked := msgs[len(msgs)-2]
	if asked.Reasoning == nil || asked.Reasoning.Text != "I should look" ||
		len(asked.Reasoning.Details) != 1 || string(asked.Reasoning.Details[0]) != string(details[0]) {
		t.Errorf("the round went back with the reasoning %+v", asked.Reasoning)
	}
}

// keepsThought is a family extension that has every earlier reply sent back
// with what it thought.
type keepsThought struct{}

func (keepsThought) Notes() api.Notes                     { return api.Notes{Role: api.RoleSystem} }
func (keepsThought) PastThought() bool                    { return true }
func (keepsThought) Body(map[string]any, api.ChatRequest) {}

// talkedWithThought is a reply that ran a tool and thought in both its rounds,
// and two that did not think, and the replies the last prompt carried. The
// details are what OpenRouter sent of a reply that asked for a tool, in the
// item its fragments make, and what it sent of one that answered.
func talkedWithThought(t *testing.T, extension api.Extension) (answered json.RawMessage, replies []api.Message) {
	t.Helper()
	look := &fakeTool{name: "search_memories", answer: "Ana lives in Lisbon"}
	asking := json.RawMessage(`{"type":"reasoning.text","text":"I need to search memories for Caio's sister's name.","format":"unknown","index":0}`)
	answered = json.RawMessage(`{"type":"reasoning.text","text":"She lives in Lisbon.","format":"unknown","index":0}`)
	f := &fakeRunner{model: chatModel(), chat: answering(
		round{reasoning: "I need to search memories for Caio's sister's name.", details: []json.RawMessage{asking},
			calls: []api.ToolCall{lookup("call_1", `{"query":"sister"}`)}},
		round{text: "Ana, in Lisbon", reasoning: "She lives in Lisbon.", details: []json.RawMessage{answered}},
		round{text: "hi"},
		round{text: "sure"},
	)}
	set := setup(f)
	set.Models[0].Settings.Extension = extension
	r := openReplyOffering(t, f, set, config.DefaultEngine(), look)
	r.say(t, "what's my sister's name?")
	r.say(t, "hey")
	r.say(t, "ok")

	requests := f.all()
	for _, m := range requests[len(requests)-1].Messages {
		if m.Role == api.RoleAssistant {
			replies = append(replies, m)
		}
	}
	if len(replies) != 2 {
		t.Fatalf("the last prompt carries %d replies, want the two before it", len(replies))
	}
	return answered, replies
}

// A family whose chat template keeps the thinking of earlier turns reads a
// reply sent back without it as one that thought nothing, and learns to think
// nothing. Its extension asks for every earlier reply with what it thought:
// the text of every round, and the details of the round that answered. The
// reply goes back as one message, which in the answer was that round, and a
// host holds signed details to the response they came in: a round's with
// another's are not what either sent. A reply that thought nothing goes back
// with nothing.
func TestAPastReplyGoesBackWithWhatItThoughtWhenItsFamilyAsks(t *testing.T) {
	answered, replies := talkedWithThought(t, keepsThought{})
	thought := replies[0].Reasoning
	if thought == nil || thought.Text != "I need to search memories for Caio's sister's name.\n\nShe lives in Lisbon." {
		t.Fatalf("the first reply went back with %+v, want what both its rounds thought", thought)
	}
	if len(thought.Details) != 1 || string(thought.Details[0]) != string(answered) {
		t.Errorf("the first reply went back with the details %s, want the answering round's as it came", thought.Details)
	}
	if replies[1].Reasoning != nil {
		t.Errorf("a reply that thought nothing went back with %+v", replies[1].Reasoning)
	}
}

// A model whose family does not ask for what earlier replies thought reads
// each of them as what it said, whether no extension serves it or one that
// asks for nothing does.
func TestAPastReplyGoesBackAsWhatItSaidWhenItsFamilyDoesNotAsk(t *testing.T) {
	for _, extension := range []api.Extension{nil, notesAsUser{}} {
		_, replies := talkedWithThought(t, extension)
		for i, reply := range replies {
			if reply.Reasoning != nil {
				t.Errorf("with %T, reply %d went back with %+v, want what it said alone", extension, i+1, reply.Reasoning)
			}
		}
	}
}

// What a model thought is signed or encrypted for it and the host it came
// from, so a reply goes back with it as it came only to the model that wrote
// it. A model whose family asks for what earlier replies thought is sent
// another model's replies with what they thought as text alone: switched to
// another model, it reads the first model's thinking as text, and switched
// back, the first model reads its own as it came.
func TestAReplyGoesBackWithWhatItThoughtAsItCameOnlyToTheModelThatWroteIt(t *testing.T) {
	details := json.RawMessage(`{"type":"reasoning.text","text":"She lives in Lisbon.","format":"unknown","index":0}`)
	f := &fakeRunner{model: chatModel(), chat: answering(
		round{text: "Ana, in Lisbon", reasoning: "She lives in Lisbon.", details: []json.RawMessage{details}},
	)}
	g := &fakeRunner{model: api.Model{ID: "other/model", Context: 100000, Chat: true, Tools: true}, chat: answering(
		round{text: "yes", reasoning: "Another model's thought."},
	)}
	first := &runners.Configured{Name: "chat", ID: f.model.ID, Runner: f, Settings: api.Settings{Extension: keepsThought{}}}
	second := &runners.Configured{Name: "other", ID: g.model.ID, Runner: g, Settings: api.Settings{Extension: keepsThought{}}}
	r := openReplyWith(t, f, &runners.Setup{
		Runners:  []runners.Runner{f, g},
		Models:   []*runners.Configured{first, second},
		Defaults: map[config.Role]*runners.Configured{config.RoleChat: first},
	})
	ctx := context.Background()
	replies := func(req api.ChatRequest) []api.Message {
		var out []api.Message
		for _, m := range req.Messages {
			if m.Role == api.RoleAssistant {
				out = append(out, m)
			}
		}
		return out
	}

	r.say(t, "where does Ana live?")
	if err := r.SetModel(ctx, config.RoleChat, "other"); err != nil {
		t.Fatal(err)
	}
	r.say(t, "you there?")
	got := replies(g.asked())
	if len(got) != 1 {
		t.Fatalf("the other model was sent %d replies, want the first model's", len(got))
	}
	if got[0].Reasoning == nil || got[0].Reasoning.Text != "She lives in Lisbon." || len(got[0].Reasoning.Details) != 0 {
		t.Errorf("the other model was sent the first model's reply with %+v, want what it thought as text alone", got[0].Reasoning)
	}

	if err := r.SetModel(ctx, config.RoleChat, "chat"); err != nil {
		t.Fatal(err)
	}
	r.say(t, "ok")
	got = replies(f.asked())
	if len(got) != 2 {
		t.Fatalf("the first model was sent %d replies, want both", len(got))
	}
	if got[0].Reasoning == nil || len(got[0].Reasoning.Details) != 1 || string(got[0].Reasoning.Details[0]) != string(details) {
		t.Errorf("the first model was sent its own reply with %+v, want what it thought as it came", got[0].Reasoning)
	}
	if got[1].Reasoning == nil || got[1].Reasoning.Text != "Another model's thought." || len(got[1].Reasoning.Details) != 0 {
		t.Errorf("the first model was sent the other model's reply with %+v, want what it thought as text alone", got[1].Reasoning)
	}
}

// A round that sends a blank line before the calls it asks for has written
// nothing: no text of it is shown, and the reply is what the next round
// writes.
func TestABlankLineBeforeACallIsNotWritten(t *testing.T) {
	look := &fakeTool{name: "search_memories", answer: "found"}
	f := &fakeRunner{model: chatModel(), chat: answering(
		round{text: "\n\n", calls: []api.ToolCall{lookup("call_1", `{"query":"Ana"}`)}},
		round{text: "found it"},
	)}
	r := openReplyOffering(t, f, setup(f), config.DefaultEngine(), look)
	r.say(t, "look it up")

	var texts []string
	for _, ev := range published(r.Engine) {
		if ev.Kind == ReplyText {
			texts = append(texts, ev.Text)
		}
	}
	if len(texts) != 1 || texts[0] != "found it" {
		t.Errorf("the reply was published as %q, want only what the round after the call wrote", texts)
	}
}

// What she is doing in the middle of a reply is said while it runs, in the
// words of the tool she asked for.
func TestANoteIsSaidForEachCall(t *testing.T) {
	look := &fakeTool{name: "search_memories", answer: "found"}
	f := &fakeRunner{model: chatModel(), chat: answering(
		round{calls: []api.ToolCall{lookup("call_1", `{"query":"Ana"}`), lookup("call_2", `{"query":"Lisbon"}`)}},
		round{text: "found them"},
	)}
	r := openReplyOffering(t, f, setup(f), config.DefaultEngine(), look)
	r.say(t, "look them up")

	var notes []string
	for _, ev := range published(r.Engine) {
		if ev.Kind == Note {
			notes = append(notes, ev.Text)
		}
	}
	if strings.Join(notes, "|") != "looking up Ana|looking up Lisbon" {
		t.Errorf("the notes are %q", notes)
	}
}

// A stop while a tool runs keeps what she had written before it, the way a
// stop keeps what a reply had written.
func TestAStopWhileAToolRunsKeepsWhatWasWritten(t *testing.T) {
	look := &fakeTool{name: "search_memories", answer: "found",
		started: make(chan struct{}), hold: make(chan struct{})}
	defer close(look.hold)
	f := &fakeRunner{model: chatModel(), chat: answering(
		round{text: "let me check", calls: []api.ToolCall{lookup("call_1", `{"query":"a"}`)}},
		round{text: "never written"},
	)}
	r := openReplyOffering(t, f, setup(f), config.DefaultEngine(), look)

	post(t, r.Engine, "look it up")
	r.clock.Advance(config.DefaultEngine().Debounce.Duration())
	<-look.started
	if stopped, err := r.Stop(context.Background()); err != nil || !stopped {
		t.Fatalf("Stop = %v, %v", stopped, err)
	}
	if _, err := r.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}

	reply, _ := stored(t, r)
	if reply.Text() != "let me check" || !reply.Interrupted {
		t.Errorf("the reply is %+v, want what was written before the call, cut short", reply)
	}
}

// A stop that lands as a round asks for a call runs none of it, and keeps what
// she had written.
func TestAStopAsARoundAsksForACallRunsNone(t *testing.T) {
	look := &fakeTool{name: "search_memories", answer: "found"}
	started := make(chan struct{})
	f := &fakeRunner{model: chatModel(), chat: func(ctx context.Context, _ api.ChatRequest, fn func(api.Chunk) error) (*api.Result, error) {
		if err := fn(api.Chunk{Kind: api.ChunkText, Text: "let me check"}); err != nil {
			return nil, err
		}
		close(started)
		<-ctx.Done()
		return &api.Result{FinishReason: "tool_calls", ToolCalls: []api.ToolCall{lookup("call_1", `{"query":"a"}`)}}, nil
	}}
	r := openReplyOffering(t, f, setup(f), config.DefaultEngine(), look)

	post(t, r.Engine, "look it up")
	r.clock.Advance(config.DefaultEngine().Debounce.Duration())
	<-started
	if stopped, err := r.Stop(context.Background()); err != nil || !stopped {
		t.Fatalf("Stop = %v, %v", stopped, err)
	}
	if _, err := r.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}

	reply, calls := stored(t, r)
	if reply.Text() != "let me check" || !reply.Interrupted {
		t.Errorf("the reply is %+v, want what was written before the call, cut short", reply)
	}
	if got := look.calls(); len(got) != 0 || len(calls) != 0 {
		t.Errorf("the tool ran %q and the entry kept %+v, want no call", got, calls)
	}
}

// A reply that has run a tool has done something, so a message that arrives
// while the tool runs does not take its place.
func TestAMessageWhileAToolRunsDoesNotTakeTheRepliesPlace(t *testing.T) {
	look := &fakeTool{name: "search_memories", answer: "found",
		started: make(chan struct{}), hold: make(chan struct{})}
	f := &fakeRunner{model: chatModel(), chat: answering(
		round{calls: []api.ToolCall{lookup("call_1", `{"query":"a"}`)}},
		round{text: "here it is"},
	)}
	r := openReplyOffering(t, f, setup(f), config.DefaultEngine(), look)

	post(t, r.Engine, "look it up")
	r.clock.Advance(config.DefaultEngine().Debounce.Duration())
	<-look.started
	post(t, r.Engine, "and one more thing")
	close(look.hold)
	waitFor(t, "the reply to end", func() bool { return seen(r.Engine, ReplyDone) || seen(r.Engine, ReplyRestarted) })

	if seen(r.Engine, ReplyRestarted) {
		t.Error("the reply was restarted after its tool had run")
	}
}

// A reply that has run a tool has done something that stands, so a round that
// fails after it keeps what the reply wrote and answers its messages: asking
// again would run the tool again. What went wrong is said all the same.
func TestARoundThatFailsAfterAToolKeepsTheReply(t *testing.T) {
	for _, wrote := range []string{"", "let me note that"} {
		t.Run(fmt.Sprintf("%q", wrote), func(t *testing.T) {
			look := &fakeTool{name: "search_memories", answer: "noted"}
			var rounds atomic.Int32
			f := &fakeRunner{model: chatModel(), chat: func(ctx context.Context, req api.ChatRequest, fn func(api.Chunk) error) (*api.Result, error) {
				switch rounds.Add(1) {
				case 1:
					return answering(round{text: wrote, calls: []api.ToolCall{lookup("call_1", `{"query":"Ana"}`)}})(ctx, req, fn)
				case 2:
					return nil, errHostAway
				}
				return says("and Lisbon too")(ctx, req, fn)
			}}
			r := openReplyOffering(t, f, setup(f), config.DefaultEngine(), look)
			ctx := context.Background()
			r.say(t, "my sister is Ana")

			entries, err := r.store.Entries(ctx, 10)
			if err != nil || len(entries) != 1 {
				t.Fatalf("entries = %+v, %v", entries, err)
			}
			if entries[0].Status != store.StatusStopped || !strings.Contains(entries[0].Error, errHostAway.Error()) {
				t.Errorf("entry = %+v, want it stopped with what went wrong", entries[0])
			}
			var failed *Event
			for _, ev := range published(r.Engine) {
				if ev.Kind == ReplyFailed {
					failed = &ev
				}
			}
			if failed == nil || failed.Text != errHostAway.Error() {
				t.Fatalf("the failure was published as %+v", failed)
			}
			kept, err := r.store.ReplyOfEntry(ctx, entries[0].ID)
			switch {
			case wrote == "":
				if err == nil || failed.Message != nil {
					t.Errorf("a reply that wrote nothing kept %+v", kept)
				}
			case err != nil || kept.Text() != wrote || !kept.Interrupted || failed.Message == nil || failed.Message.ID != kept.ID:
				t.Errorf("the reply kept %+v, %v and published %+v, want what it wrote, cut short", kept, err, failed.Message)
			}

			// The message was answered, so the next reply is for the next one
			// alone, and the tool is not run again.
			r.say(t, "and she lives in Lisbon")
			entries, err = r.store.Entries(ctx, 10)
			if err != nil || len(entries) != 2 {
				t.Fatalf("entries = %+v, %v", entries, err)
			}
			if entries[0].AfterMessageID != entries[1].UptoMessageID {
				t.Errorf("the next reply answers after %d, want after %d", entries[0].AfterMessageID, entries[1].UptoMessageID)
			}
			if got := look.calls(); len(got) != 1 {
				t.Errorf("the tool ran %d times, want once", len(got))
			}
		})
	}
}

// dater is a tool that answers with a day, as the conversation writes it.
type dater struct{ day time.Time }

func (dater) Definition() toolsapi.Definition {
	return toolsapi.Definition{Name: "date", Parameters: json.RawMessage(`{"type":"object"}`)}
}

func (dater) Note(json.RawMessage) string { return "dating" }

func (d dater) Call(_ context.Context, env toolsapi.Env, _ json.RawMessage) (string, error) {
	return env.Date(d.day), nil
}

// A tool writes a day the way the prompt it answers into does, in the zone of
// the conversation: half past eleven at night in UTC is the next day in Berlin.
func TestAToolWritesADayAsThePromptDoes(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: answering(
		round{calls: []api.ToolCall{{ID: "call_1", Name: "date", Arguments: `{}`}}},
		round{text: "that was a Sunday"},
	)}
	r := openReplyOffering(t, f, setup(f), config.DefaultEngine(),
		dater{day: time.Date(2026, 9, 19, 23, 30, 0, 0, time.UTC)})
	r.say(t, "which day was it?")

	_, calls := stored(t, r)
	if len(calls) != 1 || calls[0].Result != "Sunday, 20 September 2026" {
		t.Errorf("the calls are %+v, want the day written as Sunday, 20 September 2026", calls)
	}
}

// shower is a tool that shows the newest picture of the conversation, and says
// whether it will be shown.
type shower struct{}

func (shower) Definition() toolsapi.Definition {
	return toolsapi.Definition{Name: "get_image", Parameters: json.RawMessage(`{"type":"object"}`)}
}

func (shower) Note(json.RawMessage) string { return "looking" }

func (shower) Call(ctx context.Context, env toolsapi.Env, _ json.RawMessage) (string, error) {
	images, err := env.Images(ctx, 0, 1)
	if err != nil || len(images) == 0 {
		return "", fmt.Errorf("no picture: %v", err)
	}
	if env.Show(images[0]) {
		return "shown", nil
	}
	return "told", nil
}

// lookingAgain calls get_image when asked for the picture again, and answers
// everything else.
func lookingAgain(ctx context.Context, req api.ChatRequest, fn func(api.Chunk) error) (*api.Result, error) {
	last := req.Messages[len(req.Messages)-1]
	if last.Role == api.RoleUser && text(last) == "show me the picture again" {
		return &api.Result{FinishReason: "tool_calls",
			ToolCalls: []api.ToolCall{{ID: "call_1", Name: "get_image", Arguments: `{}`}}}, nil
	}
	return says("there it is")(ctx, req, fn)
}

// A picture a call shows goes to a model that sees images in the call's answer;
// a model that does not see is sent the answer's text alone. Nothing of it is
// sent outside the answer.
func TestAPictureACallShowsIsInItsAnswer(t *testing.T) {
	for _, sees := range []bool{true, false} {
		model := chatModel()
		model.Vision = sees
		f := &fakeRunner{model: model, chat: lookingAgain}
		r := openReplyOffering(t, f, setup(f), config.DefaultEngine(), shower{})
		sendPhoto(t, r, "look", photo(t))
		r.say(t, "show me the picture again")

		round := f.replied().Messages
		i := slices.IndexFunc(round, func(m api.Message) bool { return m.Role == api.RoleTool })
		if i < 0 {
			t.Fatalf("sees %v: the round after the call carries no answer", sees)
		}
		answer, pictures, after := text(round[i]), len(images(round[i])), round[i+1:]
		if len(after) != 0 {
			t.Errorf("sees %v: the answer was followed by %+v, want nothing after it", sees, after)
		}
		if sees && (answer != "shown" || pictures != 1) {
			t.Errorf("a model that sees was answered %q with %d pictures, want the picture in the answer", answer, pictures)
		}
		if !sees && (answer != "told" || pictures != 0) {
			t.Errorf("a model that does not see was answered %q with %d pictures, want the text alone", answer, pictures)
		}
	}
}

// A picture whose file cannot be read is not shown, so a call says what it
// showed instead of saying the picture is in its answer.
func TestAPictureWhoseFileIsGoneIsNotShown(t *testing.T) {
	model := chatModel()
	model.Vision = true
	f := &fakeRunner{model: model, chat: lookingAgain}
	r := openReplyOffering(t, f, setup(f), config.DefaultEngine(), shower{})
	sha := sendPhoto(t, r, "look", photo(t))
	if err := os.Remove(r.media.Path(sha)); err != nil {
		t.Fatal(err)
	}
	r.say(t, "show me the picture again")

	round := f.replied().Messages
	i := slices.IndexFunc(round, func(m api.Message) bool { return m.Role == api.RoleTool })
	if i < 0 {
		t.Fatal("the round after the call carries no answer")
	}
	if answer, pictures := text(round[i]), len(images(round[i])); answer != "told" || pictures != 0 {
		t.Errorf("the call was answered %q with %d pictures, want the text alone", answer, pictures)
	}
}

// A call that names no tool runs nothing, so a round that fails after it
// leaves the reply failed and its message unanswered: asking again repeats
// nothing that was done.
func TestARoundThatFailsAfterACallThatRanNothingAnswersNothing(t *testing.T) {
	look := &fakeTool{name: "search_memories", answer: "found"}
	var rounds atomic.Int32
	f := &fakeRunner{model: chatModel(), chat: func(ctx context.Context, req api.ChatRequest, fn func(api.Chunk) error) (*api.Result, error) {
		switch rounds.Add(1) {
		case 1:
			return answering(round{calls: []api.ToolCall{{ID: "call_1", Name: "read_minds", Arguments: `{}`}}})(ctx, req, fn)
		case 2:
			return nil, errHostAway
		}
		return says("sure")(ctx, req, fn)
	}}
	r := openReplyOffering(t, f, setup(f), config.DefaultEngine(), look)
	ctx := context.Background()
	r.say(t, "what do you think?")

	entries, err := r.store.Entries(ctx, 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries = %+v, %v", entries, err)
	}
	if entries[0].Status != store.StatusFailed {
		t.Errorf("entry = %+v, want it failed", entries[0])
	}

	// The next reply answers the message again.
	r.say(t, "hello?")
	entries, err = r.store.Entries(ctx, 10)
	if err != nil || len(entries) != 2 {
		t.Fatalf("entries = %+v, %v", entries, err)
	}
	if entries[0].AfterMessageID != entries[1].AfterMessageID {
		t.Errorf("the next reply answers after %d, want after %d, which the failed one was for",
			entries[0].AfterMessageID, entries[1].AfterMessageID)
	}
}

// keeper is a tool that keeps what it is asked to through the conversation.
type keeper struct{}

func (keeper) Definition() toolsapi.Definition {
	return toolsapi.Definition{Name: "remember", Parameters: json.RawMessage(`{"type":"object"}`)}
}

func (keeper) Note(json.RawMessage) string { return "remembering" }

func (keeper) Call(ctx context.Context, env toolsapi.Env, args json.RawMessage) (string, error) {
	var a struct {
		Memory   string
		Replaces []store.MemoryID
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	m, err := env.Remember(ctx, a.Memory, a.Replaces)
	if err != nil {
		return "", err
	}
	return fmt.Sprint(m.ID), nil
}

// A memory she keeps in the middle of a reply is said in the newest message the
// reply answers, even when a burst of them came before it. One she keeps later
// takes the place of it when she says so.
func TestAMemoryKeptInAReplyIsSaidInTheMessageItAnswers(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: answering(
		round{calls: []api.ToolCall{{ID: "call_1", Name: "remember",
			Arguments: `{"memory":"Caio's sister Ana lives in Lisbon."}`}}},
		round{text: "noted"},
		round{calls: []api.ToolCall{{ID: "call_2", Name: "remember",
			Arguments: `{"memory":"Caio's sister Ana lives in Porto.","replaces":[1]}`}}},
		round{text: "noted again"},
	)}
	r := openReplyOffering(t, f, setup(f), config.DefaultEngine(), keeper{})
	ctx := context.Background()

	post(t, r.Engine, "my sister is Ana")
	r.clock.Advance(time.Second)
	r.say(t, "she lives in Lisbon")

	history, err := r.History(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 3 {
		t.Fatalf("history = %+v, want the two messages and the reply", history)
	}
	newest := history[1]
	memories, err := r.store.Memories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(memories) != 1 {
		t.Fatalf("memories = %+v, want the one she kept", memories)
	}
	m := memories[0]
	if m.Content != "Caio's sister Ana lives in Lisbon." || m.Source != newest.ID || !m.SaidAt.Equal(newest.CreatedAt) {
		t.Errorf("the memory is %+v, want it said in message %d at %v", m, newest.ID, newest.CreatedAt)
	}

	r.say(t, "actually she moved to Porto")
	memories, err = r.store.Memories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(memories) != 1 || memories[0].Content != "Caio's sister Ana lives in Porto." {
		t.Errorf("memories = %+v, want the one that took the place of the first", memories)
	}
}
