// Package claude is what a Claude model takes on Venice.
//
// Venice moves a system message of a Claude request into the system prompt,
// ahead of the conversation. Told as system messages, her notes changed the
// system prompt on every reply, and no reply read anything the ones before it
// wrote. They are told as user messages, which stay where they stand.
//
// Claude writes a cache only where a request marks it, and Venice marks a
// Claude request itself: the system prompt, and the second-to-last message of
// the user. With her notes told as user messages, that message is the time told
// before the message she answers, which the next reply tells otherwise. So a
// request is marked on the last message the next reply sends again as it is,
// which is where the next reply reads what this one wrote. The marker lasts
// what Venice's own do: a longer one takes a header Venice does not document.
// A request also carries the prompt_cache_key Venice routes the requests of a
// conversation to one server by, which is where their cache is.
package claude

import (
	"errors"

	"nerdola.dev/x/paula/internal/families/api"
	chat "nerdola.dev/x/paula/internal/runners/api"
)

type extension struct{}

func Open(cache bool) (api.Extension, error) {
	if !cache {
		return nil, errors.New("Venice marks every Claude request for the cache itself, and nothing turns that off")
	}
	return extension{}, nil
}

func (extension) Notes() chat.Notes { return chat.Notes{Role: chat.RoleUser} }

func (extension) Body(body map[string]any, req chat.ChatRequest) {
	if req.CacheKey != "" {
		body["prompt_cache_key"] = req.CacheKey
	}
	if msgs, ok := body["messages"].([]map[string]any); ok && req.Standing > 0 {
		mark(msgs[req.Standing-1])
	}
}

// mark puts a marker on the last part of a message. A marker goes on a part,
// so a message written as text is sent as the one part it is; one that says
// nothing has no part to carry it.
func mark(msg map[string]any) {
	control := map[string]any{"type": "ephemeral"}
	switch content := msg["content"].(type) {
	case string:
		if content != "" {
			msg["content"] = []map[string]any{{"type": "text", "text": content, "cache_control": control}}
		}
	case []map[string]any:
		if len(content) > 0 {
			content[len(content)-1]["cache_control"] = control
		}
	}
}
