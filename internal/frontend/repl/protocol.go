package repl

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"sync"

	"nerdola.dev/x/paula/internal/frontend/api"
)

// version is the protocol this server and its client speak.
const version = 1

// lineLimit is the longest line a terminal may type.
const lineLimit = 1 << 20

// frameLimit is the longest frame read, which is a line typed or an image sent
// with one. An image goes as JSON, so as base64 and four bytes for every three:
// it holds maxImage of picture with room to spare for the line beside it.
const frameLimit = 32 << 20

// clientHello is what a terminal says of itself when it dials: the protocol it
// speaks, and how much of the conversation it wants when it opens. How the
// terminal is driven is the terminal's own business.
type clientHello struct {
	Version int `json:"version"`
	History int `json:"history"`
}

// serverHello answers it.
type serverHello struct {
	Version int `json:"version"`
}

// fromClient is one frame the client sends: the first is a hello, then a line
// typed, an image read where the terminal is, an interrupt, or the end of
// input.
type fromClient struct {
	Hello     *clientHello `json:"hello,omitempty"`
	Line      *string      `json:"line,omitempty"`
	Image     *imageFrame  `json:"image,omitempty"`
	Interrupt bool         `json:"interrupt,omitempty"`
	End       bool         `json:"end,omitempty"`
}

// imageFrame is a file the terminal read, since the file is where the terminal
// is rather than where Paula runs.
type imageFrame struct {
	Text string `json:"text,omitempty"`
	Data []byte `json:"data"`
}

// toClient is one frame the server sends: the hello answering the client's,
// text to print as it is, the ask for the next line, and the exit code.
type toClient struct {
	Hello *serverHello `json:"hello,omitempty"`
	Write *string      `json:"write,omitempty"`
	Ready bool         `json:"ready,omitempty"`
	Exit  *int         `json:"exit,omitempty"`
}

// frames reads one frame per line.
func frames[T any](r io.Reader) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 4<<10), frameLimit)
		for sc.Scan() {
			if len(bytes.TrimSpace(sc.Bytes())) == 0 {
				continue
			}
			var f T
			if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
				var zero T
				yield(zero, err)
				return
			}
			if !yield(f, nil) {
				return
			}
		}
		if err := sc.Err(); err != nil {
			var zero T
			yield(zero, err)
		}
	}
}

// writer sends frames one at a time, since a session writes from the goroutine
// that shows what happened and from the one that reads what was typed. The
// line being written on is held here too: what goes on it and what closes it
// are one step, so the two goroutines never land in the middle of each other.
type writer struct {
	mu sync.Mutex
	// open says a reply is being written on the line, so it is signed once and
	// closed before anything else is written.
	open bool
	enc  *json.Encoder
}

func newWriter(w io.Writer) *writer { return &writer{enc: json.NewEncoder(w)} }

func (w *writer) send(f any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.encode(f)
}

// encode sends one frame. The lock is held.
func (w *writer) encode(f any) error {
	err := w.enc.Encode(f)
	if err != nil && gone(err) {
		return fmt.Errorf("%w: %w", api.ErrGone, err)
	}
	return err
}

// put writes text as it is. The lock is held.
func (w *writer) put(text string) error { return w.encode(toClient{Write: &text}) }

func (w *writer) write(text string) error { return w.send(toClient{Write: &text}) }

// stream writes a piece of a reply on the line it is being written on, signed
// the first time anything goes on that line.
func (w *writer) stream(sign, text string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.open {
		w.open, text = true, sign+text
	}
	return w.put(text)
}

// endLine closes the line a reply was being written on, and does nothing when
// there is none.
func (w *writer) endLine() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.closeLine()
}

// closeLine is endLine with the lock held.
func (w *writer) closeLine() error {
	if !w.open {
		return nil
	}
	w.open = false
	return w.put("\n")
}

// line writes one whole line, which starts on a line of its own however far a
// reply being written has got.
func (w *writer) line(text string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.closeLine(); err != nil {
		return err
	}
	return w.put(text + "\n")
}

func (w *writer) ready() error { return w.send(toClient{Ready: true}) }

func (w *writer) exit(code int) error { return w.send(toClient{Exit: &code}) }
