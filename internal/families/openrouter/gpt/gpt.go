// Package gpt is what an OpenAI model from GPT-5.6 on takes on OpenRouter.
//
// OpenRouter keeps a system message where it stands in the conversation, so
// her notes are told as system messages, the role a model reads for what it is
// told rather than for what it is answering.
//
// OpenRouter keeps the requests that name one session_id on the host that
// served the first of them, and prompt_cache_key is OpenAI's own name for the
// requests that share a prefix. OpenAI places a breakpoint of its own at the
// end of the latest user message, which the next reply tells otherwise, since
// the time it is now stands before it. So the last message she was given that
// the next reply sends again as it is carries a prompt_cache_breakpoint:
// OpenAI takes one only on a text block of an input message, never on one the
// model wrote.
//
// A request caches in explicit mode, where nothing is written but at a
// breakpoint, so the time it is now and the message she answers, which the
// next reply tells otherwise, are not written at all. A cache lasts the 30
// minutes that are the only ttl the prompt caching documentation gives. In
// explicit mode a request reads only at the breakpoints it carries itself, so a
// request also carries the breakpoint the reply before it wrote. That reply
// answered the last message she was sent that this one sends again, and what
// stood of its prompt ends at the time told before that message. A model that
// is not to cache is sent explicit mode with no breakpoint, which caches
// nothing.
package gpt

import (
	"slices"

	"nerdola.dev/x/paula/internal/families/api"
	chat "nerdola.dev/x/paula/internal/runners/api"
)

type extension struct {
	marks bool
}

func Open(cache bool) (api.Extension, error) {
	return extension{marks: cache}, nil
}

func (e extension) Notes() chat.Notes { return chat.Notes{Role: chat.RoleSystem} }

func (e extension) Body(body map[string]any, req chat.ChatRequest) {
	if req.CacheKey != "" {
		body["session_id"] = req.CacheKey
		body["prompt_cache_key"] = req.CacheKey
	}
	if !e.marks {
		body["prompt_cache_options"] = map[string]any{"mode": "explicit"}
		return
	}
	body["prompt_cache_options"] = map[string]any{"mode": "explicit", "ttl": "30m"}
	msgs, ok := body["messages"].([]map[string]any)
	if !ok || req.Standing <= 0 {
		return
	}
	standing := msgs[:req.Standing]
	mark(standing)
	for i, msg := range slices.Backward(standing) {
		if msg["role"] == chat.RoleUser {
			if i > 0 {
				mark(standing[:i-1])
			}
			return
		}
	}
}

// mark puts a breakpoint on the last of the messages she was given that has a
// text block.
func mark(msgs []map[string]any) {
	for _, msg := range slices.Backward(msgs) {
		if msg["role"] != chat.RoleAssistant && breakpoint(msg) {
			return
		}
	}
}

// breakpoint puts a breakpoint on the last text block of a message, and says
// whether it had one: a message written as text is sent as the one block it
// is, and a message of pictures alone has no text block to carry it.
func breakpoint(msg map[string]any) bool {
	marker := func() map[string]any { return map[string]any{"mode": "explicit"} }
	switch content := msg["content"].(type) {
	case string:
		if content == "" {
			return false
		}
		msg["content"] = []map[string]any{{"type": "text", "text": content, "prompt_cache_breakpoint": marker()}}
		return true
	case []map[string]any:
		for _, part := range slices.Backward(content) {
			if part["type"] == "text" {
				part["prompt_cache_breakpoint"] = marker()
				return true
			}
		}
	}
	return false
}
