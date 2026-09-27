// Package runners talks to the hosted APIs Paula uses: the ones that serve the
// models she answers with, and the ones she searches the web through. It
// declares what a runner can do and which kinds of runner there are. The
// vocabulary they speak is internal/runners/api, which a caller imports beside
// this package.
package runners

import (
	"context"

	"nerdola.dev/x/paula/internal/runners/api"
)

// Runner is a hosted API Paula talks to: what it is, and whether the key may
// use it.
type Runner interface {
	Name() string
	Kind() string
	URL() string

	// Health says whether the key is allowed to make requests.
	Health(ctx context.Context) error
}

// Server is a runner that serves models: what it serves, and how they answer.
type Server interface {
	Runner
	// Settings are the runner's own, which a model's settings are laid over.
	Settings() api.Settings

	// Models is everything the runner serves, and Model is one of them.
	Models(ctx context.Context) ([]api.Model, error)
	Model(ctx context.Context, id string) (*api.Model, error)
	// Check reports every way a model and its settings do not fit what the
	// catalogue says. Whoever asks heads the list with the model's key path.
	Check(ctx context.Context, m api.Checked) []error

	// Chat sends a request and passes every chunk of the answer to fn.
	Chat(ctx context.Context, req api.ChatRequest, fn func(api.Chunk) error) (*api.Result, error)
}

// Host is what the program around a runner gives it.
type Host = api.Host
