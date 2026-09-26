package gpt

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

func open(t *testing.T, cache bool) api.Extension {
	t.Helper()
	e, err := Open(cache)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// OpenRouter keeps a system message where it stands, and a GPT model read what
// the replies before wrote with her notes told that way.
func TestHerNotesAreSystemMessages(t *testing.T) {
	if notes := open(t, true).Notes(); notes != (chat.Notes{Role: chat.RoleSystem}) {
		t.Errorf("Notes = %+v, want them as system messages", notes)
	}
}

// A request names its conversation to OpenRouter and to OpenAI, whether or
// not it caches, and one with no cache key names none.
func TestARequestNamesItsConversation(t *testing.T) {
	for _, cache := range []bool{true, false} {
		b := body()
		open(t, cache).Body(b, chat.ChatRequest{CacheKey: "paula-paula-reply", Standing: standing})
		if b["session_id"] != "paula-paula-reply" || b["prompt_cache_key"] != "paula-paula-reply" {
			t.Errorf("session_id = %v, prompt_cache_key = %v, want the request's cache key", b["session_id"], b["prompt_cache_key"])
		}
	}

	b := body()
	open(t, true).Body(b, chat.ChatRequest{Standing: standing})
	if _, ok := b["session_id"]; ok {
		t.Error("a request with no cache key names a session")
	}
	if _, ok := b["prompt_cache_key"]; ok {
		t.Error("a request with no cache key names a prompt cache key")
	}
}

// OpenAI takes a breakpoint only on a text block of an input message, so the
// one the next reply reads is on the last message she was given that it sends
// again, not on her reply after it. In explicit mode a request reads only at
// the breakpoints it carries, so it carries the one the reply before it wrote
// as well, which for the first reply was on the card. The cache lasts the 30
// minutes the documentation gives.
func TestARequestCarriesTheBreakpointTheReplyBeforeItWrote(t *testing.T) {
	e := open(t, true)
	before := map[string]any{"messages": []map[string]any{
		{"role": "system", "content": "You are Paula."},
		{"role": "system", "content": "It is now Monday, 09:00."},
		{"role": "user", "content": "hey"},
	}}
	e.Body(before, chat.ChatRequest{CacheKey: "paula-paula-reply", Standing: 1})
	b := body()
	e.Body(b, chat.ChatRequest{CacheKey: "paula-paula-reply", Standing: standing})

	options := map[string]any{"mode": "explicit", "ttl": "30m"}
	if !reflect.DeepEqual(b["prompt_cache_options"], options) {
		t.Errorf("prompt_cache_options = %v, want %v", b["prompt_cache_options"], options)
	}
	marked := func(text string) []map[string]any {
		return []map[string]any{{"type": "text", "text": text, "prompt_cache_breakpoint": map[string]any{"mode": "explicit"}}}
	}
	if got := before["messages"].([]map[string]any)[0]["content"]; !reflect.DeepEqual(got, marked("You are Paula.")) {
		t.Errorf("the card of the reply before = %v, want the breakpoint on it", got)
	}
	msgs := b["messages"].([]map[string]any)
	want := body()["messages"].([]map[string]any)
	want[0]["content"] = marked("You are Paula.")
	want[2]["content"] = marked("hey")
	for i := range msgs {
		if !reflect.DeepEqual(msgs[i], want[i]) {
			t.Errorf("message %d = %v, want %v", i, msgs[i], want[i])
		}
	}
}

// Explicit mode with no breakpoint caches nothing, which is what a model that
// is not to cache is sent.
func TestAModelThatIsNotToCacheIsGivenNoBreakpoint(t *testing.T) {
	b := body()
	open(t, false).Body(b, chat.ChatRequest{Standing: standing})
	want := body()
	want["prompt_cache_options"] = map[string]any{"mode": "explicit"}
	if !reflect.DeepEqual(b, want) {
		t.Errorf("body = %v, want %v", b, want)
	}
}

// The breakpoint goes on the text of a message that came with a picture, and
// a message of a picture alone leaves it to the input message before.
func TestTheBreakpointGoesOnText(t *testing.T) {
	e := open(t, true)
	b := body()
	msgs := b["messages"].([]map[string]any)
	msgs[2] = map[string]any{"role": "user", "content": []map[string]any{
		{"type": "text", "text": "look"},
		{"type": "image_url", "image_url": map[string]any{"url": "data:image/jpeg;base64,AAAA"}},
	}}
	e.Body(b, chat.ChatRequest{Standing: standing})
	parts := msgs[2]["content"].([]map[string]any)
	if _, ok := parts[0]["prompt_cache_breakpoint"]; !ok {
		t.Errorf("the text = %v, want the breakpoint on it", parts[0])
	}
	if _, ok := parts[1]["prompt_cache_breakpoint"]; ok {
		t.Error("the picture has the breakpoint")
	}

	b = body()
	msgs = b["messages"].([]map[string]any)
	msgs[2] = map[string]any{"role": "user", "content": []map[string]any{
		{"type": "image_url", "image_url": map[string]any{"url": "data:image/jpeg;base64,AAAA"}},
	}}
	e.Body(b, chat.ChatRequest{Standing: standing})
	if _, ok := msgs[1]["content"].([]map[string]any); !ok {
		t.Errorf("message 1 = %v, want the breakpoint on the input message before the picture", msgs[1])
	}
}
