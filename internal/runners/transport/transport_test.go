package transport

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
}

func newServer(t *testing.T, answers ...func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *server) {
	s := &server{t: t, answers: answers}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		n := int(s.requests.Add(1)) - 1
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

// sent sends a request about a model, as a chat is, and kept by the recorder
// it carries.
func sent(ctx context.Context, c *Client, rec api.Recorder) error {
	return c.Send(ctx, Ask{
		Method:   http.MethodPost,
		Path:     "/chat/completions",
		Model:    "some/model",
		Body:     []byte(`{"model":"some/model"}`),
		Recorder: rec,
		Read:     func(r io.Reader, _ *api.Record) error { return decode(r, new(struct{})) },
	})
}

// answers stands in for how an API a test is about answers, and for one that
// documents nothing of its own where a test sets none.
type answers struct {
	retry func(time.Time, int, http.Header) (time.Duration, bool)
	fail  func(int, []byte) *api.APIError
}

func (a answers) Retry(now time.Time, status int, h http.Header) (time.Duration, bool) {
	if a.retry == nil {
		return 0, false
	}
	return a.retry(now, status, h)
}

func (a answers) Error(status int, body []byte) *api.APIError {
	if a.fail == nil {
		return nil
	}
	return a.fail(status, body)
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

func client(t *testing.T, url string, a answers) (*Client, *[]time.Duration) {
	t.Helper()
	var slept []time.Duration
	return &Client{
		Runner:  "test",
		BaseURL: url + "/v1",
		Token:   "test-token-abcdefgh",
		Retries: 2,
		Answers: a,
		Sleep: func(_ context.Context, d time.Duration) error {
			slept = append(slept, d)
			return nil
		},
	}, &slept
}

func TestRecordsHeadersBodiesAndAttempts(t *testing.T) {
	ts, srv := newServer(t, answer(http.StatusOK, `{"content":"hey"}`))
	c, _ := client(t, ts.URL, answers{})

	rec := &recorder{}
	if err := sent(context.Background(), c, rec); err != nil {
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
	if r.ResponseHeaders.Get("Content-Type") != "application/json" {
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

// A post that is not a chat belongs to a turn all the same, so it is kept by
// the recorder it is given, with the body it sent and the answer it read.
func TestAPostIsRecordedLikeAChat(t *testing.T) {
	ts, _ := newServer(t, answer(http.StatusOK, `{"content":"# Agenda"}`))
	c, _ := client(t, ts.URL, answers{})
	rec := &recorder{}
	var out struct {
		Content string `json:"content"`
	}
	if err := c.Post(context.Background(), "/augment/scrape", map[string]any{"url": "https://www.agendalx.pt"}, &out, rec); err != nil {
		t.Fatal(err)
	}
	if out.Content != "# Agenda" {
		t.Errorf("read %q", out.Content)
	}
	if len(rec.ended) != 1 {
		t.Fatalf("records = %d, want one", len(rec.ended))
	}
	r := rec.ended[0]
	if r.Method != http.MethodPost || !strings.HasSuffix(r.URL, "/v1/augment/scrape") ||
		string(r.RequestBody) != `{"url":"https://www.agendalx.pt"}` || string(r.ResponseBody) != `{"content":"# Agenda"}` {
		t.Errorf("record = %s %s sent %s, answered %s", r.Method, r.URL, r.RequestBody, r.ResponseBody)
	}
}

func TestRetryWithADelayHeader(t *testing.T) {
	ts, srv := newServer(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"message":"slow down"}}`)
		},
		answer(http.StatusOK, `{"content":"hey"}`),
	)
	a := answers{
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
	c, slept := client(t, ts.URL, a)
	rec := &recorder{}
	if err := sent(context.Background(), c, rec); err != nil {
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
	// The answer the record keeps is the second try's, and so is when it began.
	if !r.FirstByteAt.Equal(r.Attempts[1].FirstByteAt) {
		t.Errorf("first byte = %v, want the second try's %v", r.FirstByteAt, r.Attempts[1].FirstByteAt)
	}
}

func TestRetryWithoutADelayHeader(t *testing.T) {
	bad := answer(http.StatusBadGateway, `{"error":{"message":"bad gateway"}}`)
	ts, srv := newServer(t, bad, bad, bad)
	a := answers{
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
	c, slept := client(t, ts.URL, a)
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
	a := answers{retry: func(time.Time, int, http.Header) (time.Duration, bool) { return 0, true }}
	c, slept := client(t, ts.URL, a)
	if err := c.Get(context.Background(), "/key", new(struct{})); err == nil {
		t.Fatal("Get succeeded")
	}
	if srv.requests.Load() != 1 || len(*slept) != 0 {
		t.Errorf("requests = %d, waits = %v, want one request", srv.requests.Load(), *slept)
	}
}

func TestErrorIsReportedAsTheAPIGaveIt(t *testing.T) {
	ts, _ := newServer(t, answer(http.StatusUnauthorized, `not json at all`))
	c, _ := client(t, ts.URL, answers{})
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
	c, _ := client(t, ts.URL, answers{})
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

	c, _ := client(t, ts.URL, answers{})
	ctx, cancel := context.WithCancel(context.Background())
	rec := &recorder{}
	go func() {
		<-started
		cancel()
	}()

	if err := sent(ctx, c, rec); err == nil {
		t.Fatal("the request succeeded")
	}
	if len(rec.ended) != 1 || rec.ended[0].Error == "" {
		t.Errorf("record = %+v, want the failure kept", rec.ended)
	}
}

func TestRecorderFailureStopsTheRequest(t *testing.T) {
	ts, srv := newServer(t)
	c, _ := client(t, ts.URL, answers{})
	if err := sent(context.Background(), c, failingRecorder{}); err == nil {
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
	c, _ := client(t, ts.URL, answers{})

	err := sent(context.Background(), c, closingRecorder{})
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

// The idle timeout cuts a request that is not a stream too, well before the
// whole-request bound it is given beside it.
func TestAHostThatTakesTheRequestAndSaysNothing(t *testing.T) {
	const idle = 80 * time.Millisecond
	ts, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		// No status, no headers, no body, until the client gives up.
		<-r.Context().Done()
	})
	c, _ := client(t, ts.URL, answers{})
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
	c, _ := client(t, ts.URL, answers{})
	rec := &recorder{}
	if err := sent(context.Background(), c, rec); err == nil {
		t.Fatal("an answer with nothing in it was taken as one")
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
	c, slept := client(t, ts.URL, answers{
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

	c, _ := client(t, ts.URL, answers{})
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

func TestADelayLongerThanTheLongestIsNotWaitedFor(t *testing.T) {
	ts, srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"message":"come back in an hour"}}`)
	})
	a := answers{
		retry: func(_ time.Time, status int, h http.Header) (time.Duration, bool) {
			if status != http.StatusTooManyRequests {
				return 0, false
			}
			return time.Hour, true
		},
	}
	c, slept := client(t, ts.URL, a)
	rec := &recorder{}
	if err := sent(context.Background(), c, rec); err == nil {
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

// A host that went quiet reads as given up on, not as cancelled.
func TestAnIdleCutSaysWhatItWas(t *testing.T) {
	const idle = 80 * time.Millisecond
	ts, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"content":`)
		w.(http.Flusher).Flush()
		// Then silence, until the client gives up on it.
		<-r.Context().Done()
	})
	c, _ := client(t, ts.URL, answers{})
	c.IdleTimeout = idle

	rec := &recorder{}
	if err := sent(context.Background(), c, rec); !errors.Is(err, api.ErrIdle) {
		t.Fatalf("error = %v, want the idle cut", err)
	}
	if !strings.Contains(rec.ended[0].Error, "idle") {
		t.Errorf("the request kept %q, want what it was", rec.ended[0].Error)
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
	c, _ := client(t, ts.URL, answers{})
	c.RequestTimeout = limit
	c.IdleTimeout = time.Minute

	start := time.Now()
	err := c.Get(context.Background(), "/models", new(struct{}))
	if !errors.Is(err, api.ErrSlow) {
		t.Fatalf("error = %v, want the answer given up on", err)
	}
	if took := time.Since(start); took > 4*limit {
		t.Errorf("the request took %s, want it given up after %s", took, limit)
	}
}

// The bound running out while a try waits to be sent again is named the same
// way as one running out in the middle of an answer.
func TestTheBoundRunningOutDuringTheWaitSaysSo(t *testing.T) {
	const limit = 100 * time.Millisecond
	ts, _ := newServer(t, answer(http.StatusTooManyRequests, `{"error":{"message":"slow down"}}`))
	c, _ := client(t, ts.URL, answers{retry: func(time.Time, int, http.Header) (time.Duration, bool) {
		return time.Minute, true
	}})
	c.RequestTimeout = limit
	c.IdleTimeout = time.Minute
	// The wait is real, and the bound ends it.
	c.Sleep = nil

	err := c.Get(context.Background(), "/models", new(struct{}))
	if !errors.Is(err, api.ErrSlow) {
		t.Fatalf("error = %v, want the answer given up on", err)
	}
}

// A try whose connection went got no answer, and the record says so: the
// status and the body of the try before it are that try's alone.
func TestATryWhoseConnectionWentLeavesNoStatus(t *testing.T) {
	ts, srv := newServer(t,
		answer(http.StatusBadGateway, `{"error":{"message":"bad gateway"}}`),
		func(w http.ResponseWriter, r *http.Request) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			conn.Close()
		},
	)
	a := answers{retry: func(_ time.Time, status int, _ http.Header) (time.Duration, bool) {
		return 0, status == http.StatusBadGateway
	}}
	c, _ := client(t, ts.URL, a)
	rec := &recorder{}
	if err := sent(context.Background(), c, rec); err == nil {
		t.Fatal("the request succeeded")
	}
	if srv.requests.Load() != 2 {
		t.Fatalf("requests = %d, want the first and the one sent again", srv.requests.Load())
	}
	r := rec.ended[0]
	if len(r.Attempts) != 2 || r.Attempts[0].Status != http.StatusBadGateway || r.Attempts[1].Status != 0 {
		t.Fatalf("attempts = %+v", r.Attempts)
	}
	if r.Status != 0 || len(r.ResponseHeaders) != 0 || len(r.ResponseBody) != 0 {
		t.Errorf("record = status %d, headers %v, body %q, want nothing of the try before",
			r.Status, r.ResponseHeaders, r.ResponseBody)
	}
	if r.Error == "" {
		t.Error("the record kept no error")
	}
}
