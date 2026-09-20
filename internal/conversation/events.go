package conversation

import (
	"context"
	"errors"
	"iter"
	"sync"

	"nerdola.dev/x/paula/internal/store"
)

// Seq numbers the events of the conversation.
type Seq int64

// keptEvents is how many events the log holds. It covers any short
// disconnect, and a reader further behind than this reads the history instead.
const keptEvents = 4096

// ErrBehind says a reader asked for events the log no longer holds, and goes
// on from the oldest it has.
var ErrBehind = errors.New("the events asked for are no longer kept")

type Kind int

const (
	MessageStored Kind = iota
	ReplyStarted
	ReplyText
	ReplyDone
	ReplyStopped
	ReplyRestarted
	ReplyFailed
)

func (k Kind) String() string {
	switch k {
	case MessageStored:
		return "message stored"
	case ReplyStarted:
		return "reply started"
	case ReplyText:
		return "reply text"
	case ReplyDone:
		return "reply done"
	case ReplyStopped:
		return "reply stopped"
	case ReplyRestarted:
		return "reply restarted"
	case ReplyFailed:
		return "reply failed"
	}
	return "unknown"
}

type Event struct {
	Seq     Seq
	Kind    Kind
	Entry   store.EntryID
	Channel string
	// From is the frontend a stored message came from.
	From string
	// Text is all of a reply's text so far, or why a reply failed.
	Text string
	// Message is the message that was stored, and is nil when none was.
	Message *store.Message
}

// copy is an event a reader may keep, since the message it carries belongs to
// the log.
func (e Event) copy() Event {
	if e.Message != nil {
		m := *e.Message
		m.Parts = append([]store.Part(nil), m.Parts...)
		e.Message = &m
	}
	return e
}

type events struct {
	mu  sync.Mutex
	seq Seq
	// kept holds the latest events, and trimmed is the newest one dropped to
	// keep that size. A reply's text replaces the text before it, which leaves
	// gaps in the numbers without dropping anything a reader missed.
	kept    []Event
	trimmed Seq
	changed chan struct{}
}

func newEvents() *events {
	return &events{changed: make(chan struct{})}
}

// publish numbers an event and wakes every reader. A reply's text replaces the
// text before it, so a slow reader sees what the reply says now instead of
// every fragment of it.
func (l *events) publish(e Event) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if n := len(l.kept); n > 0 && e.Kind == ReplyText {
		if last := l.kept[n-1]; last.Kind == ReplyText && last.Entry == e.Entry {
			l.kept = l.kept[:n-1]
		}
	}
	l.seq++
	e.Seq = l.seq
	l.kept = append(l.kept, e)
	if n := len(l.kept); n > keptEvents {
		l.trimmed = l.kept[n-keptEvents-1].Seq
		l.kept = append([]Event(nil), l.kept[n-keptEvents:]...)
	}

	close(l.changed)
	l.changed = make(chan struct{})
}

// Seq is the number of the latest event.
func (l *events) Seq() Seq {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seq
}

// Events yields the events after the given number, then waits for more until
// the context ends.
func (l *events) Events(ctx context.Context, after Seq) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		for {
			l.mu.Lock()
			behind := after < l.trimmed
			if behind {
				after = l.trimmed
			}
			var batch []Event
			for _, e := range l.kept {
				if e.Seq > after {
					batch = append(batch, e.copy())
				}
			}
			changed := l.changed
			l.mu.Unlock()

			if behind && !yield(Event{}, ErrBehind) {
				return
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
