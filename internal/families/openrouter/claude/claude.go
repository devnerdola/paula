// Package claude is what a Claude model takes on OpenRouter.
//
// OpenRouter keeps a system message where it stands in the conversation, so
// her notes are told as system messages, the role a model reads for what it is
// told rather than for what it is answering.
//
// OpenRouter keeps the requests that name one session_id on the host that
// served the first of them, which is where their cache is. Claude writes a
// cache only where a request marks it: one entry for the prefix that ends at
// each marker, which a later request reads when it ends at one of the 20
// blocks before a marker of its own. A reply's prompt ends in the time it is
// now and the message it answers, which the next reply tells otherwise, so the
// marker the next reply reads is on the last message it sends again as it is.
// The top-level cache_control marks the last block, which the rounds of a
// reply read. Nothing else is marked: with a marker on the message she was
// given before that one as well, a Claude model read nothing written after its
// cache first expired.
//
// A marker lasts an hour, the longest the prompt caching documentation gives
// one: texts are often more than five minutes apart. Writing for an hour costs
// twice the input price, so a request no later one sends again, which is one
// that knows nothing of what the next sends again as a caption or a compaction
// does, is marked nowhere. So is every request of a model that is not to
// cache, and Claude then writes nothing.
package claude

import (
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
	}
	msgs, ok := body["messages"].([]map[string]any)
	if !e.marks || !ok || req.Standing == 0 {
		return
	}
	body["cache_control"] = control()
	mark(msgs[req.Standing-1])
}

func control() map[string]any {
	return map[string]any{"type": "ephemeral", "ttl": "1h"}
}

// mark puts a marker on the last part of a message. A marker goes on a part,
// so a message written as text is sent as the one part it is; one that says
// nothing has no part to carry it.
func mark(msg map[string]any) {
	switch content := msg["content"].(type) {
	case string:
		if content != "" {
			msg["content"] = []map[string]any{{"type": "text", "text": content, "cache_control": control()}}
		}
	case []map[string]any:
		if len(content) > 0 {
			content[len(content)-1]["cache_control"] = control()
		}
	}
}
