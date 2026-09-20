// Package runners talks to the hosted APIs that answer as Paula. It declares
// what a runner can do and which kinds of runner there are. The vocabulary they
// speak is internal/runners/api, which a caller imports beside this package.
package runners

import (
	"context"

	"nerdola.dev/x/paula/internal/runners/api"
)

// Runner is a hosted API Paula talks to: what it is, what it serves, and how
// it answers.
type Runner interface {
	Name() string
	Kind() string
	URL() string
	// Settings are the runner's own, which a model's settings are laid over.
	Settings() api.Settings

	// Health says whether the key is allowed to make requests.
	Health(ctx context.Context) error

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
