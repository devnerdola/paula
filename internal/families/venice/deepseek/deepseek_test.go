package deepseek

import (
	"reflect"
	"strings"
	"testing"

	chat "nerdola.dev/x/paula/internal/runners/api"
)

func body() map[string]any {
	return map[string]any{"messages": []map[string]any{
		{"role": "system", "content": "You are Paula."},
		{"role": "user", "content": "hey"},
		{"role": "assistant", "content": "hi love"},
		{"role": "user", "content": "The next message was sent at Monday, 09:05."},
		{"role": "user", "content": "you there?"},
	}}
}

// A DeepSeek model moves a system message to the front of its prompt, where
// her notes would change it right after the card, and reads a prompt only as
// far as it starts the next one.
func TestHerNotesAreUserMessagesAndTheLastTimeIsWhenItWasSent(t *testing.T) {
	e, err := Open(true)
	if err != nil {
		t.Fatal(err)
	}
	want := chat.Notes{Role: chat.RoleUser, LastAsSent: true}
	if notes := e.Notes(); notes != want {
		t.Errorf("Notes = %+v, want %+v", notes, want)
	}
}

// Venice caches a DeepSeek model without being asked: a request names the key
// Venice routes its conversation to one server by, and nothing else.
func TestARequestNamesItsCacheKeyAndNothingElse(t *testing.T) {
	e, err := Open(true)
	if err != nil {
		t.Fatal(err)
	}
	b := body()
	e.Body(b, chat.ChatRequest{CacheKey: "paula-paula-reply", Standing: 3})
	want := body()
	want["prompt_cache_key"] = "paula-paula-reply"
	if !reflect.DeepEqual(b, want) {
		t.Errorf("body = %v, want %v", b, want)
	}

	b = body()
	e.Body(b, chat.ChatRequest{Standing: 3})
	if !reflect.DeepEqual(b, body()) {
		t.Errorf("a request with no cache key = %v, want it as it was", b)
	}
}

func TestCachingCannotBeTurnedOff(t *testing.T) {
	if _, err := Open(false); err == nil || !strings.Contains(err.Error(), "nothing turns that off") {
		t.Errorf("error = %v, want it said that nothing turns caching off", err)
	}
}
