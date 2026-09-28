package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

var now = time.Date(2026, 9, 16, 20, 22, 5, 0, time.UTC)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpenStampsTheDatabase(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	var appID, version, keys int
	var mode string
	if err := s.db.QueryRow(`PRAGMA application_id`).Scan(&appID); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`PRAGMA foreign_keys`).Scan(&keys); err != nil {
		t.Fatal(err)
	}
	if appID != 0x5041554C {
		t.Errorf("application_id = %#x, want PAUL", appID)
	}
	// The stamp is how far the migrations Paula carries have run, whatever
	// number that has reached.
	if want := schemaVersion(migrations); version != want {
		t.Errorf("user_version = %d, want %d", version, want)
	}
	if !strings.EqualFold(mode, "wal") {
		t.Errorf("journal_mode = %q, want wal", mode)
	}
	if keys != 1 {
		t.Error("foreign_keys is off")
	}
	s.Close()

	again, err := Open(dir)
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	again.Close()

	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("data dir mode = %v, want 0700", fi.Mode().Perm())
	}
}

func TestOpenRefusesAnotherDatabase(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, File))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE messages (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	_, err = Open(dir)
	if err == nil {
		t.Fatal("Open succeeded")
	}
	if !strings.Contains(err.Error(), "point data_dir at a new directory") {
		t.Errorf("error = %q", err)
	}
}

func TestMessagesRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	m := &Message{
		Role:    RoleUser,
		Channel: "telegram",
		Parts: []Part{
			{Type: PartText, Text: "look at this"},
			{Type: PartImage, SHA256: "abc", MIME: "image/jpeg"},
		},
		CreatedAt: now,
	}
	if err := s.AddMessage(ctx, m); err != nil {
		t.Fatal(err)
	}
	if m.ID == 0 {
		t.Fatal("no id was filled in")
	}

	got, err := s.Message(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Text() != "look at this" {
		t.Errorf("text = %q", got.Text())
	}
	if len(got.Images()) != 1 || got.Images()[0].SHA256 != "abc" {
		t.Errorf("images = %+v", got.Images())
	}
	if got.Channel != "telegram" || !got.CreatedAt.Equal(now) {
		t.Errorf("message = %+v", got)
	}
	if got.ReasoningDetails != nil {
		t.Errorf("a message that thought nothing reads back with %s", got.ReasoningDetails)
	}

	// The details are what OpenRouter sent of a reply that asked for a tool,
	// in the one item its fragments make.
	detail := json.RawMessage(`{"type":"reasoning.text","text":"I need to search memories for Caio's sister's name.","format":"unknown","index":0}`)
	reply := &Message{
		Role: RoleAssistant, Channel: "telegram",
		Parts:            []Part{{Type: PartText, Text: "nice"}},
		Reasoning:        "thinking",
		ReasoningDetails: []json.RawMessage{detail},
		Interrupted:      true,
		ReplyTo:          m.ID,
		CreatedAt:        now.Add(time.Second),
	}
	if err := s.AddMessage(ctx, reply); err != nil {
		t.Fatal(err)
	}
	got, err = s.Message(ctx, reply.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Reasoning != "thinking" || !got.Interrupted || got.ReplyTo != m.ID {
		t.Errorf("reply = %+v", got)
	}
	if len(got.ReasoningDetails) != 1 || !bytes.Equal(got.ReasoningDetails[0], detail) {
		t.Errorf("the details read back as %s, want them as they were kept", got.ReasoningDetails)
	}

	if _, err := s.Message(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("Message of a missing id = %v, want ErrNotFound", err)
	}
}

func TestMessageListing(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	var ids []MessageID
	for i := range 5 {
		m := &Message{
			Role:      RoleUser,
			Parts:     []Part{{Type: PartText, Text: string(rune('a' + i))}},
			CreatedAt: now.Add(time.Duration(i) * time.Minute),
		}
		if err := s.AddMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, m.ID)
	}

	got, err := s.Messages(ctx, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].ID != ids[2] || got[2].ID != ids[4] {
		t.Errorf("newest three = %v", messageIDs(got))
	}

	got, err = s.Messages(ctx, ids[2], 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != ids[0] {
		t.Errorf("older than the third = %v", messageIDs(got))
	}

	got, err = s.MessagesAfter(ctx, ids[2])
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != ids[3] {
		t.Errorf("after the third = %v", messageIDs(got))
	}

	last, err := s.LastMessage(ctx, RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	if last.ID != ids[4] {
		t.Errorf("last user message = %d, want %d", last.ID, ids[4])
	}
	if _, err := s.LastMessage(ctx, RoleAssistant); !errors.Is(err, ErrNotFound) {
		t.Errorf("last reply = %v, want ErrNotFound", err)
	}
}

func messageIDs(ms []Message) []MessageID {
	var out []MessageID
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}

func TestWhatAPictureShowsIsKeptWithIt(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	if err := s.AddMedia(ctx, "sha-a"); err != nil {
		t.Fatal(err)
	}
	// The same image again keeps what is already known about it.
	if err := s.SetCaption(ctx, "sha-a", "a cat"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMedia(ctx, "sha-a"); err != nil {
		t.Fatal(err)
	}
	m, err := s.Media(ctx, "sha-a")
	if err != nil {
		t.Fatal(err)
	}
	if m.Caption != "a cat" {
		t.Errorf("media = %+v, want the caption kept", m)
	}
	if _, err := s.Media(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Media of a missing sha = %v", err)
	}

	msg := &Message{
		Role:      RoleUser,
		Parts:     []Part{{Type: PartImage, SHA256: "sha-a", MIME: "image/jpeg"}},
		CreatedAt: now,
	}
	if err := s.AddMessage(ctx, msg); err != nil {
		t.Fatal(err)
	}

	// What went wrong is kept, and a caption clears it.
	if err := s.SetCaptionError(ctx, "sha-a", "the host said no"); err != nil {
		t.Fatal(err)
	}
	m, err = s.Media(ctx, "sha-a")
	if err != nil {
		t.Fatal(err)
	}
	if m.CaptionError != "the host said no" {
		t.Errorf("media = %+v, want why it failed", m)
	}

	if err := s.SetCaption(ctx, "sha-a", "a red square"); err != nil {
		t.Fatal(err)
	}
	m, err = s.Media(ctx, "sha-a")
	if err != nil {
		t.Fatal(err)
	}
	if m.Caption != "a red square" || m.CaptionError != "" {
		t.Errorf("media = %+v, want the failure cleared", m)
	}
}

// The pictures of the conversation are every picture a message carried, newest
// first, each with its number, its message, when that was sent and what it
// showed. A picture sent twice is one file under one number, and two pictures.
func TestThePicturesOfTheConversationAreWhatItsMessagesCarried(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	images, err := s.Images(ctx)
	if err != nil || len(images) != 0 {
		t.Fatalf("images of a conversation with none = %+v, %v", images, err)
	}
	if _, err := s.Image(ctx, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("an image of a conversation with none = %v, want not found", err)
	}

	for _, sha := range []string{"sha-a", "sha-b"} {
		if err := s.AddMedia(ctx, sha); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetCaption(ctx, "sha-a", "a red square"); err != nil {
		t.Fatal(err)
	}
	sent := func(at time.Time, parts ...Part) MessageID {
		t.Helper()
		m := &Message{Role: RoleUser, Parts: parts, CreatedAt: at}
		if err := s.AddMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
		return m.ID
	}
	first := sent(now, Part{Type: PartText, Text: "look"}, Part{Type: PartImage, SHA256: "sha-a", MIME: "image/jpeg"})
	sent(now.Add(time.Minute), Part{Type: PartText, Text: "nothing to see"})
	again := sent(now.Add(2*time.Minute),
		Part{Type: PartImage, SHA256: "sha-b", MIME: "image/jpeg"},
		Part{Type: PartImage, SHA256: "sha-a", MIME: "image/jpeg"})

	images, err = s.Images(ctx)
	if err != nil {
		t.Fatal(err)
	}
	type seen struct {
		sha     string
		message MessageID
		caption string
	}
	var got []seen
	for _, img := range images {
		got = append(got, seen{img.SHA256, img.MessageID, img.Caption})
	}
	want := []seen{{"sha-b", again, ""}, {"sha-a", again, "a red square"}, {"sha-a", first, "a red square"}}
	if !slices.Equal(got, want) {
		t.Errorf("images = %+v, want %+v", got, want)
	}
	if images[1].ID != images[2].ID || images[0].ID == images[1].ID {
		t.Errorf("numbers = %d, %d, %d, want one for each file", images[0].ID, images[1].ID, images[2].ID)
	}

	// A number is the picture as it was first sent.
	img, err := s.Image(ctx, images[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if img.MessageID != first || !img.SentAt.Equal(now) || img.Caption != "a red square" {
		t.Errorf("image = %+v, want the one sent first", img)
	}
}

// A picture she took is found by its number before any message carries it, and
// only a picture she took is. Once her message carries it, it is one of the
// pictures of the conversation, and says she sent it.
func TestAPhotoSheTookIsFoundByItsNumberBeforeItIsSent(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	e := &Entry{StartedAt: now}
	if err := s.StartEntry(ctx, e); err != nil {
		t.Fatal(err)
	}
	r := &Request{EntryID: e.ID, Purpose: PurposeReply, Runner: "venice",
		Method: "POST", URL: "https://api.venice.ai/api/v1/chat/completions", StartedAt: now}
	if err := s.AddRequest(ctx, r); err != nil {
		t.Fatal(err)
	}
	call := &ToolCall{EntryID: e.ID, RequestID: r.ID, CallID: "call_1",
		Name: "take_photo", Arguments: `{"prompt":"a selfie at the window"}`, StartedAt: now}
	if err := s.StartToolCall(ctx, call); err != nil {
		t.Fatal(err)
	}

	if err := s.AddMedia(ctx, "sha-sent"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMessage(ctx, &Message{Role: RoleUser, CreatedAt: now,
		Parts: []Part{{Type: PartImage, SHA256: "sha-sent", MIME: "image/jpeg"}}}); err != nil {
		t.Fatal(err)
	}
	id, err := s.AddPhoto(ctx, "sha-taken", call.ID)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := s.Photo(ctx, id); err != nil || m.SHA256 != "sha-taken" {
		t.Errorf("the photo she took = %+v, %v", m, err)
	}
	images, err := s.Images(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 1 {
		t.Fatalf("images = %+v, want only the one a message carried", images)
	}
	if _, err := s.Photo(ctx, images[0].ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("a picture sent to her as a photo she took = %v, want not found", err)
	}

	if err := s.AddMessage(ctx, &Message{Role: RoleAssistant, CreatedAt: now.Add(time.Minute),
		Parts: []Part{{Type: PartText, Text: "me right now"}, {Type: PartImage, SHA256: "sha-taken", MIME: "image/jpeg"}}}); err != nil {
		t.Fatal(err)
	}
	images, err = s.Images(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 2 || images[0].ID != id || images[0].Role != RoleAssistant || images[1].Role != RoleUser {
		t.Errorf("images = %+v, want hers first, then the one sent to her", images)
	}
}

// A picture is looked up by the number the model was told it by, so a database
// brought up to date keeps every picture's number, the gap a picture left
// behind it included.
func TestAPictureKeepsItsNumberWhenTheDatabaseIsBroughtUpToDate(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := openStore(dir, true, migrations[:5])
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`INSERT INTO media (sha256, caption) VALUES ('sha-a', 'a red square'), ('sha-b', ''), ('sha-c', 'a blue circle')`,
		`DELETE FROM media WHERE sha256 = 'sha-b'`,
		`INSERT INTO messages (role, parts_json, created_at) VALUES ('user',
			'[{"type":"image","sha256":"sha-a","mime":"image/jpeg"},{"type":"image","sha256":"sha-c","mime":"image/jpeg"}]', 1)`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()

	s, err = openStore(dir, true, migrations)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for number, caption := range map[int64]string{1: "a red square", 3: "a blue circle"} {
		img, err := s.Image(ctx, number)
		if err != nil {
			t.Fatalf("picture %d: %v", number, err)
		}
		if img.Caption != caption {
			t.Errorf("picture %d shows %q, want %q", number, img.Caption, caption)
		}
	}
}

// The model serving a role is the one thing the kv table holds: what is put
// there is read back, what is written twice keeps the last of it, and what is
// taken away is gone.
func TestWhatIsKeptUnderAKey(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	if _, ok, err := s.Get(ctx, "nope"); err != nil || ok {
		t.Errorf("Get of a missing key = %v, %v", ok, err)
	}
	if err := s.Set(ctx, KeyModel("chat"), "fast"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(ctx, KeyModel("chat"), "slow"); err != nil {
		t.Fatal(err)
	}
	v, ok, err := s.Get(ctx, KeyModel("chat"))
	if err != nil || !ok || v != "slow" {
		t.Errorf("Get = %q, %v, %v", v, ok, err)
	}
	if err := s.Delete(ctx, KeyModel("chat")); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Get(ctx, KeyModel("chat")); ok {
		t.Error("the key is still there")
	}
}

func TestAnEntryOnlyPointsAtMessagesThatAreThere(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	e := &Entry{StartedAt: now}
	if err := s.StartEntry(ctx, e); err != nil {
		t.Fatal(err)
	}
	e.Status = StatusDone
	e.UptoMessageID = 404
	if err := s.EndEntry(ctx, e); err == nil {
		t.Error("an entry was closed on a message that does not exist")
	}
}

func TestAnEntryIsStartedAndEnded(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	e := &Entry{Channel: "repl", StartedAt: now}
	if err := s.StartEntry(ctx, e); err != nil {
		t.Fatal(err)
	}
	if e.ID == 0 || e.Status != StatusRunning {
		t.Fatalf("entry = %+v", e)
	}

	running, err := s.RunningEntries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(running) != 1 || running[0].ID != e.ID {
		t.Errorf("running = %+v", running)
	}

	asked := &Message{Role: RoleUser,
		Parts: []Part{{Type: PartText, Text: "hey"}}, CreatedAt: now}
	if err := s.AddMessage(ctx, asked); err != nil {
		t.Fatal(err)
	}
	answer := &Message{Role: RoleAssistant,
		Parts: []Part{{Type: PartText, Text: "hey you"}}, CreatedAt: now}
	if err := s.AddMessage(ctx, answer); err != nil {
		t.Fatal(err)
	}

	e.Status = StatusDone
	e.UptoMessageID = asked.ID
	e.EndedAt = now.Add(14 * time.Second)
	if err := s.EndEntry(ctx, e); err != nil {
		t.Fatal(err)
	}

	got, err := s.Entry(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusDone || got.UptoMessageID != asked.ID {
		t.Errorf("entry = %+v", got)
	}
	if !got.EndedAt.Equal(e.EndedAt) || got.Channel != "repl" {
		t.Errorf("entry = %+v", got)
	}
	if running, _ := s.RunningEntries(ctx); len(running) != 0 {
		t.Errorf("still running = %+v", running)
	}

	second := &Entry{StartedAt: now.Add(time.Minute)}
	if err := s.StartEntry(ctx, second); err != nil {
		t.Fatal(err)
	}
	list, err := s.Entries(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != second.ID {
		t.Errorf("entries = %+v, want the newest first", list)
	}
	if _, err := s.Entry(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("Entry of a missing id = %v", err)
	}
}

func TestARequestIsKeptUnderItsEntry(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	e := &Entry{StartedAt: now}
	if err := s.StartEntry(ctx, e); err != nil {
		t.Fatal(err)
	}

	r := &Request{
		EntryID: e.ID,
		Purpose: PurposeReply,
		Runner:  "openrouter",
		Model:   "some/model",
		Method:  "POST",
		URL:     "https://openrouter.ai/api/v1/chat/completions",
		RequestHeaders: map[string][]string{
			"Content-Type":  {"application/json"},
			"Authorization": {"[redacted]"},
		},
		RequestBody: []byte(`{"model":"some/model"}`),
		StartedAt:   now,
	}
	if err := s.AddRequest(ctx, r); err != nil {
		t.Fatal(err)
	}
	r.Attempts = []Attempt{
		{StartedAt: now, Status: 429, RetryAfter: time.Second},
		{StartedAt: now.Add(time.Second), FirstByteAt: now.Add(2 * time.Second), EndedAt: now.Add(3 * time.Second), Status: 200},
	}
	r.Status = 200
	r.ResponseHeaders = map[string][]string{"Content-Type": {"text/event-stream"}}
	r.ResponseBody = []byte("data: {}\n\ndata: [DONE]\n\n")
	r.FirstByteAt = now.Add(2 * time.Second)
	r.EndedAt = now.Add(3 * time.Second)
	r.Provider = "Novita"
	r.FinishReason = "stop"
	r.Usage = &Usage{PromptTokens: 20514, CachedTokens: 19968, CompletionTokens: 210, ReasoningTokens: 180}
	r.Cost = 0.0021
	if err := s.EndRequest(ctx, r); err != nil {
		t.Fatal(err)
	}

	second := &Request{EntryID: e.ID, Purpose: PurposeCaption, Runner: "venice", Method: "POST", URL: "https://x", StartedAt: now}
	if err := s.AddRequest(ctx, second); err != nil {
		t.Fatal(err)
	}
	list, err := s.Requests(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("requests = %d", len(list))
	}
	// They come back in the order they were sent.
	if list[0].Purpose != PurposeReply || list[1].Purpose != PurposeCaption {
		t.Errorf("requests = %q, %q, want the reply first", list[0].Purpose, list[1].Purpose)
	}
	got := list[0]
	if string(got.RequestBody) != `{"model":"some/model"}` {
		t.Errorf("request body = %q", got.RequestBody)
	}
	if string(got.ResponseBody) != "data: {}\n\ndata: [DONE]\n\n" {
		t.Errorf("response body = %q", got.ResponseBody)
	}
	if got.RequestHeaders.Get("Authorization") != "[redacted]" {
		t.Errorf("headers = %v", got.RequestHeaders)
	}
	if got.ResponseHeaders.Get("Content-Type") != "text/event-stream" {
		t.Errorf("response headers = %v", got.ResponseHeaders)
	}
	if len(got.Attempts) != 2 || got.Attempts[0].Status != 429 || got.Attempts[0].RetryAfter != time.Second {
		t.Errorf("attempts = %+v", got.Attempts)
	}
	if !got.Attempts[1].FirstByteAt.Equal(now.Add(2 * time.Second)) {
		t.Errorf("attempt times = %+v", got.Attempts[1])
	}
	if got.Usage == nil || got.Usage.CachedTokens != 19968 || got.Provider != "Novita" {
		t.Errorf("request = %+v", got)
	}
	if got.Pruned {
		t.Error("a fresh request reads as pruned")
	}
}

func TestPruneKeepsTheLatestEntries(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	var entries []*Entry
	for i := range 5 {
		e := &Entry{StartedAt: now.Add(time.Duration(i) * time.Minute)}
		if err := s.StartEntry(ctx, e); err != nil {
			t.Fatal(err)
		}
		r := &Request{
			EntryID: e.ID, Purpose: PurposeReply, Runner: "r", Method: "POST",
			URL: "https://x", RequestBody: []byte("request"), StartedAt: now,
		}
		if err := s.AddRequest(ctx, r); err != nil {
			t.Fatal(err)
		}
		r.Status = 200
		r.ResponseBody = []byte("response")
		r.Usage = &Usage{PromptTokens: 10}
		if err := s.EndRequest(ctx, r); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, e)
	}

	if err := s.Prune(ctx, 2); err != nil {
		t.Fatal(err)
	}
	for i, e := range entries {
		rs, err := s.Requests(ctx, e.ID)
		if err != nil {
			t.Fatal(err)
		}
		r := rs[0]
		if i < 3 {
			if r.RequestBody != nil || r.ResponseBody != nil {
				t.Errorf("entry %d keeps its bodies", e.ID)
			}
			if !r.Pruned {
				t.Errorf("entry %d does not read as pruned", e.ID)
			}
			if r.Usage == nil || r.Usage.PromptTokens != 10 || r.Status != 200 {
				t.Errorf("entry %d lost its summary columns", e.ID)
			}
			continue
		}
		if string(r.RequestBody) != "request" || string(r.ResponseBody) != "response" {
			t.Errorf("entry %d lost its bodies", e.ID)
		}
	}
}

// The entry thrown away covers the first message, and the one that follows
// covers both.
func TestAReplyAnswersWhatTheRestartedOneWasWriting(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	first := &Message{Role: RoleUser,
		Parts: []Part{{Type: PartText, Text: "one"}}, CreatedAt: now}
	if err := s.AddMessage(ctx, first); err != nil {
		t.Fatal(err)
	}
	restarted := &Entry{UptoMessageID: first.ID, StartedAt: now}
	if err := s.StartEntry(ctx, restarted); err != nil {
		t.Fatal(err)
	}

	second := &Message{Role: RoleUser,
		Parts: []Part{{Type: PartText, Text: "two"}}, CreatedAt: now}
	if err := s.AddMessage(ctx, second); err != nil {
		t.Fatal(err)
	}
	restarted.Status = StatusRestarted
	restarted.EndedAt = now
	if err := s.EndEntry(ctx, restarted); err != nil {
		t.Fatal(err)
	}

	answering := &Entry{UptoMessageID: second.ID, StartedAt: now}
	if err := s.StartEntry(ctx, answering); err != nil {
		t.Fatal(err)
	}
	got, err := s.MessagesAnsweredBy(ctx, answering.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != first.ID || got[1].ID != second.ID {
		t.Errorf("answered %+v, want both messages", got)
	}

	// The entry that was thrown away was started for the first message, which
	// is what it says.
	got, err = s.MessagesAnsweredBy(ctx, restarted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != first.ID {
		t.Errorf("the restarted entry was started for %+v, want the first message", got)
	}
}

// The entry a run left behind mid-reply and the entry that answers afterwards
// were started for the same message.
func TestAReplyAfterAFailedOneAnswersTheSameMessages(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	msg := &Message{Role: RoleUser,
		Parts: []Part{{Type: PartText, Text: "one"}}, CreatedAt: now}
	if err := s.AddMessage(ctx, msg); err != nil {
		t.Fatal(err)
	}
	failed := &Entry{UptoMessageID: msg.ID, StartedAt: now}
	if err := s.StartEntry(ctx, failed); err != nil {
		t.Fatal(err)
	}
	failed.Status = StatusFailed
	failed.Error = "the run ended before the reply did"
	failed.EndedAt = now
	if err := s.EndEntry(ctx, failed); err != nil {
		t.Fatal(err)
	}

	answering := &Entry{UptoMessageID: msg.ID, StartedAt: now}
	if err := s.StartEntry(ctx, answering); err != nil {
		t.Fatal(err)
	}

	for _, e := range []*Entry{failed, answering} {
		got, err := s.MessagesAnsweredBy(ctx, e.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].ID != msg.ID {
			t.Errorf("entry %d was started for %+v, want the message", e.ID, got)
		}
	}
}

func TestADatabaseFromANewerPaula(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatal(err)
	}
	s.Close()

	_, err = Open(dir)
	if err == nil {
		t.Fatal("Open succeeded")
	}
	if !strings.Contains(err.Error(), "version 99") || !strings.Contains(err.Error(), "newer Paula") {
		t.Errorf("error = %v, want the version it holds", err)
	}
}

// fixtures are the migrations under testdata: a schema of their own, so that
// what the runner does is not tied to how many migrations Paula has come to
// carry. The first creates a table and the second adds a column to it.
func fixtures(t *testing.T) []migration {
	t.Helper()
	return loadMigrations(os.DirFS("testdata"), "migrations")
}

func userVersion(t *testing.T, s *Store) int {
	t.Helper()
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	return version
}

// A database opened by a build carrying migrations it has not had gets them, and
// a command that only reads says what to run first instead of reading a schema
// it does not know.
func TestADatabaseIsBroughtUpToDate(t *testing.T) {
	dir := t.TempDir()
	list := fixtures(t)

	// The build that made it carried the first migration and no more.
	s, err := openStore(dir, true, list[:1])
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	if _, err := openStore(dir, false, list); err == nil {
		t.Error("a database that is behind was opened for reading")
	} else if !strings.Contains(err.Error(), "paula serve") {
		t.Errorf("error = %v, want what to run to bring it up to date", err)
	}

	s, err = openStore(dir, true, list)
	if err != nil {
		t.Fatalf("openStore = %v, want the migrations applied", err)
	}
	defer s.Close()
	if got := userVersion(t, s); got != schemaVersion(list) {
		t.Errorf("user_version = %d, want %d", got, schemaVersion(list))
	}
}

// The second migration adds a column to the table the first creates, so they
// work in that order and in no other, and running them again would fail on a
// table and a column that are already there.
func TestMigrationsRunInOrderAndOnlyOnce(t *testing.T) {
	dir := t.TempDir()
	list := fixtures(t)
	s, err := openStore(dir, true, list)
	if err != nil {
		t.Fatalf("openStore = %v, want both migrations applied in order", err)
	}
	if _, err := s.db.Exec(`INSERT INTO notes (id, text, colour) VALUES (1, 'hi', 'red')`); err != nil {
		t.Errorf("the migrations left: %v", err)
	}
	s.Close()

	// Opening it again applies nothing.
	s, err = openStore(dir, true, list)
	if err != nil {
		t.Fatalf("openStore = %v, want the migrations left alone the second time", err)
	}
	defer s.Close()
	if got := userVersion(t, s); got != schemaVersion(list) {
		t.Errorf("user_version = %d, want %d", got, schemaVersion(list))
	}
	var notes int
	if err := s.db.QueryRow(`SELECT count(*) FROM notes`).Scan(&notes); err != nil {
		t.Fatal(err)
	}
	if notes != 1 {
		t.Errorf("notes = %d, want the row the first opening wrote", notes)
	}
}

// The files are numbered from one, without gaps.
func TestEveryMigrationIsNamedForItsVersion(t *testing.T) {
	if len(migrations) == 0 {
		t.Fatal("there are no migrations")
	}
	for i, m := range migrations {
		if m.version != i+1 {
			t.Errorf("%s is version %d, want the versions to run from 1 without gaps", m.name, m.version)
		}
	}
}

// Two files of one version would leave a database stamped by either without
// the other, so they are refused as the migrations are read.
func TestTwoMigrationsOfOneVersionAreRefused(t *testing.T) {
	fsys := fstest.MapFS{
		"migrations/0001_notes.sql":  {Data: []byte("CREATE TABLE notes (id INTEGER PRIMARY KEY);")},
		"migrations/0001_colour.sql": {Data: []byte("ALTER TABLE notes ADD COLUMN colour TEXT;")},
	}
	defer func() {
		if recover() == nil {
			t.Error("two migrations of one version were loaded")
		}
	}()
	loadMigrations(fsys, "migrations")
}

// The database lands in the directory it was asked for, and the pragmas on the
// connection are not read as part of the path.
func TestADataDirectoryWithAwkwardCharacters(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a#b?c%41d e")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := os.Stat(filepath.Join(dir, File)); err != nil {
		t.Errorf("the database is not in the directory asked for: %v", err)
	}
	var mode string
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q", mode)
	}
	var keys int
	if err := s.db.QueryRow(`PRAGMA foreign_keys`).Scan(&keys); err != nil {
		t.Fatal(err)
	}
	if keys != 1 {
		t.Errorf("foreign_keys = %d", keys)
	}
	// The database is written to as well as read, since the pragmas that carry
	// the path are what open it for both.
	if err := s.Set(context.Background(), KeyModel("chat"), "talk"); err != nil {
		t.Fatal(err)
	}
}

// A command that only reads does not turn an empty file into a conversation.
func TestReadLeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, File), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Read(dir)
	if err == nil {
		t.Fatal("Read succeeded")
	}
	if !strings.Contains(err.Error(), "holds no conversation yet") {
		t.Errorf("error = %v", err)
	}
	// The file is left exactly as it was.
	fi, err := os.Stat(filepath.Join(dir, File))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != 0 {
		t.Errorf("the file holds %d bytes, want it left alone", fi.Size())
	}
	if _, err := Read(dir); err == nil || !strings.Contains(err.Error(), "holds no conversation yet") {
		t.Errorf("the second Read = %v", err)
	}
}

// A foreign database is refused without so much as its journal mode being
// changed.
func TestADatabasePaulaDidNotCreateIsLeftAsItIs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, File)
	other, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Exec(`CREATE TABLE notes (a TEXT)`); err != nil {
		t.Fatal(err)
	}
	other.Close()

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil {
		t.Fatal("Open succeeded")
	} else if !strings.Contains(err.Error(), "did not create") {
		t.Errorf("error = %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("the database was written to before it was refused")
	}
}
