package repl_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/frontend/api"
	"nerdola.dev/x/paula/internal/frontend/repl"
	"nerdola.dev/x/paula/internal/store"
)

// dir is a short directory, since a socket path has a small limit.
func dir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "paula")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// open builds the repl the given frontends section describes.
func open(t *testing.T, dataDir, section string) *repl.Frontend {
	t.Helper()
	path := filepath.Join(t.TempDir(), "paula.yaml")
	file := "persona: paula.yaml\ndata_dir: " + dataDir + "\nrunners:\n  r:\n    type: openrouter\n" +
		"models:\n  a:\n    runner: r\n    id: x\ndefault_models:\n  chat: a\nfrontends:\n  repl:\n"
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
	f, err := repl.Open(cfg.Frontends[0].Section, api.Host{
		DataDir: cfg.DataDir,
		Names:   api.Names{Character: "Paula", User: "Caio"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// serving runs the frontend until the test ends, handing each terminal to
// session.
func serving(t *testing.T, f *repl.Frontend, session func(context.Context, api.Adapter) error) {
	t.Helper()
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.Run(ctx, session) }()
	t.Cleanup(func() {
		stop()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(2 * time.Second):
			t.Error("the repl never stopped listening")
		}
	})
	waitFor(t, "the socket", func() bool {
		fi, err := os.Stat(f.Socket())
		return err == nil && fi.Mode()&os.ModeSocket != 0
	})
}

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

// terminal is the client side of one connection.
type terminal struct {
	out        *bytes.Buffer
	lines      chan string
	closed     chan struct{}
	code       chan int
	interrupts chan struct{}
	mu         sync.Mutex
}

// dial starts a client that types what is sent to its channel.
func dial(t *testing.T, f *repl.Frontend, interactive bool) *terminal {
	t.Helper()
	term := &terminal{
		out:        &bytes.Buffer{},
		lines:      make(chan string, 8),
		closed:     make(chan struct{}),
		code:       make(chan int, 1),
		interrupts: make(chan struct{}, 1),
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		defer pw.Close()
		for {
			select {
			case line := <-term.lines:
				pw.WriteString(line + "\n")
			case <-term.closed:
				return
			}
		}
	}()
	c := repl.Client{
		Socket:      f.Socket(),
		In:          pr,
		Out:         writerTo(term),
		Err:         writerTo(term),
		Interactive: interactive,
		Interrupts:  term.interrupts,
	}
	go func() {
		term.code <- c.Run(context.Background())
		pr.Close()
	}()
	t.Cleanup(func() { close(term.closed) })
	return term
}

type termWriter struct{ t *terminal }

func (w termWriter) Write(b []byte) (int, error) {
	w.t.mu.Lock()
	defer w.t.mu.Unlock()
	return w.t.out.Write(b)
}

func writerTo(t *terminal) termWriter { return termWriter{t} }

func (t *terminal) shown() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.out.String()
}

func (t *terminal) type_(line string) { t.lines <- line }

func (t *terminal) waitFor(tb *testing.T, what, want string) {
	tb.Helper()
	waitFor(tb, what, func() bool { return strings.Contains(t.shown(), want) })
}

// reads takes what the session is given, and answers what the test says to.
func reads(inputs chan<- api.Input) func(context.Context, api.Adapter) error {
	return func(ctx context.Context, a api.Adapter) error {
		in, err := a.Start(ctx)
		if err != nil {
			return err
		}
		if p, ok := a.(api.Prompter); ok {
			if err := p.Prompt(ctx); err != nil {
				return err
			}
		}
		for {
			select {
			case got, ok := <-in:
				if !ok {
					return nil
				}
				inputs <- got
				if p, ok := a.(api.Prompter); ok {
					if err := p.Prompt(ctx); err != nil {
						return err
					}
				}
			case <-ctx.Done():
				return nil
			}
		}
	}
}

func TestWhatIsTypedArrives(t *testing.T) {
	inputs := make(chan api.Input, 4)
	f := open(t, dir(t), "")
	serving(t, f, reads(inputs))
	term := dial(t, f, true)

	term.waitFor(t, "the prompt", "> ")
	term.type_("hey you")

	select {
	case in := <-inputs:
		if in.Text != "hey you" {
			t.Errorf("input = %+v", in)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("nothing arrived")
	}
}

func TestATerminalFedAFileIsAskedForEachLine(t *testing.T) {
	inputs := make(chan api.Input, 4)
	f := open(t, dir(t), "")
	serving(t, f, reads(inputs))
	term := dial(t, f, false)

	term.type_("one")
	select {
	case in := <-inputs:
		if in.Text != "one" {
			t.Errorf("input = %+v", in)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("nothing arrived")
	}
	// A file being fed in has no prompt, and what it held is echoed.
	term.waitFor(t, "the echo", "> one")
	if strings.Contains(term.shown(), "> \n") {
		t.Errorf("a prompt was written to a terminal that is not one:\n%q", term.shown())
	}
}

func TestWhatSheSaysAndWhatTheTerminalSays(t *testing.T) {
	f := open(t, dir(t), "")
	serving(t, f, func(ctx context.Context, a api.Adapter) error {
		if err := a.Send(ctx, api.Outgoing{Text: "hey you", Hers: true}); err != nil {
			return err
		}
		return a.Send(ctx, api.Outgoing{Text: "models reset"})
	})
	term := dial(t, f, true)

	term.waitFor(t, "her line", "Paula: hey you\n")
	if !strings.Contains(term.shown(), "\nmodels reset\n") {
		t.Errorf("what the terminal itself says is signed as hers:\n%q", term.shown())
	}
}

func TestWhatWasSaidBeforeAndSomewhereElse(t *testing.T) {
	f := open(t, dir(t), "")
	serving(t, f, func(ctx context.Context, a api.Adapter) error {
		shower := a.(api.HistoryShower)
		if got := shower.History(); got != 10 {
			t.Errorf("history = %d, want the latest ten", got)
		}
		err := shower.ShowHistory(ctx, []store.Message{
			{ID: 1, Role: store.RoleUser, Channel: "repl", Parts: []store.Part{{Type: store.PartText, Text: "hey"}}},
			{ID: 2, Role: store.RoleAssistant, Parts: []store.Part{{Type: store.PartText, Text: "hey you"}}},
		})
		if err != nil {
			return err
		}
		other := a.(api.OtherChannels)
		return other.ShowUserMessage(ctx, &store.Message{
			ID: 3, Role: store.RoleUser, Channel: "telegram",
			Parts: []store.Part{{Type: store.PartText, Text: "from my phone"}},
		})
	})
	term := dial(t, f, true)

	term.waitFor(t, "the other channel", "[telegram] Caio: from my phone\n")
	for _, want := range []string{"Caio: hey\n", "Paula: hey you\n"} {
		if !strings.Contains(term.shown(), want) {
			t.Errorf("output has no %q:\n%q", want, term.shown())
		}
	}
}

func TestAnImageIsReadFromWhereTheTerminalIs(t *testing.T) {
	inputs := make(chan api.Input, 4)
	data := dir(t)
	f := open(t, data, "")
	serving(t, f, reads(inputs))

	// The name has a space in it, so the line has to be quoted for the path to
	// be read whole.
	photo, err := os.ReadFile(filepath.Join("testdata", "a photo.jpg"))
	if err != nil {
		t.Fatal(err)
	}

	term := dial(t, f, true)
	term.waitFor(t, "the prompt", "> ")
	term.type_(`/image "testdata/a photo.jpg" look at this`)

	select {
	case in := <-inputs:
		if in.Text != "look at this" || len(in.Images) != 1 {
			t.Fatalf("input = %+v", in)
		}
		if !bytes.Equal(in.Images[0], photo) {
			t.Error("the bytes that arrived are not the file's")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("nothing arrived")
	}
}

func TestAnImageThatIsNotOne(t *testing.T) {
	inputs := make(chan api.Input, 4)
	f := open(t, dir(t), "")
	serving(t, f, reads(inputs))

	notes := filepath.Join(dir(t), "notes.jpg")
	if err := os.WriteFile(notes, []byte("this is not an image"), 0o600); err != nil {
		t.Fatal(err)
	}

	term := dial(t, f, true)
	term.waitFor(t, "the prompt", "> ")
	term.type_("/image " + notes)
	term.waitFor(t, "the refusal", "is not one of")

	term.type_("/image /nope/missing.jpg")
	term.waitFor(t, "the missing file", "no such file")

	// Something that is not a file at all is refused before it is read, so
	// nothing reads a pipe or a device until the machine runs out of memory.
	pipe := filepath.Join(dir(t), "pipe")
	if err := syscall.Mkfifo(pipe, 0o600); err != nil {
		t.Fatal(err)
	}
	term.type_("/image " + pipe)
	term.waitFor(t, "the refusal", "is not a file")

	// The line that follows is the point by which any of those would have
	// arrived, had they. A line the terminal answered itself reaches the
	// session as nothing typed, which is how it asks for the next one.
	term.type_("hey")
	end := time.Now().Add(2 * time.Second)
	for time.Now().Before(end) {
		select {
		case in := <-inputs:
			if len(in.Images) != 0 {
				t.Fatalf("an image went through: %+v", in)
			}
			if in.Text == "hey" {
				return
			}
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatal("the line after the refusals never arrived")
}

func TestQuitEndsTheTerminal(t *testing.T) {
	f := open(t, dir(t), "")
	serving(t, f, reads(make(chan api.Input, 4)))
	term := dial(t, f, true)

	term.waitFor(t, "the prompt", "> ")
	before := term.shown()
	term.type_("/quit")

	select {
	case code := <-term.code:
		if code != 0 {
			t.Errorf("exit = %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the terminal never ended")
	}
	if term.shown() != before {
		t.Errorf("something was asked for after /quit:\n%q", strings.TrimPrefix(term.shown(), before))
	}
}

func TestTheClientSaysWhenNothingIsListening(t *testing.T) {
	var out, errOut bytes.Buffer
	socket := filepath.Join(dir(t), "paula.sock")
	c := repl.Client{
		Socket: socket,
		In:     strings.NewReader(""),
		Out:    &out,
		Err:    &errOut,
	}
	if code := c.Run(context.Background()); code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "nothing is listening on "+socket) {
		t.Errorf("stderr = %q", errOut.String())
	}
}

func TestASocketPathLongerThanASocketPathMayBe(t *testing.T) {
	long := strings.Repeat("a", 120)
	path := filepath.Join(t.TempDir(), "paula.yaml")
	file := "persona: paula.yaml\nrunners:\n  r:\n    type: openrouter\nmodels:\n  a:\n    runner: r\n    id: x\n" +
		"default_models:\n  chat: a\nfrontends:\n  repl:\n    socket: /tmp/" + long + "\n"
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repl.Open(cfg.Frontends[0].Section, api.Host{DataDir: cfg.DataDir}); err == nil {
		t.Fatal("Open succeeded")
	} else if !strings.Contains(err.Error(), "longer than") {
		t.Errorf("error = %v", err)
	}
}

// The socket a killed run left behind refuses connections, so it is cleared.
func TestASocketLeftBehindIsReplaced(t *testing.T) {
	data := dir(t)
	f := open(t, data, "")

	// A socket nothing is listening on any more, still in the data directory.
	addr, err := net.ResolveUnixAddr("unix", f.Socket())
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.ListenUnix("unix", addr)
	if err != nil {
		t.Fatal(err)
	}
	l.SetUnlinkOnClose(false)
	l.Close()
	if fi, err := os.Stat(f.Socket()); err != nil || fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("the socket was not left behind: %v", err)
	}

	serving(t, f, reads(make(chan api.Input, 4)))
	waitFor(t, "the socket to answer", func() bool {
		c, err := net.Dial("unix", f.Socket())
		if err != nil {
			return false
		}
		c.Close()
		return true
	})
	dial(t, f, true).waitFor(t, "the prompt", "> ")
}

// Whatever it is, it is somebody's, so it is refused rather than removed.
func TestAFileThatIsNotASocketIsLeftAlone(t *testing.T) {
	data := dir(t)
	f := open(t, data, "")
	if err := os.WriteFile(f.Socket(), []byte("not a socket at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := f.Run(context.Background(), reads(make(chan api.Input, 4)))
	if err == nil {
		t.Fatal("Run succeeded")
	}
	if !strings.Contains(err.Error(), "is not a socket") {
		t.Errorf("error = %v", err)
	}
	if _, err := os.ReadFile(f.Socket()); err != nil {
		t.Errorf("the file was removed: %v", err)
	}
}

func TestTwoTerminalsAtOnce(t *testing.T) {
	inputs := make(chan api.Input, 8)
	f := open(t, dir(t), "")
	serving(t, f, reads(inputs))

	first, second := dial(t, f, true), dial(t, f, true)
	first.waitFor(t, "the first prompt", "> ")
	second.waitFor(t, "the second prompt", "> ")

	first.type_("from the first")
	second.type_("from the second")

	said := map[string]bool{}
	for range 2 {
		select {
		case in := <-inputs:
			said[in.Text] = true
		case <-time.After(2 * time.Second):
			t.Fatal("only one terminal was heard")
		}
	}
	if !said["from the first"] || !said["from the second"] {
		t.Errorf("heard %v", said)
	}
}

func dialSocket(path string) (net.Conn, error) { return net.Dial("unix", path) }

func TestAClientOfAnotherVersion(t *testing.T) {
	f := open(t, dir(t), "")
	serving(t, f, reads(make(chan api.Input, 4)))

	conn, err := dialSocket(f.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(map[string]any{
		"hello": map[string]any{"version": 2, "interactive": true},
	}); err != nil {
		t.Fatal(err)
	}

	var said, exited bool
	dec := json.NewDecoder(conn)
	for !exited {
		var f map[string]any
		if err := dec.Decode(&f); err != nil {
			break
		}
		if text, ok := f["write"].(string); ok && strings.Contains(text, "speaks version 1") {
			said = true
		}
		if code, ok := f["exit"].(float64); ok {
			exited = true
			if code != 1 {
				t.Errorf("exit = %v, want 1", code)
			}
		}
	}
	if !said || !exited {
		t.Errorf("said %v, exited %v", said, exited)
	}
}

func TestCtrlCStopsTheReply(t *testing.T) {
	inputs := make(chan api.Input, 4)
	f := open(t, dir(t), "")
	serving(t, f, reads(inputs))
	term := dial(t, f, true)

	term.waitFor(t, "the prompt", "> ")
	term.interrupts <- struct{}{}

	select {
	case in := <-inputs:
		if !in.Stop {
			t.Errorf("input = %+v, want the reply stopped", in)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Ctrl-C did nothing")
	}
}

// A terminal being fed a file has nothing to go back to, so it leaves.
func TestCtrlCEndsATerminalFedAFile(t *testing.T) {
	inputs := make(chan api.Input, 4)
	f := open(t, dir(t), "")
	serving(t, f, reads(inputs))
	term := dial(t, f, false)

	term.type_("one")
	select {
	case <-inputs:
	case <-time.After(2 * time.Second):
		t.Fatal("nothing arrived")
	}
	term.interrupts <- struct{}{}

	select {
	case code := <-term.code:
		if code != 130 {
			t.Errorf("exit = %d, want the terminal left", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Ctrl-C left it running")
	}
}

// Two goroutines share the line: the one showing the reply, and the one reading
// what was typed. What goes on the line and what closes it are one step, so
// neither leaves a line half written or an empty one behind.
func TestAReplyAndTheTerminalWritingAtOnce(t *testing.T) {
	const rounds = 200
	f := open(t, dir(t), "")
	serving(t, f, func(ctx context.Context, a api.Adapter) error {
		s, ok := a.(api.Streamer)
		if !ok {
			return fmt.Errorf("the terminal does not stream")
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for range rounds {
				if err := s.Stream(ctx, "hey"); err != nil {
					return
				}
				if err := s.EndStream(ctx); err != nil {
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			for range rounds {
				if err := a.Send(ctx, api.Outgoing{Text: "note"}); err != nil {
					return
				}
			}
		}()
		wg.Wait()
		<-ctx.Done()
		return nil
	})
	term := dial(t, f, true)

	waitFor(t, "everything written", func() bool {
		return strings.Count(term.shown(), "note") == rounds &&
			strings.Count(term.shown(), "hey") == rounds
	})
	shown := term.shown()
	if strings.Contains(shown, "\n\n") || strings.HasPrefix(shown, "\n") {
		t.Errorf("the terminal was left with an empty line: %q", shown)
	}
	for line := range strings.SplitSeq(shown, "\n") {
		if strings.Count(line, "Paula: ") > 1 {
			t.Errorf("a line carries her name more than once: %q", line)
			break
		}
	}
}

// The README promises that two of them close the terminal. At a prompt the first
// one has no reply to stop, and the server answers it with a prompt of its own,
// which is no reason for the second one to count as a first.
func TestTwoCtrlCCloseATerminalAtAPrompt(t *testing.T) {
	inputs := make(chan api.Input, 4)
	f := open(t, dir(t), "")
	serving(t, f, reads(inputs))
	term := dial(t, f, true)

	term.waitFor(t, "the prompt", "> ")
	term.interrupts <- struct{}{}
	select {
	case in := <-inputs:
		if !in.Stop {
			t.Errorf("input = %+v, want the reply stopped", in)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Ctrl-C did nothing")
	}
	waitFor(t, "the prompt again", func() bool {
		return strings.Count(term.shown(), "> ") >= 2
	})
	term.interrupts <- struct{}{}

	select {
	case code := <-term.code:
		if code != 130 {
			t.Errorf("exit = %d, want the terminal left", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the second Ctrl-C left it running")
	}
}

// A server that says nothing back need not be killed to get out.
func TestASecondCtrlCLeavesAServerThatIsNotReading(t *testing.T) {
	f := open(t, dir(t), "")
	serving(t, f, func(ctx context.Context, _ api.Adapter) error {
		<-ctx.Done()
		return nil
	})
	term := dial(t, f, true)

	term.interrupts <- struct{}{}
	term.interrupts <- struct{}{}
	select {
	case code := <-term.code:
		if code != 130 {
			t.Errorf("exit = %d, want the terminal left", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the second Ctrl-C left it running")
	}
}

func TestAFileFedIn(t *testing.T) {
	inputs := make(chan api.Input, 4)
	f := open(t, dir(t), "")
	serving(t, f, reads(inputs))

	var out bytes.Buffer
	c := repl.Client{
		Socket: f.Socket(),
		In:     strings.NewReader("one\ntwo\n"),
		Out:    &out,
		Err:    &out,
	}
	// A client the server never asks for a line ends with the context rather
	// than holding the test open.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	code := c.Run(ctx)
	if code != 0 {
		t.Errorf("exit = %d, output %q", code, out.String())
	}

	var said []string
	for range 2 {
		select {
		case in := <-inputs:
			said = append(said, in.Text)
		default:
		}
	}
	if len(said) != 2 || said[0] != "one" || said[1] != "two" {
		t.Errorf("lines = %v, want both in order", said)
	}
	if !strings.Contains(out.String(), "> one\n> two\n") {
		t.Errorf("output = %q, want each line echoed", out.String())
	}
}

func TestAReplyArrivesAsItIsWritten(t *testing.T) {
	f := open(t, dir(t), "")
	serving(t, f, func(ctx context.Context, a api.Adapter) error {
		s := a.(api.Streamer)
		for _, piece := range []string{"hey", " you", ", how was it?"} {
			if err := s.Stream(ctx, piece); err != nil {
				return err
			}
		}
		return s.EndStream(ctx)
	})
	term := dial(t, f, true)

	term.waitFor(t, "the whole reply", "Paula: hey you, how was it?\n")
	if n := strings.Count(term.shown(), "Paula:"); n != 1 {
		t.Errorf("signed %d times:\n%q", n, term.shown())
	}
}

func TestASocketSomethingElseIsListeningOn(t *testing.T) {
	data := dir(t)
	f := open(t, data, "")
	serving(t, f, reads(make(chan api.Input, 4)))

	// A second one on the same socket says so rather than taking it over.
	again := open(t, data, "")
	err := again.Run(context.Background(), reads(make(chan api.Input, 4)))
	if err == nil {
		t.Fatal("the socket was taken over")
	}
	if !strings.Contains(err.Error(), "already listening on "+f.Socket()) {
		t.Errorf("error = %v", err)
	}

	// The one that has it still works.
	term := dial(t, f, true)
	term.waitFor(t, "the prompt", "> ")
}

// A paused terminal's connection ends with the context, so a run can stop while
// one is not reading.
func TestATerminalThatStoppedReadingDoesNotHoldTheReplUp(t *testing.T) {
	f := open(t, dir(t), "")
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var blocked atomic.Bool
	go func() {
		done <- f.Run(ctx, func(ctx context.Context, a api.Adapter) error {
			for {
				// A write that is still going when the next one would start is
				// one the terminal is not taking.
				writing := make(chan struct{})
				go func() {
					select {
					case <-writing:
					case <-time.After(100 * time.Millisecond):
						blocked.Store(true)
					}
				}()
				err := a.Send(ctx, api.Outgoing{Text: strings.Repeat("x", 4096)})
				close(writing)
				if err != nil {
					return nil
				}
			}
		})
	}()
	waitFor(t, "the socket", func() bool {
		fi, err := os.Stat(f.Socket())
		return err == nil && fi.Mode()&os.ModeSocket != 0
	})

	conn, err := net.Dial("unix", f.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(`{"hello":{"version":1,"interactive":true}}` + "\n")); err != nil {
		t.Fatal(err)
	}

	// Nothing is read from the connection, so the session fills it and blocks
	// inside a write it cannot finish.
	waitFor(t, "the terminal to stop taking what is written", func() bool {
		return blocked.Load()
	})

	stop()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the repl never stopped listening")
	}
}

// Both names come from the card, whatever they are.
func TestTheTerminalUsesTheNamesOnTheCard(t *testing.T) {
	data := dir(t)
	path := filepath.Join(t.TempDir(), "paula.yaml")
	file := "persona: ada.yaml\ndata_dir: " + data + "\nrunners:\n  r:\n    type: openrouter\n" +
		"models:\n  a:\n    runner: r\n    id: x\ndefault_models:\n  chat: a\nfrontends:\n  repl:\n"
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := repl.Open(cfg.Frontends[0].Section, api.Host{
		DataDir: cfg.DataDir,
		Names:   api.Names{Character: "Ada", User: "Tom"},
	})
	if err != nil {
		t.Fatal(err)
	}

	serving(t, f, func(ctx context.Context, a api.Adapter) error {
		if shower, ok := a.(api.HistoryShower); ok {
			err := shower.ShowHistory(ctx, []store.Message{
				{ID: 1, Role: store.RoleUser, Channel: "repl",
					Parts: []store.Part{{Type: store.PartText, Text: "hey"}}},
			})
			if err != nil {
				return err
			}
		}
		return a.Send(ctx, api.Outgoing{Text: "hey you", Hers: true})
	})
	term := dial(t, f, true)

	term.waitFor(t, "the reply", "Ada: hey you")
	if !strings.Contains(term.shown(), "Tom: hey") {
		t.Errorf("output = %q, want the name the card gives them", term.shown())
	}
}

// The line a reply is being written on is closed first, rather than the
// terminal's own words landing in the middle of it.
func TestWhatTheTerminalSaysStartsOnALineOfItsOwn(t *testing.T) {
	f := open(t, dir(t), "")
	serving(t, f, func(ctx context.Context, a api.Adapter) error {
		s := a.(api.Streamer)
		if err := s.Stream(ctx, "I think"); err != nil {
			return err
		}
		other := &store.Message{ID: 1, Role: store.RoleUser, Channel: "telegram",
			Parts: []store.Part{{Type: store.PartText, Text: "hey"}}}
		return a.(api.OtherChannels).ShowUserMessage(ctx, other)
	})
	term := dial(t, f, true)

	term.waitFor(t, "what the other terminal said", "[telegram] Caio: hey")
	if strings.Contains(term.shown(), "I think[telegram]") {
		t.Errorf("the line she was writing was not closed:\n%q", term.shown())
	}
	if !strings.Contains(term.shown(), "Paula: I think\n") {
		t.Errorf("output = %q", term.shown())
	}
}

// A terminal is told, rather than finding the socket closed under it.
func TestATerminalIsToldTheRunIsStopping(t *testing.T) {
	f := open(t, dir(t), "")
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.Run(ctx, reads(make(chan api.Input, 4))) }()
	waitFor(t, "the socket", func() bool {
		fi, err := os.Stat(f.Socket())
		return err == nil && fi.Mode()&os.ModeSocket != 0
	})

	term := dial(t, f, true)
	term.waitFor(t, "the prompt", "> ")
	stop()

	select {
	case code := <-term.code:
		if code != 1 {
			t.Errorf("exit = %d, want the terminal ended by the run", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the terminal never ended")
	}
	if !strings.Contains(term.shown(), "serve is stopping") {
		t.Errorf("output = %q, want the terminal told why", term.shown())
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the repl never stopped listening")
	}
}

// A line the terminal answered itself is echoed and the next one asked for, the
// same as a line she was given.
func TestALineTheTerminalAnsweredIsAskedForAgain(t *testing.T) {
	inputs := make(chan api.Input, 4)
	f := open(t, dir(t), "")
	serving(t, f, reads(inputs))
	term := dial(t, f, true)
	term.waitFor(t, "the prompt", "> ")

	term.type_("/image /nope/missing.jpg")
	term.waitFor(t, "the missing file", "no such file")
	waitFor(t, "the next prompt", func() bool { return strings.Count(term.shown(), "> ") == 2 })

	// A file fed in echoes the line it answered itself, and goes on.
	fed := open(t, dir(t), "socket: second.sock")
	serving(t, fed, reads(inputs))
	var out bytes.Buffer
	c := repl.Client{
		Socket: fed.Socket(),
		In:     strings.NewReader("/image /nope/missing.jpg\nhey\n"),
		Out:    writerTo(&terminal{out: &out}),
		Err:    writerTo(&terminal{out: &out}),
	}
	if code := c.Run(context.Background()); code != 0 {
		t.Errorf("exit = %d", code)
	}
	if !strings.Contains(out.String(), "> /image /nope/missing.jpg") {
		t.Errorf("output = %q, want the line it answered echoed", out.String())
	}
	waitFor(t, "the line after it", func() bool {
		select {
		case in := <-inputs:
			return in.Text == "hey"
		default:
			return false
		}
	})
}
