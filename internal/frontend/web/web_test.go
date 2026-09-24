package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/frontend/api"
	"nerdola.dev/x/paula/internal/logs"
	"nerdola.dev/x/paula/internal/media"
	"nerdola.dev/x/paula/internal/store"
)

// token is a value shaped like the one a page signs in with.
const token = "a-token-nobody-guesses"

// wait is how long a test gives the server to say something before it is a
// server that says nothing.
const wait = 5 * time.Second

// open builds the frontend a web section describes, with the log of the run it
// belongs to.
func open(t *testing.T, dataDir, section string) (*Frontend, *strings.Builder, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "paula.yaml")
	file := "persona: paula.yaml\ndata_dir: " + dataDir + "\nrunners:\n  r:\n    type: openrouter\n" +
		"models:\n  a:\n    runner: r\n    id: x\ndefault_models:\n  chat: a\nfrontends:\n  web:\n"
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
	return f, said, err
}

// sessions is what a test does with a page: it opens the page the way a
// session does, hands the adapter over, and gathers what is typed on it.
type sessions struct {
	// history is what a page opens on, and answer what the session does with
	// something that was typed.
	history []store.Message
	answer  func(context.Context, *adapter, api.Input) error
	// before runs where a session starts, which is before it takes anything of
	// the page: it is where a test puts something that happens while a browser
	// is opening the stream.
	before   func()
	adapters chan *adapter
	inputs   chan api.Input
}

// serving runs a server on the frontend, with a session on every page that
// opens.
func serving(t *testing.T, f *Frontend) (*httptest.Server, *sessions) {
	t.Helper()
	s := &sessions{
		adapters: make(chan *adapter, 4),
		inputs:   make(chan api.Input, 4),
	}
	ctx, stop := context.WithCancel(t.Context())
	srv := httptest.NewServer(f.handler(ctx, s.run))
	t.Cleanup(func() {
		// The sessions are stopped before the server is, since one still
		// writing holds open the connection it writes to.
		stop()
		srv.Close()
	})
	return srv, s
}

func (s *sessions) run(ctx context.Context, a api.Adapter) error {
	if s.before != nil {
		s.before()
	}
	in, err := a.Start(ctx)
	if err != nil {
		return err
	}
	page := a.(*adapter)
	if err := page.ShowHistory(ctx, s.history); err != nil {
		return err
	}
	s.adapters <- page
	for {
		select {
		case m, ok := <-in:
			if !ok {
				return nil
			}
			s.inputs <- m
			if s.answer != nil {
				if err := s.answer(ctx, page, m); err != nil {
					return err
				}
			}
		case <-ctx.Done():
			return nil
		}
	}
}

// opened is the page the session was opened on.
func (s *sessions) opened(t *testing.T) *adapter {
	t.Helper()
	select {
	case a := <-s.adapters:
		return a
	case <-time.After(wait):
		t.Fatal("no session was opened on the page")
		return nil
	}
}

// arrived is the next thing typed on a page to reach a session.
func (s *sessions) arrived(t *testing.T) api.Input {
	t.Helper()
	select {
	case in := <-s.inputs:
		return in
	case <-time.After(wait):
		t.Fatal("nothing arrived from the page")
		return api.Input{}
	}
}

// event is one thing that arrived on a stream, with the number it was given.
type event struct {
	id   int64
	name string
	data []byte
}

// stream is a page reading what the server sends it.
type stream struct {
	t      *testing.T
	events chan event
	// page is the number this one was given, which says what it is answered on,
	// and last the number of the event it read last.
	page string
	last int64
}

// opens a stream on the server, signed in the way a browser signs in.
func opens(t *testing.T, srv *httptest.Server) *stream {
	return opensAgain(t, srv, 0)
}

// opensAgain opens the stream the way a browser opens one that ended, saying
// the last event it read. Zero is a browser that has read none.
func opensAgain(t *testing.T, srv *httptest.Server, last int64) *stream {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/api/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth("paula", token)
	if last > 0 {
		req.Header.Set("Last-Event-ID", strconv.FormatInt(last, 10))
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the stream answered %s", resp.Status)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("the stream is %q", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("the stream is kept as %q", got)
	}
	s := &stream{t: t, events: make(chan event, 32)}
	t.Cleanup(func() { resp.Body.Close() })
	go s.read(resp.Body)
	return s
}

// read turns what the server writes into events, until the stream ends.
func (s *stream) read(body io.ReadCloser) {
	defer close(s.events)
	r := bufio.NewReader(body)
	var e event
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\n")
		switch {
		case line == "":
			if e.name != "" {
				s.events <- e
			}
			e = event{}
		case strings.HasPrefix(line, "id: "):
			e.id, _ = strconv.ParseInt(strings.TrimPrefix(line, "id: "), 10, 64)
		case strings.HasPrefix(line, "event: "):
			e.name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			e.data = []byte(strings.TrimPrefix(line, "data: "))
		}
	}
}

// next is the next event of the stream.
func (s *stream) next() event {
	s.t.Helper()
	select {
	case e, ok := <-s.events:
		if !ok {
			s.t.Fatal("the stream ended with an event still to come")
		}
		s.last = e.id
		return e
	case <-time.After(wait):
		s.t.Fatal("nothing came down the stream")
		return event{}
	}
}

// opening is what a page opens on.
type opening struct {
	Page      string `json:"page"`
	Character string `json:"character"`
	Messages  []said `json:"messages"`
	Caught    bool   `json:"caught"`
}

// synced reads the sync every page opens on, and keeps the number it carries
// so that what the page sends afterwards goes to its own session.
func (s *stream) synced() opening {
	s.t.Helper()
	e := s.next()
	if e.name != "sync" {
		s.t.Fatalf("a page opens on %q", e.name)
	}
	var out opening
	s.decode(e, &out)
	if out.Page == "" {
		s.t.Error("the page was given no number")
	}
	s.page = out.Page
	return out
}

func (s *stream) decode(e event, out any) {
	s.t.Helper()
	if err := json.Unmarshal(e.data, out); err != nil {
		s.t.Fatalf("%s: %v", e.name, err)
	}
}

// posts sends something to this page's own session.
func (s *stream) posts(srv *httptest.Server, path, kind string, body io.Reader) *http.Response {
	s.t.Helper()
	req, err := http.NewRequestWithContext(s.t.Context(), http.MethodPost, srv.URL+path, body)
	if err != nil {
		s.t.Fatal(err)
	}
	req.SetBasicAuth("paula", token)
	req.Header.Set(pageHeader, s.page)
	if kind != "" {
		req.Header.Set("Content-Type", kind)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// gets asks this page's own session for something.
func (s *stream) gets(srv *httptest.Server, path string) *http.Response {
	s.t.Helper()
	req, err := http.NewRequestWithContext(s.t.Context(), http.MethodGet, srv.URL+path, nil)
	if err != nil {
		s.t.Fatal(err)
	}
	req.SetBasicAuth("paula", token)
	req.Header.Set(pageHeader, s.page)
	resp, err := srv.Client().Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// form is a message as the page sends one.
func form(t *testing.T, text string, files map[string][]byte) (io.Reader, string) {
	t.Helper()
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	if err := w.WriteField("text", text); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		part, err := w.CreateFormFile("image", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return &b, w.FormDataContentType()
}

// picture is a JPEG built here, so what a test checks against is an image and
// not something the code under test made.
func picture(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for x := range 8 {
		for y := range 8 {
			img.Set(x, y, color.RGBA{R: uint8(x * 32), G: uint8(y * 32), B: 20, A: 255})
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// opens a frontend whose token is the one every test signs in with.
func running(t *testing.T, dataDir string) *Frontend {
	t.Helper()
	t.Setenv("PAULA_WEB_TOKEN", token)
	f, _, err := open(t, dataDir, "")
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// A server anyone on the machine can reach is a conversation anyone can read,
// so nothing is answered without the token. It goes in either half of the
// sign-in, since a browser asking for one takes whatever is typed where.
func TestARequestWithoutTheTokenIsRefused(t *testing.T) {
	t.Setenv("PAULA_WEB_TOKEN", token)
	f, said, err := open(t, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	srv, _ := serving(t, f)

	for _, c := range []struct {
		what       string
		user, pass string
		signed     bool
		want       int
	}{
		{what: "nothing at all", want: http.StatusUnauthorized},
		{what: "the wrong token", user: "paula", pass: "guess", signed: true, want: http.StatusUnauthorized},
		{what: "the token as the user name", user: token, signed: true, want: http.StatusGone},
		{what: "the token as the password", user: "paula", pass: token, signed: true, want: http.StatusGone},
	} {
		// Stopping a reply is the shortest thing to ask for. With the token it
		// reaches a page that was never opened, which is as far as it goes.
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/api/stop", nil)
		if err != nil {
			t.Fatal(err)
		}
		if c.signed {
			req.SetBasicAuth(c.user, c.pass)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("with %s the server answered %s", c.what, resp.Status)
		}
		if c.want == http.StatusUnauthorized && resp.Header.Get("WWW-Authenticate") == "" {
			t.Errorf("with %s the browser is not asked to sign in", c.what)
		}
	}
	if strings.Contains(said.String(), token) {
		t.Error("the token was written to the log")
	}
	if !strings.Contains(said.String(), "web refused") {
		t.Error("a refusal by something that gave a token is not logged")
	}
}

// A page on another site can reach a server on this machine, and the browser
// would sign it in with what it already has.
func TestARequestFromAnotherSiteIsRefused(t *testing.T) {
	srv, _ := serving(t, running(t, t.TempDir()))

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/api/stop", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth("paula", token)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a request from another site answered %s", resp.Status)
	}
}

// A page opens on what was said before it, the pictures of it among the rest,
// and is numbered so that what is typed on it is answered on it.
func TestAPageOpensOnWhatWasSaidBefore(t *testing.T) {
	at := time.Date(2026, 9, 21, 10, 30, 0, 0, time.UTC)
	srv, s := serving(t, running(t, t.TempDir()))
	s.history = []store.Message{{
		ID: 3, Role: store.RoleUser, Channel: "telegram", CreatedAt: at,
		Parts: []store.Part{
			{Type: store.PartText, Text: "look at this"},
			{Type: store.PartImage, SHA256: "abc", MIME: media.MIMEJPEG},
		},
	}}

	page := opens(t, srv)
	opened := page.synced()
	if opened.Character != "Paula" {
		t.Errorf("the page is a conversation with %q", opened.Character)
	}
	got := opened.Messages
	if len(got) != 1 {
		t.Fatalf("the page opened on %d messages", len(got))
	}
	m := got[0]
	if m.ID != 3 || m.Role != store.RoleUser || m.Channel != "telegram" || m.Text != "look at this" {
		t.Errorf("the page opened on %+v", m)
	}
	if len(m.Pictures) != 1 || m.Pictures[0] != "abc" {
		t.Errorf("the pictures of it are %v", m.Pictures)
	}
	if !m.At.Equal(at) {
		t.Errorf("it was said at %s", m.At)
	}
	if got := s.opened(t).History(); got != shown {
		t.Errorf("a page opens on %d messages", got)
	}
}

// What is typed on a page, and the pictures picked with it, go to the session
// of that page.
func TestWhatIsTypedOnAPageArrives(t *testing.T) {
	srv, s := serving(t, running(t, t.TempDir()))
	page := opens(t, srv)
	page.synced()

	jpg := picture(t)
	body, kind := form(t, "what is this", map[string][]byte{"cat.jpg": jpg})
	if resp := page.posts(srv, "/api/messages", kind, body); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("the message answered %s", resp.Status)
	}
	in := s.arrived(t)
	if in.Text != "what is this" {
		t.Errorf("the message says %q", in.Text)
	}
	if len(in.Images) != 1 || !bytes.Equal(in.Images[0], jpg) {
		t.Errorf("%d pictures arrived, and not as they were sent", len(in.Images))
	}
}

// A page whose stream has ended is not there to answer on, and the browser
// opens another as soon as it notices.
func TestSomethingTypedOnAPageThatIsNotOpenIsRefused(t *testing.T) {
	srv, _ := serving(t, running(t, t.TempDir()))
	page := opens(t, srv)
	page.synced()
	page.page = "one that was never open"

	body, kind := form(t, "anyone there", nil)
	if resp := page.posts(srv, "/api/messages", kind, body); resp.StatusCode != http.StatusGone {
		t.Errorf("a message to a page that is not open answered %s", resp.Status)
	}
}

// What she cannot see is refused where it was picked, rather than reaching the
// conversation as a message with nothing in it.
func TestSomethingThatIsNotAPictureIsRefused(t *testing.T) {
	srv, _ := serving(t, running(t, t.TempDir()))
	page := opens(t, srv)
	page.synced()

	body, kind := form(t, "read this", map[string][]byte{"notes.pdf": []byte("%PDF-1.7\nnot a picture")})
	if resp := page.posts(srv, "/api/messages", kind, body); resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("something that is not a picture answered %s", resp.Status)
	}
}

// Each text she writes is a bubble of its own, and the one she is in the
// middle of fills as it arrives rather than appearing once it is over.
func TestAReplyFillsABubbleAsSheWritesIt(t *testing.T) {
	srv, s := serving(t, running(t, t.TempDir()))
	page := opens(t, srv)
	page.synced()
	a := s.opened(t)

	for _, fragment := range []string{"I was ", "thinking\n", "\nabout that"} {
		if err := a.Stream(t.Context(), fragment); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.EndStream(t.Context()); err != nil {
		t.Fatal(err)
	}

	// What the page says is every bubble as it stands, which for one that was
	// written into is the last thing written.
	// A bubble is started, written into as the rest of that text arrives, and
	// left alone once she has finished it.
	var bubbles []message
	var edits int
	for range 3 {
		e := page.next()
		var m message
		page.decode(e, &m)
		switch e.name {
		case "message":
			bubbles = append(bubbles, m)
		case "edit":
			edits++
			for i := range bubbles {
				if bubbles[i].ID == m.ID {
					bubbles[i].Text = m.Text
				}
			}
		default:
			t.Fatalf("a reply arrived as %q", e.name)
		}
	}
	if len(bubbles) != 2 {
		t.Fatalf("the reply is %d bubbles: %+v", len(bubbles), bubbles)
	}
	if bubbles[0].Text != "I was thinking" || !bubbles[0].Hers {
		t.Errorf("the first text says %q", bubbles[0].Text)
	}
	if bubbles[1].Text != "about that" {
		t.Errorf("the second text says %q", bubbles[1].Text)
	}
	if edits == 0 {
		t.Error("nothing filled as it was written")
	}
}

// What she is doing in the middle of a reply goes to the page as a note of its
// own, for the page to show where it shows one.
func TestANoteGoesToThePage(t *testing.T) {
	srv, s := serving(t, running(t, t.TempDir()))
	page := opens(t, srv)
	page.synced()
	a := s.opened(t)

	if err := a.Note(t.Context(), "looking up Ana"); err != nil {
		t.Fatal(err)
	}
	e := page.next()
	if e.name != "note" {
		t.Fatalf("a note arrived as %q", e.name)
	}
	var got struct {
		Text string `json:"text"`
	}
	page.decode(e, &got)
	if got.Text != "looking up Ana" {
		t.Errorf("the note says %q", got.Text)
	}
}

// A page scrolled back asks for what came before what it holds. The session is
// what reads the conversation, so the ask goes to the session of that page and
// what it shows comes back to the request that asked.
func TestAPageScrolledBackAsksForWhatCameBefore(t *testing.T) {
	srv, s := serving(t, running(t, t.TempDir()))
	s.answer = func(ctx context.Context, a *adapter, in api.Input) error {
		return a.ShowOlder(ctx, in.Older, []store.Message{{
			ID: 1, Role: store.RoleUser,
			Parts: []store.Part{{Type: store.PartText, Text: "the first thing"}},
		}})
	}
	page := opens(t, srv)
	page.synced()

	resp := page.gets(srv, "/api/history?before=4")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("asking for what came before answered %s", resp.Status)
	}
	if in := s.arrived(t); in.Older != 4 {
		t.Errorf("the page asked for what came before %d", in.Older)
	}
	var out struct {
		Messages []said `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 1 || out.Messages[0].Text != "the first thing" {
		t.Errorf("what came before is %+v", out.Messages)
	}
}

// A page that gave up on an ask asks again. The answer to the one it gave up
// on is not the answer to the new one, and a page shown what it did not ask
// for scrolls back to the wrong place.
func TestAnAskThatWasGivenUpOnIsNotTheNextAnswer(t *testing.T) {
	srv, s := serving(t, running(t, t.TempDir()))
	// The session takes its time, so the ask that was given up on is answered
	// while the one that follows is already waiting.
	s.answer = func(ctx context.Context, a *adapter, in api.Input) error {
		select {
		case <-time.After(300 * time.Millisecond):
		case <-ctx.Done():
			return nil
		}
		return a.ShowOlder(ctx, in.Older, []store.Message{{
			ID: store.MessageID(in.Older) - 1, Role: store.RoleUser,
			Parts: []store.Part{{Type: store.PartText, Text: fmt.Sprintf("what came before %d", in.Older)}},
		}})
	}
	page := opens(t, srv)
	page.synced()

	// The browser asks, and stops waiting before the answer comes.
	gone, stop := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer stop()
	req, err := http.NewRequestWithContext(gone, http.MethodGet, srv.URL+"/api/history?before=10", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth("paula", token)
	req.Header.Set(pageHeader, page.page)
	if _, err := srv.Client().Do(req); err == nil {
		t.Fatal("the browser waited for an answer it was meant to give up on")
	}

	// The answer to the ask that was given up on lands while the one that
	// follows is already waiting, so what comes back has to say which ask it
	// belongs to rather than being whatever arrives first.
	resp := page.gets(srv, "/api/history?before=20")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("asking again answered %s", resp.Status)
	}
	var out struct {
		Messages []said `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 1 || out.Messages[0].Text != "what came before 20" {
		t.Errorf("the second ask was answered with %+v", out.Messages)
	}
}

// A page whose session has ended is answered at once rather than held: the
// server sets no timeouts, so a request waiting to hand something over would
// wait for as long as the browser held the connection.
func TestSomethingTypedOnAPageWhoseSessionEndedIsRefused(t *testing.T) {
	a := &adapter{f: &Frontend{}, inputs: make(chan api.Input), done: make(chan struct{})}
	// Nothing reads what is typed on this page any more, which is what the end
	// of a session leaves behind.
	a.left()

	got := make(chan error, 1)
	go func() { got <- a.typed(t.Context(), api.Input{Text: "anyone there"}) }()
	select {
	case err := <-got:
		if err != api.ErrGone {
			t.Errorf("what was typed said %v, want the page said to be gone", err)
		}
	case <-time.After(wait):
		t.Error("what was typed is waiting for a session that has ended")
	}
}

// An ask for what came before nothing is an ask for nothing.
func TestAnAskForWhatCameBeforeNothingIsRefused(t *testing.T) {
	srv, _ := serving(t, running(t, t.TempDir()))
	page := opens(t, srv)
	page.synced()

	for _, query := range []string{"", "?before=0", "?before=the-first-one"} {
		resp := page.gets(srv, "/api/history"+query)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%q answered %s", query, resp.Status)
		}
	}
}

// A browser opens the stream again on its own whenever one ends. One that read
// everything up to then holds the conversation already, and showing it again
// would scroll away from what is being read.
func TestABrowserThatMissedNothingIsNotShownItAgain(t *testing.T) {
	srv, s := serving(t, running(t, t.TempDir()))
	s.history = []store.Message{{
		ID: 1, Role: store.RoleUser, Channel: "web",
		Parts: []store.Part{{Type: store.PartText, Text: "hey"}},
	}}
	page := opens(t, srv)
	if got := page.synced(); len(got.Messages) != 1 || got.Caught {
		t.Fatalf("a page that read nothing opened on %+v", got)
	}

	again := opensAgain(t, srv, page.last)
	got := again.synced()
	if !got.Caught || len(got.Messages) != 0 {
		t.Errorf("a browser that missed nothing was shown %+v", got)
	}
	if got.Page == page.page {
		t.Error("the stream it opened is the one that ended")
	}

	// One that was away while something happened is shown where the
	// conversation stands, since what it missed is not on the screen.
	behind := opensAgain(t, srv, page.last-1)
	if got := behind.synced(); got.Caught || len(got.Messages) != 1 {
		t.Errorf("a browser that missed something was shown %+v", got)
	}
}

// Whether a browser missed anything is read where its session starts, not
// where the stream opens: what happened in between goes to the pages that were
// already open, and this one is following the conversation only from where its
// session picked it up.
func TestWhatHappenedWhileAPageWasOpeningIsNotMissed(t *testing.T) {
	srv, s := serving(t, running(t, t.TempDir()))
	s.history = []store.Message{{
		ID: 1, Role: store.RoleUser, Channel: "web",
		Parts: []store.Part{{Type: store.PartText, Text: "hey"}},
	}}
	first := opens(t, srv)
	first.synced()
	open := s.opened(t)

	// Something is said to the page already open while the browser opening the
	// stream is between the connection and its session.
	var once sync.Once
	s.before = func() {
		once.Do(func() {
			if err := open.Send(t.Context(), api.Outgoing{Text: "something happened", Hers: true}); err != nil {
				t.Error(err)
			}
		})
	}

	// It says it read everything there was when it opened the stream, which it
	// had at the time.
	again := opensAgain(t, srv, first.last)
	if got := again.synced(); got.Caught || len(got.Messages) != 1 {
		t.Errorf("a browser that missed something was shown %+v", got)
	}
}

// A reply goes to a page in pieces, and the session that follows a browser
// back shows it whole when it is done. A browser that left in the middle of
// one keeps pieces of a reply it is about to be shown, so it is shown the
// conversation again however little it missed.
func TestABrowserThatLeftWhileSheWasWritingIsShownItAgain(t *testing.T) {
	srv, s := serving(t, running(t, t.TempDir()))
	s.history = []store.Message{{
		ID: 1, Role: store.RoleUser, Channel: "web",
		Parts: []store.Part{{Type: store.PartText, Text: "hey"}},
	}}
	page := opens(t, srv)
	page.synced()

	a := s.opened(t)
	if err := a.Writing(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if e := page.next(); e.name != "replying" {
		t.Fatalf("she started writing as %q", e.name)
	}
	if err := a.Stream(t.Context(), "I was thinking"); err != nil {
		t.Fatal(err)
	}
	page.next()

	again := opensAgain(t, srv, page.last)
	if got := again.synced(); got.Caught || len(got.Messages) != 1 {
		t.Errorf("a browser that left mid-reply was shown %+v", got)
	}
}

// A picture is served by what is in it, so one the page has read once it never
// reads again.
func TestAPictureIsServedByWhatIsInIt(t *testing.T) {
	dir := t.TempDir()
	srv, _ := serving(t, running(t, dir))

	files := media.New(dir, 0)
	sha, err := files.Store(picture(t))
	if err != nil {
		t.Fatal(err)
	}
	kept, err := files.Load(sha)
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		what string
		sha  string
		want int
	}{
		{what: "one that was kept", sha: sha, want: http.StatusOK},
		{what: "one that was not", sha: strings.Repeat("0", 64), want: http.StatusNotFound},
		{what: "a path of its own", sha: "..%2f..%2fpaula.yaml", want: http.StatusNotFound},
	} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/api/media/"+c.sha, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.SetBasicAuth("paula", token)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s answered %s", c.what, resp.Status)
			continue
		}
		if c.want != http.StatusOK {
			continue
		}
		if !bytes.Equal(body, kept) {
			t.Error("what was served is not what was kept")
		}
		if got := resp.Header.Get("Cache-Control"); !strings.Contains(got, "immutable") {
			t.Errorf("a picture is served as %q", got)
		}
	}
}

// The page is served out of the binary, so a run is one file and nothing is
// fetched from anywhere. The worker is the one thing served to a browser that
// does not sign in, since a browser asks for it without the sign-in it has.
func TestThePageIsServed(t *testing.T) {
	srv, _ := serving(t, running(t, t.TempDir()))

	for _, c := range []struct {
		what, path string
		signed     bool
		want       int
		holds      string
	}{
		{what: "the page", path: "/", signed: true, want: http.StatusOK, holds: "<title>"},
		{what: "what it is built of", path: "/paula.js", signed: true, want: http.StatusOK, holds: "EventSource"},
		{what: "how it looks", path: "/paula.css", signed: true, want: http.StatusOK, holds: "prefers-color-scheme"},
		{what: "the worker", path: "/sw.js", want: http.StatusOK, holds: "notificationclick"},
		{what: "the page without the token", path: "/", want: http.StatusUnauthorized},
		{what: "something that is not there", path: "/whatever", signed: true, want: http.StatusNotFound},
	} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+c.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if c.signed {
			req.SetBasicAuth("paula", token)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s answered %s", c.what, resp.Status)
			continue
		}
		if c.holds != "" && !bytes.Contains(body, []byte(c.holds)) {
			t.Errorf("%s does not hold %q", c.what, c.holds)
		}
	}
}

// A token short enough to be part of ordinary text, or one holding what
// separates a sign-in, is a setting that cannot work.
func TestATokenThatCannotBeUsedIsReported(t *testing.T) {
	for _, c := range []struct{ what, token string }{
		{"short", "abc"},
		{"with a colon", "paula:" + token},
		{"with a space", "a token with spaces"},
	} {
		t.Setenv("PAULA_WEB_TOKEN", c.token)
		if _, _, err := open(t, t.TempDir(), ""); err == nil {
			t.Errorf("a token %s was taken", c.what)
		}
	}
}

// A run ends with no session still writing. A page that opens as the run ends
// is not run, so none is counted once the sessions have been waited for.
func TestAPageThatOpensAsTheRunEndsIsNotRun(t *testing.T) {
	f := running(t, t.TempDir())
	a := &adapter{f: f, id: "one"}
	if !f.opened(a) {
		t.Fatal("a page was refused while the run was up")
	}
	f.closed(a)
	f.wait()
	if f.opened(&adapter{f: f, id: "two"}) {
		t.Error("a page opened after the run ended")
	}
}

// A browser that closed the page is gone for good, so the session on it ends
// rather than writing to nobody.
func TestAPageThatWentAwayEndsItsSession(t *testing.T) {
	a := &adapter{
		f: &Frontend{}, id: "one",
		w:     closed{httptest.NewRecorder()},
		flush: http.NewResponseController(closed{httptest.NewRecorder()}),
	}
	if err := a.Writing(t.Context(), true); err != api.ErrGone {
		t.Errorf("telling a page that went away said %v", err)
	}
	if err := a.Send(t.Context(), api.Outgoing{Text: "anyone there"}); err != api.ErrGone {
		t.Errorf("sending to a page that went away said %v", err)
	}
}

// closed is a connection the browser is no longer reading.
type closed struct{ http.ResponseWriter }

func (closed) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
