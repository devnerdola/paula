// Package api is the vocabulary a frontend and the session showing things on
// it share: what it can do, what it shows, and what arrives from it. The
// frontends under internal/frontend import it, so none of them has to import
// internal/frontend itself.
package api

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"nerdola.dev/x/paula/internal/logs"
	"nerdola.dev/x/paula/internal/store"
)

// ErrGone says the frontend a session was showing things on is no longer
// there, so the session ends rather than writing to nothing.
var ErrGone = errors.New("the frontend is gone")

// Outgoing is one thing to show.
type Outgoing struct {
	Text string
	// Hers says she wrote it, rather than the frontend talking.
	Hers bool
	// Choices are what can be picked from what this says. A frontend that can
	// offer a choice offers them however it does that; one that cannot shows
	// the text, which says the same thing in words.
	Choices []Choice
}

// Choice is one thing that can be picked.
type Choice struct {
	Label string
	// Picked is what comes back when it is picked. It is the session's own
	// word for the choice and means nothing to a frontend, which hands it back
	// as it was given.
	Picked string
	// Current says this is what stands now.
	Current bool
}

// Input is one thing that arrived from a frontend.
type Input struct {
	Text string
	// Images are the bytes as they arrived.
	Images [][]byte
	// Stop says the reply was asked to stop by a key of its own, rather than by
	// typing the command. Nothing else of the input is read then.
	Stop bool
	// Picked is the Picked of a choice that was picked. Nothing else of the
	// input is read then.
	Picked string
	// Older asks for what was said before a message already on the screen,
	// which is answered by showing it. Nothing else of the input is read then.
	Older store.MessageID
}

// Command is one thing that can be typed at a frontend.
type Command struct {
	Name  string
	Args  string
	Short string
}

// Features is what an adapter asks for. What it can show it says by
// implementing the interfaces below.
type Features struct {
	// Channel names this frontend in the conversation.
	Channel string
	// Commands are the ones this adapter answers itself.
	Commands []Command
	// Sequential says one thing is handled at a time, and the next line is
	// asked for only once the reply is done.
	Sequential bool
}

// Bubbles is what a text reads as when it is shown a message at a time: the
// paragraphs she wrote, since a blank line is where one text ends and the next
// begins, each cut to what one message holds.
//
// fits answers how much of a text goes in one message, as a number of bytes,
// counting in whatever the frontend counts in and cutting where it would
// rather cut. A frontend that holds messages to no length passes nil. Whether
// to show a reply this way at all, and how to pace it, is the frontend's own.
func Bubbles(text string, fits func(string) int) []string {
	var out []string
	for para := range strings.SplitSeq(text, "\n\n") {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}
		if fits == nil {
			out = append(out, para)
			continue
		}
		for {
			at := fits(para)
			if at <= 0 || at >= len(para) {
				out = append(out, para)
				break
			}
			if part := strings.TrimRight(para[:at], " \n"); part != "" {
				out = append(out, part)
			}
			para = strings.TrimLeft(para[at:], " \n")
			if para == "" {
				break
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Written is a reply as it arrives, kept until each text of it is whole. A
// blank line is where one ends and the next begins, which is the same rule
// Bubbles reads a finished text by: a frontend that shows a text at a time
// adds what arrives and sends what comes back.
type Written struct {
	// wrote is what has arrived since the last text she finished, as she wrote
	// it: what is shown is trimmed, and the rest joins onto what is kept.
	wrote string
}

// Add takes what has arrived and answers with the texts she has finished,
// trimmed of the space around them. What she is in the middle of stays.
func (w *Written) Add(text string) []string {
	w.wrote += text
	var done []string
	for {
		at := strings.Index(w.wrote, "\n\n")
		if at < 0 {
			return done
		}
		if text := strings.TrimSpace(w.wrote[:at]); text != "" {
			done = append(done, text)
		}
		w.wrote = w.wrote[at+2:]
	}
}

// Rest is the text she is in the middle of, as she wrote it: what is shown of
// it is trimmed by whoever shows it, and what is kept is not, since what she
// writes next joins onto it.
func (w *Written) Rest() string { return w.wrote }

// Keep is what is left of the text she is in the middle of, for a frontend
// that has put part of it in a message it has closed.
func (w *Written) Keep(text string) { w.wrote = text }

// End is the last of what she wrote, which is whatever she was in the middle
// of when she finished, and leaves nothing behind.
func (w *Written) End() string {
	text := w.wrote
	w.wrote = ""
	return text
}

// Adapter is one way of reaching Paula.
type Adapter interface {
	Features() Features
	Start(ctx context.Context) (<-chan Input, error)
	Send(ctx context.Context, m Outgoing) error
}

// An adapter may also do any of these.
type (
	// Streamer shows a reply as it is written, without sending a message for
	// every fragment.
	Streamer interface {
		Stream(ctx context.Context, text string) error
		EndStream(ctx context.Context) error
	}
	// Prompter asks for the next line.
	Prompter interface {
		Prompt(ctx context.Context) error
	}
	// Writer is told when she starts writing and when she has stopped. What
	// that looks like is the frontend's: a status that has to be said again
	// every few seconds, a line on a page, or nothing at all.
	Writer interface {
		Writing(ctx context.Context, on bool) error
	}
	// CommandShower is told everything that can be typed here, its own
	// commands among them, as a session opens. A frontend that has somewhere
	// to list them lists them there.
	CommandShower interface {
		ShowCommands(ctx context.Context, cs []Command) error
	}
	// OtherChannels shows what was said on another frontend.
	OtherChannels interface {
		ShowUserMessage(ctx context.Context, m *store.Message) error
	}
	// HistoryShower shows what was said before this frontend opened, and says
	// how much of it it wants. An adapter that shows none does not implement
	// this, so there is one answer to whether history is shown.
	HistoryShower interface {
		History() int
		ShowHistory(ctx context.Context, ms []store.Message) error
	}
	// Backlog shows what was said before what is on the screen already, which
	// a frontend asks for with Input.Older as it is scrolled back. It is given
	// as many messages as it asks for with History, since a screenful is a
	// screenful whether it is the first or the tenth.
	//
	// before is the ask it answers. A frontend that gave up on one and asked
	// again reads that to tell the answers apart, since the one it gave up on
	// is still coming.
	Backlog interface {
		HistoryShower
		ShowOlder(ctx context.Context, before store.MessageID, ms []store.Message) error
	}
)

// Host is what the program around a frontend gives it.
type Host struct {
	// DataDir is where Paula keeps the conversation, and where a frontend puts
	// anything of its own.
	DataDir string
	Log     *slog.Logger
	// Names is who a frontend says a message is from.
	Names Names
	// Secrets is where a frontend registers the token it was given, so nothing
	// written by the run it belongs to carries it. A token that rides in the
	// URL of every request is in the error of every request that fails.
	Secrets *logs.Secrets
}

// Names are the two in the conversation, as the character card names them.
type Names struct {
	Character string
	User      string
}

// Frontend is a way of reaching Paula, named in the configuration file by its
// kind. Run calls session for each one it starts — one for a bot or a page,
// one per connection for a socket — and returns when the context ends.
type Frontend interface {
	Kind() string
	Run(ctx context.Context, session func(context.Context, Adapter) error) error
}
