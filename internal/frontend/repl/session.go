package repl

import (
	"context"
	"fmt"
	"io"
	"iter"
	"strings"

	"nerdola.dev/x/paula/internal/frontend/api"
	"nerdola.dev/x/paula/internal/media"
	"nerdola.dev/x/paula/internal/store"
)

// channel names the repl in the conversation.
const channel = "repl"

// History is how many messages a terminal being typed at opens on when it is
// given no other number.
const History = 10

// serve reads the hello of one connection and keeps a session for it.
func serve(ctx context.Context, conn io.ReadWriter, names api.Names, pictures *media.Files, session func(context.Context, api.Adapter) error) error {
	w := newWriter(conn)
	next, stop := iter.Pull2(frames[fromClient](conn))

	first, err, ok := next()
	switch {
	case !ok || err != nil:
		stop()
		return err
	case first.Hello == nil:
		stop()
		return refuse(w, "the first frame is not a hello")
	case first.Hello.Version != version:
		stop()
		return refuse(w, fmt.Sprintf("this repl speaks version %d, the client speaks %d",
			version, first.Hello.Version))
	}
	if err := w.send(toClient{Hello: &serverHello{Version: version}}); err != nil {
		return err
	}

	// Everything after the hello is read by the one goroutine that started
	// reading, which is the only one that may ask for the next frame or stop
	// asking.
	a := &adapter{
		out:      w,
		names:    names,
		pictures: pictures,
		history:  first.Hello.History,
		next:     next,
		stop:     stop,
	}
	code := 0
	err = session(ctx, a)
	// A session that never started reading leaves the reader parked after the
	// hello, and nothing else would stop it.
	if !a.started {
		a.stop()
	}
	if err != nil {
		code = 1
		if !gone(err) {
			_ = w.write("error: " + err.Error() + "\n")
		}
	}
	if ctx.Err() != nil {
		// The run is ending, not this terminal, so the terminal is told rather
		// than simply finding the socket closed.
		code = 1
		_ = w.write("\nserve is stopping\n")
	}
	return w.exit(code)
}

// refuse says why a terminal is not talked to, and ends it.
func refuse(w *writer, why string) error {
	if err := w.write(why + "\n"); err != nil {
		return err
	}
	return w.exit(1)
}

// adapter is one terminal, which takes one line at a time.
type adapter struct {
	out      *writer
	names    api.Names
	pictures *media.Files
	// history is how much of the conversation this terminal asked for.
	history int
	next    func() (fromClient, error, bool)
	stop    func()
	// started says the goroutine Start began owns next and stop from then on.
	started bool
}

func (a *adapter) Features() api.Features {
	return api.Features{
		Channel:    channel,
		Sequential: true,
		Commands: []api.Command{
			{Name: "image", Args: "PATH [TEXT]", Short: "send an image"},
			{Name: "quit", Short: "leave the repl"},
		},
	}
}

// History is how much of the conversation this terminal opens on, which it said
// when it dialled.
func (a *adapter) History() int { return a.history }

// Start reads what is typed until the connection ends.
func (a *adapter) Start(ctx context.Context) (<-chan api.Input, error) {
	inputs := make(chan api.Input)
	a.started = true
	go func() {
		defer close(inputs)
		defer a.stop()
		for {
			f, err, ok := a.next()
			if err != nil {
				a.write("error: " + err.Error())
				return
			}
			if !ok {
				return
			}
			switch {
			case f.End:
				return
			case f.Interrupt:
				if !send(ctx, inputs, api.Input{Stop: true}) {
					return
				}
			case f.Image != nil:
				if !send(ctx, inputs, api.Input{Text: f.Image.Text, Images: [][]byte{f.Image.Data}}) {
					return
				}
			case f.Line != nil:
				if !send(ctx, inputs, api.Input{Text: *f.Line}) {
					return
				}
			}
		}
	}()
	return inputs, nil
}

func send(ctx context.Context, inputs chan<- api.Input, in api.Input) bool {
	select {
	case inputs <- in:
		return true
	case <-ctx.Done():
		return false
	}
}

// Send shows one thing. What she says is hers, and everything else is the
// terminal talking. A terminal shows no picture, so a photo she sent is a
// line of its own saying where its file is.
func (a *adapter) Send(_ context.Context, m api.Outgoing) error {
	var lines []string
	if m.Text != "" || len(m.Pictures) == 0 {
		lines = append(lines, m.Text)
	}
	for _, sha := range m.Pictures {
		lines = append(lines, a.photo(sha))
	}
	for _, text := range lines {
		if m.Hers {
			text = a.names.Character + ": " + text
		}
		if err := a.out.line(text); err != nil {
			return err
		}
	}
	return nil
}

// photo is a picture as a terminal shows one: where its file is.
func (a *adapter) photo(sha string) string { return "(photo: " + a.pictures.Path(sha) + ")" }

// Stream shows a reply as she writes it: the text arrives on the line it is
// being written on, and nothing is sent again when it ends.
func (a *adapter) Stream(_ context.Context, text string) error {
	return a.out.stream(a.names.Character+": ", text)
}

// EndStream closes the line the reply was written on.
func (a *adapter) EndStream(_ context.Context) error { return a.out.endLine() }

// Prompt asks the terminal for the next line. What it shows for one is the
// terminal's own business.
func (a *adapter) Prompt(_ context.Context) error { return a.out.ready() }

// ShowHistory shows what was said before this terminal opened, after how many
// messages it is of all there are when it is not all of them.
func (a *adapter) ShowHistory(_ context.Context, ms []store.Message, said int) error {
	if len(ms) > 0 && len(ms) < said {
		if err := a.out.line(fmt.Sprintf("the last %d of %d messages", len(ms), said)); err != nil {
			return err
		}
	}
	for _, m := range ms {
		if err := a.out.line(a.messageLine(&m)); err != nil {
			return err
		}
	}
	return nil
}

// ShowUserMessage shows a message that was sent on another frontend.
func (a *adapter) ShowUserMessage(_ context.Context, m *store.Message) error {
	return a.out.line(a.messageLine(m))
}

// messageLine is one stored message as a line: who said it, where, and what,
// with where the file of every picture it carried is.
func (a *adapter) messageLine(m *store.Message) string {
	who := a.names.Character
	if m.Role == store.RoleUser {
		who = a.names.User
		if m.Channel != "" && m.Channel != channel {
			who = "[" + m.Channel + "] " + who
		}
	}
	var said []string
	if text := m.Text(); text != "" {
		said = append(said, text)
	}
	for _, p := range m.Images() {
		said = append(said, a.photo(p.SHA256))
	}
	return who + ": " + strings.Join(said, " ")
}

// write is the terminal talking, rather than anything Paula said. One that
// cannot be written to is about to end, which the next frame read says.
func (a *adapter) write(text string) {
	_ = a.out.line(text)
}
