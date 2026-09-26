// Package deepseek is what a DeepSeek model takes on Venice.
//
// A DeepSeek model reads every system message at the front of its prompt,
// beside the card. Told as system messages, her notes moved there and changed
// the prompt right after the card on every reply, and no reply read more of
// its cache than the card. Told as user messages, they stay where they stand,
// and each reply read what the ones before it had sent.
//
// DeepSeek reads as much of a prompt as starts the one before it, in steps of
// 256 tokens, and nothing marks where. With the time before the message she
// answers told as what time it is now, which the next prompt tells as when it
// was sent, the third reply of a conversation read 3840 tokens. With that time
// told as when it was sent, it read 4096: the whole of the prompt before it.
// It is the same minute whenever a reply starts as the message lands.
//
// Venice caches a prompt's prefix without being asked for a DeepSeek model, so
// what a request adds is the prompt_cache_key Venice routes the requests of a
// conversation to one server by, which is where their cache is.
//
// With tools in a request, DeepSeek's chat template keeps the thinking of
// earlier turns. A reply sent back without its thinking reads as one that
// thought nothing, and the model learns from it to skip its thinking, or to
// write the reply inside it. Every earlier reply goes back with what it
// thought.
package deepseek

import (
	"errors"

	"nerdola.dev/x/paula/internal/families/api"
	chat "nerdola.dev/x/paula/internal/runners/api"
)

type extension struct{}

func Open(cache bool) (api.Extension, error) {
	if !cache {
		return nil, errors.New("DeepSeek caches every prompt it is sent, and nothing turns that off")
	}
	return extension{}, nil
}

func (extension) Notes() chat.Notes {
	return chat.Notes{Role: chat.RoleUser, LastAsSent: true}
}

func (extension) PastThought() bool { return true }

func (extension) Body(body map[string]any, req chat.ChatRequest) {
	if req.CacheKey != "" {
		body["prompt_cache_key"] = req.CacheKey
	}
}
