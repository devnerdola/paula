package openai

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nerdola.dev/x/paula/internal/runners/api"
	"nerdola.dev/x/paula/internal/runners/transport"
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
func asked(ctx context.Context, c *transport.Client, rec api.Recorder) error {
	_, err := Chat(ctx, c, parts{}, api.ChatRequest{
		Model:    "some/model",
		Messages: []api.Message{api.Text(api.RoleUser, "hey")},
		Recorder: rec,
	}, func(api.Chunk) error { return nil })
	return err
}

// chatted sends a chat request of one message with nothing else in it.
func chatted(c *transport.Client, fn func(api.Chunk) error) error {
	_, err := Chat(context.Background(), c, parts{}, api.ChatRequest{
		Model:    "some/model",
		Messages: []api.Message{api.Text(api.RoleUser, "hey")},
	}, fn)
	return err
}

// parts stands in for the parts of an API a test is about, and answers for a
// runner that documents nothing of its own where a test sets none.
type parts struct {
	body  func(map[string]any, api.ChatRequest) error
	chunk func([]byte, *api.Result) (string, error)
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

func (parts) Retry(time.Time, int, http.Header) (time.Duration, bool) { return 0, false }

func (parts) Error(int, []byte) *api.APIError { return nil }

// recorder keeps what the client reported, the way the conversation does.
type recorder struct {
	ended []*api.Record
}

func (r *recorder) StartRequest(context.Context, *api.Record) error { return nil }

func (r *recorder) EndRequest(_ context.Context, rec *api.Record) error {
	r.ended = append(r.ended, rec)
	return nil
}

func client(t *testing.T, url string) *transport.Client {
	t.Helper()
	return &transport.Client{
		Runner:  "test",
		BaseURL: url + "/v1",
		Token:   "test-token-abcdefgh",
		Retries: 2,
		Answers: parts{},
	}
}

func TestHookFailureStopsTheRequest(t *testing.T) {
	_, _, err := chatBody(api.ChatRequest{Model: "m"}, parts{
		body: func(map[string]any, api.ChatRequest) error { return errors.New("no") },
	})
	if err == nil {
		t.Fatal("chatBody succeeded")
	}
}

// signing is an extension that writes the key of the conversation a request
// belongs to under a field of its own.
type signing struct{}

func (signing) Notes() api.Notes { return api.Notes{Role: api.RoleSystem} }

func (signing) PastThought() bool { return false }

func (signing) Body(body map[string]any, req api.ChatRequest) { body["signed"] = req.CacheKey }

// What a model was set up with beside its settings adds to the body the runner
// built. The runner sends nothing of the conversation a request belongs to by
// itself: that is for the extension to say.
func TestAnExtensionAddsToTheBodyTheRunnerBuilt(t *testing.T) {
	ts, srv := newServer(t, streamed("hey"), streamed("hey"))
	c := client(t, ts.URL)
	for _, extension := range []api.Extension{nil, signing{}} {
		_, err := Chat(context.Background(), c, parts{}, api.ChatRequest{
			Model:    "some/model",
			Messages: []api.Message{api.Text(api.RoleUser, "hey")},
			Settings: api.Settings{Extension: extension},
			CacheKey: "paula-paula-reply",
		}, func(api.Chunk) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
	}
	if strings.Contains(srv.bodies[0], "paula-paula-reply") {
		t.Errorf("a request with no extension carries its conversation's key: %s", srv.bodies[0])
	}
	if !strings.Contains(srv.bodies[1], `"signed":"paula-paula-reply"`) {
		t.Errorf("the extension's field is not in the body: %s", srv.bodies[1])
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
	c := client(t, ts.URL)
	_, err := Chat(context.Background(), c, parts{}, api.ChatRequest{
		Model:    "some/model",
		Messages: []api.Message{api.Text(api.RoleUser, "hey")},
		Tools:    []api.ToolDef{{Name: "search_memories", Parameters: json.RawMessage(`{"type":"object"}`)}},
		Settings: api.Settings{Extension: signing{}},
		CacheKey: "paula-paula-reply",
	}, func(api.Chunk) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	body := srv.bodies[0]
	messages := strings.Index(body, `"messages":`)
	for _, field := range []string{`"model":`, `"signed":`, `"stream":`, `"tools":`} {
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
			rec := &recorder{}
			if err := asked(context.Background(), client(t, ts.URL), rec); err != nil {
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
	c := client(t, ts.URL)
	ctx := context.Background()
	limit := 2048
	temperature := 0.8
	_, err := Chat(ctx, c, parts{}, api.ChatRequest{
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
			err := chatted(client(t, ts.URL), func(api.Chunk) error { return nil })
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
	body, _, err := chatBody(api.ChatRequest{
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
	body, _, err := chatBody(api.ChatRequest{
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
}

// A round asked for an answer with no call in it says so.
func TestARoundWithNoCallSaysSo(t *testing.T) {
	body, _, err := chatBody(api.ChatRequest{
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
	body, _, err := chatBody(api.ChatRequest{
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

func TestACallbackThatStopsTakingTheAnswer(t *testing.T) {
	ts, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for range 3 {
			io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"content":"one "}}]}`+"\n\n")
		}
		io.WriteString(w, "data: [DONE]\n\n")
	})

	want := errors.New("the terminal is gone")
	var chunks int
	err := chatted(client(t, ts.URL), func(api.Chunk) error {
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

	err := chatted(client(t, ts.URL), func(api.Chunk) error { return nil })
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
	if err := chatted(client(t, ts.URL), func(api.Chunk) error { return nil }); err == nil {
		t.Fatal("Chat succeeded, want a line that long refused")
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
			c := client(t, ts.URL)
			c.IdleTimeout = idle

			start := time.Now()
			err := asked(context.Background(), c, nil)
			if !errors.Is(err, api.ErrIdle) {
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

	var text strings.Builder
	err := chatted(client(t, ts.URL), func(chunk api.Chunk) error {
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

// A picture a chat carries goes to the host as its bytes and into the record
// as the sha256 it is kept under in the media directory, so the record of a
// prompt of pictures is a line for each. A chat with no picture is recorded as
// it was sent.
func TestAPictureIsRecordedByItsName(t *testing.T) {
	ts, srv := newServer(t, streamed("hey"), streamed("hey"))
	c := client(t, ts.URL)
	rec := &recorder{}
	data := []byte("a picture")
	sum := sha256.Sum256(data)
	_, err := Chat(context.Background(), c, parts{}, api.ChatRequest{
		Model: "some/model", Recorder: rec,
		Messages: []api.Message{{Role: api.RoleUser, Parts: []api.Part{
			{Type: api.PartText, Text: "look"},
			{Type: api.PartImage, MIME: "image/jpeg", Data: data},
		}}},
	}, func(api.Chunk) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	sent := srv.bodies[0]
	raw := `"url":"data:image/jpeg;base64,` + base64.StdEncoding.EncodeToString(data) + `"`
	name := `"url":"sha256:` + hex.EncodeToString(sum[:]) + `"`
	if !strings.Contains(sent, raw) {
		t.Errorf("the host was sent %s, want the picture's bytes in it", sent)
	}
	kept := string(rec.ended[0].RequestBody)
	if !strings.Contains(kept, name) || strings.Contains(kept, "base64,") {
		t.Errorf("the record kept %s, want the picture by its name", kept)
	}
	if strings.Replace(kept, name, raw, 1) != sent {
		t.Errorf("the record differs from what was sent beyond the picture:\n%s\n%s", kept, sent)
	}

	if err := asked(context.Background(), c, rec); err != nil {
		t.Fatal(err)
	}
	if kept := string(rec.ended[1].RequestBody); kept != srv.bodies[1] {
		t.Errorf("a chat with no picture was recorded as %s, want as sent: %s", kept, srv.bodies[1])
	}
}
