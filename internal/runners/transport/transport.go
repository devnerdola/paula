// Package transport is how a runner makes its requests: sent with its key,
// held to its timeouts, sent again when the API asks for it, and recorded
// whole. What one API alone documents of its answers is the runner's to say.
package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"time"

	"nerdola.dev/x/paula/internal/logs"
	"nerdola.dev/x/paula/internal/runners/api"
)

// Backoff for a status the API asks to retry without saying for how long.
const (
	firstBackoff = time.Second
	maxBackoff   = 30 * time.Second
	// slowRequest is how long a request may take before it is said at warn, the
	// level a run shows by default, so a wait nobody asked for is never silent.
	slowRequest = 5 * time.Second
	// DefaultRequestTimeout is the longest a request that is not a stream may
	// take, and what a runner that sets no request timeout of its own takes.
	// OpenRouter answers its catalogue in under a second most of the time and
	// stalls for minutes now and then, which is what the bound is for: waiting
	// costs nothing while an API is healthy, and a run that cannot read a
	// listing has no host to talk to anyway. A stream is held to the idle
	// timeout instead, since it is answered a little at a time.
	DefaultRequestTimeout = 3 * time.Minute
	// maxDelay is the longest a request waits before it is sent again, however
	// long the API asks for.
	maxDelay = 2 * time.Minute
)

// What a reader of an answer tells the client of how it ended.
var (
	// ErrNotTaken says whoever asked stopped taking the answer, so what is left
	// of it is not read.
	ErrNotTaken = errors.New("the answer was not taken")
	// ErrNoAnswer says a 200 carried nothing where the answer belongs, so its
	// body is read as the error the API wrote there.
	ErrNoAnswer = errors.New("the answer carried nothing")
)

// Answers are how one API answers any request: when it is worth sending
// again, and the shape it reports errors in. Every runner answers both, so the
// client never asks whether it has them.
type Answers interface {
	// Retry says whether a status is worth sending again, and after how long,
	// counted from the time it is given.
	Retry(now time.Time, status int, h http.Header) (time.Duration, bool)
	// Error reads the shape the API reports errors in, and reports nil for a
	// body that is not one.
	Error(status int, body []byte) *api.APIError
}

type Client struct {
	Runner  string
	BaseURL string
	Token   string
	// IdleTimeout is how long any request may go without a byte, and
	// RequestTimeout the whole of what one that is not a stream may take.
	// Neither shortens the other: a request that goes quiet is cut by the
	// first, one that keeps trickling by the second. A RequestTimeout of zero
	// is DefaultRequestTimeout.
	IdleTimeout    time.Duration
	RequestTimeout time.Duration
	Retries        int
	Answers        Answers
	HTTP           *http.Client
	Log            *slog.Logger
	// Secrets keeps the token out of what is recorded of a request.
	Secrets *logs.Secrets

	// Now and Sleep are the clock, so tests do not wait for a retry.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) sleep(ctx context.Context, d time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.New(slog.DiscardHandler)
}

// Ask is what a caller wants sent: the method and path, the body when there is
// one, the model it is about, the header only this request carries, how the
// answer is read, and what keeps a record of it.
type Ask struct {
	Method string
	Path   string
	Model  string
	Body   []byte
	// Recorded is the body as the record keeps it, where that is not the body
	// as sent: a chat names the pictures it carries in place of their bytes.
	// Nil keeps the body.
	Recorded []byte
	Header   http.Header
	Read     func(io.Reader, *api.Record) error
	Recorder api.Recorder
}

// Get asks a JSON endpoint and decodes the answer into out.
func (c *Client) Get(ctx context.Context, path string, out any) error {
	return c.Call(ctx, Ask{
		Method: http.MethodGet,
		Path:   path,
		Read:   func(r io.Reader, _ *api.Record) error { return decode(r, out) },
	})
}

// Post sends in to a JSON endpoint and decodes the answer into out. It belongs
// to a turn, like a chat, so it is kept by the recorder it is given.
func (c *Client) Post(ctx context.Context, path string, in, out any, rec api.Recorder) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return c.Call(ctx, Ask{
		Method:   http.MethodPost,
		Path:     path,
		Body:     body,
		Recorder: rec,
		Read:     func(r io.Reader, _ *api.Record) error { return decode(r, out) },
	})
}

// Call sends a request that is not a stream, held to the request timeout.
func (c *Client) Call(ctx context.Context, a Ask) error {
	ctx, cancel := c.limited(ctx)
	defer cancel()
	return c.Send(ctx, a)
}

// limited gives a request that is not a stream the whole of the request
// timeout. The idle timeout still cuts it where it goes quiet; this is the
// bound on a body that keeps trickling and so never goes quiet at all.
func (c *Client) limited(ctx context.Context) (context.Context, context.CancelFunc) {
	wait := c.RequestTimeout
	if wait <= 0 {
		wait = DefaultRequestTimeout
	}
	return context.WithTimeoutCause(ctx, wait, fmt.Errorf("%w (%s)", api.ErrSlow, wait))
}

// Send makes a request, retrying the statuses the runner names, and records
// everything it sent and received.
func (c *Client) Send(ctx context.Context, a Ask) error {
	url, err := endpoint(c.BaseURL, a.Path)
	if err != nil {
		return err
	}
	header := http.Header{"Accept": {"application/json"}}
	maps.Copy(header, a.Header)
	if a.Body != nil {
		header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		header.Set("Authorization", "Bearer "+c.Token)
	}

	kept := a.Body
	if a.Recorded != nil {
		kept = a.Recorded
	}
	rec := &api.Record{
		Runner:         c.Runner,
		Model:          a.Model,
		Method:         a.Method,
		URL:            url,
		RequestHeaders: c.redacted(header),
		RequestBody:    kept,
		StartedAt:      c.now(),
	}
	recorder := api.Recording(a.Recorder)
	if err := recorder.StartRequest(ctx, rec); err != nil {
		return err
	}

	a.Header = header
	err = c.attempts(ctx, a, rec)
	rec.EndedAt = c.now()
	if err != nil {
		rec.Error = c.Secrets.Redact(err.Error())
	}
	if rerr := recorder.EndRequest(ctx, rec); rerr != nil {
		return errors.Join(err, rerr)
	}
	return err
}

// attempts sends the request until it is answered or is not worth sending
// again, and keeps every try on the record.
func (c *Client) attempts(ctx context.Context, a Ask, rec *api.Record) error {
	for sent := 0; ; sent++ {
		attempt := api.Attempt{StartedAt: c.now()}
		status, err := c.attempt(ctx, a, rec, &attempt)
		attempt.Status = status
		attempt.EndedAt = c.now()
		if err != nil {
			attempt.Error = c.Secrets.Redact(err.Error())
		}

		wait, again := c.retry(sent, rec.Method, status, err, attempt.EndedAt, rec.ResponseHeaders)
		attempt.RetryAfter = wait
		rec.Attempts = append(rec.Attempts, attempt)
		c.logAttempt(ctx, rec, attempt, again)

		if !again {
			return err
		}
		if err := c.sleep(ctx, wait); err != nil {
			return cut(ctx, err)
		}
	}
}

// retry says how long to wait before sending a request again, and whether it
// is worth it. An API that asks to be left alone for longer than maxDelay is
// reported instead, rather than holding a reply in silence. A request that
// never got a status because the connection went is sent again only when it
// is a GET, which asks the host to do nothing: a POST is reported, since
// nothing says the host did not take it. One that never got a status because
// it was given up on, by a timeout or a cancel, is not sent again either way.
func (c *Client) retry(sent int, method string, status int, err error, at time.Time, h http.Header) (time.Duration, bool) {
	if status == http.StatusOK || sent >= c.Retries {
		return 0, false
	}
	if status == 0 {
		if method != http.MethodGet || api.Gone(err) {
			return 0, false
		}
		return backoff(sent), true
	}
	wait, ok := c.Answers.Retry(at, status, h)
	if !ok {
		return 0, false
	}
	if wait <= 0 {
		wait = backoff(sent)
	}
	if wait > maxDelay {
		return 0, false
	}
	return wait, true
}

// logAttempt says how one try went. A try that is being sent again, or that
// took longer than slowRequest, is said at the level a run shows by default,
// since whoever is waiting for it is owed a word about why.
func (c *Client) logAttempt(ctx context.Context, rec *api.Record, a api.Attempt, again bool) {
	took := a.EndedAt.Sub(a.StartedAt)
	level := slog.LevelDebug
	if again || took > slowRequest {
		level = slog.LevelWarn
	}
	c.log().Log(ctx, level, "request attempt",
		"runner", rec.Runner, "model", rec.Model, "url", rec.URL,
		"status", a.Status, "duration", took, "retry_after", a.RetryAfter)
}

// backoff is how long to wait before sending a request again when the API says
// nothing about it: a second, doubling, up to the longest.
func backoff(sent int) time.Duration {
	wait := firstBackoff
	for range sent {
		wait *= 2
		if wait >= maxBackoff {
			return maxBackoff
		}
	}
	return wait
}

// attempt makes one try of a request, and reports the status it came back
// with.
func (c *Client) attempt(ctx context.Context, a Ask, rec *api.Record, attempt *api.Attempt) (int, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	var reader io.Reader
	if a.Body != nil {
		reader = bytes.NewReader(a.Body)
	}
	req, err := http.NewRequestWithContext(ctx, rec.Method, rec.URL, reader)
	if err != nil {
		return 0, err
	}
	req.Header = a.Header.Clone()
	// What the record says of the answer is this try's: one whose connection
	// went got no status, no body and no first byte, not the ones of the try
	// before it.
	rec.Status, rec.ResponseHeaders, rec.ResponseBody, rec.FirstByteAt = 0, nil, nil, time.Time{}

	// The wait for an answer counts as silence too, so a host that takes the
	// request and says nothing is given up on.
	idle := &idleReader{
		timeout: c.IdleTimeout,
		cancel:  func() { cancel(api.ErrIdle) },
		now:     c.now,
		onFirst: func(t time.Time) {
			attempt.FirstByteAt, rec.FirstByteAt = t, t
		},
	}
	idle.start()
	defer idle.stop()

	// A request that has not come back is said out loud while it is out, since
	// an attempt is otherwise only logged once it is over, and a host that
	// stalls looks the same from outside as a run that stopped.
	waiting := time.AfterFunc(slowRequest, func() {
		c.log().Warn("waiting for an answer", "runner", rec.Runner,
			"model", rec.Model, "url", rec.URL, "for", slowRequest)
	})

	resp, err := c.client().Do(req)
	// The answer is what was waited for, however long the rest of it takes:
	// a reply arrives for as long as she is writing, and the idle timeout is
	// what says a stream has stopped arriving.
	waiting.Stop()
	if err != nil {
		return 0, cut(ctx, err)
	}
	defer resp.Body.Close()
	headers := c.now()

	idle.r = resp.Body
	rec.Status = resp.StatusCode
	rec.ResponseHeaders = c.redacted(resp.Header)
	var seen bytes.Buffer

	tee := io.TeeReader(idle, &seen)
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, tee)
		rec.ResponseBody = seen.Bytes()
		idle.arrived(headers)
		return resp.StatusCode, c.apiError(resp.StatusCode, seen.Bytes())
	}

	err = cut(ctx, a.Read(tee, rec))
	if !errors.Is(err, ErrNotTaken) && !errors.Is(err, api.ErrIdle) {
		// What is left of a stream is read so the whole answer is recorded,
		// unless the caller has stopped taking it or the host went quiet.
		_, _ = io.Copy(io.Discard, tee)
	}
	rec.ResponseBody = seen.Bytes()
	idle.arrived(headers)
	if errors.Is(err, ErrNoAnswer) {
		// A 200 that carried nothing where the answer belongs carries the
		// answer in its body, which is an error the API wrote there.
		err = c.apiError(resp.StatusCode, seen.Bytes())
	}
	return resp.StatusCode, err
}

// endpoint is the path under the base URL, keeping whatever query the base
// already carries.
func endpoint(base, path string) (string, error) {
	b, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	ref, err := url.Parse(path)
	if err != nil {
		return "", err
	}
	out := *b
	out.Path = strings.TrimSuffix(b.Path, "/") + ref.Path
	q := b.Query()
	for k, values := range ref.Query() {
		for _, v := range values {
			q.Add(k, v)
		}
	}
	out.RawQuery = q.Encode()
	return out.String(), nil
}

func (c *Client) apiError(status int, body []byte) error {
	if e := c.Answers.Error(status, body); e != nil {
		return e
	}
	message := strings.TrimSpace(string(body))
	if message == "" {
		message = "the answer was empty"
	}
	return &api.APIError{Status: status, Message: message}
}

func decode(r io.Reader, out any) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// redacted is the header as it is recorded: the one that carries the key is
// masked whatever it holds, and any other value that holds a secret is redacted.
func (c *Client) redacted(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, vs := range h {
		values := make([]string, len(vs))
		for i, v := range vs {
			if strings.EqualFold(k, "Authorization") {
				values[i] = logs.Mask
				continue
			}
			values[i] = c.Secrets.Redact(v)
		}
		out[k] = values
	}
	return out
}

// cut names why a request was cut off here, since a cancelled context reads
// the same however it was cancelled.
func cut(ctx context.Context, err error) error {
	if err == nil || !(errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return err
	}
	cause := context.Cause(ctx)
	if cause != nil && !errors.Is(cause, context.Canceled) && !errors.Is(cause, context.DeadlineExceeded) {
		return cause
	}
	return err
}

// idleReader cancels a request when no byte arrives for the idle timeout. The
// APIs send keep-alive comments while a host works, so silence means the
// connection is gone.
type idleReader struct {
	r       io.Reader
	timeout time.Duration
	cancel  context.CancelFunc
	now     func() time.Time
	onFirst func(time.Time)

	timer *time.Timer
	first bool
}

func (i *idleReader) start() {
	if i.timeout > 0 {
		i.timer = time.AfterFunc(i.timeout, i.cancel)
	}
}

func (i *idleReader) stop() {
	if i.timer != nil {
		i.timer.Stop()
	}
}

func (i *idleReader) Read(p []byte) (int, error) {
	n, err := i.r.Read(p)
	if n > 0 {
		i.arrived(i.now())
		if i.timer != nil {
			i.timer.Reset(i.timeout)
		}
	}
	return n, err
}

// arrived marks when the answer began. An answer with an empty body began
// with its headers.
func (i *idleReader) arrived(t time.Time) {
	if i.first {
		return
	}
	i.first = true
	if i.onFirst != nil {
		i.onFirst(t)
	}
}
