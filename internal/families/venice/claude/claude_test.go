package claude

import (
	"reflect"
	"strings"
	"testing"

	"nerdola.dev/x/paula/internal/families/api"
	chat "nerdola.dev/x/paula/internal/runners/api"
)

// body is a reply's body as the runner hands it over: the card, the history
// with the time before each message she was sent, and the time it is now
// before the message she is answering, her notes told as user messages. The
// next reply sends its first four messages again as they are.
func body() map[string]any {
	return map[string]any{"messages": []map[string]any{
		{"role": "system", "content": "You are Paula."},
		{"role": "user", "content": "The next message was sent at Monday, 09:00."},
		{"role": "user", "content": "hey"},
		{"role": "assistant", "content": "hi love"},
		{"role": "user", "content": "It is now Monday, 09:05."},
		{"role": "user", "content": "you there?"},
	}}
}

const standing = 4

func open(t *testing.T) api.Extension {
	t.Helper()
	e, err := Open(true)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// Venice moves a system message into the system prompt, where her notes would
// change it on every reply.
func TestHerNotesAreUserMessages(t *testing.T) {
	if notes := open(t).Notes(); notes != (chat.Notes{Role: chat.RoleUser}) {
		t.Errorf("Notes = %+v, want them as user messages", notes)
	}
}

// Venice's own marker on the second-to-last user message lands on the time
// told before the message she answers, which the next reply tells otherwise.
// The last message the next reply sends again, her reply, is marked, and
// nothing else. The request names the key Venice routes its conversation to
// one server by.
func TestTheLastMessageTheNextReplySendsAgainIsTheOneMarked(t *testing.T) {
	b := body()
	open(t).Body(b, chat.ChatRequest{CacheKey: "paula-paula-reply", Standing: standing})
	want := body()
	want["prompt_cache_key"] = "paula-paula-reply"
	want["messages"].([]map[string]any)[3]["content"] = []map[string]any{
		{"type": "text", "text": "hi love", "cache_control": map[string]any{"type": "ephemeral"}},
	}
	if !reflect.DeepEqual(b, want) {
		t.Errorf("body = %v, want %v", b, want)
	}
}

// A message sent as parts carries the marker on its last one, a picture as
// much as a text.
func TestAMessageOfPartsIsMarkedOnItsLast(t *testing.T) {
	b := body()
	msgs := b["messages"].([]map[string]any)
	msgs[3] = map[string]any{"role": "user", "content": []map[string]any{
		{"type": "text", "text": "look"},
		{"type": "image_url", "image_url": map[string]any{"url": "data:image/jpeg;base64,AAAA"}},
	}}
	open(t).Body(b, chat.ChatRequest{Standing: standing})
	parts := msgs[3]["content"].([]map[string]any)
	if _, ok := parts[0]["cache_control"]; ok {
		t.Error("the text before the picture is marked")
	}
	if !reflect.DeepEqual(parts[1]["cache_control"], map[string]any{"type": "ephemeral"}) {
		t.Errorf("the picture = %v, want it marked", parts[1])
	}
}

func TestCachingCannotBeTurnedOff(t *testing.T) {
	if _, err := Open(false); err == nil || !strings.Contains(err.Error(), "nothing turns that off") {
		t.Errorf("error = %v, want it said that nothing turns caching off", err)
	}
}
