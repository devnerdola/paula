package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"nerdola.dev/x/paula/internal/store"
)

var when = time.Date(2026, 9, 16, 20, 22, 5, 123000000, time.UTC)

// filled is a data directory holding one reply, and one attempt at another
// that failed before it wrote anything.
func filled(t *testing.T) (string, store.EntryID, store.EntryID) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx := context.Background()
	mine := &store.Message{
		Role: store.RoleUser, Channel: "repl",
		Parts: []store.Part{
			{Type: store.PartText, Text: "what did my sister say"},
			{Type: store.PartImage, SHA256: "abc123", MIME: "image/jpeg"},
		},
		CreatedAt: when,
	}
	if err := s.AddMessage(ctx, mine); err != nil {
		t.Fatal(err)
	}

	entry := &store.Entry{
		Channel: "repl", UptoMessageID: mine.ID, StartedAt: when,
	}
	if err := s.StartEntry(ctx, entry); err != nil {
		t.Fatal(err)
	}

	req := &store.Request{
		EntryID: entry.ID, Purpose: store.PurposeReply, Runner: "openrouter",
		Model: "some/model", Method: "POST", URL: "https://example/v1/chat/completions",
		RequestHeaders: map[string][]string{"Authorization": {"[redacted]"}},
		RequestBody: []byte(`{"model":"some/model","reasoning":{"enabled":true,"effort":"high"},` +
			`"messages":[{"role":"system","content":"you are a character"},` +
			`{"role":"user","content":[{"type":"text","text":"what did my sister say"},` +
			`{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,AQIDBA=="}}]}]}`),
		StartedAt: when,
	}
	if err := s.AddRequest(ctx, req); err != nil {
		t.Fatal(err)
	}
	req.Status = 200
	req.ResponseHeaders = map[string][]string{"Content-Type": {"text/event-stream"}}
	req.ResponseBody = []byte("data: [DONE]\n\n")
	req.Attempts = []store.Attempt{{StartedAt: when, EndedAt: when.Add(time.Second), Status: 200}}
	req.FirstByteAt = when.Add(880 * time.Millisecond)
	req.EndedAt = when.Add(2 * time.Second)
	req.Provider = "Novita"
	req.FinishReason = "stop"
	req.Usage = &store.Usage{PromptTokens: 20514, CachedTokens: 19968, CompletionTokens: 210, ReasoningTokens: 180}
	req.Cost = 0.0021
	if err := s.EndRequest(ctx, req); err != nil {
		t.Fatal(err)
	}

	reply := &store.Message{
		Role: store.RoleAssistant, Channel: "repl",
		Parts:     []store.Part{{Type: store.PartText, Text: "she said she is coming over"}},
		Reasoning: "she asked about Ana",
		ReplyTo:   mine.ID, EntryID: entry.ID, CreatedAt: when.Add(2 * time.Second),
	}
	if err := s.AddMessage(ctx, reply); err != nil {
		t.Fatal(err)
	}
	entry.Status = store.StatusDone
	entry.EndedAt = when.Add(2 * time.Second)
	if err := s.EndEntry(ctx, entry); err != nil {
		t.Fatal(err)
	}

	// A second attempt, which looked at the photo and then failed before the
	// reply itself was sent.
	failed := &store.Entry{
		Channel: "repl", AfterMessageID: mine.ID,
		UptoMessageID: mine.ID, StartedAt: when.Add(time.Minute),
	}
	if err := s.StartEntry(ctx, failed); err != nil {
		t.Fatal(err)
	}
	failedReq := &store.Request{
		EntryID: failed.ID, Purpose: store.PurposeCaption, Runner: "venice", Model: "other/model",
		Method: "POST", URL: "https://example/v1/chat/completions",
		RequestBody: []byte(`{"model":"other/model","messages":[{"role":"user","content":` +
			`[{"type":"text","text":"Describe this image in one or two sentences."}]}]}`),
		StartedAt: when.Add(time.Minute),
	}
	if err := s.AddRequest(ctx, failedReq); err != nil {
		t.Fatal(err)
	}
	failedReq.Status = 502
	failedReq.Error = "502: bad gateway"
	failedReq.EndedAt = when.Add(time.Minute + time.Second)
	if err := s.EndRequest(ctx, failedReq); err != nil {
		t.Fatal(err)
	}
	failed.Status = store.StatusFailed
	failed.Error = "502: bad gateway"
	failed.EndedAt = when.Add(time.Minute + time.Second)
	if err := s.EndEntry(ctx, failed); err != nil {
		t.Fatal(err)
	}

	return dir, entry.ID, failed.ID
}

func turnsConfig(t *testing.T, dir string) string {
	t.Helper()
	return configFile(t, "data_dir: "+dir+
		"\nrunners:\n  r:\n    type: openrouter\nmodels:\n  a:\n    runner: r\n    id: x\ndefault_models:\n  chat: a\n")
}

func TestTurnsList(t *testing.T) {
	dir, entry, failed := filled(t)
	code, out, errOut := exec(t, "-config", turnsConfig(t, dir), "turns")
	if code != 0 {
		t.Fatalf("code = %d, stderr %s", code, errOut)
	}

	header := line(t, out, "ID ")
	for _, want := range []string{"TIME", "STATUS", "MODELS", "REQUESTS",
		"PROMPT", "CACHED", "COMPLETION", "REASONING", "COST", "DURATION"} {
		if !strings.Contains(header, want) {
			t.Errorf("the header has no %q: %s", want, header)
		}
	}

	// The newest entry comes first.
	rows := strings.Split(strings.TrimSpace(out), "\n")
	if !strings.HasPrefix(rows[1], strconv.FormatInt(int64(failed), 10)+" ") {
		t.Errorf("the first row is %q, want the latest entry", rows[1])
	}

	row := line(t, out, strconv.FormatInt(int64(entry), 10)+" ")
	for _, want := range []string{"done", "some/model", "20514", "19968", "210", "180", "$0.002100"} {
		if !strings.Contains(row, want) {
			t.Errorf("the reply row has no %q: %s", want, row)
		}
	}
}

func TestTurnsSummary(t *testing.T) {
	dir, entry, _ := filled(t)
	code, out, errOut := exec(t, "-config", turnsConfig(t, dir), "turns", strconv.FormatInt(int64(entry), 10))
	if code != 0 {
		t.Fatalf("code = %d, stderr %s", code, errOut)
	}

	for _, want := range []string{
		"entry " + strconv.FormatInt(int64(entry), 10) + "  done  repl",
		"started   2026-09-16",
		"answers   messages 1",
		"reply     message 2",
		"cost      $0.002100",
		"settings  reasoning on, effort high",
		"--- system",
		"you are a character",
		"--- user",
		"what did my sister say",
		// The four bytes of the picture, not the six of the base64 it went as.
		"image image/jpeg, 4 bytes",
		"--- reasoning",
		"she asked about Ana",
		"--- reply, message 2",
		"she said she is coming over",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output has no %q:\n%s", want, out)
		}
	}

	// Every request of the entry is listed, with what it cost.
	req := line(t, out, "1 ")
	for _, want := range []string{"reply", "openrouter", "some/model", "Novita", "200", "stop"} {
		if !strings.Contains(req, want) {
			t.Errorf("the request row has no %q: %s", want, req)
		}
	}
}

func TestTurnsOfAnEntryWithNoReply(t *testing.T) {
	dir, _, failed := filled(t)
	code, out, _ := exec(t, "-config", turnsConfig(t, dir), "turns", strconv.FormatInt(int64(failed), 10))
	if code != 0 {
		t.Fatalf("code = %d:\n%s", code, out)
	}
	if !strings.Contains(out, "entry "+strconv.FormatInt(int64(failed), 10)+"  failed") {
		t.Errorf("output = %s", out)
	}
	if !strings.Contains(out, "--- request 1, caption") {
		t.Errorf("the request's own prompt is not shown:\n%s", out)
	}
	if !strings.Contains(out, "Describe this image") {
		t.Errorf("output = %s", out)
	}
}

func TestTurnsDump(t *testing.T) {
	dir, entry, _ := filled(t)
	code, out, errOut := exec(t, "-config", turnsConfig(t, dir), "turns", "-dump", strconv.FormatInt(int64(entry), 10))
	if code != 0 {
		t.Fatalf("code = %d, stderr %s", code, errOut)
	}

	for _, want := range []string{
		"== message 1  user  repl",
		"image abc123 image/jpeg",
		"== request 1  reply  openrouter  some/model",
		"POST https://example/v1/chat/completions",
		"attempt 1  started ",
		"request headers",
		"Authorization: [redacted]",
		"request body",
		`"model":"some/model"`,
		"response headers",
		"Content-Type: text/event-stream",
		"response body",
		"data: [DONE]",
		"provider Novita  finish stop  prompt 20514  cached 19968",
		"== message 2  assistant  repl",
		"reasoning",
		"text",
		"she said she is coming over",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output has no %q:\n%s", want, out)
		}
	}
}

func TestTurnsShowsWhenBodiesWerePruned(t *testing.T) {
	dir, entry, _ := filled(t)
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Prune(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	s.Close()

	cfg := turnsConfig(t, dir)
	if _, out, _ := exec(t, "-config", cfg, "turns", strconv.FormatInt(int64(entry), 10)); !strings.Contains(out, "bodies pruned (engine.log_keep)") {
		t.Errorf("the entry does not say the bodies are gone:\n%s", out)
	}
	_, out, _ := exec(t, "-config", cfg, "turns", "-dump", strconv.FormatInt(int64(entry), 10))
	if !strings.Contains(out, "bodies pruned (engine.log_keep)") {
		t.Errorf("output does not say the bodies are gone:\n%s", out)
	}
	if strings.Contains(out, "you are a character") {
		t.Errorf("a body that was pruned is still shown:\n%s", out)
	}
	// What the request cost is kept.
	if !strings.Contains(out, "prompt 20514") {
		t.Errorf("the summary of a pruned request was lost:\n%s", out)
	}
}

func TestTurnsOfAnEntryThatIsNotThere(t *testing.T) {
	dir, _, _ := filled(t)
	code, _, errOut := exec(t, "-config", turnsConfig(t, dir), "turns", "99")
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(errOut, "not found") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestTurnsUsage(t *testing.T) {
	dir, _, _ := filled(t)
	cfg := turnsConfig(t, dir)
	if code, _, _ := exec(t, "-config", cfg, "turns", "nope"); code != 2 {
		t.Error("an entry that is not a number was taken")
	}
	if code, _, _ := exec(t, "-config", cfg, "turns", "-dump"); code != 2 {
		t.Error("-dump with no entry was taken")
	}
	if code, _, _ := exec(t, "-config", cfg, "turns", "1", "2"); code != 2 {
		t.Error("two entries at once were taken")
	}
}

func TestTurnsLimit(t *testing.T) {
	dir, _, failed := filled(t)
	_, out, _ := exec(t, "-config", turnsConfig(t, dir), "turns", "-n", "1")
	rows := strings.Split(strings.TrimSpace(out), "\n")
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want the header and one entry:\n%s", len(rows), out)
	}
	if !strings.HasPrefix(rows[1], strconv.FormatInt(int64(failed), 10)+" ") {
		t.Errorf("row = %q, want the newest entry", rows[1])
	}
}

func TestTurnsWithoutADatabase(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := exec(t, "-config", turnsConfig(t, dir), "turns")
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(errOut, "holds no conversation yet") {
		t.Errorf("stderr = %q", errOut)
	}
	if _, err := os.Stat(filepath.Join(dir, store.File)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a database was left behind: %v", err)
	}
}

func TestTurnsCountMustBeACount(t *testing.T) {
	dir, _, _ := filled(t)
	code, out, _ := exec(t, "-config", turnsConfig(t, dir), "turns", "-n", "-1")
	if code != 2 {
		t.Errorf("code = %d, want the count to be refused; output %q", code, out)
	}
}

func TestTurnsOfARequestThatCarriedNoBody(t *testing.T) {
	dir, _, _ := filled(t)
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	entry := &store.Entry{StartedAt: when}
	if err := s.StartEntry(ctx, entry); err != nil {
		t.Fatal(err)
	}
	req := &store.Request{
		EntryID: entry.ID, Purpose: store.PurposeCaption, Runner: "venice",
		Method: "GET", URL: "https://example/v1/models", StartedAt: when,
	}
	if err := s.AddRequest(ctx, req); err != nil {
		t.Fatal(err)
	}
	req.Status = 200
	req.ResponseBody = []byte(`{"data":[]}`)
	req.EndedAt = when.Add(time.Second)
	if err := s.EndRequest(ctx, req); err != nil {
		t.Fatal(err)
	}
	entry.Status = store.StatusDone
	entry.EndedAt = req.EndedAt
	if err := s.EndEntry(ctx, entry); err != nil {
		t.Fatal(err)
	}
	s.Close()

	_, out, _ := exec(t, "-config", turnsConfig(t, dir), "turns", strconv.FormatInt(int64(entry.ID), 10))
	if strings.Contains(out, "bodies pruned") {
		t.Errorf("a request that carried no body is called pruned:\n%s", out)
	}
}
