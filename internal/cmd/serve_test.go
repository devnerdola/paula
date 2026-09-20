package cmd

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nerdola.dev/x/paula/internal/store"
)

// shortDir is a data directory under /tmp, since the socket the repl listens on
// goes inside it and a socket path is held to about a hundred bytes.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "paula")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestTheDataDirectoryIsHeldByOneRun(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")

	unlock, err := lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lock(dir); err == nil {
		t.Fatal("the directory was held twice")
	} else if !strings.Contains(err.Error(), "serve is already running on "+dir) {
		t.Errorf("error = %v", err)
	}

	// What one run leaves is free for the next.
	unlock()
	again, err := lock(dir)
	if err != nil {
		t.Fatalf("the directory stayed held: %v", err)
	}
	again()
}

func TestServeSaysWhenAnotherRunHoldsTheDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	unlock, err := lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	// The runner is opened, which needs the key, but never asked anything: the
	// directory is taken before a listing is read.
	t.Setenv("OPENROUTER_API_KEY", "test-token-abcdefgh")
	cfg := configFile(t, `
persona: `+card(t, "ada.yaml")+`
data_dir: `+dir+`
runners:
  openrouter:
    type: openrouter
models:
  talk:
    runner: openrouter
    id: deepseek/deepseek-v4-pro-0813
default_models:
  chat: talk
`)
	code, _, errOut := exec(t, "-config", cfg, "serve")
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(errOut, "serve is already running on "+dir) {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestReplSaysWhenNothingIsServing(t *testing.T) {
	dir := shortDir(t)
	t.Setenv("OPENROUTER_API_KEY", "test-token-abcdefgh")
	cfg := configFile(t, `
data_dir: `+dir+`
runners:
  openrouter:
    type: openrouter
models:
  talk:
    runner: openrouter
    id: x
default_models:
  chat: talk
frontends:
  repl:
`)
	code, _, errOut := exec(t, "-config", cfg, "repl")
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(errOut, "nothing is listening on ") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestServeAndReplTakeNoArguments(t *testing.T) {
	for _, name := range []string{"serve", "repl"} {
		if code, _, _ := exec(t, name, "extra"); code != 2 {
			t.Errorf("%s took an argument", name)
		}
	}
}

// waitFor gives a run a moment to reach what a test is about to check.
func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for range 400 {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// typed is what a terminal types, which the repl command reads the way it reads
// a file being fed in: a line at a time, when the run is ready for one.
func typed(lines ...string) io.Reader {
	var b strings.Builder
	for _, line := range lines {
		b.WriteString(line + "\n")
	}
	return strings.NewReader(b.String())
}

// The API answers what OpenRouter answered when the fixtures were captured, the
// repl command talks to serve over the socket it listens on, and what the
// terminal showed is held against what the database kept.
func TestAConversationFromEndToEnd(t *testing.T) {
	dir := shortDir(t)
	ts := openrouterCatalogue(t)
	t.Setenv("OPENROUTER_API_KEY", "test-token-abcdefgh")
	cfg := configFile(t, `
data_dir: `+dir+`
runners:
  openrouter:
    type: openrouter
    url: `+ts.URL+`/v1
models:
  talk:
    runner: openrouter
    id: deepseek/deepseek-v4-pro-0813
default_models:
  chat: talk
engine:
  debounce: 10ms
frontends:
  repl:
`)

	// The run is stopped the way an interrupt stops it: by the context the
	// command was given.
	serving, stop := context.WithCancel(context.Background())
	defer stop()
	served := make(chan int, 1)
	go func() {
		code, _, _ := execWith(t, serving, typed(), "-config", cfg, "serve")
		served <- code
	}()
	socket := filepath.Join(dir, "paula.sock")
	waitFor(t, "the repl to listen", func() bool {
		c, err := net.Dial("unix", socket)
		if err != nil {
			return false
		}
		c.Close()
		return true
	})

	code, out, errOut := execWith(t, context.Background(), typed("hey"), "-config", cfg, "repl")
	if code != 0 {
		t.Fatalf("repl = %d, %q", code, errOut)
	}
	if !strings.Contains(out, "> hey") {
		t.Errorf("the terminal did not echo the line:\n%s", out)
	}
	// She is called what the card calls her.
	if !strings.Contains(out, "Ada: Hi, I'm here.") {
		t.Errorf("the reply did not reach the terminal:\n%s", out)
	}

	stop()
	select {
	case code := <-served:
		if code != 0 {
			t.Errorf("serve = %d, want it to end cleanly", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop")
	}

	// What the terminal showed is what the conversation kept.
	s, err := store.Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	messages, err := s.Messages(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 {
		t.Fatalf("the conversation holds %d messages, want what was said and the reply", len(messages))
	}
	if messages[0].Text() != "hey" || messages[0].Role != store.RoleUser {
		t.Errorf("message = %+v", messages[0])
	}
	if messages[1].Text() != "Hi, I'm here." || messages[1].Role != store.RoleAssistant {
		t.Errorf("reply = %+v", messages[1])
	}
	if messages[1].ReplyTo != messages[0].ID {
		t.Errorf("the reply answers %d, want %d", messages[1].ReplyTo, messages[0].ID)
	}

	entries, err := s.Entries(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Status != store.StatusDone {
		t.Fatalf("entries = %+v, want one that ended done", entries)
	}
	requests, err := s.Requests(ctx, entries[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 || requests[0].Status != 200 || requests[0].Provider == "" {
		t.Errorf("requests = %+v, want the one that was sent, as it came back", requests)
	}
}

// A run with nothing listening is a run nobody can reach.
func TestServeEndsWhenAFrontendCannotServe(t *testing.T) {
	dir := shortDir(t)

	// Something else holds the socket the repl would listen on.
	taken, err := net.Listen("unix", filepath.Join(dir, "paula.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()

	ts := openrouterCatalogue(t)
	t.Setenv("OPENROUTER_API_KEY", "test-token-abcdefgh")
	cfg := configFile(t, `
persona: `+card(t, "ada.yaml")+`
data_dir: `+dir+`
runners:
  openrouter:
    type: openrouter
    url: `+ts.URL+`/v1
models:
  talk:
    runner: openrouter
    id: deepseek/deepseek-v4-pro-0813
default_models:
  chat: talk
frontends:
  repl:
`)
	done := make(chan int, 1)
	go func() {
		code, _, _ := exec(t, "-config", cfg, "serve")
		done <- code
	}()
	select {
	case code := <-done:
		if code != 1 {
			t.Errorf("code = %d, want 1", code)
		}
	case <-time.After(10 * time.Second):
		t.Error("serve kept running with nothing listening")
	}
}
