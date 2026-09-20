// Package api is the vocabulary a frontend and the session showing things on
// it share: what it can do, what it shows, and what arrives from it. The
// frontends under internal/frontend import it, so none of them has to import
// internal/frontend itself.
package api

import (
	"context"
	"errors"
	"log/slog"

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
}

// Input is one thing that arrived from a frontend.
type Input struct {
	Text string
	// Images are the bytes as they arrived.
	Images [][]byte
	// Stop says the reply was asked to stop by a key of its own, rather than by
	// typing the command. Nothing else of the input is read then.
	Stop bool
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
)

// Host is what the program around a frontend gives it.
type Host struct {
	// DataDir is where Paula keeps the conversation, and where a frontend puts
	// anything of its own.
	DataDir string
	Log     *slog.Logger
	// Names is who a frontend says a message is from.
	Names Names
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
