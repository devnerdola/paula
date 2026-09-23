// Package openai speaks the OpenAI-compatible HTTP API the hosted runners
// serve, and records everything it sends and receives.
package openai

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

// Hooks are the parts of a request and a stream only one API documents. Every
// runner answers all four, so the client never asks whether it has them.
type Hooks interface {
	// Body adds the runner's own fields to a chat request body.
	Body(body map[string]any, req api.ChatRequest) error
	// EmbedBody does the same for a request that turns text into vectors. Most
	// of what a chat request carries has no meaning here; where the request may
	// be routed has the same meaning it always had.
	EmbedBody(body map[string]any, req api.EmbedRequest) error
	// Message adds the runner's own fields to one message of a chat request.
	// It is where an assistant message hands back the reasoning it came with,
	// in the field each API documents for it.
	Message(out map[string]any, m api.Message)
	// Chunk reads the runner's own fields of a stream chunk. The reasoning
	// text it returns is passed on, and it fills in what it knows of the
	// result.
	Chunk(raw []byte, res *api.Result) (string, error)
	// End finishes what Chunk made of the result, once the stream is done.
	End(res *api.Result)
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
	Hooks          Hooks
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

// ask is what a caller wants sent: the method and path, the body when there is
// one, the model it is about, the header only this request carries, how the
// answer is read, and what keeps a record of it.
type ask struct {
	method   string
	path     string
	model    string
	body     []byte
	header   http.Header
	read     func(io.Reader, *api.Record) error
	recorder api.Recorder
	// dropAnswer leaves the bytes the host answered with out of the record. An
	// answer that is a number for every dimension of every input runs to
	// megabytes, and a dump of it says nothing the request and what it cost do
	// not. An answer that is an error is kept whatever this says.
	dropAnswer bool
}

// Get asks a JSON endpoint and decodes the answer into out.
func (c *Client) Get(ctx context.Context, path string, out any) error {
	ctx, cancel := c.limited(ctx)
	defer cancel()
	return c.send(ctx, ask{
		method: http.MethodGet,
		path:   path,
		read:   func(r io.Reader, _ *api.Record) error { return decode(r, out) },
	})
}

// limited gives a request that is not a stream the whole of the request
// timeout. The idle timeout still cuts it where it goes quiet; this is the
// bound on a body that keeps trickling and so never goes quiet at all.
func (c *Client) limited(ctx context.Context) (context.Context, context.CancelFunc) {
	wait := c.RequestTimeout
	if wait <= 0 {
		wait = DefaultRequestTimeout
	}
	return context.WithTimeoutCause(ctx, wait, fmt.Errorf("%w (%s)", ErrSlow, wait))
}

// Chat sends a chat request and passes every chunk of the stream to fn.
func (c *Client) Chat(ctx context.Context, req api.ChatRequest, fn func(api.Chunk) error) (*api.Result, error) {
	body, err := chatBody(req, c.Hooks)
	if err != nil {
		return nil, err
	}
	b, err := encode(body)
	if err != nil {
		return nil, err
	}
	res := new(api.Result)
	err = c.send(ctx, ask{
		method:   http.MethodPost,
		path:     "/chat/completions",
		model:    req.Model,
		body:     b,
		recorder: req.Recorder,
		read: func(r io.Reader, rec *api.Record) error {
			err := c.stream(r, res, fn)
			record(rec, res.Provider, res.FinishReason, res.Usage)
			return err
		},
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// embedded is the shape both APIs answer an embedding request in.
type embedded struct {
	Data []struct {
		// Index is which input the vector is for. A host that says nothing
		// leaves it nil rather than reading as the first input.
		Index     *int      `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
	} `json:"usage"`
}

// Embed turns each string into a vector. The answer is a whole body rather
// than a stream, so it is held to the request timeout like a listing is.
func (c *Client) Embed(ctx context.Context, req api.EmbedRequest) (*api.EmbedResult, error) {
	if len(req.Input) == 0 {
		return &api.EmbedResult{}, nil
	}
	body := map[string]any{"model": req.Model, "input": req.Input}
	if err := c.Hooks.EmbedBody(body, req); err != nil {
		return nil, err
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	out := &api.EmbedResult{Vectors: make([][]float32, len(req.Input))}
	ctx, cancel := c.limited(ctx)
	defer cancel()
	err = c.send(ctx, ask{
		method:     http.MethodPost,
		path:       "/embeddings",
		model:      req.Model,
		body:       b,
		recorder:   req.Recorder,
		dropAnswer: true,
		read: func(r io.Reader, rec *api.Record) error {
			raw, err := io.ReadAll(r)
			if err != nil {
				return err
			}
			var answer embedded
			if err := json.Unmarshal(raw, &answer); err != nil {
				return err
			}
			if len(answer.Data) == 0 {
				// An error written where the vectors belong is the shape an
				// error with a status of its own has, and says what went wrong
				// rather than leaving it to read as a vector that is missing.
				if e := c.Hooks.Error(http.StatusOK, raw); e != nil {
					return e
				}
			}
			for i, e := range answer.Data {
				// A host answers in the order it was asked, and says that order
				// anyway; what it says is what counts, and where it says
				// nothing the order it answered in is what is left.
				at := i
				if e.Index != nil {
					at = *e.Index
				}
				if at < 0 || at >= len(out.Vectors) {
					return fmt.Errorf("the answer holds a vector for input %d of %d", at, len(req.Input))
				}
				if out.Vectors[at] != nil {
					return fmt.Errorf("the answer holds two vectors for input %d of %d", at, len(req.Input))
				}
				out.Vectors[at] = e.Embedding
			}
			for i, v := range out.Vectors {
				if len(v) == 0 {
					return fmt.Errorf("the answer holds no vector for input %d of %d", i, len(req.Input))
				}
			}
			// An embedding writes nothing, so what it cost is what it read.
			out.Usage = api.Usage{PromptTokens: answer.Usage.PromptTokens}
			record(rec, "", "", out.Usage)
			return nil
		},
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// send makes a request, retrying the statuses the runner names, and records
// everything it sent and received.
func (c *Client) send(ctx context.Context, a ask) error {
	url, err := endpoint(c.BaseURL, a.path)
	if err != nil {
		return err
	}
	header := http.Header{"Accept": {"application/json"}}
	maps.Copy(header, a.header)
	if a.body != nil {
		header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		header.Set("Authorization", "Bearer "+c.Token)
	}

	rec := &api.Record{
		Runner:         c.Runner,
		Model:          a.model,
		Method:         a.method,
		URL:            url,
		RequestHeaders: c.redacted(header),
		RequestBody:    a.body,
		StartedAt:      c.now(),
	}
	recorder := api.Recording(a.recorder)
	if err := recorder.StartRequest(ctx, rec); err != nil {
		return err
	}

	a.header = header
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
func (c *Client) attempts(ctx context.Context, a ask, rec *api.Record) error {
	for sent := 0; ; sent++ {
		attempt := api.Attempt{StartedAt: c.now()}
		status, err := c.attempt(ctx, a, rec, &attempt)
		attempt.Status = status
		attempt.EndedAt = c.now()
		if err != nil {
			attempt.Error = c.Secrets.Redact(err.Error())
		}

		wait, again := c.retry(sent, status, attempt.EndedAt, rec.ResponseHeaders)
		attempt.RetryAfter = wait
		rec.Attempts = append(rec.Attempts, attempt)
		c.logAttempt(ctx, rec, attempt, again)

		if !again {
			return err
		}
		if err := c.sleep(ctx, wait); err != nil {
			return err
		}
	}
}

// retry says how long to wait before sending a request again, and whether it
// is worth it. An API that asks to be left alone for longer than maxDelay is
// reported instead, rather than holding a reply in silence. Only a status the
// API answered is sent again: a request that never got one, because the
// connection went, is reported, since nothing says the host did not take it.
func (c *Client) retry(sent, status int, at time.Time, h http.Header) (time.Duration, bool) {
	if status == 0 || status == http.StatusOK || sent >= c.Retries {
		return 0, false
	}
	wait, ok := c.Hooks.Retry(at, status, h)
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
func (c *Client) attempt(ctx context.Context, a ask, rec *api.Record, attempt *api.Attempt) (int, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	var reader io.Reader
	if a.body != nil {
		reader = bytes.NewReader(a.body)
	}
	req, err := http.NewRequestWithContext(ctx, rec.Method, rec.URL, reader)
	if err != nil {
		return 0, err
	}
	req.Header = a.header.Clone()

	// The wait for an answer counts as silence too, so a host that takes the
	// request and says nothing is given up on.
	idle := &idleReader{
		timeout: c.IdleTimeout,
		cancel:  func() { cancel(ErrIdle) },
		now:     c.now,
		onFirst: func(t time.Time) {
			attempt.FirstByteAt = t
			if rec.FirstByteAt.IsZero() {
				rec.FirstByteAt = t
			}
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

	// An answer that is not kept is not copied either: megabytes of numbers
	// would be held twice over, once to be read and once to be thrown away.
	from := io.Reader(tee)
	if a.dropAnswer {
		from = idle
	}
	err = cut(ctx, a.read(from, rec))
	if !errors.Is(err, errCallback) && !errors.Is(err, ErrIdle) {
		// What is left of a stream is read so the whole answer is recorded,
		// unless the caller has stopped taking it or the host went quiet.
		_, _ = io.Copy(io.Discard, from)
	}
	rec.ResponseBody = seen.Bytes()
	idle.arrived(headers)
	if errors.Is(err, errNoEvents) {
		// A 200 that streamed nothing at all carries the answer in its body,
		// which is an error the API wrote where the events belong.
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

// record keeps what the answer said about itself: the host that served it, how
// it finished, and what it cost. paula turns reads them back.
func record(rec *api.Record, provider, finish string, usage api.Usage) {
	if rec == nil {
		return
	}
	rec.Provider = provider
	rec.FinishReason = finish
	// An answer that said nothing about what it cost is not one that cost
	// nothing, and paula turns adds these up.
	if usage != (api.Usage{}) {
		rec.Usage = &usage
	}
}

func (c *Client) apiError(status int, body []byte) error {
	if e := c.Hooks.Error(status, body); e != nil {
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

// Why a request was cut off is vocabulary a caller reads, so it is the shared
// one: a conversation tells a host that refused from a connection that went.
var (
	ErrIdle = api.ErrIdle
	ErrSlow = api.ErrSlow
)

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
