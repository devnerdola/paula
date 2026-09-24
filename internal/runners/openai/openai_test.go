package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nerdola.dev/x/paula/internal/runners/api"
)

// server answers with what each call returns, in order.
type server struct {
	t        *testing.T
	answers  []func(w http.ResponseWriter, r *http.Request)
	requests atomic.Int32
	bodies   []string
}

func newServer(t *testing.T, answers ...func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *server) {
	s := &server{t: t, answers: answers}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		n := int(s.requests.Add(1)) - 1
		s.bodies = append(s.bodies, string(b))
		if n < len(s.answers) {
			s.answers[n](w, r)
			return
		}
		s.t.Errorf("request %d was not expected", n+1)
	}))
	t.Cleanup(ts.Close)
	return ts, s
}

func answer(status int, body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}
}

// streamed answers a chat request with one chunk of text and the end of the
// stream.
func streamed(text string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},"+
			"\"finish_reason\":\"stop\"}]}\n\n", text)
		io.WriteString(w, "data: [DONE]\n\n")
	}
}

// asked makes the request a record is kept of: a chat request carrying the
// recorder that keeps it, which is the only kind the conversation records.
func asked(ctx context.Context, c *Client, rec api.Recorder) error {
	_, err := c.Chat(ctx, api.ChatRequest{
		Model:    "some/model",
		Messages: []api.Message{api.Text(api.RoleUser, "hey")},
		Recorder: rec,
	}, func(api.Chunk) error { return nil })
	return err
}

// parts stands in for the parts of an API a test is about, and answers for a
// runner that documents nothing of its own where a test sets none.
type parts struct {
	body  func(map[string]any, api.ChatRequest) error
	chunk func([]byte, *api.Result) (string, error)
	retry func(time.Time, int, http.Header) (time.Duration, bool)
	fail  func(int, []byte) *api.APIError
}

func (p parts) Body(body map[string]any, req api.ChatRequest) error {
	if p.body == nil {
		return nil
	}
	return p.body(body, req)
}

// Message adds nothing: what a runner hands back with a message is its own.
func (parts) Message(map[string]any, api.Message) {}

func (parts) End(*api.Result) {}

func (p parts) Chunk(raw []byte, res *api.Result) (string, error) {
	if p.chunk == nil {
		return "", nil
	}
	return p.chunk(raw, res)
}

func (p parts) Retry(now time.Time, status int, h http.Header) (time.Duration, bool) {
	if p.retry == nil {
		return 0, false
	}
	return p.retry(now, status, h)
}

func (p parts) Error(status int, body []byte) *api.APIError {
	if p.fail == nil {
		return nil
	}
	return p.fail(status, body)
}

// recorder keeps what the client reported, the way the conversation does.
type recorder struct {
	started []*api.Record
	ended   []*api.Record
}

func (r *recorder) StartRequest(_ context.Context, rec *api.Record) error {
	r.started = append(r.started, rec)
	return nil
}

func (r *recorder) EndRequest(_ context.Context, rec *api.Record) error {
	r.ended = append(r.ended, rec)
	return nil
}

func client(t *testing.T, url string, hooks Hooks) (*Client, *[]time.Duration) {
	t.Helper()
	var slept []time.Duration
	return &Client{
		Runner:  "test",
		BaseURL: url + "/v1",
		Token:   "test-token-abcdefgh",
		Retries: 2,
		Hooks:   hooks,
		Sleep: func(_ context.Context, d time.Duration) error {
			slept = append(slept, d)
			return nil
		},
	}, &slept
}

func TestRecordsHeadersBodiesAndAttempts(t *testing.T) {
	ts, srv := newServer(t, streamed("hey"))
	c, _ := client(t, ts.URL, parts{})

	rec := &recorder{}
	if err := asked(context.Background(), c, rec); err != nil {
		t.Fatal(err)
	}
	if len(rec.started) != 1 || len(rec.ended) != 1 {
		t.Fatalf("records = %d started, %d ended", len(rec.started), len(rec.ended))
	}

	r := rec.ended[0]
	if r.Runner != "test" || r.Model != "some/model" {
		t.Errorf("record = %+v", r)
	}
	if r.Method != http.MethodPost || !strings.HasSuffix(r.URL, "/v1/chat/completions") {
		t.Errorf("record = %s %s", r.Method, r.URL)
	}
	if got := r.RequestHeaders.Get("Authorization"); got != "[redacted]" {
		t.Errorf("Authorization = %q, want it redacted", got)
	}
	if !strings.Contains(string(r.RequestBody), `"model":"some/model"`) {
		t.Errorf("request body = %s, want the bytes that were sent", r.RequestBody)
	}
	if !strings.Contains(string(r.ResponseBody), `"content":"hey"`) {
		t.Errorf("response body = %q, want the bytes that came back", r.ResponseBody)
	}
	if r.ResponseHeaders.Get("Content-Type") != "text/event-stream" {
		t.Errorf("response headers = %v", r.ResponseHeaders)
	}
	if len(r.Attempts) != 1 || r.Attempts[0].Status != 200 {
		t.Fatalf("attempts = %+v", r.Attempts)
	}
	if r.Attempts[0].FirstByteAt.IsZero() || r.FirstByteAt.IsZero() {
		t.Error("no first byte time was kept")
	}
	if r.StartedAt.IsZero() || r.EndedAt.IsZero() || r.Error != "" {
		t.Errorf("record = %+v", r)
	}
	if srv.requests.Load() != 1 {
		t.Errorf("requests = %d", srv.requests.Load())
	}
}

func TestRetryWithADelayHeader(t *testing.T) {
	ts, srv := newServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"message":"slow down"}}`)
		},
		streamed("hey"),
	)
	hooks := parts{
		retry: func(_ time.Time, status int, h http.Header) (time.Duration, bool) {
			if status != http.StatusTooManyRequests {
				return 0, false
			}
			if v := h.Get("Retry-After"); v == "3" {
				return 3 * time.Second, true
			}
			return 0, true
		},
	}
	c, slept := client(t, ts.URL, hooks)
	rec := &recorder{}
	if err := asked(context.Background(), c, rec); err != nil {
		t.Fatal(err)
	}
	if srv.requests.Load() != 2 {
		t.Errorf("requests = %d, want 2", srv.requests.Load())
	}
	if len(*slept) != 1 || (*slept)[0] != 3*time.Second {
		t.Errorf("waits = %v, want the delay the API gave", *slept)
	}
	r := rec.ended[0]
	if len(r.Attempts) != 2 || r.Attempts[0].Status != 429 || r.Attempts[0].RetryAfter != 3*time.Second {
		t.Errorf("attempts = %+v", r.Attempts)
	}
	if r.Attempts[0].Error == "" {
		t.Error("the first attempt kept no error")
	}
}

func TestRetryWithoutADelayHeader(t *testing.T) {
	bad := answer(http.StatusBadGateway, `{"error":{"message":"bad gateway"}}`)
	ts, srv := newServer(t, bad, bad, bad)
	hooks := parts{
		retry: func(_ time.Time, status int, h http.Header) (time.Duration, bool) {
			return 0, status == http.StatusBadGateway
		},
		fail: func(status int, body []byte) *api.APIError {
			var e struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			json.Unmarshal(body, &e)
			return &api.APIError{Status: status, Message: e.Error.Message}
		},
	}
	c, slept := client(t, ts.URL, hooks)
	err := c.Get(context.Background(), "/key", new(struct{}))
	if err == nil {
		t.Fatal("Get succeeded")
	}
	if want := "502: bad gateway"; err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
	if srv.requests.Load() != 3 {
		t.Errorf("requests = %d, want the first and two retries", srv.requests.Load())
	}
	if len(*slept) != 2 || (*slept)[0] != time.Second || (*slept)[1] != 2*time.Second {
		t.Errorf("waits = %v, want a second doubling", *slept)
	}
}

func TestNoRetryAfterTheAnswerStarted(t *testing.T) {
	ts, srv := newServer(t, answer(http.StatusOK, `not json`))
	hooks := parts{retry: func(time.Time, int, http.Header) (time.Duration, bool) { return 0, true }}
	c, slept := client(t, ts.URL, hooks)
	if err := c.Get(context.Background(), "/key", new(struct{})); err == nil {
		t.Fatal("Get succeeded")
	}
	if srv.requests.Load() != 1 || len(*slept) != 0 {
		t.Errorf("requests = %d, waits = %v, want one request", srv.requests.Load(), *slept)
	}
}

func TestErrorIsReportedAsTheAPIGaveIt(t *testing.T) {
	ts, _ := newServer(t, answer(http.StatusUnauthorized, `not json at all`))
	c, _ := client(t, ts.URL, parts{})
	err := c.Get(context.Background(), "/key", new(struct{}))
	var e *api.APIError
	if !errors.As(err, &e) {
		t.Fatalf("error = %v", err)
	}
	if e.Status != 401 || e.Message != "not json at all" {
		t.Errorf("error = %+v", e)
	}
}

func TestIdleTimeout(t *testing.T) {
	const idle = 80 * time.Millisecond
	ts, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":`)
		w.(http.Flusher).Flush()
		// The rest never comes: the handler ends with the request the client
		// gave up on, rather than outliving it.
		<-r.Context().Done()
	})
	c, _ := client(t, ts.URL, parts{})
	c.IdleTimeout = idle

	start := time.Now()
	if err := c.Get(context.Background(), "/key", new(struct{})); err == nil {
		t.Fatal("Get succeeded")
	}
	if time.Since(start) > 5*idle {
		t.Errorf("the request took %s, want it cancelled after %s of silence", time.Since(start), idle)
	}
}

func TestCancellation(t *testing.T) {
	reached := make(chan struct{})
	started := make(chan struct{})
	ts, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":`)
		w.(http.Flusher).Flush()
		close(started)
		<-reached
	})
	t.Cleanup(func() { close(reached) })

	c, _ := client(t, ts.URL, parts{})
	ctx, cancel := context.WithCancel(context.Background())
	rec := &recorder{}
	go func() {
		<-started
		cancel()
	}()

	if err := asked(ctx, c, rec); err == nil {
		t.Fatal("the request succeeded")
	}
	if len(rec.ended) != 1 || rec.ended[0].Error == "" {
		t.Errorf("record = %+v, want the failure kept", rec.ended)
	}
}

func TestRecorderFailureStopsTheRequest(t *testing.T) {
	ts, srv := newServer(t)
	c, _ := client(t, ts.URL, parts{})
	if err := asked(context.Background(), c, failingRecorder{}); err == nil {
		t.Fatal("the request succeeded")
	}
	if srv.requests.Load() != 0 {
		t.Errorf("requests = %d, want none", srv.requests.Load())
	}
}

type failingRecorder struct{}

func (failingRecorder) StartRequest(context.Context, *api.Record) error {
	return fmt.Errorf("the store is gone")
}
func (failingRecorder) EndRequest(context.Context, *api.Record) error { return nil }

// closingRecorder takes the request and cannot write down how it ended.
type closingRecorder struct{}

func (closingRecorder) StartRequest(context.Context, *api.Record) error { return nil }
func (closingRecorder) EndRequest(context.Context, *api.Record) error {
	return fmt.Errorf("the store is gone")
}

// The failed request is what the API said; the record that could not be closed
// is a conversation losing its history.
func TestARequestAndItsRecordBothFailing(t *testing.T) {
	ts, _ := newServer(t, answer(http.StatusUnauthorized, `{"error":{"message":"User not found."}}`))
	c, _ := client(t, ts.URL, parts{})

	err := asked(context.Background(), c, closingRecorder{})
	if err == nil {
		t.Fatal("the request succeeded")
	}
	if _, ok := errors.AsType[*api.APIError](err); !ok {
		t.Errorf("error = %v, want what the API said", err)
	}
	if !strings.Contains(err.Error(), "the store is gone") {
		t.Errorf("error = %v, want the record that could not be closed", err)
	}
}

func TestHookFailureStopsTheRequest(t *testing.T) {
	_, err := chatBody(api.ChatRequest{Model: "m"}, parts{
		body: func(map[string]any, api.ChatRequest) error { return errors.New("no") },
	})
	if err == nil {
		t.Fatal("chatBody succeeded")
	}
}

// The idle timeout cuts a request that is not a stream too, well before the
// whole-request bound it is given beside it.
func TestAHostThatTakesTheRequestAndSaysNothing(t *testing.T) {
	const idle = 80 * time.Millisecond
	ts, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		// No status, no headers, no body, until the client gives up.
		<-r.Context().Done()
	})
	c, _ := client(t, ts.URL, parts{})
	c.IdleTimeout = idle

	start := time.Now()
	if err := c.Get(context.Background(), "/key", new(struct{})); err == nil {
		t.Fatal("Get succeeded")
	}
	if took := time.Since(start); took > 5*idle {
		t.Errorf("the request took %s, want it given up after %s of silence", took, idle)
	}
}

func TestBackoff(t *testing.T) {
	for _, tc := range []struct {
		sent int
		want time.Duration
	}{
		{0, time.Second},
		{1, 2 * time.Second},
		{4, 16 * time.Second},
		{5, 30 * time.Second},
		{100, 30 * time.Second},
	} {
		if got := backoff(tc.sent); got != tc.want {
			t.Errorf("backoff after %d = %s, want %s", tc.sent, got, tc.want)
		}
	}
}

func TestAnAnswerWithNoBodyStillSaysWhenItArrived(t *testing.T) {
	ts, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
	})
	c, _ := client(t, ts.URL, parts{})
	rec := &recorder{}
	if err := asked(context.Background(), c, rec); err == nil {
		t.Fatal("an answer with nothing in it was taken as a reply")
	}
	r := rec.ended[0]
	if r.FirstByteAt.IsZero() || r.Attempts[0].FirstByteAt.IsZero() {
		t.Error("an answer with an empty body kept no first byte time")
	}
}

func TestTheDelayIsCountedFromTheClientsClock(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	ts, _ := newServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Reset", strconv.FormatInt(now.Add(45*time.Second).Unix(), 10))
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":"slow down"}`)
		},
		answer(http.StatusOK, `{"ok":true}`),
	)
	c, slept := client(t, ts.URL, parts{
		retry: func(now time.Time, status int, h http.Header) (time.Duration, bool) {
			if status != http.StatusTooManyRequests {
				return 0, false
			}
			n, err := strconv.ParseInt(h.Get("X-Reset"), 10, 64)
			if err != nil {
				return 0, true
			}
			return time.Unix(n, 0).Sub(now), true
		},
	})
	c.Now = func() time.Time { return now }

	ctx := context.Background()
	if err := c.Get(ctx, "/key", new(struct{})); err != nil {
		t.Fatal(err)
	}
	if len(*slept) != 1 || (*slept)[0] != 45*time.Second {
		t.Errorf("waits = %v, want the delay counted from the client's clock", *slept)
	}
}

func TestTheBaseURLKeepsItsOwnQuery(t *testing.T) {
	var asked string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(ts.Close)

	c, _ := client(t, ts.URL, parts{})
	c.BaseURL = ts.URL + "/v1?deployment=paula"
	ctx := context.Background()
	if err := c.Get(ctx, "/models?type=all", new(struct{})); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(asked, "/v1/models?") {
		t.Fatalf("asked %q", asked)
	}
	if !strings.Contains(asked, "deployment=paula") || !strings.Contains(asked, "type=all") {
		t.Errorf("asked %q, want both queries", asked)
	}
}

func TestTheCacheKeyIsSent(t *testing.T) {
	ts, srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"content":"hey","role":"assistant"},"finish_reason":"stop"}]}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	})
	c, _ := client(t, ts.URL, parts{})
	ctx := context.Background()
	_, err := c.Chat(ctx, api.ChatRequest{
		Model:    "some/model",
		Messages: []api.Message{api.Text(api.RoleUser, "hey")},
		CacheKey: "paula-paula-reply",
	}, func(api.Chunk) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(srv.bodies[0], `"prompt_cache_key":"paula-paula-reply"`) {
		t.Errorf("body = %s", srv.bodies[0])
	}
}

// The conversation is what grows from one request to the next, so it is the
// last of the body: the model, the settings and the tools come before it.
func TestTheConversationIsTheLastOfTheBody(t *testing.T) {
	ts, srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"content":"hey","role":"assistant"},"finish_reason":"stop"}]}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	})
	c, _ := client(t, ts.URL, parts{})
	_, err := c.Chat(context.Background(), api.ChatRequest{
		Model:    "some/model",
		Messages: []api.Message{api.Text(api.RoleUser, "hey")},
		Tools:    []api.ToolDef{{Name: "search_memories", Parameters: json.RawMessage(`{"type":"object"}`)}},
		CacheKey: "paula-paula-reply",
	}, func(api.Chunk) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	body := srv.bodies[0]
	messages := strings.Index(body, `"messages":`)
	for _, field := range []string{`"model":`, `"prompt_cache_key":`, `"stream":`, `"tools":`} {
		if at := strings.Index(body, field); at < 0 || at > messages {
			t.Errorf("%s comes after the conversation: %s", field, body)
		}
	}
	if !strings.HasSuffix(body, `"role":"user"}]}`) {
		t.Errorf("the body ends %q, want the conversation", body[max(0, len(body)-40):])
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Errorf("the body is not JSON: %v", err)
	}
}

// A record says what the stream reported, or nothing at all. A stream that
// reported nothing is not one that cost zero, and paula turns adds these up.
func TestWhatARecordSaysAboutTheTokens(t *testing.T) {
	chunk := `data: {"choices":[{"index":0,"delta":{"content":"hey","role":"assistant"},"finish_reason":"stop"}]%s}` + "\n\n"
	for _, tc := range []struct {
		name  string
		usage string
		want  *api.Usage
	}{
		{"nothing said", "", nil},
		{"what the stream said", `,"usage":{"prompt_tokens":11,"completion_tokens":3}`,
			&api.Usage{PromptTokens: 11, CompletionTokens: 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, chunk, tc.usage)
				io.WriteString(w, "data: [DONE]\n\n")
			})
			c, _ := client(t, ts.URL, parts{})
			rec := &recorder{}
			if err := asked(context.Background(), c, rec); err != nil {
				t.Fatal(err)
			}
			got := rec.ended[0].Usage
			switch {
			case tc.want == nil && got != nil:
				t.Errorf("usage = %+v, want nothing said about the tokens", got)
			case tc.want != nil && (got == nil || *got != *tc.want):
				t.Errorf("usage = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestTheSettingsBothAPIsDocumentAreSent(t *testing.T) {
	ts, srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"content":"hey","role":"assistant"},"finish_reason":"stop"}]}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	})
	c, _ := client(t, ts.URL, parts{})
	ctx := context.Background()
	limit := 2048
	temperature := 0.8
	_, err := c.Chat(ctx, api.ChatRequest{
		Model:    "some/model",
		Messages: []api.Message{api.Text(api.RoleUser, "hey")},
		Settings: api.Settings{
			Sampling: api.SamplingSettings{Temperature: &temperature},
			Output:   api.OutputSettings{MaxTokens: &limit},
		},
	}, func(api.Chunk) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"max_tokens":2048`, `"temperature":0.8`} {
		if !strings.Contains(srv.bodies[0], want) {
			t.Errorf("body = %s, want %s in it", srv.bodies[0], want)
		}
	}
}

func TestAnAnswerThatCarriesNoEvents(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"an error written where the events belong", `{"error":{"message":"upstream is down"}}`, "upstream is down"},
		{"nothing at all", "", "the answer was empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, tc.body)
			})
			c, _ := client(t, ts.URL, parts{})
			ctx := context.Background()
			_, err := c.Chat(ctx, api.ChatRequest{
				Model:    "some/model",
				Messages: []api.Message{api.Text(api.RoleUser, "hey")},
			}, func(api.Chunk) error { return nil })
			if err == nil {
				t.Fatal("Chat succeeded, want the answer reported")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want %q in it", err, tc.want)
			}
		})
	}
}

// The pieces go as one, joined by a blank line, which is how the conversation
// joins them when it reads the message back.
func TestAMessageOfSeveralTextParts(t *testing.T) {
	body, err := chatBody(api.ChatRequest{
		Model: "some/model",
		Messages: []api.Message{{Role: api.RoleUser, Parts: []api.Part{
			{Type: api.PartText, Text: "one"},
			{Type: api.PartText, Text: "two"},
		}}},
	}, parts{})
	if err != nil {
		t.Fatal(err)
	}
	msgs, ok := body["messages"].([]map[string]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("messages = %+v", body["messages"])
	}
	if got := msgs[0]["content"]; got != "one\n\ntwo" {
		t.Errorf("content = %q, want the parts joined by a blank line", got)
	}
}

// A round that asked for tools goes back with what it asked for, and each
// answer goes back under the call it answers, as both APIs document them.
func TestARoundOfCallsGoesBackAsTheAPIsDocumentIt(t *testing.T) {
	params := json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`)
	body, err := chatBody(api.ChatRequest{
		Model: "some/model",
		Messages: []api.Message{
			api.Text(api.RoleUser, "where does Ana live?"),
			{Role: api.RoleAssistant, ToolCalls: []api.ToolCall{
				{ID: "call_1", Name: "search_memories", Arguments: `{"query":"Ana"}`},
			}},
			{Role: api.RoleTool, ToolCallID: "call_1",
				Parts: []api.Part{{Type: api.PartText, Text: "Ana lives in Lisbon"}}},
		},
		Tools: []api.ToolDef{{Name: "search_memories", Description: "Looks something up.", Parameters: params}},
	}, parts{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Messages []struct {
			Role       string  `json:"role"`
			Content    *string `json:"content"`
			ToolCallID string  `json:"tool_call_id"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
		Tools []struct {
			Type     string `json:"type"`
			Function struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
		ToolChoice *string `json:"tool_choice"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}

	asked := got.Messages[1]
	// What says nothing beside a call is null rather than text.
	if asked.Content != nil {
		t.Errorf("the call's message says %q, want null beside the call", *asked.Content)
	}
	if len(asked.ToolCalls) != 1 || asked.ToolCalls[0].ID != "call_1" || asked.ToolCalls[0].Type != "function" ||
		asked.ToolCalls[0].Function.Name != "search_memories" || asked.ToolCalls[0].Function.Arguments != `{"query":"Ana"}` {
		t.Errorf("the call went as %+v", asked.ToolCalls)
	}
	answered := got.Messages[2]
	if answered.Role != "tool" || answered.ToolCallID != "call_1" || answered.Content == nil ||
		*answered.Content != "Ana lives in Lisbon" {
		t.Errorf("the answer went as %+v", answered)
	}
	if len(got.Tools) != 1 || got.Tools[0].Type != "function" || got.Tools[0].Function.Name != "search_memories" ||
		string(got.Tools[0].Function.Parameters) != string(params) {
		t.Errorf("the tools went as %+v", got.Tools)
	}
	// Nothing is said of the choice unless there is one to say.
	if got.ToolChoice != nil {
		t.Errorf("tool_choice = %q, want it left out", *got.ToolChoice)
	}
}

// A round asked for an answer with no call in it says so.
func TestARoundWithNoCallSaysSo(t *testing.T) {
	body, err := chatBody(api.ChatRequest{
		Model:      "some/model",
		Messages:   []api.Message{api.Text(api.RoleUser, "hey")},
		ToolChoice: api.ToolChoiceNone,
	}, parts{})
	if err != nil {
		t.Fatal(err)
	}
	if got := body["tool_choice"]; got != "none" {
		t.Errorf("tool_choice = %v, want none", got)
	}
}

// A request that offers no tools says nothing of them, so it is the request it
// always was.
func TestARequestOfferingNoToolsSaysNothingOfThem(t *testing.T) {
	body, err := chatBody(api.ChatRequest{
		Model:    "some/model",
		Messages: []api.Message{api.Text(api.RoleUser, "hey")},
	}, parts{})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"tools", "tool_choice"} {
		if _, ok := body[key]; ok {
			t.Errorf("the body carries %s: %v", key, body[key])
		}
	}
}

func TestADelayLongerThanTheLongestIsNotWaitedFor(t *testing.T) {
	ts, srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"message":"come back in an hour"}}`)
	})
	hooks := parts{
		retry: func(_ time.Time, status int, h http.Header) (time.Duration, bool) {
			if status != http.StatusTooManyRequests {
				return 0, false
			}
			return time.Hour, true
		},
	}
	c, slept := client(t, ts.URL, hooks)
	rec := &recorder{}
	if err := asked(context.Background(), c, rec); err == nil {
		t.Fatal("the request succeeded")
	}
	if srv.requests.Load() != 1 {
		t.Errorf("requests = %d, want the one", srv.requests.Load())
	}
	if len(*slept) != 0 {
		t.Errorf("waits = %v, want none", *slept)
	}
	attempts := rec.ended[0].Attempts
	if len(attempts) != 1 || attempts[0].RetryAfter != 0 {
		t.Errorf("attempts = %+v, want the one that ended it, with no wait", attempts)
	}
}

func TestACallbackThatStopsTakingTheAnswer(t *testing.T) {
	ts, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for range 3 {
			io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"content":"one "}}]}`+"\n\n")
		}
		io.WriteString(w, "data: [DONE]\n\n")
	})
	c, _ := client(t, ts.URL, parts{})
	ctx := context.Background()

	want := errors.New("the terminal is gone")
	var chunks int
	_, err := c.Chat(ctx, api.ChatRequest{
		Model:    "some/model",
		Messages: []api.Message{api.Text(api.RoleUser, "hey")},
	}, func(api.Chunk) error {
		chunks++
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want the one the callback gave", err)
	}
	if chunks != 1 {
		t.Errorf("chunks = %d, want the stream left after the first", chunks)
	}
}

// An error written into the stream is the one place an API can report a failure
// once it has answered 200. It comes back as the error it is, under the status it
// names, rather than as a reply that stopped early.
func TestAnErrorWhereAChunkBelongs(t *testing.T) {
	ts, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"content":"one "}}]}`+"\n\n")
		io.WriteString(w, `data: {"error":{"code":429,"message":"Rate limit exceeded"},`+
			`"choices":[{"index":0,"delta":{"content":""},"finish_reason":"error"}]}`+"\n\n")
	})
	c, _ := client(t, ts.URL, parts{})

	_, err := c.Chat(context.Background(), api.ChatRequest{
		Model:    "some/model",
		Messages: []api.Message{api.Text(api.RoleUser, "hey")},
	}, func(api.Chunk) error { return nil })
	if err == nil {
		t.Fatal("Chat succeeded")
	}
	var e *api.APIError
	if !errors.As(err, &e) {
		t.Fatalf("error = %v, want the one the stream carried", err)
	}
	if e.Status != 429 || !strings.Contains(e.Message, "Rate limit exceeded") {
		t.Errorf("error = %+v, want the status the stream named", e)
	}
}

func TestALineOverTheLimit(t *testing.T) {
	ts, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\""+strings.Repeat("x", lineLimit)+"\"}}]}\n\n")
	})
	c, _ := client(t, ts.URL, parts{})
	ctx := context.Background()
	_, err := c.Chat(ctx, api.ChatRequest{
		Model:    "some/model",
		Messages: []api.Message{api.Text(api.RoleUser, "hey")},
	}, func(api.Chunk) error { return nil })
	if err == nil {
		t.Fatal("Chat succeeded, want a line that long refused")
	}
}

// A host that went quiet reads as given up on, not as cancelled.
func TestAnIdleCutSaysWhatItWas(t *testing.T) {
	const idle = 80 * time.Millisecond
	ts, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"choices":[{"delta":{"content":"one"}}]}`+"\n\n")
		w.(http.Flusher).Flush()
		// Then silence, until the client gives up on it.
		<-r.Context().Done()
	})
	c, _ := client(t, ts.URL, parts{})
	c.IdleTimeout = idle

	rec := &recorder{}
	if err := asked(context.Background(), c, rec); !errors.Is(err, ErrIdle) {
		t.Fatalf("error = %v, want the idle cut", err)
	}
	if !strings.Contains(rec.ended[0].Error, "idle") {
		t.Errorf("the request kept %q, want what it was", rec.ended[0].Error)
	}
}

// The failure a hosted API really has: it accepts the request, and then the
// reply never starts. It is given up on after the idle timeout, whether the
// silence begins before the headers or after them, rather than being waited on
// for as long as the host holds the connection open.
func TestAHostThatTakesTheReplyAndSaysNothing(t *testing.T) {
	const idle = 80 * time.Millisecond
	for _, tc := range []struct {
		name   string
		answer func(w http.ResponseWriter, r *http.Request)
	}{
		{"before the headers", func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		}},
		{"after them", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, _ := newServer(t, tc.answer)
			c, _ := client(t, ts.URL, parts{})
			c.IdleTimeout = idle

			start := time.Now()
			err := asked(context.Background(), c, nil)
			if !errors.Is(err, ErrIdle) {
				t.Fatalf("error = %v, want the host given up on", err)
			}
			if took := time.Since(start); took > 20*idle {
				t.Errorf("the reply took %s, want it given up after %s", took, idle)
			}
		})
	}
}

// A connection dropped mid-reply is an error, not a short reply: the answer
// would otherwise be half a sentence with nothing to say why.
func TestAStreamThatIsCutOff(t *testing.T) {
	ts, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"choices":[{"delta":{"content":"one "}}]}`+"\n\n")
		w.(http.Flusher).Flush()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		conn.Close()
	})
	c, _ := client(t, ts.URL, parts{})

	var text strings.Builder
	_, err := c.Chat(context.Background(),
		api.ChatRequest{Model: "some/model", Messages: []api.Message{api.Text(api.RoleUser, "hey")}},
		func(chunk api.Chunk) error {
			text.WriteString(chunk.Text)
			return nil
		})
	if err == nil {
		t.Fatalf("Chat = %q, want the connection that dropped reported", text.String())
	}
	// What did arrive arrived: it is the end of the stream that is missing,
	// which is what the error is about.
	if text.String() != "one " {
		t.Errorf("text = %q, want what the host managed to send", text.String())
	}
}

// A listing or a key is held to the whole-request bound the runner set, which
// the idle timeout neither shortens nor lengthens: one cuts a request that goes
// quiet, the other one that keeps trickling.
func TestTheBoundOnARequestThatIsNotAStream(t *testing.T) {
	for _, tc := range []struct {
		name    string
		idle    time.Duration
		request time.Duration
		want    time.Duration
	}{
		{"the bound the runner set", 2 * time.Minute, 90 * time.Second, 90 * time.Second},
		{"an idle timeout shorter than it", 50 * time.Millisecond, time.Minute, time.Minute},
		{"no bound set", 2 * time.Minute, 0, DefaultRequestTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Client{IdleTimeout: tc.idle, RequestTimeout: tc.request}
			ctx, cancel := c.limited(context.Background())
			defer cancel()
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Fatal("the request was given no deadline")
			}
			if got := time.Until(deadline); got > tc.want || got < tc.want-time.Second {
				t.Errorf("the request has %s, want about %s", got, tc.want)
			}
		})
	}
}

// A body that keeps trickling never goes quiet, so the whole-request bound is
// the only thing that ends it.
func TestAnAnswerThatTakesTooLong(t *testing.T) {
	const limit = 150 * time.Millisecond
	ts, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":[`)
		w.(http.Flusher).Flush()
		// A byte now and then, for longer than the request is given.
		for range 20 {
			io.WriteString(w, " ")
			w.(http.Flusher).Flush()
			time.Sleep(limit / 4)
		}
	})
	c, _ := client(t, ts.URL, parts{})
	c.RequestTimeout = limit
	c.IdleTimeout = time.Minute

	start := time.Now()
	err := c.Get(context.Background(), "/models", new(struct{}))
	if !errors.Is(err, ErrSlow) {
		t.Fatalf("error = %v, want the answer given up on", err)
	}
	if took := time.Since(start); took > 4*limit {
		t.Errorf("the request took %s, want it given up after %s", took, limit)
	}
}
