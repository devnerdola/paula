// Package web is the page Paula is texted from: one HTTP server, a session for
// every browser open on it, and what the conversation does as it happens.
package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/frontend/api"
	"nerdola.dev/x/paula/internal/logs"
	"nerdola.dev/x/paula/internal/media"
	"nerdola.dev/x/paula/internal/store"
)

// Kind is the type written in the configuration file.
const Kind = "web"

const (
	// defaultListen is where the server listens when the file says nothing.
	defaultListen = ":8484"
	// defaultTokenEnv is where the token is read from.
	defaultTokenEnv = "PAULA_WEB_TOKEN"
	// shown is how much of the conversation a page opens on, and how much more
	// it is given each time it is scrolled back.
	shown = 50
	// beat is how often a comment goes down a stream with nothing to say. A
	// proxy closes a connection that has been quiet for long enough, and a
	// conversation is quiet most of the time.
	beat = 25 * time.Second
	// retry is what the browser is asked to wait before opening the stream
	// again, which it does on its own whenever one ends.
	retry = 2 * time.Second
	// uploadMax is the most one message may carry, its pictures and all.
	uploadMax = 32 << 20
	// picturesMax is how many pictures go in one message.
	picturesMax = 4
)

type settings struct {
	Listen   string `yaml:"listen"`
	TokenEnv string `yaml:"token_env"`
}

type Frontend struct {
	listen string
	token  string
	log    *slog.Logger
	names  api.Names
	files  *media.Files

	// told numbers everything the run has put on a page, whichever page it went
	// to. A browser opening the stream again says the number it had, which is
	// what says whether it missed anything while it was away.
	told atomic.Int64
	// bubbles numbers the bubbles of the run, as told numbers the events. A
	// browser that missed nothing keeps what is on its screen, so a bubble of
	// the stream that follows must not land on one that is already there.
	bubbles atomic.Int64
	// writing counts the pages a reply is arriving on. What a browser holds of
	// one is a piece of it, and a piece is not what it will be, so a browser
	// that left while she was writing is shown the conversation again however
	// little it missed.
	writing atomic.Int64

	// pages are the browsers open on the conversation, by the number each was
	// given as it opened. What is typed on one is answered on the same one.
	mu    sync.Mutex
	pages map[string]*adapter
	// live is every session running on a page, so a run ends with none of them
	// still writing.
	live sync.WaitGroup
}

// Open reads the web section and builds the server it describes.
func Open(s config.Section, h api.Host) (*Frontend, error) {
	cfg := settings{Listen: defaultListen, TokenEnv: defaultTokenEnv}
	if err := s.Decode(&cfg); err != nil {
		return nil, err
	}
	// Every problem of the section is reported at once, as the file's are.
	var problems []error
	token := os.Getenv(cfg.TokenEnv)
	switch {
	case cfg.TokenEnv == "":
		problems = append(problems, fmt.Errorf("%s.token_env: the name of an environment variable is needed", s.Path()))
	case token == "":
		problems = append(problems, fmt.Errorf("%s.token_env: environment variable %s is not set", s.Path(), cfg.TokenEnv))
	case !tokenShape(token):
		problems = append(problems, fmt.Errorf("%s.token_env: %s does not hold a token of at least %d characters, without a colon or a space",
			s.Path(), cfg.TokenEnv, logs.MinSecret))
	}
	if cfg.Listen == "" {
		problems = append(problems, fmt.Errorf("%s.listen: an address to listen on is needed", s.Path()))
	}
	if err := errors.Join(problems...); err != nil {
		return nil, err
	}
	h.Secrets.Add(token)

	log := h.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Frontend{
		listen: cfg.Listen,
		token:  token,
		log:    log,
		names:  h.Names,
		// The pictures are read back as they were kept, so nothing here says
		// how large one may be.
		files: media.New(h.DataDir, 0),
		pages: make(map[string]*adapter),
	}, nil
}

// tokenShape reports whether a value can be used as a token. It goes in the
// user name or the password of an ordinary sign-in, where a colon is what
// separates the two, and a short one would be redacted out of the text around
// it wherever it appeared.
func tokenShape(token string) bool {
	return len(token) >= logs.MinSecret && !strings.ContainsAny(token, ": \t\r\n")
}

func (f *Frontend) Kind() string { return Kind }

// Run serves the page until the context ends, with a session on every browser
// that opens the stream.
func (f *Frontend) Run(ctx context.Context, session func(context.Context, api.Adapter) error) error {
	l, err := net.Listen("tcp", f.listen)
	if err != nil {
		return err
	}
	f.log.Info("web listening", "address", l.Addr().String())

	srv := &http.Server{Handler: f.handler(ctx, session)}
	go func() {
		<-ctx.Done()
		// The streams are open for as long as a browser is, so there is nothing
		// to wait to finish: the sessions on them end with the context.
		srv.Close()
	}()
	defer f.live.Wait()
	if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// page is everything a browser is served: the page itself and what it is built
// of, which is served from the binary rather than from a directory beside it.
//
//go:embed page
var page embed.FS

// worker is the one file served to a browser that does not sign in. A browser
// asks for it without the sign-in it has, and it holds nothing of the
// conversation: what a notification says the page hands it.
const worker = "sw.js"

// handler is everything the page reaches, behind the sign-in.
func (f *Frontend) handler(ctx context.Context, session func(context.Context, api.Adapter) error) http.Handler {
	files, err := fs.Sub(page, "page")
	if err != nil {
		panic(err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServerFS(files))
	mux.HandleFunc("GET /api/events", func(w http.ResponseWriter, r *http.Request) {
		f.events(ctx, session, w, r)
	})
	mux.HandleFunc("POST /api/messages", f.posted)
	mux.HandleFunc("POST /api/pick", f.picked)
	mux.HandleFunc("POST /api/stop", f.stopped)
	mux.HandleFunc("GET /api/history", f.history)
	mux.HandleFunc("GET /api/media/{sha}", f.picture)

	out := http.NewServeMux()
	out.Handle("GET /"+worker, http.FileServerFS(files))
	out.Handle("/", f.guard(mux))
	return out
}

// guard is what every request passes first: it comes from the page itself, and
// it carries the token.
func (f *Frontend) guard(h http.Handler) http.Handler {
	// A browser on another site can reach a server on this machine, and would
	// be asked for the sign-in the browser already has.
	cross := http.NewCrossOriginProtection()
	return cross.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !f.allowed(r) {
			if _, _, given := r.BasicAuth(); given {
				f.log.Info("web refused", "address", r.RemoteAddr, "path", r.URL.Path)
			}
			w.Header().Set("WWW-Authenticate", `Basic realm="paula", charset="UTF-8"`)
			http.Error(w, "the token is wrong", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
}

// allowed reports whether a request carries the token. It is taken as either
// half of an ordinary sign-in, so a browser that asks for one takes whatever is
// typed into either box.
func (f *Frontend) allowed(r *http.Request) bool {
	user, pass, ok := r.BasicAuth()
	if !ok {
		return false
	}
	want := []byte(f.token)
	// Both halves are compared whichever matched, so what is compared says
	// nothing about how long the answer took.
	as := subtle.ConstantTimeCompare([]byte(user), want)
	ap := subtle.ConstantTimeCompare([]byte(pass), want)
	return as|ap == 1
}

// events opens the stream and runs a session on it. The session writes what
// happens down the same connection, so it lasts as long as the page is open.
func (f *Frontend) events(ctx context.Context, session func(context.Context, api.Adapter) error, w http.ResponseWriter, r *http.Request) {
	number := make([]byte, 16)
	if _, err := rand.Read(number); err != nil {
		http.Error(w, "the page could not be numbered", http.StatusInternalServerError)
		return
	}
	a := &adapter{
		f:      f,
		id:     hex.EncodeToString(number),
		w:      w,
		flush:  http.NewResponseController(w),
		inputs: make(chan api.Input),
		older:  make(chan answer, 1),
		done:   make(chan struct{}),
		// A browser opens the stream again on its own whenever one ends, and
		// says the last event it read. Whether it missed anything is read where
		// the session starts, which is after this.
		read: lastRead(r),
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	// A proxy that buffers holds an event until it has enough of them, which
	// for a conversation is until it is over.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if err := a.write(fmt.Sprintf("retry: %d\n\n", retry.Milliseconds())); err != nil {
		return
	}

	// The page goes when the browser does, and when the run does.
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		<-r.Context().Done()
		stop()
	}()

	f.live.Add(1)
	defer f.live.Done()
	f.opened(a)
	defer f.closed(a)
	// The beat runs beside the session, and the response belongs to the server
	// again the moment this returns. A reply that was arriving here is arriving
	// nowhere now.
	defer a.left()
	defer a.says(false)
	go a.beating(ctx)

	if err := session(ctx, a); err != nil && ctx.Err() == nil {
		f.log.Error("web session", "page", a.id, "error", err)
	}
}

// lastRead is the event a browser says it read last, which it sends whenever
// it opens the stream again, and zero from one that has read none.
//
// What is made of it is in Start: a browser that read everything there was
// keeps its screen, and one that did not is shown the conversation again. The
// number covers every page of the run, so a second browser that was told
// something is a first browser that is shown it again — what it misses is a
// reply arriving while it was away, and showing the conversation again is what
// that is for.
func lastRead(r *http.Request) int64 {
	last, err := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64)
	if err != nil || last < 0 {
		return 0
	}
	return last
}

// page is the browser a request came from, which is the one it is answered on.
func (f *Frontend) page(r *http.Request) (*adapter, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.pages[r.Header.Get(pageHeader)]
	return a, ok
}

func (f *Frontend) opened(a *adapter) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pages[a.id] = a
}

func (f *Frontend) closed(a *adapter) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.pages, a.id)
}

// pageHeader is how a request says which browser it came from. The number is
// the one the stream opened with.
const pageHeader = "X-Paula-Page"

// typed hands one thing to the session of the page it was typed on. A page the
// server no longer knows is one whose stream ended, and the browser opens
// another as soon as it notices.
func (f *Frontend) typed(w http.ResponseWriter, r *http.Request, in api.Input) {
	a, ok := f.page(r)
	if !ok {
		http.Error(w, "that page is not open any more", http.StatusGone)
		return
	}
	if err := a.typed(r.Context(), in); err != nil {
		http.Error(w, "that page is not open any more", http.StatusGone)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// posted takes a message and the pictures with it.
func (f *Frontend) posted(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, uploadMax)
	if err := r.ParseMultipartForm(uploadMax); err != nil {
		http.Error(w, "the message is more than this takes", http.StatusRequestEntityTooLarge)
		return
	}
	defer r.MultipartForm.RemoveAll()

	files := r.MultipartForm.File["image"]
	if len(files) > picturesMax {
		http.Error(w, fmt.Sprintf("a message takes %d pictures", picturesMax), http.StatusRequestEntityTooLarge)
		return
	}
	var images [][]byte
	for _, fh := range files {
		file, err := fh.Open()
		if err != nil {
			http.Error(w, "that picture could not be read", http.StatusBadRequest)
			return
		}
		b, err := io.ReadAll(file)
		file.Close()
		if err != nil {
			http.Error(w, "that picture could not be read", http.StatusBadRequest)
			return
		}
		if media.Detect(b) == "" {
			http.Error(w, fmt.Sprintf("%s is not a picture she can see", fh.Filename), http.StatusUnsupportedMediaType)
			return
		}
		images = append(images, b)
	}
	f.typed(w, r, api.Input{Text: r.FormValue("text"), Images: images})
}

// picked takes a choice of a menu, as the session worded it.
func (f *Frontend) picked(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Picked string `json:"picked"`
	}
	if !read(w, r, &body) {
		return
	}
	if body.Picked == "" {
		http.Error(w, "nothing was picked", http.StatusBadRequest)
		return
	}
	f.typed(w, r, api.Input{Picked: body.Picked})
}

func (f *Frontend) stopped(w http.ResponseWriter, r *http.Request) {
	f.typed(w, r, api.Input{Stop: true})
}

// history answers with what was said before a message the page holds, which is
// what it shows as it is scrolled back. The session is what reads the
// conversation, so the ask goes to the session of the page and the answer
// comes back to the request that made it.
func (f *Frontend) history(w http.ResponseWriter, r *http.Request) {
	before, err := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	if err != nil || before <= 0 {
		http.Error(w, "before is the message to look back from", http.StatusBadRequest)
		return
	}
	a, ok := f.page(r)
	if !ok {
		http.Error(w, "that page is not open any more", http.StatusGone)
		return
	}
	messages, err := a.asks(r.Context(), store.MessageID(before))
	if err != nil {
		http.Error(w, "that page is not open any more", http.StatusGone)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(struct {
		Messages []said `json:"messages"`
	}{messages})
}

// bodyMax is the most read of a request that is not a message: they carry a
// number or a word of the session's own.
const bodyMax = 4 << 10

// read decodes what a request says, and answers the page itself when it says
// something else.
func read(w http.ResponseWriter, r *http.Request, out any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, bodyMax)).Decode(out); err != nil {
		http.Error(w, "that is not something this takes", http.StatusBadRequest)
		return false
	}
	return true
}

// picture serves a picture of the conversation. They are named by what is in
// them, so one that was read once never changes.
func (f *Frontend) picture(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha")
	if len(sha) != 64 || strings.Trim(sha, "0123456789abcdef") != "" {
		http.NotFound(w, r)
		return
	}
	b, err := f.files.Load(sha)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", media.MIMEJPEG)
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(b))
}
