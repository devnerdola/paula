package api

import (
	"log/slog"

	"nerdola.dev/x/paula/internal/logs"
)

// Host is what the program around a runner gives it.
type Host struct {
	Log *slog.Logger
	// Secrets is where a runner registers the token it was given, so nothing
	// written by the run it belongs to carries it.
	Secrets *logs.Secrets
}
