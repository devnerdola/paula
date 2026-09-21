package conversation

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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
)

// fakeRunner answers with whatever a test hands it, and remembers what it was
// asked.
type fakeRunner struct {
	mu       sync.Mutex
	model    api.Model
	requests []api.ChatRequest
	chat     func(ctx context.Context, req api.ChatRequest, fn func(api.Chunk) error) (*api.Result, error)
	// models answers the catalogue when a test wants one that cannot be read,
	// and embed the vectors when a test is about what they come to.
	models func() ([]api.Model, error)
	embed  func(api.EmbedRequest) (*api.EmbedResult, error)
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

// Embed answers with a vector for each string, and records the request the
// way a runner does, so what embedded what is in the turn log.
func (f *fakeRunner) Embed(ctx context.Context, req api.EmbedRequest) (*api.EmbedResult, error) {
	f.mu.Lock()
	embed := f.embed
	f.mu.Unlock()

	rec := api.Recording(req.Recorder)
	body, err := json.Marshal(map[string]any{"model": req.Model, "input": req.Input})
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

	out := &api.EmbedResult{Vectors: make([][]float32, len(req.Input))}
	for i := range req.Input {
		out.Vectors[i] = []float32{1, 0, 0}
	}
	if embed != nil {
		out, err = embed(req)
	}
	record.EndedAt = time.Now()
	record.Status = 200
	if err != nil {
		record.Error = err.Error()
	}
	if rerr := rec.EndRequest(ctx, record); rerr != nil && err == nil {
		return nil, rerr
	}
	return out, err
}

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

// setup is a conversation's models: one that chats, and one that embeds, the
// way a run has both since serve asks for them before it opens.
func setup(f *fakeRunner) *runners.Setup {
	m := &runners.Configured{Name: "chat", ID: f.model.ID, Runner: f}
	v := &fakeRunner{model: embedModel()}
	vectors := &runners.Configured{Name: "vectors", ID: v.model.ID, Runner: v}
	return &runners.Setup{
		Runners: []runners.Runner{f, v},
		Models:  []*runners.Configured{m, vectors},
		Defaults: map[config.Role]*runners.Configured{
			config.RoleChat:  m,
			config.RoleEmbed: vectors,
		},
	}
}

// embedModel is a model that embeds and writes nothing, as a catalogue says of
// one.
func embedModel() api.Model {
	return api.Model{ID: "some/vectors", Context: 8192, Embeddings: true}
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
		Engine:  config.DefaultEngine(),
		Clock:   c,
		Log:     slog.New(slog.NewTextHandler(written, nil)),
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

	history, err := r.History(context.Background(), 0, 10)
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

func TestAReplyWithNoText(t *testing.T) {
	f := &fakeRunner{model: chatModel()}
	f.chat = func(context.Context, api.ChatRequest, func(api.Chunk) error) (*api.Result, error) {
		return &api.Result{FinishReason: "length"}, nil
	}
	r := openReply(t, f)
	r.say(t, "hey")

	var failed Event
	for _, ev := range published(r.Engine) {
		if ev.Kind == ReplyFailed {
			failed = ev
		}
	}
	if want := "the model returned no text (finish reason length)"; failed.Text != want {
		t.Errorf("failure = %q, want %q", failed.Text, want)
	}
	if history, _ := r.History(context.Background(), 0, 10); len(history) != 1 {
		t.Errorf("history = %+v, want nothing stored", history)
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

	history, err := r.History(context.Background(), 0, 10)
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

	history, err := r.History(context.Background(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 3 || history[2].Text() != "both then" {
		t.Errorf("history = %+v, want the restarted reply kept nothing", history)
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
	history, err := r.History(context.Background(), 0, 100)
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

func TestOnlyTheLatestImagesAreSent(t *testing.T) {
	model := chatModel()
	model.Vision = true
	f := &fakeRunner{model: model, chat: says("nice")}
	r := openReply(t, f)

	sendPhoto(t, r, "one", photo(t))
	sendPhoto(t, r, "two", photo(t))
	sendPhoto(t, r, "three", photo(t))

	// engine.image_turns is 2, so the oldest of the three is described.
	var sent, described int
	for _, m := range f.asked().Messages[1:] {
		if len(images(m)) > 0 {
			sent++
		}
		if strings.Contains(text(m), "[photo") {
			described++
		}
	}
	if sent != 2 || described != 1 {
		t.Errorf("%d photos sent and %d described, want 2 and 1", sent, described)
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

	history, err := r.History(context.Background(), 0, 10)
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
	history, err := r.History(context.Background(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 {
		t.Errorf("history = %+v, want only what he said", history)
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

	history, err := r.History(ctx, 0, 10)
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
