// Package api is the vocabulary the runners and their callers share: what is
// asked of a model, what comes back, and how a request is recorded. The
// runners under internal/runners import it, so none of them has to import
// internal/runners itself.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// A request cut off here says so in its own words: a context that was
// cancelled reads the same however it was cancelled, and what came of a
// request is read by whoever asked for it.
var (
	// ErrIdle says a request was cut off because nothing arrived for the idle
	// timeout. Without it, that reads the same as a stop.
	ErrIdle = errors.New("nothing arrived for the idle timeout")
	// ErrSlow says a request that is not a stream was still arriving when its
	// time ran out. The request timeout it was given is named with it, since a
	// runner sets its own.
	ErrSlow = errors.New("the answer took longer than a request may take")
)

// Gone reports whether an error is a request that never finished: the
// connection went, or it was given up on. What a host said is not one of
// these, and nothing is learned from a request that was cut off.
func Gone(err error) bool {
	return errors.Is(err, ErrIdle) || errors.Is(err, ErrSlow) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// Model is what a catalogue says about a model.
type Model struct {
	ID                string
	Context           int
	Chat              bool
	Vision            bool
	Tools             bool
	Reasoning         bool
	Mandatory         bool
	Efforts           []string
	StructuredOutputs bool
	Parameters        []string // nil when the catalogue does not list them
}

// Needs is what a model has to be able to do.
type Needs struct {
	Chat   bool
	Vision bool
	Tools  bool
	// Context is the prompt the model has to hold. Zero takes whatever it
	// serves.
	Context int
}

// Checked is a model a runner is asked to hold against its catalogue: the id
// the API knows it by, the settings a request to it would carry, and what it
// has to be able to do. What comes back is headed with the model's key path by
// whoever asked, so a runner never writes one.
type Checked struct {
	ID       string
	Settings Settings
	Needs    Needs
}

// With returns what either set of needs asks for.
func (n Needs) With(o Needs) Needs {
	return Needs{
		Chat:    n.Chat || o.Chat,
		Vision:  n.Vision || o.Vision,
		Tools:   n.Tools || o.Tools,
		Context: max(n.Context, o.Context),
	}
}

// Roles a message can have. A tool message is what a tool answered, sent back
// to the model that asked for it.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Part types a message is made of.
const (
	PartText  = "text"
	PartImage = "image"
)

type Part struct {
	Type string
	Text string
	MIME string
	Data []byte
}

type Message struct {
	Role  string
	Parts []Part
	// ToolCalls are what an assistant message asked to be run, and ToolCallID
	// the call a tool message answers.
	ToolCalls  []ToolCall
	ToolCallID string
	// Reasoning is what the model thought on its way to an assistant message.
	// It goes back with it, since a model that reasons reads its own thinking
	// again: across the rounds of a reply, and in the replies before it, which
	// a model offered tools reads as it reads its own.
	Reasoning *Reasoning
}

// Reasoning is a model's thinking as its runner received it: the text, and
// the items the host sent beside it, in order, to be handed back unchanged.
type Reasoning struct {
	Text    string
	Details []json.RawMessage
}

// Text builds a text message.
func Text(role, text string) Message {
	return Message{Role: role, Parts: []Part{{Type: PartText, Text: text}}}
}

// ToolDef is a tool a model is offered: what it is called, what it does, and
// the JSON schema of what it takes.
type ToolDef struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// ToolCall is a tool a model asked to be run, with the arguments exactly as it
// wrote them.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// ToolChoiceNone asks a model for an answer with no tool call in it.
const ToolChoiceNone = "none"

type ChatRequest struct {
	Model    string
	Messages []Message
	Settings Settings
	// Tools are what the model may ask to be run, and ToolChoice is empty or
	// ToolChoiceNone.
	Tools      []ToolDef
	ToolChoice string
	// CacheKey groups the requests that share a prompt prefix, and Standing is
	// how many of the first messages the next request of the group sends again
	// as they are, or zero when that is not known.
	CacheKey string
	Standing int
	// Recorder keeps what was sent and what came back. A request made with none
	// is not recorded, which is every request that belongs to no turn.
	Recorder Recorder
}

type ChunkKind int

const (
	ChunkText ChunkKind = iota
	ChunkReasoning
)

type Chunk struct {
	Kind ChunkKind
	Text string
}

type Usage struct {
	PromptTokens     int
	CachedTokens     int
	CacheWriteTokens int
	CompletionTokens int
	ReasoningTokens  int
	Cost             float64
}

type Result struct {
	Provider     string
	FinishReason string
	Usage        Usage
	// Reasoning is what the model thought on its way to the reply, and
	// ReasoningDetails what the host sent beside it, whole, to be handed back
	// unchanged with the reply.
	Reasoning        string
	ReasoningDetails []json.RawMessage
	// ToolCalls are the tools the model asked to be run, whole, in the order
	// it asked for them.
	ToolCalls []ToolCall
}

// Missing reports what is asked of the model that it cannot do.
func (m *Model) Missing(n Needs) []error {
	var errs []error
	want := func(ok bool, what string) {
		if !ok {
			errs = append(errs, fmt.Errorf("the model does not do %s", what))
		}
	}
	if n.Chat {
		want(m.Chat, "chat")
	}
	if n.Vision {
		want(m.Vision, "vision")
	}
	if n.Tools {
		want(m.Tools, "tools")
	}
	return errs
}
