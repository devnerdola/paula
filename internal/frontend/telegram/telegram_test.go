package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/frontend/api"
	"nerdola.dev/x/paula/internal/logs"
	"nerdola.dev/x/paula/internal/store"
)

// token is a value shaped like a bot token: the bot's number, a colon, and the
// secret.
const token = "123456:AAHfakefakefakefakefakefakefake"

// bot answers the Bot API methods Paula calls, from what a test gave it.
type bot struct {
	mu sync.Mutex
	// answers are the updates each getUpdates gives, in turn. Once they run
	// out it answers with none, as a bot that is waiting does.
	answers [][]update
	asked   []int64          // the offset each getUpdates was given
	sent    []string         // the text of each sendMessage
	acted   int              // how often it was told she is writing
	offered []string         // the commands it was told the bot answers
	keys    []button         // the buttons under the message sent last
	tapped  int              // how many taps were answered
	lastID  int64            // the number the message sent last was given
	edits   []edited         // what was written over a message, in order
	order   []int64          // the messages of the chat, oldest first
	holds   map[int64]string // what each of them says now
	files   map[string][]byte
	fail    func(method string) (int, string)
	// reading is told that a download was asked for, which the bot then never
	// answers.
	reading chan struct{}
	server  *httptest.Server
}

// edited is one thing written over a message that was already sent.
type edited struct {
	Message int64
	Text    string
}

func newBot(t *testing.T, answers ...[]update) *bot {
	t.Helper()
	b := &bot{answers: answers, files: map[string][]byte{}}
	b.server = httptest.NewServer(http.HandlerFunc(b.serve))
	t.Cleanup(b.server.Close)
	return b
}

func (b *bot) serve(w http.ResponseWriter, r *http.Request) {
	// A download held open is one the run is still reading when it ends, and it
	// is let go of when the request is given up on.
	if b.reading != nil && strings.Contains(r.URL.Path, "/file/bot") {
		select {
		case b.reading <- struct{}{}:
		case <-r.Context().Done():
		}
		<-r.Context().Done()
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")

	path := strings.TrimPrefix(r.URL.Path, "/")
	if after, ok := strings.CutPrefix(path, "file/bot"+token+"/"); ok {
		file, held := b.files[after]
		if !held {
			http.NotFound(w, r)
			return
		}
		w.Write(file)
		return
	}
	method := strings.TrimPrefix(path, "bot"+token+"/")

	if b.fail != nil {
		if code, why := b.fail(method); code != 0 {
			// A code below zero is the connection dropping, which is what an
			// error carrying the URL of the request comes of.
			if code < 0 {
				panic(http.ErrAbortHandler)
			}
			json.NewEncoder(w).Encode(map[string]any{
				"ok": false, "error_code": code, "description": why,
			})
			return
		}
	}

	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	switch method {
	case "getMe":
		json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "result": map[string]any{"username": "paula_bot"},
		})
	case "getUpdates":
		offset, _ := body["offset"].(float64)
		b.asked = append(b.asked, int64(offset))
		var out []update
		if len(b.answers) > 0 {
			out, b.answers = b.answers[0], b.answers[1:]
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": out})
	case "sendMessage":
		text, _ := body["text"].(string)
		b.sent = append(b.sent, text)
		b.keys = nil
		if markup, ok := body["reply_markup"].(map[string]any); ok {
			for _, row := range markup["inline_keyboard"].([]any) {
				for _, k := range row.([]any) {
					key := k.(map[string]any)
					b.keys = append(b.keys, button{
						Text: key["text"].(string), Data: key["callback_data"].(string),
					})
				}
			}
		}
		b.lastID++
		b.order = append(b.order, b.lastID)
		if b.holds == nil {
			b.holds = map[int64]string{}
		}
		b.holds[b.lastID] = text
		json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "result": map[string]any{"message_id": b.lastID},
		})
	case "editMessageText":
		id := int64(body["message_id"].(float64))
		text, _ := body["text"].(string)
		b.edits = append(b.edits, edited{Message: id, Text: text})
		b.holds[id] = text
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{}})
	case "answerCallbackQuery":
		b.tapped++
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
	case "sendChatAction":
		b.acted++
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
	case "setMyCommands":
		b.offered = nil
		for _, c := range body["commands"].([]any) {
			b.offered = append(b.offered, c.(map[string]any)["command"].(string))
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
	case "getFile":
		id, _ := body["file_id"].(string)
		json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "result": map[string]any{"file_path": "photos/" + id + ".jpg"},
		})
	default:
		http.NotFound(w, r)
	}
}

func (b *bot) said() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.sent...)
}

// keyboard is the buttons under the message that was sent last.
func (b *bot) keyboard() []button {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]button(nil), b.keys...)
}

// shown is what the chat says now: each message as it stands, oldest first,
// which for one that was written over is the last thing written.
func (b *bot) shown() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, 0, len(b.order))
	for _, id := range b.order {
		out = append(out, b.holds[id])
	}
	return out
}

// written is what was written over a message that was already sent.
func (b *bot) written() []edited {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]edited(nil), b.edits...)
}

// taps is how many taps were answered.
func (b *bot) taps() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tapped
}

// commands are the ones the client was told the bot answers.
func (b *bot) commands() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.offered...)
}

// actions is how often the chat was told she is writing.
func (b *bot) actions() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.acted
}

// waitFor waits for something the bot was told, since a status is said from a
// goroutine of its own.
func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for range 200 {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (b *bot) offsets() []int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]int64(nil), b.asked...)
}

// from is an update of a message sent by a user.
func from(id int64, user int64, m message) update {
	m.From = &struct {
		ID int64 `json:"id"`
	}{ID: user}
	return update{UpdateID: id, Message: &m}
}

// open builds the frontend a telegram section describes, pointed at the bot.
func open(t *testing.T, b *bot, dataDir, section string) (*Frontend, *strings.Builder) {
	t.Helper()
	t.Setenv("TELEGRAM_TOKEN", token)
	path := filepath.Join(t.TempDir(), "paula.yaml")
	file := "persona: paula.yaml\ndata_dir: " + dataDir + "\nrunners:\n  r:\n    type: openrouter\n" +
		"models:\n  a:\n    runner: r\n    id: x\ndefault_models:\n  chat: a\nfrontends:\n  telegram:\n"
	for line := range strings.SplitSeq(section, "\n") {
		if line != "" {
			file += "    " + line + "\n"
		}
	}
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	// The log is written the way a run writes it, through the same secrets the
	// frontend registers its token with.
	said := &strings.Builder{}
	secrets := new(logs.Secrets)
	f, err := Open(cfg.Frontends[0].Section, api.Host{
		DataDir: cfg.DataDir,
		Log:     logs.New(said, slog.LevelInfo, logs.FormatText, secrets),
		Names:   api.Names{Character: "Paula", User: "Caio"},
		Secrets: secrets,
	})
	if err != nil {
		t.Fatal(err)
	}
	if b != nil {
		f.client.url = b.server.URL
	}
	return f, said
}

// inputs runs the frontend and gathers what arrives, until want of them have,
// and returns them with the run stopped.
func inputs(t *testing.T, f *Frontend, want int) []api.Input {
	t.Helper()
	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	got := make(chan []api.Input, 1)
	go func() {
		var out []api.Input
		_ = f.Run(ctx, func(ctx context.Context, a api.Adapter) error {
			in, err := a.Start(ctx)
			if err != nil {
				return err
			}
			for m := range in {
				out = append(out, m)
				if len(out) >= want {
					break
				}
			}
			got <- out
			return nil
		})
	}()
	select {
	case out := <-got:
		return out
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for what was sent to arrive")
		return nil
	}
}

func TestWhatIsSentArrives(t *testing.T) {
	b := newBot(t, []update{
		from(10, 7, message{Text: "hey"}),
		from(11, 7, message{Text: "are you there"}),
	})
	f, _ := open(t, b, t.TempDir(), "user_id: 7")

	got := inputs(t, f, 2)
	if len(got) != 2 || got[0].Text != "hey" || got[1].Text != "are you there" {
		t.Fatalf("arrived = %+v, want both messages in order", got)
	}
	// The first ask is for whatever the API still holds; the next is for what
	// came after the last update taken.
	if offsets := b.offsets(); len(offsets) < 1 || offsets[0] != 0 {
		t.Errorf("offsets = %v, want the first ask to name none", offsets)
	}
}

// A bot is reachable by anyone who finds it, and the conversation is with one
// person.
func TestOnlyTheOnePersonIsAnswered(t *testing.T) {
	b := newBot(t, []update{
		from(10, 8, message{Text: "hello?"}),
		from(11, 7, message{Text: "hey"}),
	})
	f, said := open(t, b, t.TempDir(), "user_id: 7")

	got := inputs(t, f, 1)
	if len(got) != 1 || got[0].Text != "hey" {
		t.Fatalf("arrived = %+v, want only what the one person sent", got)
	}
	if !strings.Contains(said.String(), "a message from someone else") {
		t.Errorf("the log holds %q, want the message that was left alone", said.String())
	}
	if len(b.said()) != 0 {
		t.Errorf("sent %q, want nothing said to someone who is not served", b.said())
	}
}

// A run that ended answered what it took, so the run after it asks for what
// came next rather than answering the same messages again.
func TestTheRunAfterOneAsksForWhatCameNext(t *testing.T) {
	dir := t.TempDir()
	b := newBot(t, []update{from(10, 7, message{Text: "hey"})})
	f, _ := open(t, b, dir, "user_id: 7")
	inputs(t, f, 1)

	// An update is written down once the session has taken it, which is just
	// after it arrives here.
	var kept []byte
	for range 100 {
		if b, err := os.ReadFile(filepath.Join(dir, offsetFile)); err == nil {
			kept = b
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if strings.TrimSpace(string(kept)) != "11" {
		t.Fatalf("kept %q, want the one to ask for after the update that was taken", kept)
	}

	next := newBot(t, []update{from(11, 7, message{Text: "still there?"})})
	again, _ := open(t, next, dir, "user_id: 7")
	inputs(t, again, 1)
	offsets := next.offsets()
	if len(offsets) == 0 || offsets[0] != 11 {
		t.Errorf("offsets = %v, want the first ask to be for what came after 10", offsets)
	}
}

func TestAPhotoArrivesAsAnImage(t *testing.T) {
	b := newBot(t, []update{from(10, 7, message{
		Caption: "look at this",
		Photo: []photoSize{
			{FileID: "small", Width: 90, Height: 90},
			{FileID: "large", Width: 800, Height: 600},
		},
	})})
	b.files["photos/large.jpg"] = square(t)
	b.files["photos/small.jpg"] = []byte("not an image")
	f, _ := open(t, b, t.TempDir(), "user_id: 7")

	got := inputs(t, f, 1)
	if len(got) != 1 || got[0].Text != "look at this" {
		t.Fatalf("arrived = %+v, want the caption as its text", got)
	}
	// The largest size is the one worth reading: a thumbnail is what the others
	// are.
	if len(got[0].Images) != 1 || !bytes.Equal(got[0].Images[0], b.files["photos/large.jpg"]) {
		t.Errorf("arrived with %d images, want the largest size", len(got[0].Images))
	}
}

// A run that ends while what was sent is still being read leaves that update
// for the next run, rather than writing it down as one that was answered.
func TestAnUpdateStillBeingReadIsLeftForTheNextRun(t *testing.T) {
	dir := t.TempDir()
	b := newBot(t, []update{from(10, 7, message{
		Photo: []photoSize{{FileID: "large", Width: 800, Height: 600}},
	})})
	b.reading = make(chan struct{})
	f, _ := open(t, b, dir, "user_id: 7")

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = f.Run(ctx, func(ctx context.Context, a api.Adapter) error {
			in, err := a.Start(ctx)
			if err != nil {
				return err
			}
			<-in
			return nil
		})
	}()

	select {
	case <-b.reading:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the photo to be read")
	}
	stop()
	<-done

	if _, err := os.Stat(filepath.Join(dir, offsetFile)); !os.IsNotExist(err) {
		kept, _ := os.ReadFile(filepath.Join(dir, offsetFile))
		t.Errorf("kept %q, want the update left for the next run", kept)
	}
}

// What she cannot read is said so, rather than going into the conversation as
// nothing.
func TestSomethingSheCannotReadIsSaidSo(t *testing.T) {
	b := newBot(t, []update{
		from(10, 7, message{}),
		from(11, 7, message{Text: "hey"}),
	})
	f, _ := open(t, b, t.TempDir(), "user_id: 7")

	got := inputs(t, f, 1)
	if len(got) != 1 || got[0].Text != "hey" {
		t.Fatalf("arrived = %+v, want only what she can read", got)
	}
	if len(b.said()) != 1 || !strings.Contains(b.said()[0], "photos") {
		t.Errorf("sent %q, want what she reads said back", b.said())
	}
}

// The token is in the URL of every request, so it is in the error of every
// request that fails.
func TestTheTokenIsNotWrittenDown(t *testing.T) {
	b := newBot(t)
	// The connection is dropped rather than refused, since what carries the URL
	// is the error the HTTP client makes of a request it could not finish.
	b.fail = func(method string) (int, string) {
		if method == "getUpdates" {
			return -1, ""
		}
		return 0, ""
	}
	f, said := open(t, b, t.TempDir(), "user_id: 7")

	ctx, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()
	_ = f.Run(ctx, func(ctx context.Context, a api.Adapter) error {
		in, err := a.Start(ctx)
		if err != nil {
			return err
		}
		<-in
		return nil
	})
	if !strings.Contains(said.String(), "asking telegram what arrived") {
		t.Fatalf("the log holds %q, want what went wrong", said.String())
	}
	if !strings.Contains(said.String(), logs.Mask) {
		t.Errorf("the log holds %q, want the URL of the request in it", said.String())
	}
	if strings.Contains(said.String(), token) {
		t.Error("the log carries the bot token")
	}
}

// A connection that stalls after the request is written answers never, and the
// loop that asks what arrived has no other way out: the bot would go quiet for
// the rest of the run with nothing to say why. Every call is given up on.
func TestARequestThatIsNeverAnsweredIsGivenUpOn(t *testing.T) {
	var asked atomic.Int64
	// A request that is taken and never answered. The handler lets go of it in
	// its own time as well, so the server can be closed at the end.
	done := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		select {
		case <-r.Context().Done():
		case <-done:
		}
	}))
	// Cleanups run in reverse, so the requests are let go of before the server
	// is closed on them.
	t.Cleanup(ts.Close)
	t.Cleanup(func() { close(done) })
	c := &client{
		url: ts.URL, token: token, http: &http.Client{}, wait: 100 * time.Millisecond,
		pause: func(context.Context, time.Duration) error { return nil },
	}

	ctx := context.Background()
	// The API holds getUpdates open for as long as it is asked to, so what
	// bounds it is that and the margin, not the margin alone.
	started := time.Now()
	if _, err := c.updates(ctx, 0, 200*time.Millisecond); err == nil {
		t.Error("a request that was never answered came back")
	}
	if took := time.Since(started); took > 5*time.Second {
		t.Errorf("getUpdates took %v, want it given up on", took)
	}

	if _, err := c.send(ctx, 7, "hey", nil); err == nil {
		t.Error("a message that was never answered for came back")
	}
	if _, err := c.me(ctx); err == nil {
		t.Error("a token check that was never answered came back")
	}
	// A message that never got an answer is sent again, since what she says is
	// several messages and one of them missing would cut her off. Asking what
	// arrived, and for the bot, is asked once and comes round again on its own.
	if n, want := asked.Load(), int64(2+sendTries); n != want {
		t.Errorf("the server was asked %d times, want %d", n, want)
	}
}

// A chat told to show a reply as it is written puts what she has so far in one
// message and writes over it as more arrives, rather than waiting for the
// whole of it.
func TestAReplyShownAsItIsWritten(t *testing.T) {
	b := newBot(t)
	f, _ := open(t, b, t.TempDir(), "user_id: 7\nstream_edits: true")
	s := &editing{adapter: &adapter{f: f, inputs: make(chan api.Input)}}
	ctx := context.Background()

	// Whitespace on its own is no message: Telegram refuses one.
	if err := s.Stream(ctx, "  "); err != nil {
		t.Fatal(err)
	}
	if said := b.said(); len(said) != 0 {
		t.Fatalf("sent %q, want nothing said of whitespace", said)
	}

	if err := s.Stream(ctx, "hey"); err != nil {
		t.Fatal(err)
	}
	if said := b.said(); len(said) != 1 || said[0] != "hey" {
		t.Fatalf("sent %q, want the first of it in a message", said)
	}
	// What follows within the moment is not written over at once, and is there
	// by the time the stream ends.
	if err := s.Stream(ctx, " you"); err != nil {
		t.Fatal(err)
	}
	if err := s.EndStream(ctx); err != nil {
		t.Fatal(err)
	}
	edits := b.written()
	if len(edits) != 1 || edits[0].Text != "hey you" {
		t.Fatalf("wrote %+v, want the whole of it over the message it started as", edits)
	}
	if said := b.said(); len(said) != 1 {
		t.Errorf("sent %q, want it in the one message", said)
	}
}

// Each text she writes is a message of its own whether or not the chat is
// shown her writing it. What being shown it adds is the message filling as she
// writes; what she broke the reply into is the same either way.
func TestTheSameTextsWhetherTheyAreWatchedOrNot(t *testing.T) {
	// What she writes, in the fragments it arrives in.
	fragments := []string{"hey", ", how are you?\n", "\nI was ", "thinking about the douro", "\n\nanyway"}
	want := []string{"hey, how are you?", "I was thinking about the douro", "anyway"}

	plain := newBot(t)
	f, _ := open(t, plain, t.TempDir(), "user_id: 7")
	a := &adapter{f: f, inputs: make(chan api.Input)}

	watched := newBot(t)
	g, _ := open(t, watched, t.TempDir(), "user_id: 7\nstream_edits: true")
	s := &editing{adapter: &adapter{f: g, inputs: make(chan api.Input)}}

	ctx := context.Background()
	for _, each := range []api.Streamer{a, s} {
		for _, text := range fragments {
			if err := each.Stream(ctx, text); err != nil {
				t.Fatal(err)
			}
		}
		if err := each.EndStream(ctx); err != nil {
			t.Fatal(err)
		}
	}

	// What the chat says at the end, which for a message that was written over
	// is the last thing written.
	if said := plain.shown(); !slices.Equal(said, want) {
		t.Errorf("the chat says %q, want %q", said, want)
	}
	if said := watched.shown(); !slices.Equal(said, want) {
		t.Errorf("watched, the chat says %q, want %q", said, want)
	}
	// The one being watched fills as she writes; the other is written once and
	// left alone.
	if len(watched.written()) == 0 {
		t.Error("nothing was written over, so nothing was watched being written")
	}
	if n := len(plain.written()); n != 0 {
		t.Errorf("wrote over a message %d times, want it sent once and left", n)
	}
}

// A reply longer than a message holds carries on in a message of its own,
// which the rest of it is then written over.
func TestAReplyShownAsItIsWrittenOutgrowingOneMessage(t *testing.T) {
	b := newBot(t)
	f, _ := open(t, b, t.TempDir(), "user_id: 7\nstream_edits: true")
	s := &editing{adapter: &adapter{f: f, inputs: make(chan api.Input)}}
	ctx := context.Background()

	if err := s.Stream(ctx, strings.Repeat("a", maxUnits+100)); err != nil {
		t.Fatal(err)
	}
	if err := s.EndStream(ctx); err != nil {
		t.Fatal(err)
	}
	said := b.said()
	if len(said) != 2 {
		t.Fatalf("sent %d messages, want it carried on in another", len(said))
	}
	for _, m := range said {
		if units(m) > maxUnits {
			t.Errorf("a message is %d units, want at most %d", units(m), maxUnits)
		}
	}
	for _, e := range b.written() {
		if units(e.Text) > maxUnits {
			t.Errorf("wrote %d units over a message, want at most %d", units(e.Text), maxUnits)
		}
	}
}

// What comes back when a button is tapped is held to 64 bytes by Telegram, and
// what a session calls a choice is as long as it needs to be. The adapter tags
// each one and remembers which is which, so nothing is left unpickable.
func TestAChoiceIsOfferedAsAButtonAndComesBackWhole(t *testing.T) {
	b := newBot(t)
	f, _ := open(t, b, t.TempDir(), "user_id: 7")
	f.pause = func(context.Context, time.Duration) error { return nil }
	a := &adapter{f: f, inputs: make(chan api.Input)}

	long := "model:chat:" + strings.Repeat("a-rather-long-model-name/", 6)
	err := a.Send(context.Background(), api.Outgoing{Text: "what serves chat", Choices: []api.Choice{
		{Label: "fast", Picked: "model:chat:fast", Current: true},
		{Label: "long", Picked: long},
	}})
	if err != nil {
		t.Fatal(err)
	}
	keys := b.keyboard()
	if len(keys) != 2 {
		t.Fatalf("offered %+v, want a button for each choice", keys)
	}
	if !strings.HasPrefix(keys[0].Text, "✓") {
		t.Errorf("the first button says %q, want the one that stands marked", keys[0].Text)
	}
	for _, k := range keys {
		if len(k.Data) > 64 {
			t.Errorf("a button carries %d bytes, want at most 64", len(k.Data))
		}
	}

	// A tap comes back as the session's own word for the choice, however long.
	in, ok := a.tapped(context.Background(), &tap{
		ID: "1", Data: keys[1].Data,
		From: &struct {
			ID int64 `json:"id"`
		}{ID: 7},
	})
	if !ok || in.Picked != long {
		t.Errorf("tapped = %+v, %v, want the choice it stands for", in, ok)
	}
	if b.taps() != 1 {
		t.Errorf("answered %d taps, want the one that was tapped", b.taps())
	}

	// A tap from anyone else is answered and left alone.
	if _, ok := a.tapped(context.Background(), &tap{
		ID: "2", Data: keys[0].Data,
		From: &struct {
			ID int64 `json:"id"`
		}{ID: 8},
	}); ok {
		t.Error("a tap from someone who is not served was taken")
	}
}

// A client sends this of its own accord when the chat is opened. Answering it
// would have her reply to something nobody said.
func TestTheMessageAClientSendsOnOpeningIsNotOne(t *testing.T) {
	b := newBot(t, []update{
		from(10, 7, message{Text: "/start"}),
		from(11, 7, message{Text: "/start with something after it"}),
		from(12, 7, message{Text: "hey"}),
	})
	f, _ := open(t, b, t.TempDir(), "user_id: 7")

	got := inputs(t, f, 1)
	if len(got) != 1 || got[0].Text != "hey" {
		t.Fatalf("arrived = %+v, want only what was said", got)
	}
	if said := b.said(); len(said) != 0 {
		t.Errorf("sent %q, want nothing said about it", said)
	}
}

// What can be typed is offered by the client as it is typed, so a person does
// not have to know it.
func TestWhatCanBeTypedIsOfferedByTheClient(t *testing.T) {
	b := newBot(t)
	f, _ := open(t, b, t.TempDir(), "user_id: 7")
	a := &adapter{f: f, inputs: make(chan api.Input)}

	err := a.ShowCommands(context.Background(), []api.Command{
		{Name: "models", Args: "[reset]", Short: "which model serves each role"},
		{Name: "help", Short: "what you can type"},
		// Telegram takes lower case, digits and underscores, so one it would
		// refuse is left out rather than refusing the lot.
		{Name: "Shout", Short: "no"},
		{Name: "no-dashes", Short: "no"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"models", "help"}
	if got := b.commands(); !slices.Equal(got, want) {
		t.Errorf("offered %q, want %q", got, want)
	}
}

// Telegram drops the typing status after about five seconds, and as soon as
// anything is sent, so it is said again for as long as she is still writing —
// and not once she has stopped.
func TestTheChatIsToldSheIsWritingUntilSheStops(t *testing.T) {
	b := newBot(t)
	f, _ := open(t, b, t.TempDir(), "user_id: 7")
	f.pause = func(context.Context, time.Duration) error { return nil }
	a := &adapter{f: f, inputs: make(chan api.Input)}

	ctx := t.Context()
	if err := a.Writing(ctx, true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the status to be said again", func() bool { return b.actions() > 1 })

	if err := a.Writing(ctx, false); err != nil {
		t.Fatal(err)
	}
	// It stops being said once she has stopped: what is counted now is what it
	// stays at.
	settled := b.actions()
	time.Sleep(50 * time.Millisecond)
	if now := b.actions(); now > settled+1 {
		t.Errorf("the status was said %d more times after she stopped", now-settled)
	}
}

// A blank line is where one text ends and the next begins, so each of them is
// sent as she finishes writing it rather than all of them at the end.
func TestATextIsSentAsSheFinishesWritingIt(t *testing.T) {
	b := newBot(t)
	f, _ := open(t, b, t.TempDir(), "user_id: 7")
	a := &adapter{f: f, inputs: make(chan api.Input)}
	ctx := context.Background()

	// What she is still writing waits for the rest of it.
	if err := a.Stream(ctx, "hey"); err != nil {
		t.Fatal(err)
	}
	if said := b.said(); len(said) != 0 {
		t.Fatalf("sent %q, want nothing of a text she is still writing", said)
	}

	// The blank line ends it, and it goes.
	if err := a.Stream(ctx, ", how are you?\n\nI was thinking"); err != nil {
		t.Fatal(err)
	}
	if said := b.said(); !slices.Equal(said, []string{"hey, how are you?"}) {
		t.Fatalf("sent %q, want the text she finished", said)
	}

	// What she was in the middle of goes when she has finished writing.
	if err := a.EndStream(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{"hey, how are you?", "I was thinking"}
	if said := b.said(); !slices.Equal(said, want) {
		t.Errorf("sent %q, want %q", said, want)
	}
}

// One paragraph longer than Telegram takes is still one thing she said, and
// goes in as many messages as it takes.
func TestAParagraphTooLongForOneMessage(t *testing.T) {
	b := newBot(t)
	f, _ := open(t, b, t.TempDir(), "user_id: 7")
	f.pause = func(context.Context, time.Duration) error { return nil }
	a := &adapter{f: f, inputs: make(chan api.Input)}

	line := strings.Repeat("a", 1000) + "\n"
	if err := a.Send(context.Background(), api.Outgoing{Text: strings.Repeat(line, 6), Hers: true}); err != nil {
		t.Fatal(err)
	}
	said := b.said()
	if len(said) < 2 {
		t.Fatalf("sent %d messages, want the paragraph cut into several", len(said))
	}
	for _, m := range said {
		if units(m) > maxUnits {
			t.Errorf("a message is %d units, want at most %d", units(m), maxUnits)
		}
	}
}

// What she says is often several messages, one after the other. One the API
// asked to be given a moment is sent again, with the rest behind it in the
// order she wrote them, rather than leaving her mid-sentence.
func TestAMessageTheAPICouldNotTakeIsSentAgain(t *testing.T) {
	b := newBot(t)
	var sends atomic.Int64
	b.fail = func(method string) (int, string) {
		if method == "sendMessage" && sends.Add(1) == 2 {
			return 429, "Too Many Requests: retry after 0"
		}
		return 0, ""
	}
	f, _ := open(t, b, t.TempDir(), "user_id: 7")
	f.client.pause = func(context.Context, time.Duration) error { return nil }
	a := &adapter{f: f, inputs: make(chan api.Input)}

	ctx := context.Background()
	for _, text := range []string{"one", "two", "three"} {
		if err := a.Send(ctx, api.Outgoing{Text: text, Hers: true}); err != nil {
			t.Fatalf("sending %q: %v", text, err)
		}
	}
	if said := b.said(); len(said) != 3 ||
		said[0] != "one" || said[1] != "two" || said[2] != "three" {
		t.Errorf("sent %q, want all three in the order she wrote them", said)
	}
}

// What the API refuses outright it refuses again, so it is not asked twice.
func TestAMessageTheAPIRefusesIsNotSentAgain(t *testing.T) {
	b := newBot(t)
	var sends atomic.Int64
	b.fail = func(method string) (int, string) {
		if method == "sendMessage" {
			sends.Add(1)
			return 400, "Bad Request: message is too long"
		}
		return 0, ""
	}
	f, _ := open(t, b, t.TempDir(), "user_id: 7")
	a := &adapter{f: f, inputs: make(chan api.Input)}

	err := a.Send(context.Background(), api.Outgoing{Text: "hey", Hers: true})
	if err == nil || !strings.Contains(err.Error(), "too long") {
		t.Errorf("error = %v, want what the API refused it for", err)
	}
	if n := sends.Load(); n != 1 {
		t.Errorf("sent %d times, want it asked once", n)
	}
}

// A token the API refuses is a bot nothing ever reaches, so the run says so
// rather than waiting on a chat it will never be told about.
func TestATokenTheAPIRefusesStopsTheRun(t *testing.T) {
	b := newBot(t)
	b.fail = func(method string) (int, string) {
		if method == "getMe" {
			return 401, "Unauthorized"
		}
		return 0, ""
	}
	f, _ := open(t, b, t.TempDir(), "user_id: 7")

	err := f.Run(context.Background(), func(context.Context, api.Adapter) error {
		t.Error("a session was opened on a bot the API refuses")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "Unauthorized") {
		t.Errorf("error = %v, want what the API said", err)
	}
}

func TestASectionSaysEveryThingItIsMissing(t *testing.T) {
	t.Setenv("TELEGRAM_TOKEN", "")
	path := filepath.Join(t.TempDir(), "paula.yaml")
	file := "persona: paula.yaml\nrunners:\n  r:\n    type: openrouter\n" +
		"models:\n  a:\n    runner: r\n    id: x\ndefault_models:\n  chat: a\nfrontends:\n  telegram:\n"
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Open(cfg.Frontends[0].Section, api.Host{DataDir: cfg.DataDir, Secrets: new(logs.Secrets)})
	if err == nil {
		t.Fatal("a section with no token and nobody to serve was taken")
	}
	for _, want := range []string{"TELEGRAM_TOKEN is not set", "user_id"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want %q in it", err, want)
		}
	}

	// A value that is not a token is one that landed in the wrong variable.
	t.Setenv("TELEGRAM_TOKEN", "not-a-bot-token")
	_, err = Open(cfg.Frontends[0].Section, api.Host{DataDir: cfg.DataDir, Secrets: new(logs.Secrets)})
	if err == nil || !strings.Contains(err.Error(), "does not hold a bot token") {
		t.Errorf("error = %v, want it to say what the variable holds", err)
	}
}

// Telegram takes a message of 4096 UTF-16 units, which is not 4096 of what Go
// counts, and a reply is cut where she wrote a break.
func TestHowMuchOfAReplyGoesInOneMessage(t *testing.T) {
	short := "hey you"
	if fits(short) != len(short) {
		t.Errorf("fits(%q) = %d, want the whole of it", short, fits(short))
	}

	line := strings.Repeat("a", 1000) + "\n"
	long := strings.Repeat(line, 5)
	at := fits(long)
	if at <= 0 || at >= len(long) {
		t.Fatalf("fits = %d of %d, want a cut", at, len(long))
	}
	if units(long[:at]) > maxUnits {
		t.Errorf("the first message is %d units, want at most %d", units(long[:at]), maxUnits)
	}
	if long[at] != '\n' {
		t.Errorf("it cuts at %q, want a line break", long[at])
	}

	// An emoji is one rune and two of what Telegram counts, so what fits is
	// counted its way.
	emoji := strings.Repeat("😀", maxUnits)
	at = fits(emoji)
	if units(emoji[:at]) > maxUnits {
		t.Errorf("the first message is %d units, want at most %d", units(emoji[:at]), maxUnits)
	}
	if at >= len(emoji) {
		t.Errorf("fits = %d of %d, want a cut", at, len(emoji))
	}

	// A byte that is no character at all is one unit and one byte, so the cut
	// stays inside the text.
	broken := strings.Repeat("a", maxUnits-1) + "\xff\xff"
	if at = fits(broken); at <= 0 || at > len(broken) {
		t.Errorf("fits = %d of %d, want a cut inside the text", at, len(broken))
	}
}

// What was said on another frontend is shown, since the bot is the only thing
// that writes in the chat and what she answers would read as an answer to
// nothing.
func TestWhatWasSaidElsewhereIsShown(t *testing.T) {
	b := newBot(t)
	f, _ := open(t, b, t.TempDir(), "user_id: 7")
	a := &adapter{f: f, inputs: make(chan api.Input)}

	m := &store.Message{Role: store.RoleUser, Parts: []store.Part{{Type: store.PartText, Text: "hey"}}}
	if err := a.ShowUserMessage(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if said := b.said(); len(said) != 1 || said[0] != "Caio: hey" {
		t.Errorf("sent %q, want it shown as the person who said it", said)
	}

	// A long one goes in as many messages as it takes, like anything else the
	// chat is sent.
	long := &store.Message{Role: store.RoleUser, Parts: []store.Part{
		{Type: store.PartText, Text: strings.Repeat("a", 3*maxUnits)},
	}}
	if err := a.ShowUserMessage(context.Background(), long); err != nil {
		t.Fatal(err)
	}
	said := b.said()[1:]
	if len(said) < 2 {
		t.Fatalf("sent %d messages, want it in as many as Telegram takes it in", len(said))
	}
	for _, s := range said {
		if units(s) > maxUnits {
			t.Errorf("a message is %d units, want at most %d", units(s), maxUnits)
		}
	}
}

// square is a JPEG, as a photo that arrives is.
func square(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for x := range 8 {
		for y := range 8 {
			img.Set(x, y, color.RGBA{R: 200, G: 40, B: 40, A: 255})
		}
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, nil); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
