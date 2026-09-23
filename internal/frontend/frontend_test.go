package frontend

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/conversation"
	"nerdola.dev/x/paula/internal/frontend/api"
	"nerdola.dev/x/paula/internal/store"
)

// talk is a conversation a test drives by hand.
type talk struct {
	mu         sync.Mutex
	events     []conversation.Event
	seq        conversation.Seq
	changed    chan struct{}
	posted     []conversation.NewMessage
	history    []store.Message
	models     conversation.Models
	set        []string
	resets     int
	pending    int
	stopped    bool
	stops      int
	stopErr    error
	setErr     error
	historyErr error
	behindAt   conversation.Seq

	summary   *store.Summary
	memories  []store.Memory
	asked     []string
	forgot    []store.MemoryID
	forgetErr error
}

func newTalk() *talk {
	return &talk{changed: make(chan struct{})}
}

func (t *talk) publish(e conversation.Event) {
	t.mu.Lock()
	t.seq++
	e.Seq = t.seq
	switch e.Kind {
	case conversation.ReplyDone, conversation.ReplyStopped, conversation.ReplyFailed:
		if t.pending > 0 {
			t.pending--
		}
	}
	t.events = append(t.events, e)
	close(t.changed)
	t.changed = make(chan struct{})
	t.mu.Unlock()
}

// fallBehind makes the next read say the events asked for are gone.
func (t *talk) fallBehind(history []store.Message) {
	t.mu.Lock()
	t.behindAt = t.seq + 1
	t.history = history
	close(t.changed)
	t.changed = make(chan struct{})
	t.mu.Unlock()
}

// Post stores the message and tells every session about it, the way the
// conversation does, saying which one it came from.
func (t *talk) Post(_ context.Context, m conversation.NewMessage) error {
	t.mu.Lock()
	t.posted = append(t.posted, m)
	t.pending++
	t.mu.Unlock()

	t.publish(conversation.Event{
		Kind:    conversation.MessageStored,
		Channel: m.Channel,
		From:    m.From,
		Message: &store.Message{
			ID:      store.MessageID(replies.Add(1)),
			Role:    store.RoleUser,
			Channel: m.Channel,
			Parts:   []store.Part{{Type: store.PartText, Text: m.Text}},
		},
	})
	return nil
}

// sent is what was posted, read the way another goroutine has to.
func (t *talk) sent() []conversation.NewMessage {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]conversation.NewMessage(nil), t.posted...)
}

func (t *talk) Stop(context.Context) (bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stops++
	return t.stopped, t.stopErr
}

// Wait blocks until nothing is being written, the way the conversation does.
func (t *talk) Wait(ctx context.Context) (conversation.Seq, error) {
	for {
		t.mu.Lock()
		pending, seq, changed := t.pending, t.seq, t.changed
		t.mu.Unlock()
		if pending == 0 {
			return seq, nil
		}
		select {
		case <-ctx.Done():
			return seq, ctx.Err()
		case <-changed:
		}
	}
}

// Standing answers with the number of the latest event and the latest message
// stored, taken together, the way the conversation does.
func (t *talk) Standing(context.Context) (conversation.Seq, store.MessageID, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var latest store.MessageID
	for _, m := range t.history {
		latest = max(latest, m.ID)
	}
	for _, e := range t.events {
		if e.Message != nil {
			latest = max(latest, e.Message.ID)
		}
	}
	return t.seq, latest, nil
}

func (t *talk) Events(ctx context.Context, after conversation.Seq) iter.Seq2[conversation.Event, error] {
	return func(yield func(conversation.Event, error) bool) {
		told := false
		for {
			t.mu.Lock()
			var batch []conversation.Event
			for _, e := range t.events {
				if e.Seq > after {
					batch = append(batch, e)
				}
			}
			behind := t.behindAt > 0 && !told
			changed := t.changed
			t.mu.Unlock()

			if behind {
				told = true
				if !yield(conversation.Event{}, conversation.ErrBehind) {
					return
				}
			}
			for _, e := range batch {
				if !yield(e, nil) {
					return
				}
				after = e.Seq
			}
			select {
			case <-ctx.Done():
				return
			case <-changed:
			}
		}
	}
}

// History answers with the newest messages older than one, as the
// conversation does, and with the newest of all when before is zero.
func (t *talk) History(_ context.Context, before store.MessageID, limit int) ([]store.Message, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.historyErr != nil {
		return nil, t.historyErr
	}
	var out []store.Message
	for _, m := range t.history {
		if before == 0 || m.ID < before {
			out = append(out, m)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

func (t *talk) Since(_ context.Context, after store.MessageID) ([]store.Message, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []store.Message
	for _, m := range t.history {
		if m.ID > after {
			out = append(out, m)
		}
	}
	return out, nil
}

func (t *talk) Models(context.Context) (conversation.Models, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.models, nil
}

func (t *talk) SetModel(_ context.Context, role config.Role, name string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.setErr != nil {
		return t.setErr
	}
	t.set = append(t.set, string(role)+"="+name)
	return nil
}

func (t *talk) ResetModels(context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.resets++
	return nil
}

func (t *talk) Summary(context.Context) (*store.Summary, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.summary, nil
}

func (t *talk) Memories(_ context.Context, query string, limit int) ([]store.Memory, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.asked = append(t.asked, query)
	if len(t.memories) > limit {
		return t.memories[:limit], nil
	}
	return t.memories, nil
}

func (t *talk) Forget(_ context.Context, id store.MemoryID) ([]store.Memory, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.forgot = append(t.forgot, id)
	if t.forgetErr != nil {
		return nil, t.forgetErr
	}
	return t.memories, nil
}

// screen is an adapter that writes down everything it was asked to do.
type screen struct {
	mu       sync.Mutex
	features api.Features
	// history is how much a screen that shows it asks for.
	history int
	inputs  chan api.Input
	lines   []string
	sent    []api.Outgoing
	started chan struct{}
}

func newScreen(f api.Features) *screen {
	return &screen{features: f, inputs: make(chan api.Input, 8), started: make(chan struct{})}
}

func (s *screen) Features() api.Features { return s.features }

func (s *screen) Start(context.Context) (<-chan api.Input, error) {
	close(s.started)
	return s.inputs, nil
}

func (s *screen) Send(_ context.Context, m api.Outgoing) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, m)
	s.write("send " + m.Text)
	return nil
}

// Writing records that she started or stopped writing, as a frontend that
// shows it is told.
func (s *screen) Writing(_ context.Context, on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if on {
		s.write("writing")
	} else {
		s.write("stopped writing")
	}
	return nil
}

// write records one thing that happened, with the lock already held.
func (s *screen) write(line string) { s.lines = append(s.lines, line) }

func (s *screen) opened() <-chan struct{} { return s.started }

func (s *screen) log() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.lines...)
}

func (s *screen) messages() []api.Outgoing {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]api.Outgoing(nil), s.sent...)
}

// The optional interfaces, each on a screen of its own.
type streaming struct{ *screen }

func (s *streaming) Stream(_ context.Context, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.write("stream " + text)
	return nil
}

func (s *streaming) EndStream(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.write("end stream")
	return nil
}

type showing struct{ *screen }

// History is what a screen showing it asks for. A zero one shows none, which is
// how a frontend that opens on nothing reads.
func (s *showing) History() int { return s.history }

func (s *showing) ShowUserMessage(_ context.Context, m *store.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.write("elsewhere " + m.Channel + ": " + m.Text())
	return nil
}

func (s *showing) ShowHistory(_ context.Context, ms []store.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range ms {
		s.write("history " + m.Role + ": " + m.Text())
	}
	return nil
}

// scrolling shows what was said before what is on the screen, the way a page
// scrolled back does.
type scrolling struct{ *showing }

func (s *scrolling) ShowOlder(_ context.Context, before store.MessageID, ms []store.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(ms) == 0 {
		s.write(fmt.Sprintf("older than %d: none", before))
		return nil
	}
	for _, m := range ms {
		s.write(fmt.Sprintf("older than %d %s: %s", before, m.Role, m.Text()))
	}
	return nil
}

// gone is a frontend that went away once it was open: showing her says so.
type gone struct{ *screen }

func (g *gone) Send(context.Context, api.Outgoing) error { return api.ErrGone }

type prompting struct{ *screen }

func (s *prompting) Prompt(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.write("prompt")
	return nil
}

// run starts a session, stops it when the test ends, and fails the test if it
// ended with an error. It returns once the session has opened the frontend, so
// nothing a test publishes is missed.
func run(t *testing.T, adapter api.Adapter, conv Conversation) {
	t.Helper()
	errs, stop := running(t, adapter, conv)
	t.Cleanup(func() {
		stop()
		if err := <-errs; err != nil {
			t.Error(err)
		}
	})
}

// running is run for a test that is about the ending: it hands back what the
// session ends with, and the stop that ends it.
func running(t *testing.T, adapter api.Adapter, conv Conversation) (<-chan error, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	errs := make(chan error, 1)
	go func() {
		errs <- New(adapter, Options{Conv: conv}).Run(ctx)
	}()
	t.Cleanup(cancel)

	type starter interface{ opened() <-chan struct{} }
	if s, ok := adapter.(starter); ok {
		select {
		case <-s.opened():
		case <-time.After(2 * time.Second):
			t.Fatal("the session never opened the frontend")
		}
	}
	return errs, cancel
}

// A frontend that is gone answers nothing from here on, so a session that
// stays is a session writing to nobody.
func TestASessionEndsWhenTheFrontendIsGone(t *testing.T) {
	s := &gone{newScreen(api.Features{Channel: "repl"})}
	tk := newTalk()
	ended, stop := running(t, s, tk)
	defer stop()

	// She says something, and showing it finds the frontend gone.
	for _, e := range reply(1, "hey you") {
		tk.publish(e)
	}

	select {
	case err := <-ended:
		if !errors.Is(err, api.ErrGone) {
			t.Errorf("the session ended with %v, want the frontend that is gone", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the session kept going with nothing to write to")
	}
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

func sawLine(s *screen, want string) func() bool {
	return func() bool {
		return slices.Contains(s.log(), want)
	}
}

// replies numbers the messages the helper below builds, since a conversation
// never stores the same message twice.
var replies atomic.Int64

func reply(entry store.EntryID, text string) []conversation.Event {
	msg := &store.Message{ID: store.MessageID(replies.Add(1)), Role: store.RoleAssistant,
		Parts: []store.Part{{Type: store.PartText, Text: text}}}
	return []conversation.Event{
		{Kind: conversation.ReplyStarted, Entry: entry},
		{Kind: conversation.ReplyText, Entry: entry, Text: text},
		{Kind: conversation.ReplyDone, Entry: entry, Message: msg},
	}
}

func TestAReplyIsStreamed(t *testing.T) {
	s := &streaming{newScreen(api.Features{Channel: "repl"})}
	tk := newTalk()
	run(t, s, tk)

	tk.publish(conversation.Event{Kind: conversation.ReplyStarted, Entry: 1})
	tk.publish(conversation.Event{Kind: conversation.ReplyText, Entry: 1, Text: "he"})
	tk.publish(conversation.Event{Kind: conversation.ReplyText, Entry: 1, Text: "hello"})
	tk.publish(conversation.Event{Kind: conversation.ReplyText, Entry: 1, Text: "hello you"})

	waitFor(t, "the stream", sawLine(s.screen, "stream  you"))
	var streamed strings.Builder
	for _, l := range s.log() {
		if after, ok := strings.CutPrefix(l, "stream "); ok {
			streamed.WriteString(after)
		}
	}
	if streamed.String() != "hello you" {
		t.Errorf("streamed %q, want each piece once", streamed.String())
	}
}

// A frontend cannot take back what it printed, so the stream is all it gets.
func TestAReplyWrittenOutIsNotSentAgain(t *testing.T) {
	s := &streaming{newScreen(api.Features{Channel: "repl"})}
	tk := newTalk()
	run(t, s, tk)

	for _, e := range reply(1, "hello you") {
		tk.publish(e)
	}
	// The reply that follows is the point by which the first would have been
	// sent again, had it been.
	for _, e := range reply(2, "and this one") {
		tk.publish(e)
	}
	waitFor(t, "the second reply", sawLine(s.screen, "stream and this one"))

	if got := s.messages(); len(got) != 0 {
		t.Errorf("sent %+v, want the replies left as they were written", got)
	}
}

// noting is a screen with a place of its own for what she is doing.
type noting struct{ *streaming }

func (s *noting) Note(_ context.Context, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.write("note " + text)
	return nil
}

// A note goes below what she has written so far, so the stream that text is
// on is closed first, and the reply goes on below it.
func TestANoteGoesBelowWhatWasWritten(t *testing.T) {
	s := &noting{&streaming{newScreen(api.Features{Channel: "web"})}}
	tk := newTalk()
	run(t, s, tk)

	msg := &store.Message{ID: store.MessageID(replies.Add(1)), Role: store.RoleAssistant,
		Parts: []store.Part{{Type: store.PartText, Text: "let me check\n\nfound it"}}}
	for _, e := range []conversation.Event{
		{Kind: conversation.ReplyStarted, Entry: 1},
		{Kind: conversation.ReplyText, Entry: 1, Text: "let me check"},
		{Kind: conversation.Note, Entry: 1, Text: "looking up Ana"},
		{Kind: conversation.ReplyText, Entry: 1, Text: "let me check\n\nfound it"},
		{Kind: conversation.ReplyDone, Entry: 1, Message: msg},
	} {
		tk.publish(e)
	}
	waitFor(t, "the reply to end", sawLine(s.screen, "end stream"))

	var got []string
	for _, l := range s.log() {
		if l != "writing" && l != "stopped writing" {
			got = append(got, l)
		}
	}
	want := []string{"stream let me check", "end stream", "note looking up Ana",
		"stream found it", "end stream"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("the screen shows %q, want %q", got, want)
	}
}

// A note closes the stream a reply was on, and a reply with nothing written
// after it is on the screen already: it is not sent again when it ends.
func TestAReplyANoteClosedIsNotSentAgain(t *testing.T) {
	s := &streaming{newScreen(api.Features{Channel: "repl"})}
	tk := newTalk()
	run(t, s, tk)

	msg := &store.Message{ID: store.MessageID(replies.Add(1)), Role: store.RoleAssistant,
		Parts: []store.Part{{Type: store.PartText, Text: "got it"}}}
	for _, e := range []conversation.Event{
		{Kind: conversation.ReplyStarted, Entry: 1},
		{Kind: conversation.ReplyText, Entry: 1, Text: "got it"},
		{Kind: conversation.Note, Entry: 1, Text: "remembering"},
		{Kind: conversation.ReplyDone, Entry: 1, Message: msg},
	} {
		tk.publish(e)
	}
	// A screen with nowhere of its own for a note is sent it in brackets.
	waitFor(t, "the note", sawLine(s.screen, "send (remembering)"))
	for _, e := range reply(2, "and this one") {
		tk.publish(e)
	}
	waitFor(t, "the next reply", sawLine(s.screen, "stream and this one"))

	for _, m := range s.messages() {
		if m.Hers {
			t.Errorf("sent %q again, want the reply left as it was written", m.Text)
		}
	}
}

// The mark goes on the end of what is already on the screen, rather than
// arriving as a message of its own.
func TestAStoppedReplySaysSoOnTheEndOfTheStream(t *testing.T) {
	base := newScreen(api.Features{Channel: "repl"})
	s := &streaming{base}
	tk := newTalk()
	run(t, s, tk)

	msg := &store.Message{ID: store.MessageID(replies.Add(1)), Role: store.RoleAssistant,
		Parts: []store.Part{{Type: store.PartText, Text: "hello yo"}}}
	tk.publish(conversation.Event{Kind: conversation.ReplyStarted, Entry: 1})
	tk.publish(conversation.Event{Kind: conversation.ReplyText, Entry: 1, Text: "hello yo"})
	tk.publish(conversation.Event{Kind: conversation.ReplyStopped, Entry: 1, Message: msg})

	waitFor(t, "the end of the stream", sawLine(base, "end stream"))
	// She is writing from the moment the reply starts until the last of it is
	// on the screen, which is after the mark that says it was stopped.
	want := []string{"writing", "stream hello yo", "stream  [stopped]", "end stream", "stopped writing"}
	if got := base.log(); !slices.Equal(got, want) {
		t.Errorf("wrote %v, want %v", got, want)
	}
	if got := s.messages(); len(got) != 0 {
		t.Errorf("sent %+v, want the mark written on the end of the reply", got)
	}
}

// A reply stopped while a tool ran has had its stream closed by the note, so
// the mark opens one of its own below it, where there is nothing to stand
// apart from.
func TestAReplyStoppedAfterANoteSaysSoBelowIt(t *testing.T) {
	base := newScreen(api.Features{Channel: "repl"})
	s := &streaming{base}
	tk := newTalk()
	run(t, s, tk)

	msg := &store.Message{ID: store.MessageID(replies.Add(1)), Role: store.RoleAssistant,
		Parts: []store.Part{{Type: store.PartText, Text: "let me check"}}}
	for _, e := range []conversation.Event{
		{Kind: conversation.ReplyStarted, Entry: 1},
		{Kind: conversation.ReplyText, Entry: 1, Text: "let me check"},
		{Kind: conversation.Note, Entry: 1, Text: "looking up Ana"},
		{Kind: conversation.ReplyStopped, Entry: 1, Message: msg},
	} {
		tk.publish(e)
	}
	waitFor(t, "the mark", sawLine(base, "stream [stopped]"))
	waitFor(t, "the end of the stream", func() bool {
		return slices.Contains(base.log(), "stopped writing")
	})
}

func TestAStoppedReplySaysSo(t *testing.T) {
	s := newScreen(api.Features{Channel: "repl"})
	tk := newTalk()
	run(t, s, tk)

	msg := &store.Message{ID: 99, Role: store.RoleAssistant,
		Parts: []store.Part{{Type: store.PartText, Text: "I was saying"}}}
	tk.publish(conversation.Event{Kind: conversation.ReplyStarted, Entry: 1})
	tk.publish(conversation.Event{Kind: conversation.ReplyStopped, Entry: 1, Message: msg})

	waitFor(t, "the reply", func() bool { return len(s.messages()) == 1 })
	if got := s.messages()[0].Text; got != "I was saying [stopped]" {
		t.Errorf("reply = %q", got)
	}
}

func TestAFailedReplySaysWhy(t *testing.T) {
	s := newScreen(api.Features{Channel: "repl"})
	tk := newTalk()
	run(t, s, tk)

	tk.publish(conversation.Event{Kind: conversation.ReplyStarted, Entry: 1})
	tk.publish(conversation.Event{Kind: conversation.ReplyFailed, Entry: 1, Text: "429: slow down"})

	waitFor(t, "the failure", func() bool { return len(s.messages()) == 1 })
	if got := s.messages()[0].Text; got != "error: 429: slow down" {
		t.Errorf("message = %q", got)
	}
}

// A reply that failed after it had run a tool keeps what it wrote, which is
// shown before what went wrong.
func TestAFailedReplyThatKeptWhatItWroteShowsIt(t *testing.T) {
	s := newScreen(api.Features{Channel: "repl"})
	tk := newTalk()
	run(t, s, tk)

	msg := &store.Message{ID: store.MessageID(replies.Add(1)), Role: store.RoleAssistant,
		Parts: []store.Part{{Type: store.PartText, Text: "let me note that"}}, Interrupted: true}
	tk.publish(conversation.Event{Kind: conversation.ReplyStarted, Entry: 1})
	tk.publish(conversation.Event{Kind: conversation.ReplyFailed, Entry: 1, Message: msg, Text: "the host went away"})

	waitFor(t, "the failure", func() bool { return len(s.messages()) == 2 })
	got := s.messages()
	if got[0].Text != "let me note that" || !got[0].Hers || got[1].Text != "error: the host went away" {
		t.Errorf("messages = %+v, want what she wrote and then why it ended", got)
	}
}

func TestARestartedReplyShowsNothing(t *testing.T) {
	s := &streaming{newScreen(api.Features{Channel: "repl"})}
	tk := newTalk()
	run(t, s, tk)

	tk.publish(conversation.Event{Kind: conversation.ReplyStarted, Entry: 1})
	tk.publish(conversation.Event{Kind: conversation.ReplyText, Entry: 1, Text: "wait"})
	tk.publish(conversation.Event{Kind: conversation.ReplyRestarted, Entry: 1})
	waitFor(t, "the stream to end", sawLine(s.screen, "end stream"))

	if len(s.messages()) != 0 {
		t.Errorf("messages = %+v, want none", s.messages())
	}
}

func TestWhatWasSaidSomewhereElse(t *testing.T) {
	base := newScreen(api.Features{Channel: "repl"})
	s := &showing{base}
	tk := newTalk()
	run(t, s, tk)

	mine := &store.Message{ID: 7, Role: store.RoleUser, Channel: "telegram",
		Parts: []store.Part{{Type: store.PartText, Text: "from my phone"}}}
	tk.publish(conversation.Event{Kind: conversation.MessageStored, Channel: "telegram", Message: mine})
	waitFor(t, "the message", sawLine(base, "elsewhere telegram: from my phone"))

	// Another terminal of the same frontend is shown as well, since it is not
	// this session that said it.
	here := &store.Message{ID: 8, Role: store.RoleUser, Channel: "repl",
		Parts: []store.Part{{Type: store.PartText, Text: "typed in the other terminal"}}}
	tk.publish(conversation.Event{Kind: conversation.MessageStored, Channel: "repl",
		From: "repl-elsewhere", Message: here})
	waitFor(t, "the other terminal", sawLine(base, "elsewhere repl: typed in the other terminal"))
}

func TestWhatWasSaidBefore(t *testing.T) {
	base := newScreen(api.Features{Channel: "repl"})
	base.history = 10
	s := &struct {
		*screen
		*showing
		*prompting
	}{}
	s.screen, s.showing, s.prompting = base, &showing{base}, &prompting{base}

	tk := newTalk()
	tk.history = []store.Message{
		{ID: 1, Role: store.RoleUser, Channel: "repl", Parts: []store.Part{{Type: store.PartText, Text: "hey"}}},
		{ID: 2, Role: store.RoleAssistant, Parts: []store.Part{{Type: store.PartText, Text: "hello"}}},
	}
	run(t, s, tk)

	waitFor(t, "the history", sawLine(base, "history assistant: hello"))
	waitFor(t, "the prompt", sawLine(base, "prompt"))
}

// opening asks for what was said before only once a test says so, which is
// where a frontend that takes a moment to open finds the conversation: a
// message may be stored between where the session starts and the read.
type opening struct {
	*screen
	ready chan struct{}
}

func (o *opening) History() int {
	<-o.ready
	return o.screen.history
}

func (o *opening) ShowHistory(_ context.Context, ms []store.Message) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, m := range ms {
		o.write("history " + m.Role + ": " + m.Text())
	}
	return nil
}

// A session reads where the conversation stands, and reads what was said
// before a moment later. What the second read carried is on the screen, so the
// events carrying it are not shown a second time.
func TestWhatWasReadBackIsNotShownTwice(t *testing.T) {
	base := newScreen(api.Features{Channel: "telegram"})
	base.history = 10
	s := &opening{screen: base, ready: make(chan struct{})}

	tk := newTalk()
	run(t, s, tk)

	// The reply lands after the session read where the conversation stood, and
	// before it read what was said before, so the read carries it.
	stored := &store.Message{ID: store.MessageID(replies.Add(1)), EntryID: 1,
		Role: store.RoleAssistant, Channel: "telegram",
		Parts: []store.Part{{Type: store.PartText, Text: "here I am"}}}
	tk.mu.Lock()
	tk.history = append(tk.history, *stored)
	tk.mu.Unlock()
	tk.publish(conversation.Event{Kind: conversation.ReplyStarted, Entry: 1})
	tk.publish(conversation.Event{Kind: conversation.ReplyDone, Entry: 1, Message: stored})
	close(s.ready)
	waitFor(t, "what was said before", sawLine(base, "history assistant: here I am"))

	// The next reply says the events before it have been handled by now.
	for _, e := range reply(2, "and again") {
		tk.publish(e)
	}
	waitFor(t, "the reply after it", sawLine(base, "send and again"))
	if slices.Contains(base.log(), "send here I am") {
		t.Error("the reply the history carried was shown a second time")
	}
}

// A frontend that shows the conversation as one long page asks for what came
// before what it holds, and is given a screenful of it: what it opened on is
// what it is given again.
func TestWhatWasSaidBeforeWhatIsOnTheScreen(t *testing.T) {
	base := newScreen(api.Features{Channel: "web"})
	base.history = 2
	s := &scrolling{&showing{base}}

	tk := newTalk()
	for id := 1; id <= 4; id++ {
		tk.history = append(tk.history, store.Message{
			ID: store.MessageID(id), Role: store.RoleUser, Channel: "web",
			Parts: []store.Part{{Type: store.PartText, Text: fmt.Sprintf("the %dth thing", id)}},
		})
	}
	run(t, s, tk)
	waitFor(t, "what the screen opened on", sawLine(base, "history user: the 4th thing"))

	s.inputs <- api.Input{Older: 3}
	waitFor(t, "what came before it", sawLine(base, "older than 3 user: the 2th thing"))
	if slices.Contains(base.log(), "older than 3 user: the 4th thing") {
		t.Error("what was on the screen already was shown again")
	}
}

// An ask is answered whatever came of it. A frontend that waits for what it
// asked for, and is told what went wrong instead, waits for an answer that is
// never coming.
func TestWhatCameBeforeIsAnsweredEvenWhenItCannotBeRead(t *testing.T) {
	base := newScreen(api.Features{Channel: "web"})
	base.history = 2
	s := &scrolling{&showing{base}}

	tk := newTalk()
	tk.history = []store.Message{
		{ID: 1, Role: store.RoleUser, Channel: "web", Parts: []store.Part{{Type: store.PartText, Text: "hey"}}},
	}
	run(t, s, tk)

	// The conversation goes unreadable once the screen is open on it, since a
	// screen that could not open is a session that ended.
	waitFor(t, "what the screen opened on", sawLine(base, "history user: hey"))
	tk.mu.Lock()
	tk.historyErr = errors.New("the conversation could not be read")
	tk.mu.Unlock()

	s.inputs <- api.Input{Older: 3}
	waitFor(t, "what went wrong", sawLine(base, "send error: the conversation could not be read"))
	waitFor(t, "the ask being answered", sawLine(base, "older than 3: none"))
}

func TestWhatIsTypedIsPosted(t *testing.T) {
	s := newScreen(api.Features{Channel: "repl"})
	tk := newTalk()
	run(t, s, tk)

	s.inputs <- api.Input{Text: "  hey there  "}
	waitFor(t, "the message", func() bool { return len(tk.sent()) == 1 })
	got := tk.sent()[0]
	if got.Channel != "repl" || got.Text != "hey there" {
		t.Errorf("posted = %+v", got)
	}
}

func TestAnImageIsPostedWithItsText(t *testing.T) {
	s := newScreen(api.Features{Channel: "repl"})
	tk := newTalk()
	run(t, s, tk)

	s.inputs <- api.Input{Text: "look", Images: [][]byte{{1, 2, 3}}}
	waitFor(t, "the message", func() bool { return len(tk.sent()) == 1 })
	got := tk.sent()[0]
	if got.Text != "look" || len(got.Images) != 1 {
		t.Errorf("posted = %+v, want the text and the image as it arrived", got)
	}
}

func TestCommands(t *testing.T) {
	s := newScreen(api.Features{Channel: "repl"})
	tk := newTalk()
	tk.models = conversation.Models{Roles: []conversation.RoleModels{
		{Role: "chat", Current: "pro", Default: "pro", Options: []string{"pro", "flash"}},
		{Role: "vision", Current: "flash", Saved: true, Options: []string{"flash"}},
	}}
	run(t, s, tk)

	type step struct {
		typed string
		want  string
	}
	for _, tc := range []step{
		{"/models", "chat: pro (pro, flash)\nvision: flash, saved"},
		{"/model chat flash", "chat: flash"},
		{"/model", "/model takes a role and a model, such as /model chat fast"},
		{"/stop", "nothing to stop"},
		{"/nope", "unknown command /nope, try /help"},
	} {
		before := len(s.messages())
		s.inputs <- api.Input{Text: tc.typed}
		waitFor(t, tc.typed, func() bool { return len(s.messages()) > before })
		if got := s.messages()[before].Text; got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.typed, got, tc.want)
		}
	}

	tk.mu.Lock()
	defer tk.mu.Unlock()
	if len(tk.posted) != 0 {
		t.Errorf("a command was posted as a message: %+v", tk.posted)
	}
	if len(tk.set) != 1 || tk.set[0] != "chat=flash" {
		t.Errorf("models set = %v", tk.set)
	}
}

func TestHelpListsEveryCommand(t *testing.T) {
	s := newScreen(api.Features{
		Channel:  "repl",
		Commands: []api.Command{{Name: "image", Args: "PATH [TEXT]", Short: "send a photo"}},
	})
	tk := newTalk()
	run(t, s, tk)

	s.inputs <- api.Input{Text: "/help"}
	waitFor(t, "the help", func() bool { return len(s.messages()) == 1 })
	got := s.messages()[0].Text
	for _, want := range []string{"/models", "/model ROLE NAME", "/stop", "/image PATH [TEXT]", "/help"} {
		if !strings.Contains(got, want) {
			t.Errorf("help has no %q:\n%s", want, got)
		}
	}
}

func TestModelsReset(t *testing.T) {
	s := newScreen(api.Features{Channel: "repl"})
	tk := newTalk()
	tk.models = conversation.Models{Roles: []conversation.RoleModels{{Role: "chat", Current: "pro"}}}
	run(t, s, tk)

	s.inputs <- api.Input{Text: "/models reset"}
	waitFor(t, "the reset", func() bool { return len(s.messages()) == 2 })
	if got := s.messages()[0].Text; got != "models reset" {
		t.Errorf("message = %q", got)
	}
	tk.mu.Lock()
	defer tk.mu.Unlock()
	if tk.resets != 1 {
		t.Errorf("resets = %d", tk.resets)
	}
}

// A session says when she starts writing and when the last of what she wrote
// is on the screen. What a frontend makes of that is its own.
func TestAFrontendIsToldWhenSheIsWriting(t *testing.T) {
	s := newScreen(api.Features{Channel: "repl"})
	tk := newTalk()
	run(t, s, tk)

	msg := &store.Message{ID: store.MessageID(replies.Add(1)), Role: store.RoleAssistant,
		Parts: []store.Part{{Type: store.PartText, Text: "hey you"}}}
	tk.publish(conversation.Event{Kind: conversation.ReplyStarted, Entry: 1})
	waitFor(t, "the word that she is writing", sawLine(s, "writing"))

	tk.publish(conversation.Event{Kind: conversation.ReplyDone, Entry: 1, Message: msg})
	waitFor(t, "the word that she has stopped", sawLine(s, "stopped writing"))

	// What she wrote is on the screen before she is said to have stopped.
	want := []string{"writing", "send hey you", "stopped writing"}
	if got := s.log(); !slices.Equal(got, want) {
		t.Errorf("wrote %v, want %v", got, want)
	}

	// A reply that fails says so too: nothing of it is coming.
	tk.publish(conversation.Event{Kind: conversation.ReplyStarted, Entry: 2})
	waitFor(t, "the second reply", func() bool { return len(s.log()) > 3 })
	tk.publish(conversation.Event{Kind: conversation.ReplyFailed, Entry: 2, Text: "the host is away"})
	waitFor(t, "the word that she has stopped", func() bool {
		return slices.Contains(s.log()[3:], "stopped writing")
	})
}

// A session says what can be picked and what picking it means. A frontend that
// can offer a choice hands back the one that was picked, without reading it.
func TestWhatCanBePickedOfTheModelsMenu(t *testing.T) {
	s := newScreen(api.Features{Channel: "repl"})
	tk := newTalk()
	tk.models = conversation.Models{Roles: []conversation.RoleModels{
		{Role: config.RoleChat, Current: "fast", Default: "fast", Options: []string{"fast", "pro"}},
		{Role: config.RoleEmbed, Current: "vectors", Default: "vectors", Options: []string{"vectors"}},
	}}
	run(t, s, tk)

	s.inputs <- api.Input{Text: "/models"}
	waitFor(t, "the menu", func() bool { return len(s.messages()) == 1 })
	menu := s.messages()[0]
	// It says the same thing in words, for a frontend that offers no choices.
	if !strings.Contains(menu.Text, "chat: fast") {
		t.Errorf("menu = %q, want what serves each role", menu.Text)
	}
	if len(menu.Choices) != 3 {
		t.Fatalf("offered %+v, want one per role and one to reset", menu.Choices)
	}

	// Picking a role offers the models that can serve it, the one that stands
	// now marked as such.
	s.inputs <- api.Input{Picked: menu.Choices[0].Picked}
	waitFor(t, "the role", func() bool { return len(s.messages()) == 2 })
	role := s.messages()[1]
	var current int
	for _, c := range role.Choices {
		if c.Current {
			current++
		}
	}
	if len(role.Choices) != 3 || current != 1 {
		t.Fatalf("offered %+v, want one per model, the one that stands marked, and a way back", role.Choices)
	}

	// Picking one has the role served by it.
	s.inputs <- api.Input{Picked: role.Choices[1].Picked}
	waitFor(t, "the menu again", func() bool { return len(s.messages()) == 3 })
	tk.mu.Lock()
	set := append([]string(nil), tk.set...)
	tk.mu.Unlock()
	if !slices.Equal(set, []string{"chat=pro"}) {
		t.Errorf("set %v, want the model that was picked", set)
	}

	// A choice from something long gone is said to be gone, not acted on.
	s.inputs <- api.Input{Picked: "nothing:like:this"}
	waitFor(t, "the answer", func() bool { return len(s.messages()) == 4 })
	if got := s.messages()[3].Text; !strings.Contains(got, "not something to pick") {
		t.Errorf("message = %q", got)
	}
}

func TestTheSummaryCommand(t *testing.T) {
	s := newScreen(api.Features{Channel: "repl"})
	tk := newTalk()
	run(t, s, tk)

	// Nothing has been folded away yet, so there is nothing to show.
	s.inputs <- api.Input{Text: "/summary"}
	waitFor(t, "the answer", func() bool { return len(s.messages()) == 1 })
	if got := s.messages()[0].Text; got != "no summary yet" {
		t.Errorf("message = %q", got)
	}

	// What it says it covers is the conversation's own time: the message it was
	// written up to, whenever the writing happened.
	tk.mu.Lock()
	tk.summary = &store.Summary{
		Content:    "they talked about Lisbon",
		CoversUpto: time.Date(2026, 9, 16, 20, 22, 0, 0, time.UTC),
	}
	tk.mu.Unlock()

	s.inputs <- api.Input{Text: "/summary"}
	waitFor(t, "the summary", func() bool { return len(s.messages()) == 2 })
	got := s.messages()[1].Text
	want := "summary up to Wednesday, 16 September 2026, 20:22:\nthey talked about Lisbon"
	if got != want {
		t.Errorf("message = %q, want %q", got, want)
	}
}

func TestTheMemoryCommands(t *testing.T) {
	s := newScreen(api.Features{Channel: "repl"})
	tk := newTalk()
	run(t, s, tk)

	s.inputs <- api.Input{Text: "/memory"}
	waitFor(t, "the answer", func() bool { return len(s.messages()) == 1 })
	if got := s.messages()[0].Text; got != "no memories yet" {
		t.Errorf("message = %q", got)
	}

	said := time.Date(2026, 9, 14, 20, 22, 0, 0, time.UTC)
	tk.mu.Lock()
	tk.memories = []store.Memory{{ID: 7, Content: "Caio's sister lives in Lisbon.", SaidAt: said}}
	tk.mu.Unlock()

	// A memory is listed by the number it can be forgotten by, and the day it
	// was said.
	s.inputs <- api.Input{Text: "/memory lisbon"}
	waitFor(t, "the memories", func() bool { return len(s.messages()) == 2 })
	if got := s.messages()[1].Text; got != "#7 (said on Monday, 14 September 2026) Caio's sister lives in Lisbon." {
		t.Errorf("message = %q", got)
	}

	s.inputs <- api.Input{Text: "/forget 7"}
	waitFor(t, "what was forgotten", func() bool { return len(s.messages()) == 3 })
	if got := s.messages()[2].Text; !strings.HasPrefix(got, "forgot:\n#7 ") {
		t.Errorf("message = %q", got)
	}

	// A number is what it takes, and it says so when it is given anything else.
	s.inputs <- api.Input{Text: "/forget everything"}
	waitFor(t, "the answer", func() bool { return len(s.messages()) == 4 })
	if got := s.messages()[3].Text; !strings.HasPrefix(got, "/forget takes the number") {
		t.Errorf("message = %q", got)
	}

	tk.mu.Lock()
	defer tk.mu.Unlock()
	// What was typed after the command is what she is asked for; nothing after
	// it is the newest.
	if !slices.Equal(tk.asked, []string{"", "lisbon"}) {
		t.Errorf("asked for %q, want the listing and then the query", tk.asked)
	}
	if !slices.Equal(tk.forgot, []store.MemoryID{7}) {
		t.Errorf("forgot %v, want the memory that was named", tk.forgot)
	}
}

func TestACommandThatFails(t *testing.T) {
	s := newScreen(api.Features{Channel: "repl"})
	tk := newTalk()
	tk.setErr = errors.New("no model is called \"gone\"")
	run(t, s, tk)

	s.inputs <- api.Input{Text: "/model chat gone"}
	waitFor(t, "the failure", func() bool { return len(s.messages()) == 1 })
	if got := s.messages()[0].Text; got != `error: no model is called "gone"` {
		t.Errorf("message = %q", got)
	}
}

func goroutines() int { return runtime.NumGoroutine() }

func TestNothingIsShownTwiceAfterFallingBehind(t *testing.T) {
	base := newScreen(api.Features{Channel: "repl"})
	base.history = 10
	s := &showing{base}
	tk := newTalk()
	run(t, s, tk)

	// She replied while the session was away, and the events are still there
	// to be read as well as the history.
	msg := store.Message{ID: 5, Role: store.RoleAssistant,
		Parts: []store.Part{{Type: store.PartText, Text: "you missed this"}}}
	tk.fallBehind([]store.Message{msg})
	tk.publish(conversation.Event{Kind: conversation.ReplyStarted, Entry: 1})
	tk.publish(conversation.Event{Kind: conversation.ReplyDone, Entry: 1, Message: &msg})

	waitFor(t, "what was missed", func() bool { return len(base.messages()) >= 1 })

	// What she says next is the point by which the first would have been shown
	// twice, had it been.
	next := store.Message{ID: 6, Role: store.RoleAssistant,
		Parts: []store.Part{{Type: store.PartText, Text: "and this"}}}
	tk.publish(conversation.Event{Kind: conversation.ReplyStarted, Entry: 2})
	tk.publish(conversation.Event{Kind: conversation.ReplyDone, Entry: 2, Message: &next})
	waitFor(t, "what she said next", sawLine(base, "send and this"))

	if got := base.messages(); len(got) != 2 {
		t.Errorf("messages = %+v, want each shown once", got)
	}
}

func TestOneLineAtATimeShowsTheReplyBeforeThePrompt(t *testing.T) {
	base := newScreen(api.Features{Channel: "repl", Sequential: true})
	s := &prompting{base}
	tk := newTalk()
	run(t, s, tk)
	waitFor(t, "the first prompt", sawLine(base, "prompt"))

	// Typing a message answers it, and only then asks for the next line.
	go func() {
		waitFor(t, "the message", func() bool { return len(tk.sent()) == 1 })
		for _, e := range reply(1, "hello you") {
			tk.publish(e)
		}
	}()
	base.inputs <- api.Input{Text: "hey"}

	waitFor(t, "the second prompt", func() bool {
		var prompts int
		for _, l := range base.log() {
			if l == "prompt" {
				prompts++
			}
		}
		return prompts == 2
	})

	var order []string
	for _, l := range base.log() {
		if l == "prompt" || strings.HasPrefix(l, "send ") {
			order = append(order, l)
		}
	}
	want := []string{"prompt", "send hello you", "prompt"}
	if strings.Join(order, "|") != strings.Join(want, "|") {
		t.Errorf("order = %v, want %v", order, want)
	}
}

// The terminal is asked for a line once the reply already being written has
// finished, not as soon as it opens.
func TestASequentialFrontendOpenedWhileSheIsWriting(t *testing.T) {
	tk := newTalk()
	tk.mu.Lock()
	tk.pending = 1
	tk.mu.Unlock()
	tk.publish(conversation.Event{Kind: conversation.ReplyStarted, Entry: 1})

	base := newScreen(api.Features{Channel: "repl", Sequential: true})
	s := &prompting{base}
	run(t, s, tk)
	waitFor(t, "the first prompt", sawLine(base, "prompt"))

	base.inputs <- api.Input{Text: "hey"}
	waitFor(t, "the message", func() bool { return len(tk.sent()) == 1 })

	// The reply she was already writing is not the one this line is waiting
	// for, so the next line is still not asked for.
	for _, e := range reply(1, "what I was saying") {
		tk.publish(e)
	}
	waitFor(t, "the reply she was writing", sawLine(base, "send what I was saying"))
	if got := prompts(base); got != 1 {
		t.Errorf("prompts = %d, want the next line not asked for yet", got)
	}

	for _, e := range reply(2, "and now yours") {
		tk.publish(e)
	}
	waitFor(t, "the next line to be asked for", func() bool { return prompts(base) == 2 })
}

// prompts is how many times the next line was asked for.
func prompts(s *screen) int {
	var n int
	for _, l := range s.log() {
		if l == "prompt" {
			n++
		}
	}
	return n
}

func TestNothingIsLeftRunningWhenASessionEnds(t *testing.T) {
	base := newScreen(api.Features{Channel: "repl"})
	s := &streaming{base}
	tk := newTalk()

	before := goroutines()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := New(s, Options{Conv: tk}).Run(ctx); err != nil {
			t.Error(err)
		}
	}()
	<-base.opened()
	for _, e := range reply(1, "hey you") {
		tk.publish(e)
	}
	waitFor(t, "the reply", sawLine(base, "stream hey you"))

	// The frontend closes, which is what a repl connection ending does. The
	// context it was given lives on.
	close(base.inputs)
	<-done
	cancel()

	waitFor(t, "the goroutines to go", func() bool { return goroutines() <= before })
}

// failing sends nothing until it is told to work again.
type failing struct {
	*screen
	broken atomic.Bool
}

func (f *failing) Send(ctx context.Context, m api.Outgoing) error {
	if f.broken.Load() {
		f.screen.mu.Lock()
		f.screen.write("failed " + m.Text)
		f.screen.mu.Unlock()
		return errors.New("the frontend is not listening")
	}
	return f.screen.Send(ctx, m)
}

func TestASessionOutlivesAMessageThatCouldNotBeSent(t *testing.T) {
	f := &failing{screen: newScreen(api.Features{Channel: "telegram"})}
	f.broken.Store(true)
	tk := newTalk()
	run(t, f, tk)

	for _, e := range reply(1, "this one is lost") {
		tk.publish(e)
	}
	waitFor(t, "the message that could not be sent", sawLine(f.screen, "failed this one is lost"))

	f.broken.Store(false)
	for _, e := range reply(2, "this one gets through") {
		tk.publish(e)
	}
	waitFor(t, "the next reply", sawLine(f.screen, "send this one gets through"))
}

// refusing takes no list of what can be typed, the way a client asked for one
// too often refuses it.
type refusing struct{ *screen }

func (r *refusing) ShowCommands(context.Context, []api.Command) error {
	return errors.New("too many requests")
}

func TestASessionOutlivesAListingItCouldNotShow(t *testing.T) {
	r := &refusing{newScreen(api.Features{Channel: "telegram"})}
	tk := newTalk()
	run(t, r, tk)

	for _, e := range reply(1, "she answers anyway") {
		tk.publish(e)
	}
	waitFor(t, "the reply", sawLine(r.screen, "send she answers anyway"))
}

// asking fails to ask for the next line, which is all a sequential frontend
// has to wait for.
type asking struct {
	*screen
	broken atomic.Bool
}

func (a *asking) Prompt(ctx context.Context) error {
	if a.broken.Load() {
		return errors.New("the terminal is gone")
	}
	a.screen.mu.Lock()
	a.screen.write("prompt")
	a.screen.mu.Unlock()
	return nil
}

func TestASequentialSessionEndsWhenItCannotAskForTheNextLine(t *testing.T) {
	a := &asking{screen: newScreen(api.Features{Channel: "repl", Sequential: true})}
	tk := newTalk()

	ctx := t.Context()
	done := make(chan error, 1)
	go func() { done <- New(a, Options{Conv: tk}).Run(ctx) }()

	waitFor(t, "the first prompt", sawLine(a.screen, "prompt"))
	a.broken.Store(true)
	a.inputs <- api.Input{Text: "/help"}

	select {
	case err := <-done:
		if err == nil {
			t.Error("the session ended with no reason")
		}
	case <-time.After(2 * time.Second):
		t.Error("the session is waiting for a line it never asked for")
	}
}

func TestCatchingUpShowsWhatAnotherTerminalSaid(t *testing.T) {
	base := newScreen(api.Features{Channel: "repl"})
	base.history = 10
	s := &showing{base}
	tk := newTalk()
	tk.history = []store.Message{
		{ID: 1, Role: store.RoleUser, Channel: "repl", Parts: []store.Part{{Type: store.PartText, Text: "hey"}}},
	}
	run(t, s, tk)
	waitFor(t, "the history", sawLine(base, "history user: hey"))

	tk.fallBehind([]store.Message{
		{ID: 1, Role: store.RoleUser, Channel: "repl", Parts: []store.Part{{Type: store.PartText, Text: "hey"}}},
		{ID: 6, Role: store.RoleUser, Channel: "repl", Parts: []store.Part{{Type: store.PartText, Text: "from the other window"}}},
	})

	waitFor(t, "the other window", sawLine(base, "elsewhere repl: from the other window"))
}

func TestAReplyIsReadAsItIsWritten(t *testing.T) {
	base := newScreen(api.Features{Channel: "repl", Sequential: true})
	s := &sequentialStream{screen: base}
	tk := newTalk()
	run(t, s, tk)

	waitFor(t, "the first prompt", sawLine(base, "prompt"))
	s.inputs <- api.Input{Text: "hey"}

	tk.publish(conversation.Event{Kind: conversation.ReplyStarted, Entry: 1})
	tk.publish(conversation.Event{Kind: conversation.ReplyText, Entry: 1, Text: "hey"})
	tk.publish(conversation.Event{Kind: conversation.ReplyText, Entry: 1, Text: "hey you"})

	// She is still writing, and it is already on the screen.
	waitFor(t, "the reply as it is written", sawLine(base, "stream  you"))
	for _, l := range base.log() {
		if l == "prompt" && seenAfter(base, l, "stream  you") {
			t.Error("the next line was asked for before the reply was done")
		}
	}

	msg := &store.Message{ID: store.MessageID(replies.Add(1)), Role: store.RoleAssistant,
		Parts: []store.Part{{Type: store.PartText, Text: "hey you"}}}
	tk.publish(conversation.Event{Kind: conversation.ReplyDone, Entry: 1, Message: msg})
	waitFor(t, "the second prompt", func() bool {
		var prompts int
		for _, l := range base.log() {
			if l == "prompt" {
				prompts++
			}
		}
		return prompts == 2
	})
}

// sequentialStream is a terminal: it writes a reply out and asks for the next
// line.
type sequentialStream struct{ *screen }

func (s *sequentialStream) Stream(_ context.Context, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.write("stream " + text)
	return nil
}

func (s *sequentialStream) EndStream(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.write("end stream")
	return nil
}

func (s *sequentialStream) Prompt(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.write("prompt")
	return nil
}

func seenAfter(s *screen, line, after string) bool {
	log := s.log()
	var at = -1
	for i, l := range log {
		if l == after {
			at = i
		}
	}
	for i, l := range log {
		if l == line && i > at {
			return true
		}
	}
	return false
}

func TestASessionDoesNotShowWhatWasTypedIntoIt(t *testing.T) {
	base := newScreen(api.Features{Channel: "repl"})
	s := &showing{base}
	tk := newTalk()
	run(t, s, tk)

	s.inputs <- api.Input{Text: "hey you"}
	waitFor(t, "the message to be posted", func() bool { return len(tk.sent()) == 1 })

	// What another terminal said is the point by which this line would have
	// been shown, had it been.
	tk.publish(conversation.Event{Kind: conversation.MessageStored, Channel: "repl",
		From: "repl-elsewhere", Message: &store.Message{ID: 99, Role: store.RoleUser,
			Channel: "repl", Parts: []store.Part{{Type: store.PartText, Text: "and this"}}}})
	waitFor(t, "the other terminal", sawLine(base, "elsewhere repl: and this"))

	for _, l := range base.log() {
		if strings.Contains(l, "hey you") {
			t.Errorf("what was typed here was shown again: %q", l)
		}
	}
}

func TestTwoSessionsShowEachOthersLines(t *testing.T) {
	first := newScreen(api.Features{Channel: "repl"})
	second := newScreen(api.Features{Channel: "repl"})
	tk := newTalk()
	run(t, &showing{first}, tk)
	run(t, &showing{second}, tk)

	first.inputs <- api.Input{Text: "from the first"}

	waitFor(t, "the other terminal", sawLine(second, "elsewhere repl: from the first"))
	for _, l := range first.log() {
		if strings.Contains(l, "from the first") {
			t.Errorf("the terminal it was typed into showed it again: %q", l)
		}
	}
}

// The rest of the reply reaches the terminal before the session ends.
func TestASessionEndsOnlyOnceSheHasFinished(t *testing.T) {
	base := newScreen(api.Features{Channel: "repl", Sequential: true})
	s := &sequentialStream{screen: base}
	tk := newTalk()

	ctx := t.Context()
	done := make(chan error, 1)
	go func() { done <- New(s, Options{Conv: tk}).Run(ctx) }()

	waitFor(t, "the first prompt", sawLine(base, "prompt"))
	s.inputs <- api.Input{Text: "hey"}
	waitFor(t, "the message", func() bool { return len(tk.sent()) == 1 })

	// She starts writing, and the terminal closes while she is.
	tk.publish(conversation.Event{Kind: conversation.ReplyStarted, Entry: 1})
	close(s.inputs)

	// What she writes after that is shown, so the session was still there.
	tk.publish(conversation.Event{Kind: conversation.ReplyText, Entry: 1, Text: "here"})
	waitFor(t, "what she was writing", sawLine(base, "stream here"))
	select {
	case <-done:
		t.Fatal("the session ended while she was still writing")
	default:
	}

	for _, e := range reply(1, "here you go") {
		tk.publish(e)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the session never ended")
	}
	if !sawLine(base, "stream here you go")() {
		t.Errorf("what she wrote was not shown:\n%v", base.log())
	}
}

// The key a terminal stops with does what the command does.
func TestAskingToStopWithoutTyping(t *testing.T) {
	s := newScreen(api.Features{Channel: "repl"})
	tk := newTalk()
	tk.stopped = true
	run(t, s, tk)

	s.inputs <- api.Input{Stop: true}
	waitFor(t, "the stop", func() bool {
		tk.mu.Lock()
		defer tk.mu.Unlock()
		return tk.stops == 1
	})
	if got := s.messages(); len(got) != 0 {
		t.Errorf("sent %+v, want nothing said when there was a reply to stop", got)
	}

	// With nothing to stop it says so.
	tk.mu.Lock()
	tk.stopped = false
	tk.mu.Unlock()
	s.inputs <- api.Input{Stop: true}
	waitFor(t, "what it said", sawLine(s, "send nothing to stop"))
}

// twoAtOnce yields both of its adapters at the same time, the way a socket
// does when two terminals dial together.
type twoAtOnce struct{ first, second api.Adapter }

func (t *twoAtOnce) Kind() string { return "two" }

func (t *twoAtOnce) Run(ctx context.Context, session func(context.Context, api.Adapter) error) error {
	var wg sync.WaitGroup
	for _, a := range []api.Adapter{t.first, t.second} {
		wg.Go(func() {
			_ = session(ctx, a)
		})
	}
	wg.Wait()
	return nil
}

func TestEachConnectionGetsItsOwnSession(t *testing.T) {
	first, second := newScreen(api.Features{Channel: "repl"}), newScreen(api.Features{Channel: "repl"})
	tk := newTalk()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, &twoAtOnce{first: first, second: second}, Options{Conv: tk})
	}()

	<-first.opened()
	<-second.opened()
	for _, e := range reply(1, "hey you") {
		tk.publish(e)
	}
	waitFor(t, "the reply on the first terminal", sawLine(first, "send hey you"))
	waitFor(t, "the reply on the second terminal", sawLine(second, "send hey you"))

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the frontend never stopped")
	}
}

// What was missed and what came after it each reach the screen once, in
// whichever order they arrive.
func TestASessionThatFellBehindShowsEachThingOnce(t *testing.T) {
	hey := store.Message{ID: 1, Role: store.RoleUser, Channel: "repl",
		Parts: []store.Part{{Type: store.PartText, Text: "hey"}}}
	missed := store.Message{ID: 5, Role: store.RoleAssistant, EntryID: 1,
		Parts: []store.Part{{Type: store.PartText, Text: "you missed this"}}}
	elsewhere := store.Message{ID: 6, Role: store.RoleUser, Channel: "telegram",
		Parts: []store.Part{{Type: store.PartText, Text: "from my phone"}}}

	t.Run("it falls behind with what came after already in the log", func(t *testing.T) {
		base := newScreen(api.Features{Channel: "repl"})
		base.history = 10
		s := &showing{base}
		tk := newTalk()
		tk.history = []store.Message{hey}
		run(t, s, tk)
		waitFor(t, "the history", sawLine(base, "history user: hey"))

		tk.history = []store.Message{hey, missed, elsewhere}
		tk.fallBehind(tk.history)
		tk.publish(conversation.Event{Kind: conversation.MessageStored,
			Channel: "telegram", From: "telegram-1", Message: &elsewhere})

		waitFor(t, "what was missed", sawLine(base, "send you missed this"))
		waitFor(t, "what came after", sawLine(base, "elsewhere telegram: from my phone"))
		shownOnce(t, base, "send you missed this", "elsewhere telegram: from my phone")
		if got := base.messages(); len(got) != 1 || !got[0].Hers {
			t.Errorf("messages = %+v, want the one she wrote, marked as hers", got)
		}
	})

	t.Run("it falls behind after handling what came before", func(t *testing.T) {
		base := newScreen(api.Features{Channel: "repl"})
		base.history = 10
		s := &showing{base}
		tk := newTalk()
		tk.history = []store.Message{hey}
		run(t, s, tk)
		waitFor(t, "the history", sawLine(base, "history user: hey"))

		tk.publish(conversation.Event{Kind: conversation.MessageStored,
			Channel: "telegram", From: "telegram-1", Message: &elsewhere})
		waitFor(t, "what came after", sawLine(base, "elsewhere telegram: from my phone"))

		// Then it falls behind, and reads back what she said afterwards.
		later := store.Message{ID: 9, Role: store.RoleAssistant, EntryID: 2,
			Parts: []store.Part{{Type: store.PartText, Text: "you missed this"}}}
		tk.history = []store.Message{hey, elsewhere, later}
		tk.fallBehind(tk.history)
		waitFor(t, "what was missed", sawLine(base, "send you missed this"))
		shownOnce(t, base, "send you missed this", "elsewhere telegram: from my phone")
	})
}

// shownOnce says each of the lines is on the screen exactly once.
func shownOnce(t *testing.T, s *screen, lines ...string) {
	t.Helper()
	for _, want := range lines {
		var n int
		for _, l := range s.log() {
			if l == want {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%q is on the screen %d times:\n%v", want, n, s.log())
		}
	}
}

func TestAReplyCaughtUpOnIsNotWrittenOutAgain(t *testing.T) {
	base := newScreen(api.Features{Channel: "repl"})
	base.history = 10
	s := &struct {
		*screen
		*streaming
		*showing
	}{}
	s.screen, s.streaming, s.showing = base, &streaming{base}, &showing{base}
	tk := newTalk()
	tk.history = []store.Message{
		{ID: 1, Role: store.RoleUser, Channel: "repl", Parts: []store.Part{{Type: store.PartText, Text: "hey"}}},
	}
	run(t, s, tk)
	waitFor(t, "the history", sawLine(base, "history user: hey"))

	// She replied while the session was away, and the events of that reply are
	// still in the log when it catches up.
	missed := store.Message{ID: 2, Role: store.RoleAssistant, EntryID: 1,
		Parts: []store.Part{{Type: store.PartText, Text: "you missed this"}}}
	tk.fallBehind([]store.Message{tk.history[0], missed})
	tk.publish(conversation.Event{Kind: conversation.ReplyStarted, Entry: 1})
	tk.publish(conversation.Event{Kind: conversation.ReplyText, Entry: 1, Text: "you missed this"})
	tk.publish(conversation.Event{Kind: conversation.ReplyDone, Entry: 1, Message: &missed})

	waitFor(t, "what was missed", sawLine(base, "send you missed this"))

	// What she says next is the point by which the missed one would have been
	// written out again, had it been.
	next := store.Message{ID: 3, Role: store.RoleAssistant, EntryID: 2,
		Parts: []store.Part{{Type: store.PartText, Text: "and this"}}}
	tk.publish(conversation.Event{Kind: conversation.ReplyStarted, Entry: 2})
	tk.publish(conversation.Event{Kind: conversation.ReplyText, Entry: 2, Text: "and this"})
	tk.publish(conversation.Event{Kind: conversation.ReplyDone, Entry: 2, Message: &next})
	waitFor(t, "what she said next", sawLine(base, "stream and this"))

	for _, l := range base.log() {
		if strings.Contains(l, "stream you missed this") {
			t.Errorf("the reply that was read back was written out again:\n%v", base.log())
		}
	}
}

// storing is a conversation that stores a reply between the two reads a
// session makes as it opens, which is the window a session must not have.
type storing struct {
	*talk
	reply store.Message
}

func (c *storing) Standing(ctx context.Context) (conversation.Seq, store.MessageID, error) {
	seq, latest, err := c.talk.Standing(ctx)
	// She finishes a reply the instant after the session read where it stands.
	c.talk.mu.Lock()
	c.talk.history = append(c.talk.history, c.reply)
	c.talk.mu.Unlock()
	c.talk.publish(conversation.Event{Kind: conversation.ReplyStarted, Entry: 1})
	c.talk.publish(conversation.Event{Kind: conversation.ReplyDone, Entry: 1, Message: &c.reply})
	return seq, latest, err
}

// The reply lands between the session reading where the conversation stands and
// the frontend opening, and the frontend shows no history to catch it.
func TestAReplyStoredWhileTheFrontendOpensIsShown(t *testing.T) {
	s := newScreen(api.Features{Channel: "repl"})
	tk := newTalk()
	conv := &storing{talk: tk, reply: store.Message{ID: 7, Role: store.RoleAssistant, EntryID: 1,
		Parts: []store.Part{{Type: store.PartText, Text: "just finished this"}}}}
	run(t, s, conv)

	waitFor(t, "the reply", sawLine(s, "send just finished this"))
}
