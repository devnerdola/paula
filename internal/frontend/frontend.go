// Package frontend keeps one session for every way of reaching Paula: it
// shows what the conversation does, and tells the conversation what is typed.
// It declares which kinds of frontend there are and what a session needs of the
// conversation. What a frontend can do is internal/frontend/api, which a caller
// imports beside this package.
package frontend

import (
	"context"
	"iter"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/conversation"
	"nerdola.dev/x/paula/internal/store"
)

// Seq numbers the events of the conversation, which a session follows from
// where it started. A message id is a number of its own.
type Seq = conversation.Seq

// Conversation is everything a session does with the conversation: what it
// tells it, how it keeps up with what happens, and the model serving each role.
type Conversation interface {
	Post(ctx context.Context, m conversation.NewMessage) error
	Stop(ctx context.Context) (bool, error)

	Events(ctx context.Context, after Seq) iter.Seq2[conversation.Event, error]
	Standing(ctx context.Context) (seq Seq, message store.MessageID, err error)
	Wait(ctx context.Context) (Seq, error)
	History(ctx context.Context, before store.MessageID, limit int) ([]store.Message, error)
	Since(ctx context.Context, after store.MessageID) ([]store.Message, error)

	Models(ctx context.Context) (conversation.Models, error)
	SetModel(ctx context.Context, role config.Role, name string) error
	ResetModels(ctx context.Context) error

	Summary(ctx context.Context) (*store.Summary, error)
	Memories(ctx context.Context, query string, limit int) ([]store.Memory, error)
	Forget(ctx context.Context, id store.MemoryID) ([]store.Memory, error)
}
