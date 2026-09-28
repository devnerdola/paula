// Package api is the vocabulary a tool and the conversation running it share:
// what a tool is, and what it reaches of the conversation while it runs. The
// tools under internal/tools import it, so none of them has to import
// internal/tools itself. A tool can make a runner's request, so this package
// takes internal/runners/api as well.
package api

import (
	"context"
	"encoding/json"
	"time"

	runnersapi "nerdola.dev/x/paula/internal/runners/api"
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

// Instructor is a tool that has something to say in her system prompt beyond
// what the tool says of itself: when she is to use it, as a rule of hers
// rather than the card's.
type Instructor interface {
	Instructions() string
}

// Lookup is a tool whose calls only look something up and change nothing, or
// whose answer is of use only to a round that can act on it, so a call of one
// is worth running only when what it answers reaches a model.
type Lookup interface {
	LooksUp()
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
	// answers reads like the prompt it goes back into, and Time a moment of one.
	Date(t time.Time) string
	Time(t time.Time) string
	// Memories are the memories that hold the words of a query.
	Memories(ctx context.Context, query string, limit int) ([]store.Memory, error)
	// LatestMemories are the memories that stand, newest first, from the one
	// at from on and at most limit of them.
	LatestMemories(ctx context.Context, from, limit int) ([]store.Memory, error)
	// Remember keeps a memory, said in the newest message the reply answers,
	// in place of the memories it replaces.
	Remember(ctx context.Context, content string, replaces []store.MemoryID) (*store.Memory, error)
	// Forget takes a memory away, and the ones it replaced with it.
	Forget(ctx context.Context, id store.MemoryID) ([]store.Memory, error)
	// Images are the pictures of the conversation, newest first, from the one
	// at from on and at most limit of them, and Image the one of a number.
	Images(ctx context.Context, from, limit int) ([]store.Image, error)
	Image(ctx context.Context, id int64) (*store.Image, error)
	// Show has a picture sent to the model in the call's answer, and reports
	// whether it will be: a model shown a picture there is sent it, and any
	// other is not, since it can read what the picture showed.
	Show(img store.Image) bool
	// Photo is a picture she took: the image model's picture of her, made
	// from the reference as the prompt says, in a shape of runnersapi.Shapes
	// or the one the model chooses. SendPhoto has one she took go with the
	// reply.
	Photo(ctx context.Context, prompt, shape string, reference []byte) (*store.Image, error)
	SendPhoto(ctx context.Context, id int64) error
	// Now is what time it is, by the conversation's clock.
	Now() time.Time
	// Callbacks are the call backs she scheduled that have not fired, soonest
	// first. Schedule keeps one, under the reply that asked for it; Move gives
	// one another time; Cancel takes one away.
	Callbacks(ctx context.Context) ([]store.Callback, error)
	Schedule(ctx context.Context, at time.Time, reason string) (*store.Callback, error)
	Move(ctx context.Context, id store.CallbackID, at time.Time) error
	Cancel(ctx context.Context, id store.CallbackID) error
	// Recorder keeps a request the call makes of a runner under the reply that
	// asked for the call.
	Recorder() runnersapi.Recorder
}

// Host is what the program around a tool gives it.
type Host struct {
	// Names are who a tool's description speaks of, and Language what a tool
	// asks to be written in, as the character card has them.
	Names    Names
	Language string
	// Searchers are the runners that search the web, by name.
	Searchers map[string]runnersapi.Searcher
	// Avatar is the file of her picture, and empty when there is none. Image
	// says a model is set for the image role, which makes a picture from it.
	Avatar string
	Image  bool
}

// Names are the two in the conversation, as the character card names them.
type Names struct {
	Character string
	User      string
}
