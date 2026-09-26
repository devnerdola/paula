package claude

import (
	"reflect"
	"testing"

	"nerdola.dev/x/paula/internal/families/api"
	chat "nerdola.dev/x/paula/internal/runners/api"
)

// body is a reply's body as the runner hands it over: the card, the history
// with the time before each message she was sent, and the time it is now
// before the message she is answering. The next reply sends its first four
// messages again as they are.
func body() map[string]any {
	return map[string]any{"messages": []map[string]any{
		{"role": "system", "content": "You are Paula."},
		{"role": "system", "content": "The next message was sent at Monday, 09:00."},
		{"role": "user", "content": "hey"},
		{"role": "assistant", "content": "hi love"},
		{"role": "system", "content": "It is now Monday, 09:05."},
		{"role": "user", "content": "you there?"},
	}}
}

const standing = 4

// marker is a marker as the prompt caching documentation writes one that lasts
// an hour.
var marker = map[string]any{"type": "ephemeral", "ttl": "1h"}

func open(t *testing.T, cache bool) api.Extension {
	t.Helper()
	e, err := Open(cache)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// OpenRouter keeps a system message where it stands, and a Claude model read
// what the replies before wrote with her notes told that way.
func TestHerNotesAreSystemMessages(t *testing.T) {
	if notes := open(t, true).Notes(); notes != (chat.Notes{Role: chat.RoleSystem}) {
		t.Errorf("Notes = %+v, want them as system messages", notes)
	}
}

// Claude writes a cache only where a request marks it. The end of the prompt is
// marked for the rounds of a reply, and the last message the next reply sends
// again, her reply, for the next reply, both for an hour. The message she was
// given before it is not marked: with it marked as well, the cache stopped
// moving forward once it had expired. The request names the session its
// conversation is kept on one host by.
func TestTheLastMessageTheNextReplySendsAgainIsTheOneMarked(t *testing.T) {
	b := body()
	open(t, true).Body(b, chat.ChatRequest{CacheKey: "paula-paula-reply", Standing: standing})

	if b["session_id"] != "paula-paula-reply" {
		t.Errorf("session_id = %v, want the request's cache key", b["session_id"])
	}
	if !reflect.DeepEqual(b["cache_control"], marker) {
		t.Errorf("cache_control = %v, want %v", b["cache_control"], marker)
	}
	msgs := b["messages"].([]map[string]any)
	want := body()["messages"].([]map[string]any)
	want[3]["content"] = []map[string]any{{"type": "text", "text": "hi love", "cache_control": marker}}
	for i := range msgs {
		if !reflect.DeepEqual(msgs[i], want[i]) {
			t.Errorf("message %d = %v, want %v", i, msgs[i], want[i])
		}
	}
}

// A model that is not to cache is marked nowhere, and names its session all
// the same.
func TestAModelThatIsNotToCacheIsMarkedNowhere(t *testing.T) {
	b := body()
	open(t, false).Body(b, chat.ChatRequest{CacheKey: "paula-paula-reply", Standing: standing})
	want := body()
	want["session_id"] = "paula-paula-reply"
	if !reflect.DeepEqual(b, want) {
		t.Errorf("body = %v, want %v", b, want)
	}
}

// A request that knows nothing of what the next one sends again, as a caption
// or a fold does, writes what no later request reads, at twice the input
// price, so it is marked nowhere. One with no cache key names no session.
func TestARequestNoLaterOneSendsAgainIsMarkedNowhere(t *testing.T) {
	b := body()
	open(t, true).Body(b, chat.ChatRequest{})
	if !reflect.DeepEqual(b, body()) {
		t.Errorf("body = %v, want it as it was", b)
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
	open(t, true).Body(b, chat.ChatRequest{Standing: standing})
	parts := msgs[3]["content"].([]map[string]any)
	if _, ok := parts[0]["cache_control"]; ok {
		t.Error("the text before the picture is marked")
	}
	if !reflect.DeepEqual(parts[1]["cache_control"], marker) {
		t.Errorf("the picture = %v, want it marked", parts[1])
	}
}
