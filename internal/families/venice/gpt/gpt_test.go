package gpt

import (
	"reflect"
	"strings"
	"testing"

	chat "nerdola.dev/x/paula/internal/runners/api"
)

// body is a reply's body as the runner hands it over: the card, the history
// with the time before each message she was sent, and the time the message she
// is answering was sent, her notes told as user messages.
func body() map[string]any {
	return map[string]any{"messages": []map[string]any{
		{"role": "system", "content": "You are Paula."},
		{"role": "user", "content": "The next message was sent at Monday, 09:00."},
		{"role": "user", "content": "hey"},
		{"role": "assistant", "content": "hi love"},
		{"role": "user", "content": "The next message was sent at Monday, 09:05."},
		{"role": "user", "content": "you there?"},
	}}
}

// Told as system messages, her notes left a GPT model on Venice reading
// nothing of its cache. As user messages it read the card, and the whole of the
// prompt before once the time before the message she answers was told as the
// next prompt tells it.
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

// A request names the key Venice routes its conversation to one server by, and
// its messages go as the runner built them.
func TestARequestNamesItsCacheKey(t *testing.T) {
	e, err := Open(true)
	if err != nil {
		t.Fatal(err)
	}
	b := body()
	e.Body(b, chat.ChatRequest{CacheKey: "paula-paula-reply", Standing: 4})
	want := body()
	want["prompt_cache_key"] = "paula-paula-reply"
	if !reflect.DeepEqual(b, want) {
		t.Errorf("body = %v, want %v", b, want)
	}
}

func TestCachingCannotBeTurnedOff(t *testing.T) {
	if _, err := Open(false); err == nil || !strings.Contains(err.Error(), "nothing that turns that off") {
		t.Errorf("error = %v, want it said that nothing turns caching off", err)
	}
}
