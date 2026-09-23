// Package api is the vocabulary a tool and the conversation running it share:
// what a tool is, and what it reaches of the conversation while it runs. The
// tools under internal/tools import it, so none of them has to import
// internal/tools itself.
package api

import (
	"context"
	"encoding/json"
	"time"

	"nerdola.dev/x/paula/internal/store"
)

// Tool is something she can do in the middle of a reply: what a model is told
// it is, what she says while it runs, and the running of it.
type Tool interface {
	Definition() Definition
	// Note is what is shown while the call runs, from the arguments it was
	// asked with.
	Note(args json.RawMessage) string
	// Call runs it. What it answers goes back to the model, and so does what
	// went wrong, as the result of the call.
	Call(ctx context.Context, env Env, args json.RawMessage) (string, error)
}

// Definition is what a model is told of a tool: what it is called, what it
// does, and the JSON schema of what it takes.
type Definition struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// Env is what a call reaches of the conversation. It is given with each call
// rather than when the tool is opened, since a call belongs to the reply that
// asked for it.
type Env interface {
	// Date is a day as the conversation writes one to a model, so what a tool
	// answers reads like the prompt it goes back into.
	Date(t time.Time) string
	// Memories are the memories closest in meaning to a query.
	Memories(ctx context.Context, query string, limit int) ([]store.Memory, error)
	// Remember keeps a memory, said in the newest message the reply answers,
	// in place of the memories it replaces.
	Remember(ctx context.Context, content string, replaces []store.MemoryID) (*store.Memory, error)
	// Forget takes a memory away, and the ones it replaced with it.
	Forget(ctx context.Context, id store.MemoryID) ([]store.Memory, error)
}

// Host is what the program around a tool gives it.
type Host struct {
	// Names are who a tool's description speaks of, and Language what a tool
	// asks to be written in, as the character card has them.
	Names    Names
	Language string
}

// Names are the two in the conversation, as the character card names them.
type Names struct {
	Character string
	User      string
}
