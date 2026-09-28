// Package openai speaks the OpenAI-compatible chat API the hosted runners
// serve: the body of a request, and the stream it is answered in.
package openai

import (
	"context"
	"io"
	"net/http"

	"nerdola.dev/x/paula/internal/runners/api"
	"nerdola.dev/x/paula/internal/runners/transport"
)

// Hooks are the parts of a chat request and its stream only one API
// documents. Every runner that serves models answers all four, so a chat never
// asks whether it has them.
type Hooks interface {
	// Body adds the runner's own fields to a chat request body.
	Body(body map[string]any, req api.ChatRequest) error
	// Message adds the runner's own fields to one message of a chat request.
	// It is where an assistant message hands back the reasoning it came with,
	// in the field each API documents for it.
	Message(out map[string]any, m api.Message)
	// Chunk reads the runner's own fields of a stream chunk. The reasoning
	// text it returns is passed on, and it fills in what it knows of the
	// result.
	Chunk(raw []byte, res *api.Result) (string, error)
	// End finishes what Chunk made of the result, once the stream is done.
	End(res *api.Result)
}

// Chat sends a chat request through the client and passes every chunk of the
// stream to fn. The hooks are what the runner adds to a chat; how its API
// answers is the client's.
func Chat(ctx context.Context, c *transport.Client, hooks Hooks, req api.ChatRequest, fn func(api.Chunk) error) (*api.Result, error) {
	body, pictures, err := chatBody(req, hooks)
	if err != nil {
		return nil, err
	}
	b, err := encode(body)
	if err != nil {
		return nil, err
	}
	kept, err := recorded(body, pictures)
	if err != nil {
		return nil, err
	}
	res := new(api.Result)
	err = c.Send(ctx, transport.Ask{
		Method:   http.MethodPost,
		Path:     "/chat/completions",
		Model:    req.Model,
		Body:     b,
		Recorded: kept,
		Recorder: req.Recorder,
		Read: func(r io.Reader, rec *api.Record) error {
			err := stream(r, res, fn, hooks, c.Answers)
			record(rec, res.Provider, res.FinishReason, res.Usage)
			return err
		},
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// record keeps what the answer said about itself: the host that served it, how
// it finished, and what it cost. paula turns reads them back.
func record(rec *api.Record, provider, finish string, usage api.Usage) {
	if rec == nil {
		return
	}
	rec.Provider = provider
	rec.FinishReason = finish
	// An answer that said nothing about what it cost is not one that cost
	// nothing, and paula turns adds these up.
	if usage != (api.Usage{}) {
		rec.Usage = &usage
	}
}
