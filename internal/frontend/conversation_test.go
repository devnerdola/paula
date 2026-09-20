package frontend

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/conversation"
	"nerdola.dev/x/paula/internal/frontend/api"
	"nerdola.dev/x/paula/internal/persona"
	"nerdola.dev/x/paula/internal/runners"
	runnersapi "nerdola.dev/x/paula/internal/runners/api"
	"nerdola.dev/x/paula/internal/store"
)

// The tests here run a session against the conversation itself: a database in a
// temporary directory, the engine, and a runner the test answers through.

// saying is the runner the engine writes its replies through. It answers with
// the text the test set, and holds the reply there while hold is open.
type saying struct {
	mu    sync.Mutex
	model runnersapi.Model
	text  string
	hold  chan struct{}
	begun chan struct{}
}

func (s *saying) Name() string                 { return "fake" }
func (s *saying) Kind() string                 { return "fake" }
func (s *saying) URL() string                  { return "fake:///v1" }
func (s *saying) Health(context.Context) error { return nil }

func (s *saying) Models(context.Context) ([]runnersapi.Model, error) {
	return []runnersapi.Model{s.model}, nil
}

func (s *saying) Model(_ context.Context, id string) (*runnersapi.Model, error) {
	m := s.model
	return &m, nil
}

func (s *saying) Check(context.Context, runnersapi.Checked) []error { return nil }
func (s *saying) Settings() runnersapi.Settings                     { return runnersapi.Settings{} }

func (s *saying) Chat(ctx context.Context, _ runnersapi.ChatRequest, fn func(runnersapi.Chunk) error) (*runnersapi.Result, error) {
	s.mu.Lock()
	hold, begun, text := s.hold, s.begun, s.text
	s.mu.Unlock()

	if err := fn(runnersapi.Chunk{Kind: runnersapi.ChunkText, Text: text}); err != nil {
		return nil, err
	}
	if begun != nil {
		close(begun)
	}
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &runnersapi.Result{FinishReason: "stop"}, nil
}

// talking is the conversation as it really is, with the runner a test answers
// through and the database it keeps.
type talking struct {
	*conversation.Engine
	runner *saying
	store  *store.Store
}

func conversationFor(t *testing.T, r *saying) *talking {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	m := &runners.Configured{Name: "chat", Path: "models.chat", ID: r.model.ID, Runner: r}
	cfg := config.DefaultEngine()
	cfg.Debounce = config.Duration(10 * time.Millisecond)
	e, err := conversation.Open(context.Background(), conversation.Options{
		Store: st,
		Runners: &runners.Setup{
			Runners:  []runners.Runner{r},
			Models:   []*runners.Configured{m},
			Defaults: map[config.Role]*runners.Configured{config.RoleChat: m},
		},
		Persona: &persona.Card{ID: "paula", Name: "Paula", User: persona.User{Name: "Caio"}},
		Engine:  cfg,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return &talking{Engine: e, runner: r, store: st}
}

func answering(text string) *saying {
	return &saying{
		model: runnersapi.Model{ID: "some/model", Context: 100000, Chat: true, Tools: true},
		text:  text,
	}
}

// The whole way through: what is typed is stored, answered by the model, shown
// on the screen, and left in the conversation for the next session to read.
func TestALineIsAnsweredAndKept(t *testing.T) {
	conv := conversationFor(t, answering("hey you"))
	s := newScreen(api.Features{Channel: "repl"})
	run(t, s, conv)

	s.inputs <- api.Input{Text: "hey"}
	waitFor(t, "the reply", func() bool {
		return strings.Contains(strings.Join(s.log(), "\n"), "send hey you")
	})

	messages, err := conv.History(context.Background(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 {
		t.Fatalf("the conversation holds %+v, want the line and the reply", messages)
	}
	if messages[0].Text() != "hey" || messages[0].Channel != "repl" {
		t.Errorf("what was said = %+v", messages[0])
	}
	if messages[1].Text() != "hey you" || messages[1].ReplyTo != messages[0].ID {
		t.Errorf("the reply = %+v", messages[1])
	}
}

// What was written stays on the screen with the mark, and the entry ends as
// stopped.
func TestAReplyIsStoppedOnTheScreenAndInTheConversation(t *testing.T) {
	r := answering("half a ")
	r.hold, r.begun = make(chan struct{}), make(chan struct{})
	conv := conversationFor(t, r)
	s := &streaming{newScreen(api.Features{Channel: "repl"})}
	run(t, s, conv)

	s.inputs <- api.Input{Text: "hey"}
	<-r.begun
	waitFor(t, "the text to be streamed", func() bool {
		return strings.Contains(strings.Join(s.log(), "\n"), "stream half a ")
	})

	s.inputs <- api.Input{Stop: true}
	waitFor(t, "the mark", func() bool {
		return strings.Contains(strings.Join(s.log(), "\n"), "stream  [stopped]")
	})
	close(r.hold)

	waitFor(t, "the entry to end", func() bool {
		entries, err := conv.store.Entries(context.Background(), 1)
		return err == nil && len(entries) == 1 && entries[0].Status == store.StatusStopped
	})
	messages, err := conv.History(context.Background(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[1].Text() != "half a" || !messages[1].Interrupted {
		t.Errorf("the conversation holds %+v, want what she had written, marked", messages)
	}
}

// The engine gives the session the history, and it goes on from where the
// conversation stands rather than showing it again.
func TestASecondSessionOpensOnWhatWasSaid(t *testing.T) {
	conv := conversationFor(t, answering("hey you"))
	first := newScreen(api.Features{Channel: "repl"})
	run(t, first, conv)

	first.inputs <- api.Input{Text: "hey"}
	waitFor(t, "the reply", func() bool {
		return strings.Contains(strings.Join(first.log(), "\n"), "send hey you")
	})

	second := &showing{newScreen(api.Features{Channel: "repl"})}
	second.history = 10
	run(t, second, conv)
	waitFor(t, "what was said", func() bool {
		return strings.Contains(strings.Join(second.log(), "\n"), "history user: hey")
	})
	log := strings.Join(second.log(), "\n")
	if !strings.Contains(log, "history assistant: hey you") {
		t.Errorf("the second session was not given the reply:\n%s", log)
	}
	if strings.Contains(log, "send hey you") {
		t.Errorf("the second session showed the reply it had just been given:\n%s", log)
	}
}
