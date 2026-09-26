// Package gpt is what an OpenAI model from GPT-5.6 on takes on Venice.
//
// Told as system messages, her notes left a GPT model on Venice reading
// nothing of its cache, not even the card every prompt starts with. They are
// told as user messages, which stay where they stand.
//
// Venice takes no prompt_cache_options, and a GPT model there read no more of
// its cache with prompt_cache_breakpoint than without it, so where a cache is
// written is OpenAI's own choice: at the end of the prompt. A request read the
// whole of the one before it when that prompt started it as it was sent, and
// nothing past the card when the time before the message she answered was told
// as what time it is now, which the next prompt tells as when it was sent. So
// that time is told as when it was sent here too. It is the same minute
// whenever a reply starts as the message lands.
//
// From GPT-5.6 on, how long a cache lasts is set by prompt_cache_options alone,
// and nothing but explicit mode, which it also sets, turns caching off. A cache
// lasts at least the 30 minutes OpenAI keeps one by default. Venice routes the
// requests of a conversation to one server by prompt_cache_key, which is where
// their cache is.
package gpt

import (
	"errors"

	"nerdola.dev/x/paula/internal/families/api"
	chat "nerdola.dev/x/paula/internal/runners/api"
)

type extension struct{}

func Open(cache bool) (api.Extension, error) {
	if !cache {
		return nil, errors.New("a GPT model caches every prompt it is sent, and Venice takes nothing that turns that off")
	}
	return extension{}, nil
}

func (extension) Notes() chat.Notes {
	return chat.Notes{Role: chat.RoleUser, LastAsSent: true}
}

func (extension) Body(body map[string]any, req chat.ChatRequest) {
	if req.CacheKey != "" {
		body["prompt_cache_key"] = req.CacheKey
	}
}
