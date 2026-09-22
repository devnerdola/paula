package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"nerdola.dev/x/paula/internal/frontend/api"
	"nerdola.dev/x/paula/internal/store"
)

// adapter is one page: the stream everything goes down, and what is typed on
// it on the way back.
type adapter struct {
	f      *Frontend
	id     string
	inputs chan api.Input

	// mu is held to write to the stream. What the session says goes out an
	// event at a time, and the beat goes between them.
	mu    sync.Mutex
	w     http.ResponseWriter
	flush *http.ResponseController
	// gone says nothing more goes down this stream: the browser stopped reading
	// it, or the request it rode on is over.
	gone bool

	// read is the last event the browser says it read when it opens the stream
	// again, and caught that it read everything there was: one that did is not
	// shown the conversation over again.
	read   int64
	caught bool
	// saying is whether a reply is arriving on this page, which the session
	// says and the end of the page takes back.
	saying atomic.Bool
	// done is closed once the session on this page has ended, so nothing waits
	// to hand it something it will never take.
	done chan struct{}
	// older is where what a page asked for is handed to the request that asked
	// for it, and asking holds it to one ask at a time.
	older  chan answer
	asking sync.Mutex

	// bubble is the one she is filling, 0 when she is between them, text what it
	// holds, and wrote the reply as far as it has arrived. All three are
	// touched from the session's goroutine alone.
	bubble int64
	text   string
	wrote  api.Written
}

// said is a message of the conversation as the page reads it. The pictures are
// what they hold, which is what they are served under.
type said struct {
	ID       store.MessageID `json:"id"`
	Role     string          `json:"role"`
	Channel  string          `json:"channel"`
	Text     string          `json:"text"`
	Pictures []string        `json:"pictures,omitempty"`
	At       time.Time       `json:"at"`
}

func saying(m *store.Message) said {
	out := said{
		ID: m.ID, Role: m.Role, Channel: m.Channel,
		Text: m.Text(), At: m.CreatedAt,
	}
	for _, p := range m.Images() {
		out.Pictures = append(out.Pictures, p.SHA256)
	}
	return out
}

func sayings(ms []store.Message) []said {
	out := make([]said, 0, len(ms))
	for _, m := range ms {
		out = append(out, saying(&m))
	}
	return out
}

func (a *adapter) Features() api.Features {
	return api.Features{Channel: Kind}
}

// Start hands over what is typed on the page. It is called where the session
// reads how far the conversation has got, which is the moment a browser that
// missed nothing is caught up: everything after it, the session is told and
// tells the page.
func (a *adapter) Start(context.Context) (<-chan api.Input, error) {
	a.caught = a.read > 0 && a.read == a.f.told.Load() && a.f.writing.Load() == 0
	return a.inputs, nil
}

// Send puts one thing on the page, which is the paragraphs of it: a page shows
// a message at a time, the way the chat it looks like does.
func (a *adapter) Send(_ context.Context, m api.Outgoing) error {
	texts := api.Bubbles(m.Text, nil)
	if len(texts) == 0 && len(m.Choices) == 0 {
		return nil
	}
	if len(texts) == 0 {
		// A choice with nothing said before it is still a choice to offer.
		texts = []string{""}
	}
	for i, text := range texts {
		bubble := message{Text: text, Hers: m.Hers}
		if i == len(texts)-1 {
			bubble.Choices = choices(m.Choices)
		}
		if err := a.bubbled(bubble); err != nil {
			return err
		}
	}
	return nil
}

// Stream shows a reply as she writes it: every text in a bubble of its own,
// and the one she is in the middle of as it fills.
func (a *adapter) Stream(_ context.Context, text string) error {
	for _, done := range a.wrote.Add(text) {
		if err := a.show(done, true); err != nil {
			return err
		}
	}
	return a.show(a.wrote.Rest(), false)
}

// EndStream shows the last of what she wrote, which is whatever she was in the
// middle of when she finished.
func (a *adapter) EndStream(context.Context) error {
	return a.show(a.wrote.End(), true)
}

// show puts a text in the bubble she is filling, or in one of its own when
// there is none open. done says she has finished it, so what comes next starts
// a bubble of its own. What is shown is trimmed of the space around it.
func (a *adapter) show(text string, done bool) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	var err error
	switch {
	case a.bubble == 0:
		a.bubble = a.f.bubbles.Add(1)
		err = a.event("message", message{ID: a.bubble, Text: text, Hers: true})
	case text != a.text:
		err = a.event("edit", message{ID: a.bubble, Text: text})
	}
	a.text = text
	if done {
		a.bubble, a.text = 0, ""
	}
	return err
}

// bubbled puts a text on the page as one she has finished, which is what a
// reply read back and everything the session says itself are.
func (a *adapter) bubbled(m message) error {
	m.ID = a.f.bubbles.Add(1)
	return a.event("message", m)
}

// Writing says whether a reply is being written, which is what the page shows
// while it waits.
func (a *adapter) Writing(_ context.Context, on bool) error {
	a.says(on)
	return a.event("replying", struct {
		On bool `json:"on"`
	}{on})
}

// says keeps the count of the pages a reply is arriving on, which is what a
// browser opening the stream again is held against. A page whose session ended
// mid-reply says so as it goes, since nothing else will.
func (a *adapter) says(on bool) {
	if a.saying.Swap(on) == on {
		return
	}
	if on {
		a.f.writing.Add(1)
	} else {
		a.f.writing.Add(-1)
	}
}

// ShowCommands hands the page everything that can be typed on it, for it to
// offer however it does.
func (a *adapter) ShowCommands(_ context.Context, cs []api.Command) error {
	out := make([]command, 0, len(cs))
	for _, c := range cs {
		out = append(out, command{Name: c.Name, Args: c.Args, Short: c.Short})
	}
	return a.event("commands", struct {
		Commands []command `json:"commands"`
	}{out})
}

// command is one thing that can be typed, as the page reads it.
type command struct {
	Name  string `json:"name"`
	Args  string `json:"args,omitempty"`
	Short string `json:"short"`
}

// ShowUserMessage shows what was said somewhere else: another page, or another
// frontend altogether.
func (a *adapter) ShowUserMessage(_ context.Context, m *store.Message) error {
	return a.event("said", saying(m))
}

// History is how much of the conversation a page opens on, and how much more
// it is given every time it is scrolled back.
func (a *adapter) History() int { return shown }

// ShowHistory opens the page on what was said before it. It carries the number
// the page was given, which is what says where a message typed on it goes.
//
// A browser that opened the stream again holding everything that had happened
// is given the number alone: the conversation on the screen is the
// conversation, and showing it again would scroll away from what is being read.
func (a *adapter) ShowHistory(_ context.Context, ms []store.Message) error {
	out := struct {
		Page string `json:"page"`
		// Character is who the page is a conversation with, which is what it
		// calls itself.
		Character string `json:"character,omitempty"`
		Messages  []said `json:"messages,omitempty"`
		Caught    bool   `json:"caught,omitempty"`
	}{Page: a.id, Character: a.f.names.Character, Caught: a.caught}
	if !a.caught {
		out.Messages = sayings(ms)
	}
	return a.event("sync", out)
}

// answer is what came back of one ask, with the ask it answers.
type answer struct {
	before   store.MessageID
	messages []said
}

// ShowOlder hands what was asked for to the request that asked for it, rather
// than down the stream: it is the answer to one thing the page did, not
// something that happened in the conversation. One nobody is waiting for is
// one the page gave up on, and it is dropped rather than kept for whoever asks
// next.
func (a *adapter) ShowOlder(_ context.Context, before store.MessageID, ms []store.Message) error {
	select {
	case <-a.older:
	default:
	}
	select {
	case a.older <- answer{before, sayings(ms)}:
	default:
	}
	return nil
}

// asks the session for what came before a message, and waits for the answer to
// that ask. A request that was given up on is answered all the same, so what
// comes back says which ask it belongs to rather than being whatever arrives
// first.
func (a *adapter) asks(ctx context.Context, before store.MessageID) ([]said, error) {
	a.asking.Lock()
	defer a.asking.Unlock()
	if err := a.typed(ctx, api.Input{Older: before}); err != nil {
		return nil, err
	}
	for {
		select {
		case got := <-a.older:
			if got.before == before {
				return got.messages, nil
			}
		case <-a.done:
			return nil, api.ErrGone
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// message is one bubble of the page: what it says, who said it, and what can
// be picked from it. An edit carries the number of the bubble it is about.
type message struct {
	ID      int64    `json:"id"`
	Text    string   `json:"text"`
	Hers    bool     `json:"hers,omitempty"`
	Choices []choice `json:"choices,omitempty"`
}

// choice is one thing that can be picked. What comes back when it is picked is
// the session's own word for it, which the page hands back as it was given.
type choice struct {
	Label   string `json:"label"`
	Picked  string `json:"picked"`
	Current bool   `json:"current,omitempty"`
}

func choices(cs []api.Choice) []choice {
	out := make([]choice, 0, len(cs))
	for _, c := range cs {
		out = append(out, choice{Label: c.Label, Picked: c.Picked, Current: c.Current})
	}
	return out
}

// typed hands one thing to the session of this page. A session that has ended
// takes nothing more, and a request that waited for it to would wait as long
// as the browser held the connection open.
func (a *adapter) typed(ctx context.Context, in api.Input) error {
	select {
	case a.inputs <- in:
		return nil
	case <-a.done:
		return api.ErrGone
	case <-ctx.Done():
		return ctx.Err()
	}
}

// event writes one thing that happened. Every event of the run is numbered,
// whichever page it went to: a browser that opens the stream again says the
// number it had, and that is what says whether it missed anything.
func (a *adapter) event(name string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	at := a.f.told.Add(1)
	a.mu.Lock()
	defer a.mu.Unlock()
	// JSON holds no line of its own, so what it says is one line of the event.
	return a.writing(fmt.Sprintf("id: %d\nevent: %s\ndata: %s\n\n", at, name, b))
}

// beating keeps the connection in use. A proxy closes one that has said
// nothing for long enough, and a conversation is quiet most of the time.
func (a *adapter) beating(ctx context.Context) {
	t := time.NewTicker(beat)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.mu.Lock()
			err := a.writing(": beat\n\n")
			a.mu.Unlock()
			if err != nil {
				return
			}
		}
	}
}

// write is writing with the lock taken for it.
func (a *adapter) write(s string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.writing(s)
}

// left says the request the stream rode on is over, so nothing more is written
// to it: the response is the server's again as soon as the handler returns. A
// beat in the middle of writing one finishes first, and whatever was waiting
// to be handed to the session is told there is nobody to hand it to.
func (a *adapter) left() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.gone {
		return
	}
	a.gone = true
	close(a.done)
}

// writing puts bytes on the stream, with the lock already held. A browser that
// closed the page is gone for good: nothing more is written to it, and the
// session on it ends.
func (a *adapter) writing(s string) error {
	if a.gone {
		return api.ErrGone
	}
	if _, err := io.WriteString(a.w, s); err != nil {
		a.gone = true
		return api.ErrGone
	}
	if err := a.flush.Flush(); err != nil {
		a.gone = true
		return api.ErrGone
	}
	return nil
}
