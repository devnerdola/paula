// Package api is the vocabulary the runners and their callers share: what is
// asked of a model, what comes back, and how a request is recorded. The
// runners under internal/runners import it, so none of them has to import
// internal/runners itself.
package api

import "fmt"

// Model is what a catalogue says about a model.
type Model struct {
	ID      string
	Context int
	Chat    bool
	Vision  bool
	Tools   bool
	// Embeddings says the model turns text into a vector rather than into more
	// text, which is all it does: a model that embeds writes nothing.
	Embeddings        bool
	Reasoning         bool
	Mandatory         bool
	Efforts           []string
	StructuredOutputs bool
	Parameters        []string // nil when the catalogue does not list them
}

// Needs is what a model has to be able to do.
type Needs struct {
	Chat       bool
	Vision     bool
	Tools      bool
	Embeddings bool
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
		Chat:       n.Chat || o.Chat,
		Vision:     n.Vision || o.Vision,
		Tools:      n.Tools || o.Tools,
		Embeddings: n.Embeddings || o.Embeddings,
		Context:    max(n.Context, o.Context),
	}
}

// Roles a message can have.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
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
}

// Text builds a text message.
func Text(role, text string) Message {
	return Message{Role: role, Parts: []Part{{Type: PartText, Text: text}}}
}

type ChatRequest struct {
	Model    string
	Messages []Message
	Settings Settings
	// CacheKey groups the requests that share a prompt prefix.
	CacheKey string
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
	// Reasoning is what the model thought on its way to the reply.
	Reasoning string
}

// EmbedRequest asks a model to turn text into vectors, one for each string it
// is given. Several go in one request, since an embedding is short and the
// wait is the same whether one or a hundred are asked for.
type EmbedRequest struct {
	Model string
	Input []string
	// Settings are the model's, as a chat request carries them. What they say
	// about where a request may be routed governs the memories too: they are
	// the conversation, in the words a fold left it in.
	Settings Settings
	// Recorder keeps what was sent and what came back, as a chat request does.
	Recorder Recorder
}

// EmbedResult is a vector for each string that was sent, in the order they
// were sent in.
type EmbedResult struct {
	Vectors [][]float32
	Usage   Usage
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
	if n.Embeddings {
		want(m.Embeddings, "embeddings")
	}
	return errs
}
