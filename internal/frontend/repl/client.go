package repl

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"

	"nerdola.dev/x/paula/internal/media"
)

// prompt is what a terminal being typed at shows when a line is asked for.
const prompt = "> "

// Client is the terminal side: it dials the socket, relays what is typed, and
// prints what comes back.
type Client struct {
	Socket string
	In     io.Reader
	Out    io.Writer
	Err    io.Writer
	// Interactive says the terminal is being typed at, rather than fed a file.
	Interactive bool
	// Interrupts is Ctrl-C.
	Interrupts <-chan struct{}
}

// Run talks to the server until it says to exit, and answers with the code it
// gave. A connection that drops ends with 1.
func (c Client) Run(ctx context.Context) int {
	conn, err := net.Dial("unix", c.Socket)
	if err != nil {
		fmt.Fprintf(c.Err, "nothing is listening on %s: start paula serve, and list repl under frontends\n", c.Socket)
		return 1
	}
	defer conn.Close()

	// Reading what is typed and reading what comes back both end with this
	// call, however it ends.
	ctx, stop := context.WithCancel(ctx)
	defer stop()

	out := json.NewEncoder(conn)
	if err := out.Encode(c.hello()); err != nil {
		return c.fail(err)
	}

	answers := make(chan toClient)
	failed := make(chan error, 1)
	go func() {
		defer close(answers)
		for f, err := range frames[toClient](conn) {
			if err != nil {
				failed <- err
				return
			}
			select {
			case answers <- f:
			case <-ctx.Done():
				return
			}
		}
	}()

	// asked says a Ctrl-C was sent with nothing typed since, so the next one
	// closes the terminal.
	var asked bool

	reading := make(chan error, 1)
	typed := c.lines(ctx, reading)
	// A terminal being fed a file sends a line only when one is asked for, so
	// what is typed is read again only once the server is ready.
	waiting := typed
	if !c.Interactive {
		waiting = nil
	}

	for {
		select {
		case <-ctx.Done():
			return 1

		case err := <-failed:
			return c.fail(err)

		case f, ok := <-answers:
			if !ok {
				// Whatever ended the reading says why; a connection that simply
				// went says nothing, so the terminal says it.
				select {
				case err := <-failed:
					return c.fail(err)
				default:
					fmt.Fprintln(c.Err, "paula repl: the connection ended")
					return 1
				}
			}
			switch {
			case f.Write != nil:
				fmt.Fprint(c.Out, *f.Write)
			case f.Ready:
				// A terminal being typed at shows a prompt for the line it is
				// asked for; one being fed a file simply reads the next.
				if c.Interactive {
					fmt.Fprint(c.Out, prompt)
				}
				waiting = typed
			case f.Exit != nil:
				return *f.Exit
			}

		case err := <-reading:
			// Failing to read what is typed is the terminal gone, not ended.
			return c.fail(err)

		case <-c.Interrupts:
			// A terminal being fed a file has nothing to go back to, so it
			// leaves. One being typed at stops the reply being written, and a
			// second Ctrl-C with nothing typed in between leaves, whether the
			// first one found a reply to stop or the server said nothing.
			if !c.Interactive || asked {
				fmt.Fprintln(c.Err, "paula repl: leaving")
				return 130
			}
			asked = true
			if err := out.Encode(fromClient{Interrupt: true}); err != nil {
				return 1
			}

		case line, ok := <-waiting:
			if !ok {
				// Nothing more will be typed, so the server is told and only
				// what it still has to say is waited for.
				waiting, typed = nil, nil
				if err := out.Encode(fromClient{End: true}); err != nil {
					return c.fail(err)
				}
				continue
			}
			// Something was typed, so Ctrl-C goes back to stopping the reply.
			asked = false
			if !c.Interactive {
				// A terminal being fed a file shows the line it read, since
				// nobody typed it where it could be seen.
				fmt.Fprint(c.Out, prompt+line+"\n")
			}
			f, ok := c.frame(line)
			if !ok {
				// The line was the terminal's to answer and it could not: one
				// being fed a file reads the next, and one being typed at asks
				// for a prompt.
				if !c.Interactive {
					waiting = typed
					continue
				}
				f = fromClient{Line: new(string)}
			}
			if f.End {
				// Nothing more will be typed, and only what the server still
				// has to say is waited for.
				typed = nil
			}
			if !c.Interactive || f.End {
				waiting = nil
			}
			if err := out.Encode(f); err != nil {
				return c.fail(err)
			}
		}
	}
}

// fail says what went wrong and is the exit code of a terminal that cannot go
// on.
func (c Client) fail(err error) int {
	fmt.Fprintf(c.Err, "paula repl: %v\n", err)
	return 1
}

func (c Client) hello() fromClient {
	// A terminal being fed a file asks for no history: what is piped in is
	// answered, not read back.
	want := 0
	if c.Interactive {
		want = history
	}
	return fromClient{Hello: &clientHello{Version: version, History: want}}
}

// maxImage is the largest file a terminal sends: above any photo, and small
// enough that the base64 of it still fits a frame (see frameLimit).
const maxImage = 16 << 20

// frame turns a typed line into what is sent: the end of the terminal, an image
// the terminal read itself, or the line as it was typed. It reports false for a
// line the terminal could not answer.
func (c Client) frame(line string) (fromClient, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "/") {
		return fromClient{Line: &line}, true
	}
	name, args, _ := strings.Cut(strings.TrimPrefix(trimmed, "/"), " ")
	switch name {
	case "quit":
		// The commands of the terminal itself are the terminal's to answer.
		return fromClient{End: true}, true
	case "image":
	default:
		return fromClient{Line: &line}, true
	}
	path, text := splitPath(strings.TrimSpace(args))
	if path == "" {
		fmt.Fprintln(c.Err, "/image takes a path, such as /image photo.jpg look at this")
		return fromClient{}, false
	}
	data, err := c.read(path)
	if err != nil {
		fmt.Fprintf(c.Err, "%v\n", err)
		return fromClient{}, false
	}
	if media.Detect(data) == "" {
		fmt.Fprintf(c.Err, "%s is not one of %s\n", path, strings.Join(media.Types(), ", "))
		return fromClient{}, false
	}
	return fromClient{Image: &imageFrame{Text: text, Data: data}}, true
}

// read reads a file the terminal named, from where the terminal is.
func (c Client) read(path string) ([]byte, error) {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		path = filepath.Join(home, path[2:])
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a file", path)
	}
	if fi.Size() > maxImage {
		return nil, fmt.Errorf("%s is %d bytes, more than the %d an image may have",
			path, fi.Size(), maxImage)
	}
	return os.ReadFile(path)
}

// splitPath reads the path a line starts with, which may be quoted or have its
// spaces escaped, and the text after it.
func splitPath(args string) (string, string) {
	if len(args) > 0 && (args[0] == '"' || args[0] == '\'') {
		if end := strings.IndexByte(args[1:], args[0]); end >= 0 {
			return args[1 : end+1], strings.TrimSpace(args[end+2:])
		}
	}
	var path strings.Builder
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == '\\' && i+1 < len(args):
			i++
			path.WriteByte(args[i])
		case args[i] == ' ':
			return path.String(), strings.TrimSpace(args[i+1:])
		default:
			path.WriteByte(args[i])
		}
	}
	return path.String(), ""
}

// lines reads what is typed, one line at a time. A read that failed while the
// terminal was still talking is reported on failed; one that failed because it
// ended is not.
func (c Client) lines(ctx context.Context, failed chan<- error) chan string {
	out := make(chan string)
	go func() {
		defer close(out)
		sc := bufio.NewScanner(c.In)
		sc.Buffer(make([]byte, 0, 4<<10), lineLimit)
		for sc.Scan() {
			select {
			case out <- sc.Text():
			case <-ctx.Done():
				return
			}
		}
		if err := sc.Err(); err != nil && ctx.Err() == nil {
			select {
			case failed <- err:
			default:
			}
		}
	}()
	return out
}
