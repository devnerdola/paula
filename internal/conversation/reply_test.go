package conversation

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/persona"
	"nerdola.dev/x/paula/internal/runners"
	"nerdola.dev/x/paula/internal/runners/api"
	"nerdola.dev/x/paula/internal/store"
	toolsapi "nerdola.dev/x/paula/internal/tools/api"
)

// fakeRunner answers with whatever a test hands it, and remembers what it was
// asked.
type fakeRunner struct {
	mu       sync.Mutex
	model    api.Model
	requests []api.ChatRequest
	chat     func(ctx context.Context, req api.ChatRequest, fn func(api.Chunk) error) (*api.Result, error)
	// models answers the catalogue when a test wants one that cannot be read.
	models func() ([]api.Model, error)
}

func (f *fakeRunner) Name() string                 { return "fake" }
func (f *fakeRunner) Kind() string                 { return "fake" }
func (f *fakeRunner) URL() string                  { return "fake:///v1" }
func (f *fakeRunner) Health(context.Context) error { return nil }
func (f *fakeRunner) Models(context.Context) ([]api.Model, error) {
	if f.models != nil {
		return f.models()
	}
	return []api.Model{f.model}, nil
}

func (f *fakeRunner) Model(_ context.Context, id string) (*api.Model, error) {
	if f.models != nil {
		if _, err := f.models(); err != nil {
			return nil, err
		}
	}
	if id != f.model.ID {
		return nil, fmt.Errorf("fake serves no model called %q", id)
	}
	m := f.model
	return &m, nil
}

func (f *fakeRunner) Check(context.Context, api.Checked) []error {
	return nil
}

func (f *fakeRunner) Settings() api.Settings { return api.Settings{} }

func (f *fakeRunner) Chat(ctx context.Context, req api.ChatRequest, fn func(api.Chunk) error) (*api.Result, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()

	// A runner reports every request to the recorder the request carries.
	rec := api.Recording(req.Recorder)
	body, err := json.Marshal(map[string]any{"model": req.Model})
	if err != nil {
		return nil, err
	}
	record := &api.Record{
		Runner: f.Name(), Model: req.Model,
		Method: "POST", URL: f.URL(), RequestBody: body, StartedAt: time.Now(),
	}
	if err := rec.StartRequest(ctx, record); err != nil {
		return nil, err
	}
	res, err := f.chat(ctx, req, fn)
	record.EndedAt = time.Now()
	record.Status = 200
	record.ResponseBody = []byte("data: [DONE]\n\n")
	if err != nil {
		record.Error = err.Error()
	}
	if rerr := rec.EndRequest(ctx, record); rerr != nil && err == nil {
		return nil, rerr
	}
	return res, err
}

func (f *fakeRunner) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeRunner) asked() api.ChatRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return api.ChatRequest{}
	}
	return f.requests[len(f.requests)-1]
}

// replied is the last request of a reply, which is not always the last
// request: a fold works beside the conversation and sends its own.
func (f *fakeRunner) replied() api.ChatRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, v := range slices.Backward(f.requests) {
		if purpose(v) == store.PurposeReply {
			return v
		}
	}
	return api.ChatRequest{}
}

// sentFor is every request of one purpose, in the order they went out.
func (f *fakeRunner) sentFor(want string) []api.ChatRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []api.ChatRequest
	for _, req := range f.requests {
		if purpose(req) == want {
			out = append(out, req)
		}
	}
	return out
}

// says answers with one chunk of text.
func says(text string) func(context.Context, api.ChatRequest, func(api.Chunk) error) (*api.Result, error) {
	return func(_ context.Context, _ api.ChatRequest, fn func(api.Chunk) error) (*api.Result, error) {
		for _, word := range strings.SplitAfter(text, " ") {
			if word == "" {
				continue
			}
			if err := fn(api.Chunk{Kind: api.ChunkText, Text: word}); err != nil {
				return nil, err
			}
		}
		return &api.Result{FinishReason: "stop", Reasoning: "thinking"}, nil
	}
}

func setup(f *fakeRunner) *runners.Setup {
	m := &runners.Configured{Name: "chat", ID: f.model.ID, Runner: f}
	return &runners.Setup{
		Runners:  []runners.Runner{f},
		Models:   []*runners.Configured{m},
		Defaults: map[config.Role]*runners.Configured{config.RoleChat: m},
	}
}

func fullCard() *persona.Card {
	return &persona.Card{
		ID:          "paula",
		Name:        "Paula",
		Language:    "English",
		Personality: persona.List{"She teases a little."},
		User:        persona.User{Name: "Caio"},
	}
}

type replyEngine struct {
	*Engine
	runner *fakeRunner
	clock  *fakeClock
	store  *store.Store
	log    *logged
}

// logged is what the engine wrote to its log, for a test that holds it to
// something it says. The engine writes from the goroutine of a reply and the
// test reads from its own, so both go through the lock.
type logged struct {
	mu      sync.Mutex
	written strings.Builder
}

func (l *logged) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.written.Write(p)
}

func (l *logged) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.written.String()
}

func openReply(t *testing.T, f *fakeRunner) *replyEngine {
	t.Helper()
	return openReplyWith(t, f, setup(f))
}

// openReplyWith opens an engine over a setup of the test's own, for a test
// about which models are there.
func openReplyWith(t *testing.T, f *fakeRunner, set *runners.Setup) *replyEngine {
	t.Helper()
	return openReplyOffering(t, f, set, config.DefaultEngine())
}

// openReplyOffering opens an engine that offers tools, under engine settings of
// the test's own.
func openReplyOffering(t *testing.T, f *fakeRunner, set *runners.Setup, cfg config.Engine, tools ...toolsapi.Tool) *replyEngine {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	c := newClock()
	written := &logged{}
	e, err := Open(context.Background(), Options{
		Store:   st,
		Runners: set,
		Persona: fullCard(),
		Engine:  cfg,
		Clock:   c,
		Log:     slog.New(slog.NewTextHandler(written, nil)),
		Tools:   tools,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return &replyEngine{Engine: e, runner: f, clock: c, store: st, log: written}
}

func (r *replyEngine) say(t *testing.T, text string) {
	t.Helper()
	post(t, r.Engine, text)
	r.clock.Advance(config.DefaultEngine().Debounce.Duration())
	if _, err := r.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func chatModel() api.Model {
	return api.Model{ID: "some/model", Context: 100000, Chat: true, Tools: true}
}

func TestAReplyIsStoredAndPublished(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: says("hi there you")}
	r := openReply(t, f)
	r.say(t, "hey")

	history, _, err := r.History(context.Background(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 {
		t.Fatalf("history = %+v", history)
	}
	reply := history[1]
	if reply.Role != store.RoleAssistant || reply.Text() != "hi there you" {
		t.Errorf("reply = %+v", reply)
	}
	if reply.Reasoning != "thinking" {
		t.Errorf("reply = %+v", reply)
	}
	if reply.ReplyTo != history[0].ID || reply.EntryID == 0 {
		t.Errorf("reply = %+v", reply)
	}
	if reply.Interrupted {
		t.Error("the reply reads as interrupted")
	}

	// The text is published as it arrives, each event carrying all of it.
	var texts []string
	var done Event
	for _, ev := range published(r.Engine) {
		switch ev.Kind {
		case ReplyText:
			texts = append(texts, ev.Text)
		case ReplyDone:
			done = ev
		}
	}
	if len(texts) == 0 || texts[len(texts)-1] != "hi there you" {
		t.Errorf("texts = %q", texts)
	}
	if done.Message == nil || done.Message.ID != reply.ID {
		t.Errorf("the reply that was done = %+v", done.Message)
	}
}

// plain is what every model is told right after the card.
const plain = "Everything you write is a message you send to Caio, except an answer of [nothing] alone, which " +
	"sends nothing. Your messages reach Caio as plain text, the way a text message does: write no markdown or " +
	"HTML, and write an address as it is rather than as a link. A blank line between two paragraphs is where " +
	"one text ends and the next begins. When you answer with no text at all, a message from the app, not from " +
	"Caio, says so, and you answer again."

func TestThePromptOfAReply(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: says("hello")}
	r := openReply(t, f)
	r.say(t, "hey")
	r.say(t, "you there?")

	req := f.asked()
	if req.Model != "some/model" {
		t.Errorf("model = %q", req.Model)
	}
	var shape []string
	for _, m := range req.Messages {
		shape = append(shape, m.Role+": "+text(m))
	}
	if len(shape) == 0 {
		t.Fatal("the prompt is empty")
	}
	// The card opens the prompt, and internal/persona is held to its wording.
	if !strings.HasPrefix(shape[0], "system: You are Paula, texting with Caio.") {
		t.Errorf("the prompt opens with %q, want the card", shape[0])
	}
	// Every model is told after the card how her messages arrive, whatever the
	// card says. A model no family extension serves reads the times as system
	// messages, which need no word on whose they are.
	if !strings.HasSuffix(shape[0], "\n\n"+plain) {
		t.Errorf("the card is followed by %q, want %q and nothing on whose the times are", shape[0], plain)
	}
	// Then every message in order, with the time before each of mine: when an
	// older one was sent, and what time it is now before the one she answers.
	want := []string{
		"system: The next message was sent at Wednesday, 16 September 2026, 22:22 UTC+02:00.",
		"user: hey",
		"assistant: hello",
		"system: It is now Wednesday, 16 September 2026, 22:22 UTC+02:00.",
		"user: you there?",
	}
	if got := shape[1:]; !slices.Equal(got, want) {
		t.Errorf("the prompt after the card is\n%s\n\nwant\n%s",
			strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// notesAsUser is a family extension that has her notes told as user messages.
type notesAsUser struct{}

func (notesAsUser) Notes() api.Notes                     { return api.Notes{Role: api.RoleUser} }
func (notesAsUser) PastThought() bool                    { return false }
func (notesAsUser) Body(map[string]any, api.ChatRequest) {}

// A model whose family reads her notes as user messages is sent the times in
// that role, and is told right after the card that they come from the app
// rather than from Caio.
func TestHerNotesAreToldInTheRoleTheModelsFamilyGivesThem(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: says("hello")}
	set := setup(f)
	set.Models[0].Settings.Extension = notesAsUser{}
	r := openReplyWith(t, f, set)
	r.say(t, "hey")
	r.say(t, "you there?")

	var shape []string
	for _, m := range f.asked().Messages {
		shape = append(shape, m.Role+": "+text(m))
	}
	if len(shape) != 6 {
		t.Fatalf("the prompt is\n%s\n\nwant the card and five messages", strings.Join(shape, "\n"))
	}
	told := "Some messages come from the app you and Caio text through, not from Caio: " +
		"the one before each of Caio's messages saying when it was sent, " +
		"and the one saying what time it is now. They are for you to know, never to answer."
	if !strings.HasPrefix(shape[0], "system: You are Paula, texting with Caio.") || !strings.HasSuffix(shape[0], "\n\n"+told) {
		t.Errorf("the system message is %q, want the card and then %q", shape[0], told)
	}
	want := []string{
		"user: The next message was sent at Wednesday, 16 September 2026, 22:22 UTC+02:00.",
		"user: hey",
		"assistant: hello",
		"user: It is now Wednesday, 16 September 2026, 22:22 UTC+02:00.",
		"user: you there?",
	}
	if got := shape[1:]; !slices.Equal(got, want) {
		t.Errorf("the prompt after the card is\n%s\n\nwant\n%s",
			strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// notesAsSent is a family extension that has her notes told as user messages,
// and the time before the message she answers as when it was sent.
type notesAsSent struct{}

func (notesAsSent) Notes() api.Notes {
	return api.Notes{Role: api.RoleUser, LastAsSent: true}
}
func (notesAsSent) PastThought() bool                    { return false }
func (notesAsSent) Body(map[string]any, api.ChatRequest) {}

// A host that reads only the whole of an earlier prompt needs every prompt to
// start with the one before it. A family that tells the time before the
// message she answers as when it was sent tells it the way the next prompt
// does, so the next prompt only adds to it. The sentence after the card names
// no time it is now.
func TestAPromptThatTellsTheLastTimeAsSentStartsTheNext(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: says("hello")}
	set := setup(f)
	set.Models[0].Settings.Extension = notesAsSent{}
	r := openReplyWith(t, f, set)
	r.say(t, "hey")
	r.say(t, "you there?")
	r.say(t, "hello??")

	requests := f.all()
	if len(requests) != 3 {
		t.Fatalf("%d requests were sent, want three", len(requests))
	}
	for i := range len(requests) - 1 {
		was, next := requests[i].Messages, requests[i+1].Messages
		if len(next) <= len(was) || !reflect.DeepEqual(next[:len(was)], was) {
			t.Errorf("prompt %d does not start with prompt %d as it was sent", i+2, i+1)
		}
	}
	last := requests[2].Messages
	if got := last[len(last)-2].Role + ": " + text(last[len(last)-2]); got !=
		"user: The next message was sent at Wednesday, 16 September 2026, 22:22 UTC+02:00." {
		t.Errorf("the message she answers is told after %q, want when it was sent", got)
	}
	told := "Some messages come from the app you and Caio text through, not from Caio: " +
		"the one before each of Caio's messages saying when it was sent. " +
		"They are for you to know, never to answer."
	if card := text(last[0]); !strings.HasSuffix(card, "\n\n"+told) {
		t.Errorf("the system message is %q, want the card and then %q", card, told)
	}
}

// What time it is now is told before the message she is answering. The last of
// what a prompt carries is not always that message: a reply written while the
// next one arrived carries the higher id of the two, and one whose own message
// the summary covers keeps the place its id gives it, so the prompt would end
// on her own earlier reply and never say what time it is.
func TestTheTimeIsToldBeforeTheMessageSheIsAnswering(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: says("hm")}
	r := openReply(t, f)
	ctx := context.Background()

	// A reply that answers a message the summary covers, and a message after
	// it that is the one being answered now.
	covered := add(t, r, store.RoleUser, "the one it answers")
	if err := r.store.Fold(ctx, &store.Summary{UptoMessageID: covered,
		Content: "they talked"}); err != nil {
		t.Fatal(err)
	}
	answering := add(t, r, store.RoleUser, "and this one")
	reply := &store.Message{Role: store.RoleAssistant, Channel: "repl",
		ReplyTo: covered, CreatedAt: r.clock.Now(),
		Parts: []store.Part{{Type: store.PartText, Text: "what she said before"}}}
	if err := r.store.AddMessage(ctx, reply); err != nil {
		t.Fatal(err)
	}

	m, err := r.roleModel(ctx, config.RoleChat)
	if err != nil {
		t.Fatal(err)
	}
	a := &attempt{entry: &store.Entry{Channel: "repl", UptoMessageID: answering}}
	prompt, _, err := r.prompt(ctx, a, m)
	if err != nil {
		t.Fatal(err)
	}

	var said []string
	for _, msg := range prompt {
		said = append(said, msg.Role+": "+text(msg))
	}
	var now int
	for i, s := range said {
		if strings.HasPrefix(s, "system: It is now ") {
			now = i
		}
	}
	if now == 0 {
		t.Fatalf("the prompt never says what time it is:\n%s", strings.Join(said, "\n"))
	}
	if said[now+1] != "user: and this one" {
		t.Errorf("the time stands before %q, want the message she is answering", said[now+1])
	}
}

func text(m api.Message) string {
	var b strings.Builder
	for _, p := range m.Parts {
		if p.Type == api.PartText {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// mine is the first message of a prompt that I sent, since a prompt carries
// the card and the time before it as messages of their own.
func mine(req api.ChatRequest) api.Message {
	for _, m := range req.Messages {
		if m.Role == api.RoleUser {
			return m
		}
	}
	return api.Message{}
}

func TestAReplyGoesAfterTheMessagesItAnswers(t *testing.T) {
	hold := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	f := &fakeRunner{model: chatModel()}
	f.chat = func(ctx context.Context, req api.ChatRequest, fn func(api.Chunk) error) (*api.Result, error) {
		first := false
		once.Do(func() { first = true })
		if first {
			fn(api.Chunk{Kind: api.ChunkText, Text: "one moment"})
			close(started)
			<-hold
		} else {
			fn(api.Chunk{Kind: api.ChunkText, Text: "and now"})
		}
		return &api.Result{FinishReason: "stop"}, nil
	}

	r := openReply(t, f)
	post(t, r.Engine, "first")
	r.clock.Advance(config.DefaultEngine().Debounce.Duration())
	<-started

	// This arrives while the first reply is still being written, so it is
	// stored between the message and the reply that answers it.
	post(t, r.Engine, "second")
	close(hold)
	waitFor(t, "the first reply to end", func() bool { return seen(r.Engine, ReplyDone) })
	r.clock.Advance(config.DefaultEngine().Debounce.Duration())
	waitFor(t, "the second reply", func() bool { return f.count() == 2 })
	if _, err := r.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}

	// The card opens the prompt and a time stands before each message of mine;
	// the conversation is what the two of us said.
	var roles []string
	var texts []string
	for _, m := range f.asked().Messages {
		if m.Role == api.RoleSystem {
			continue
		}
		roles = append(roles, m.Role)
		texts = append(texts, text(m))
	}
	if strings.Join(roles, ",") != "user,assistant,user" {
		t.Errorf("roles = %v", roles)
	}
	if !strings.HasSuffix(texts[0], "first") || texts[1] != "one moment" || !strings.HasSuffix(texts[2], "second") {
		t.Errorf("messages = %q, want the reply after the message it answers", texts)
	}
}

// A picture sent with nothing said about it is the picture. A part with no
// text in it beside it is a message an OpenAI-compatible host refuses, which
// would fail the reply for as long as the picture is sent as one.
func TestAPictureSentWithNothingSaidAboutIt(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: says("nice one")}
	f.model.Vision = true
	r := openReplyWith(t, f, setup(f))

	sendPhoto(t, r, "", photo(t))

	mine := mine(f.asked())
	if len(mine.Parts) != 1 || mine.Parts[0].Type != api.PartImage {
		t.Errorf("the message is %+v, want the picture and nothing else", mine.Parts)
	}
	for _, p := range mine.Parts {
		if p.Type == api.PartText && p.Text == "" {
			t.Error("the message carries a text part with nothing in it")
		}
	}
}

// A reply with no text fails, saying how the model finished. One the model
// ended itself is asked for once more first; one cut off at the most it
// writes, or one that ended without saying how, is not.
func TestAReplyWithNoText(t *testing.T) {
	for _, c := range []struct {
		finish, said string
		asked        int
	}{{"stop", "stop", 2}, {"length", "length", 1}, {"", "none", 1}} {
		t.Run(c.said, func(t *testing.T) {
			f := &fakeRunner{model: chatModel()}
			f.chat = func(context.Context, api.ChatRequest, func(api.Chunk) error) (*api.Result, error) {
				return &api.Result{FinishReason: c.finish}, nil
			}
			r := openReply(t, f)
			r.say(t, "hey")

			var failed Event
			for _, ev := range published(r.Engine) {
				if ev.Kind == ReplyFailed {
					failed = ev
				}
			}
			if want := "the model returned no text (finish reason " + c.said + ")"; failed.Text != want {
				t.Errorf("failure = %q, want %q", failed.Text, want)
			}
			if n := len(f.all()); n != c.asked {
				t.Errorf("the model was asked %d times, want %d", n, c.asked)
			}
			if history, _, _ := r.History(context.Background(), 0, 10); len(history) != 1 {
				t.Errorf("history = %+v, want nothing stored", history)
			}
		})
	}
}

// An answer with nothing in it is asked for once more, saying what sends
// nothing, and what she writes then is her reply. The note is told in the role
// the model's family gives her notes, which the system message names as the
// app's.
func TestAnAnswerWithNothingInItIsAskedForOnceMore(t *testing.T) {
	for _, c := range []struct {
		name      string
		extension api.Extension
		role      string
	}{{"no family", nil, api.RoleSystem}, {"notes as user", notesAsUser{}, api.RoleUser}} {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeRunner{model: chatModel(), chat: answering(round{}, round{text: "sorry, I was miles away"})}
			set := setup(f)
			set.Models[0].Settings.Extension = c.extension
			r := openReplyWith(t, f, set)
			r.say(t, "hey")

			requests := f.all()
			if len(requests) != 2 {
				t.Fatalf("the model was asked %d times, want twice", len(requests))
			}
			first, again := requests[0].Messages, requests[1].Messages
			if len(again) != len(first)+1 || !reflect.DeepEqual(again[:len(first)], first) {
				t.Fatalf("asked again with %+v, want the first prompt and a note after it", again)
			}
			note := again[len(first)]
			if want := "You wrote nothing, so nothing was sent. To send nothing, answer with [nothing] alone; " +
				"otherwise write your message."; note.Role != c.role || text(note) != want {
				t.Errorf("the note is %s: %q, want %s: %q", note.Role, text(note), c.role, want)
			}
			if named := "When you answer with no text at all, a message from the app, not from Caio, says so"; !strings.Contains(text(first[0]), named) {
				t.Errorf("the system message is %q, want it to name the note as the app's", text(first[0]))
			}
			if reply, _ := stored(t, r); reply.Text() != "sorry, I was miles away" {
				t.Errorf("the reply is %q, want what she wrote when asked again", reply.Text())
			}
		})
	}
}

// An answer of what sends nothing ends the turn answered with nothing sent, and
// none of it is shown as it arrives, whatever its case.
func TestAnAnswerOfNothingSendsNothing(t *testing.T) {
	f := &fakeRunner{model: chatModel()}
	f.chat = func(_ context.Context, _ api.ChatRequest, fn func(api.Chunk) error) (*api.Result, error) {
		for _, piece := range []string{"[No", "thing", "]\n"} {
			if err := fn(api.Chunk{Kind: api.ChunkText, Text: piece}); err != nil {
				return nil, err
			}
		}
		return &api.Result{FinishReason: "stop"}, nil
	}
	r := openReply(t, f)
	ctx := context.Background()
	r.say(t, "ok, good night")

	if n := len(f.all()); n != 1 {
		t.Errorf("the model was asked %d times, want once", n)
	}
	entries, err := r.store.Entries(ctx, 1)
	if err != nil || len(entries) != 1 || entries[0].Status != store.StatusDone {
		t.Fatalf("entries = %+v, %v, want the turn ended done", entries, err)
	}
	if reply, err := r.store.ReplyOfEntry(ctx, entries[0].ID); err == nil {
		t.Errorf("the turn stored the reply %+v, want none", reply)
	}
	if answered, _ := r.store.AnsweredUpto(ctx); answered != entries[0].UptoMessageID || answered == 0 {
		t.Errorf("answered up to %d, want the message she sent nothing to, %d", answered, entries[0].UptoMessageID)
	}
	var done []Event
	for _, ev := range published(r.Engine) {
		switch ev.Kind {
		case ReplyText:
			t.Errorf("%q was shown", ev.Text)
		case ReplyFailed:
			t.Errorf("the turn failed: %s", ev.Text)
		case ReplyDone:
			done = append(done, ev)
		}
	}
	if len(done) != 1 || done[0].Message != nil {
		t.Errorf("reply done events = %+v, want one carrying no message", done)
	}
}

// What begins as the answer that sends nothing does is shown once it is
// something else, and so is what ends before it is.
func TestWhatOnlyBeginsLikeNothingIsShown(t *testing.T) {
	for _, pieces := range [][]string{{"[nothing]", " to report, I'm fine"}, {"[no"}} {
		want := strings.Join(pieces, "")
		t.Run(want, func(t *testing.T) {
			f := &fakeRunner{model: chatModel()}
			f.chat = func(_ context.Context, _ api.ChatRequest, fn func(api.Chunk) error) (*api.Result, error) {
				for _, piece := range pieces {
					if err := fn(api.Chunk{Kind: api.ChunkText, Text: piece}); err != nil {
						return nil, err
					}
				}
				return &api.Result{FinishReason: "stop"}, nil
			}
			r := openReply(t, f)
			r.say(t, "how was it?")

			var shown string
			for _, ev := range published(r.Engine) {
				if ev.Kind == ReplyText {
					shown = ev.Text
				}
			}
			if shown != want {
				t.Errorf("shown %q, want %q", shown, want)
			}
			if reply, _ := stored(t, r); reply.Text() != want {
				t.Errorf("the reply is %q, want %q", reply.Text(), want)
			}
		})
	}
}

func TestTheAPIErrorIsReportedAsItIs(t *testing.T) {
	f := &fakeRunner{model: chatModel()}
	f.chat = func(context.Context, api.ChatRequest, func(api.Chunk) error) (*api.Result, error) {
		return nil, &api.APIError{Status: 429, Message: "slow down"}
	}
	r := openReply(t, f)
	r.say(t, "hey")

	var failed Event
	for _, ev := range published(r.Engine) {
		if ev.Kind == ReplyFailed {
			failed = ev
		}
	}
	if failed.Text != "429: slow down" {
		t.Errorf("failure = %q", failed.Text)
	}
}

func TestAStoppedReplyKeepsWhatItHadWritten(t *testing.T) {
	started := make(chan struct{})
	f := &fakeRunner{model: chatModel()}
	f.chat = func(ctx context.Context, _ api.ChatRequest, fn func(api.Chunk) error) (*api.Result, error) {
		fn(api.Chunk{Kind: api.ChunkText, Text: "I was saying"})
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}

	r := openReply(t, f)
	post(t, r.Engine, "hey")
	r.clock.Advance(config.DefaultEngine().Debounce.Duration())
	<-started

	if stopped, err := r.Stop(context.Background()); err != nil || !stopped {
		t.Fatalf("Stop = %v, %v", stopped, err)
	}
	if _, err := r.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}

	history, _, err := r.History(context.Background(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 {
		t.Fatalf("history = %+v, want what had been written kept", history)
	}
	if history[1].Text() != "I was saying" || !history[1].Interrupted {
		t.Errorf("reply = %+v", history[1])
	}
}

func TestARestartedReplyKeepsNothing(t *testing.T) {
	started := make(chan struct{})
	var once sync.Once
	f := &fakeRunner{model: chatModel()}
	f.chat = func(ctx context.Context, _ api.ChatRequest, fn func(api.Chunk) error) (*api.Result, error) {
		first := false
		once.Do(func() { first = true })
		if !first {
			fn(api.Chunk{Kind: api.ChunkText, Text: "both then"})
			return &api.Result{FinishReason: "stop"}, nil
		}
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}

	r := openReply(t, f)
	post(t, r.Engine, "one")
	r.clock.Advance(config.DefaultEngine().Debounce.Duration())
	<-started
	post(t, r.Engine, "two")

	waitFor(t, "the restart", func() bool { return seen(r.Engine, ReplyRestarted) })
	r.clock.Advance(config.DefaultEngine().Debounce.Duration())
	if _, err := r.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}

	history, _, err := r.History(context.Background(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 3 || history[2].Text() != "both then" {
		t.Errorf("history = %+v, want the restarted reply kept nothing", history)
	}
}

// What may yet be the answer that sends nothing is not shown, so it has not
// started the reply: a message that arrives while it streams restarts the
// reply, as it does one that has written nothing.
func TestAReplyHoldingBackWhatMaySendNothingIsRestarted(t *testing.T) {
	started := make(chan struct{})
	var once sync.Once
	f := &fakeRunner{model: chatModel()}
	f.chat = func(ctx context.Context, _ api.ChatRequest, fn func(api.Chunk) error) (*api.Result, error) {
		first := false
		once.Do(func() { first = true })
		if !first {
			fn(api.Chunk{Kind: api.ChunkText, Text: "both then"})
			return &api.Result{FinishReason: "stop"}, nil
		}
		fn(api.Chunk{Kind: api.ChunkText, Text: "[noth"})
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}

	r := openReply(t, f)
	post(t, r.Engine, "good night")
	r.clock.Advance(config.DefaultEngine().Debounce.Duration())
	<-started
	post(t, r.Engine, "wait, one more thing")

	waitFor(t, "the restart", func() bool { return seen(r.Engine, ReplyRestarted) })
	r.clock.Advance(config.DefaultEngine().Debounce.Duration())
	if _, err := r.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	history, _, err := r.History(context.Background(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 3 || history[2].Text() != "both then" {
		t.Errorf("history = %+v, want one reply to both messages", history)
	}
}

// A stop that lands while what may yet be the answer that sends nothing is
// held back keeps none of it: it was never shown, and it may have been that
// answer.
func TestAStopWhileHoldingBackWhatMaySendNothingKeepsNoneOfIt(t *testing.T) {
	started := make(chan struct{})
	f := &fakeRunner{model: chatModel()}
	f.chat = func(ctx context.Context, _ api.ChatRequest, fn func(api.Chunk) error) (*api.Result, error) {
		fn(api.Chunk{Kind: api.ChunkText, Text: "[noth"})
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}

	r := openReply(t, f)
	post(t, r.Engine, "good night")
	r.clock.Advance(config.DefaultEngine().Debounce.Duration())
	<-started
	if stopped, err := r.Stop(context.Background()); err != nil || !stopped {
		t.Fatalf("Stop = %v, %v", stopped, err)
	}
	if _, err := r.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}

	if history, _, _ := r.History(context.Background(), 0, 10); len(history) != 1 {
		t.Errorf("history = %+v, want nothing of the reply kept", history)
	}
	for _, ev := range published(r.Engine) {
		if ev.Kind == ReplyText {
			t.Errorf("%q was shown", ev.Text)
		}
	}
}

func TestTheRequestIsRecordedUnderItsEntry(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: says("hello")}
	r := openReply(t, f)
	r.say(t, "hey")

	ctx := context.Background()
	entries, err := r.store.Entries(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %+v", entries)
	}
	if entries[0].Status != store.StatusDone {
		t.Errorf("entry = %+v", entries[0])
	}

	requests, err := r.store.Requests(ctx, entries[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 {
		t.Fatalf("requests = %+v, want the one the reply was written by", requests)
	}
	req := requests[0]
	if req.Purpose != store.PurposeReply || req.Runner != f.Name() || req.Model != f.model.ID {
		t.Errorf("request = %+v, want the reply the fake was asked for", req)
	}
	if req.Method != "POST" || req.URL != f.URL() || req.Status != 200 {
		t.Errorf("request = %s %s answered %d", req.Method, req.URL, req.Status)
	}
	if !strings.Contains(string(req.RequestBody), f.model.ID) {
		t.Errorf("request body = %s, want what was sent", req.RequestBody)
	}
	if len(req.ResponseBody) == 0 || req.StartedAt.IsZero() || req.EndedAt.IsZero() {
		t.Errorf("request = %+v, want what came back and when", req)
	}
}

func TestAModelThatCannotBeUsed(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: says("hello")}
	r := openReply(t, f)
	if err := r.store.Set(context.Background(), store.KeyModel("chat"), "gone"); err != nil {
		t.Fatal(err)
	}
	// A saved model the file no longer names falls back to the default.
	r.say(t, "hey")
	if f.count() != 1 {
		t.Errorf("requests = %d, want the default used", f.count())
	}
}

func TestNoRunnerAtAll(t *testing.T) {
	st := newStore(t)
	c := newClock()
	e, err := Open(context.Background(), Options{
		Store: st, Persona: fullCard(), Engine: config.DefaultEngine(), Clock: c,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })

	post(t, e, "hey")
	c.Advance(config.DefaultEngine().Debounce.Duration())
	if _, err := e.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	var failed Event
	for _, ev := range published(e) {
		if ev.Kind == ReplyFailed {
			failed = ev
		}
	}
	if !strings.Contains(failed.Text, "no runner is set up") {
		t.Errorf("failure = %q", failed.Text)
	}
}

// A time is written to the minute, in the zone the run keeps.
func TestTheTimeSheIsTold(t *testing.T) {
	at := time.Date(2026, 9, 15, 20, 22, 0, 0, time.UTC)
	if got, want := timeText(berlin, at), "Tuesday, 15 September 2026, 22:22 UTC+02:00"; got != want {
		t.Errorf("timeText = %q, want %q", got, want)
	}
	if got, want := timeText(time.UTC, at), "Tuesday, 15 September 2026, 20:22 UTC+00:00"; got != want {
		t.Errorf("timeText = %q, want %q", got, want)
	}
}

// photo is a small image, as bytes from a frontend.
func photo(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "photo.png"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// sendPhoto sends an image and returns the sha256 it was kept under.
func sendPhoto(t *testing.T, r *replyEngine, text string, data []byte) string {
	t.Helper()
	err := r.Post(context.Background(), NewMessage{
		Channel: "repl", Text: text, Images: [][]byte{data},
	})
	if err != nil {
		t.Fatal(err)
	}
	r.clock.Advance(config.DefaultEngine().Debounce.Duration())
	if _, err := r.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	history, _, err := r.History(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(history) - 1; i >= 0; i-- {
		if img := history[i].Images(); len(img) > 0 {
			return img[0].SHA256
		}
	}
	t.Fatal("no image was kept")
	return ""
}

func images(m api.Message) []api.Part {
	var out []api.Part
	for _, p := range m.Parts {
		if p.Type == api.PartImage {
			out = append(out, p)
		}
	}
	return out
}

func TestAnImageGoesToAModelThatSeesImages(t *testing.T) {
	model := chatModel()
	model.Vision = true
	f := &fakeRunner{model: model, chat: says("nice")}
	r := openReply(t, f)
	sendPhoto(t, r, "look at this", photo(t))

	mine := mine(f.asked())
	if got := images(mine); len(got) != 1 {
		t.Fatalf("images = %d, want the photo sent", len(got))
	}
	if got := images(mine)[0]; got.MIME != "image/jpeg" || len(got.Data) == 0 {
		t.Errorf("image = %q, %d bytes", got.MIME, len(got.Data))
	}
	if strings.Contains(text(mine), "[photo") {
		t.Errorf("the photo was described as well as sent: %q", text(mine))
	}
}

func TestAnImageIsDescribedToAModelThatDoesNotSeeImages(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: says("nice")}
	r := openReply(t, f)
	sendPhoto(t, r, "look at this", photo(t))

	mine := mine(f.asked())
	if got := images(mine); len(got) != 0 {
		t.Errorf("images = %d, want none sent", len(got))
	}
	if !strings.HasSuffix(text(mine), "look at this\n[photo]") {
		t.Errorf("message = %q", text(mine))
	}
}

func TestAnImageIsDescribedByItsCaption(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: says("nice")}
	r := openReply(t, f)
	sha := sendPhoto(t, r, "look at this", photo(t))
	if err := r.store.SetCaption(context.Background(), sha, "a red square"); err != nil {
		t.Fatal(err)
	}
	r.say(t, "and this one?")

	if got := text(mine(f.asked())); !strings.HasSuffix(got, "[photo: a red square]") {
		t.Errorf("message = %q", got)
	}
}

// A model that sees images is sent every picture its prompt carries as the
// picture, however many there are and however old: none is ever replaced by
// what it showed.
func TestAModelThatSeesIsSentEveryPicture(t *testing.T) {
	model := chatModel()
	model.Vision = true
	f := &fakeRunner{model: model, chat: says("nice")}
	r := openReply(t, f)

	sendPhoto(t, r, "one", photo(t))
	sendPhoto(t, r, "two", photo(t))
	sendPhoto(t, r, "three", photo(t))
	r.say(t, "and now?")

	var sent, described int
	for _, m := range f.asked().Messages[1:] {
		if len(images(m)) > 0 {
			sent++
		}
		if strings.Contains(text(m), "[photo") {
			described++
		}
	}
	if sent != 3 || described != 0 {
		t.Errorf("%d photos sent and %d described, want all 3 sent", sent, described)
	}
}

func TestTheOldestBodiesAreDropped(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: says("hello")}
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := config.DefaultEngine()
	cfg.LogKeep = 1
	c := newClock()
	e, err := Open(context.Background(), Options{
		Store: st, Runners: setup(f), Persona: fullCard(), Engine: cfg, Clock: c,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })

	ctx := context.Background()
	for _, text := range []string{"one", "two", "three"} {
		post(t, e, text)
		c.Advance(cfg.Debounce.Duration())
		if _, err := e.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}

	entries, err := st.Entries(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 2 {
		t.Fatalf("entries = %d, want more than engine.log_keep", len(entries))
	}

	newest, err := st.Requests(ctx, entries[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(newest) == 0 || newest[0].Pruned {
		t.Errorf("the newest entry lost its bodies")
	}
	oldest, err := st.Requests(ctx, entries[len(entries)-1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(oldest) == 0 {
		t.Fatal("the oldest entry made no request")
	}
	if !oldest[0].Pruned {
		t.Errorf("the oldest entry kept its bodies with engine.log_keep at %d", cfg.LogKeep)
	}
}

func TestARestartedReplyThatFinishedKeepsNothing(t *testing.T) {
	// The reply finishes exactly as the restart lands, which it may.
	letGo := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	f := &fakeRunner{model: chatModel()}
	f.chat = func(ctx context.Context, _ api.ChatRequest, fn func(api.Chunk) error) (*api.Result, error) {
		first := false
		once.Do(func() { first = true })
		if !first {
			fn(api.Chunk{Kind: api.ChunkText, Text: "both then"})
			return &api.Result{FinishReason: "stop"}, nil
		}
		close(started)
		<-letGo
		// It answers in full, ignoring that the context was cancelled.
		fn(api.Chunk{Kind: api.ChunkText, Text: "too late"})
		return &api.Result{FinishReason: "stop"}, nil
	}

	r := openReply(t, f)
	post(t, r.Engine, "one")
	r.clock.Advance(config.DefaultEngine().Debounce.Duration())
	<-started
	post(t, r.Engine, "two")
	close(letGo)

	waitFor(t, "the restart", func() bool { return seen(r.Engine, ReplyRestarted) })
	r.clock.Advance(config.DefaultEngine().Debounce.Duration())
	if _, err := r.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}

	history, _, err := r.History(context.Background(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range history {
		if m.Text() == "too late" {
			t.Fatalf("a restarted reply was kept: %+v", history)
		}
	}
	if len(history) != 3 || history[2].Text() != "both then" {
		t.Errorf("history = %+v", history)
	}
}

func TestWaitDoesNotHangOnceTheConversationIsClosed(t *testing.T) {
	started := make(chan struct{})
	f := &fakeRunner{model: chatModel()}
	f.chat = func(ctx context.Context, _ api.ChatRequest, fn func(api.Chunk) error) (*api.Result, error) {
		fn(api.Chunk{Kind: api.ChunkText, Text: "half a"})
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	r := openReply(t, f)

	post(t, r.Engine, "hey")
	r.clock.Advance(config.DefaultEngine().Debounce.Duration())
	<-started

	waiting := make(chan struct{})
	go func() {
		defer close(waiting)
		if _, err := r.Wait(context.Background()); err != nil {
			t.Error(err)
		}
	}()

	r.Close()
	select {
	case <-waiting:
	case <-time.After(2 * time.Second):
		t.Fatal("Wait never returned after the conversation closed")
	}

	// A run that ends keeps nothing of the half sentence it was writing.
	history, _, err := r.History(context.Background(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 {
		t.Errorf("history = %+v, want only what was sent to her", history)
	}
}

func TestAReplyCarriesTheReasoningTheCatalogueAllows(t *testing.T) {
	model := chatModel()
	model.Reasoning = true
	model.Efforts = []string{"low", "high", "max"}
	f := &fakeRunner{model: model, chat: says("thinking about it")}
	r := openReply(t, f)

	r.say(t, "hey")

	got := f.asked().Settings.Reasoning
	if got.Mode != api.ReasoningOn || got.Effort != "high" {
		t.Errorf("reasoning = %+v, want it on at the middle effort the catalogue lists", got)
	}
}

func TestRequestsOfTheSamePurposeShareACacheKey(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: says("hey you")}
	eyes := &fakeRunner{model: visionModel(), chat: says("a red square")}
	r := openSeeing(t, f, eyes)

	sendPhoto(t, r, "look at this", photo(t))

	if got := f.asked().CacheKey; got != "paula-paula-reply" {
		t.Errorf("the reply asked under %q", got)
	}
	if got := eyes.asked().CacheKey; got != "paula-paula-caption" {
		t.Errorf("the description asked under %q", got)
	}
}

// A reply the model finished as the close landed is stored, ends its entry, and
// is not reported as a failure.
func TestAReplyThatLandsAsTheRunStopsIsKept(t *testing.T) {
	ctx := context.Background()
	writing := make(chan struct{})
	f := &fakeRunner{model: chatModel()}
	f.chat = func(ctx context.Context, _ api.ChatRequest, fn func(api.Chunk) error) (*api.Result, error) {
		close(writing)
		// The stream finishes as the run is being stopped.
		<-ctx.Done()
		if err := fn(api.Chunk{Kind: api.ChunkText, Text: "made it"}); err != nil {
			return nil, err
		}
		return &api.Result{FinishReason: "stop"}, nil
	}
	r := openReply(t, f)

	post(t, r.Engine, "hey")
	r.clock.Advance(config.DefaultEngine().Debounce.Duration())
	<-writing
	r.Close()

	history, _, err := r.History(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || history[1].Text() != "made it" {
		t.Fatalf("history = %+v, want the reply that came back", history)
	}
	entries, err := r.store.Entries(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if entries[0].Status != store.StatusDone || entries[0].Error != "" {
		t.Errorf("entry = %+v, want it done", entries[0])
	}
	reply, err := r.store.ReplyOfEntry(ctx, entries[0].ID)
	if err != nil || reply.ID != history[1].ID {
		t.Errorf("the entry holds %v, %v, want the reply %d", reply, err, history[1].ID)
	}
	for _, ev := range published(r.Engine) {
		if ev.Kind == ReplyFailed {
			t.Errorf("a reply that came back whole was reported as %+v", ev)
		}
	}
	answered, err := r.store.AnsweredUpto(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if answered != history[0].ID {
		t.Errorf("answered up to %d, want the message the reply answered", answered)
	}
}
