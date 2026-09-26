package conversation

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"nerdola.dev/x/paula/internal/runners/api"
	"nerdola.dev/x/paula/internal/store"
)

// The prompt is the persona, the history and the user input, and nothing of
// what she remembers: memories are reached through her tools only.
func TestNoMemoryIsInThePrompt(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: says("hello")}
	r := openReply(t, f)
	ctx := context.Background()

	r.say(t, "hey")
	first, err := r.store.MessagesAfter(ctx, 0)
	if err != nil || len(first) == 0 {
		t.Fatalf("messages = %+v, %v", first, err)
	}
	if err := r.store.Remember(ctx, &store.Memory{Content: "Caio's sister is Ana.", Source: first[0].ID}); err != nil {
		t.Fatal(err)
	}
	r.say(t, "you there?")

	prompt := f.replied().Messages
	for i, m := range prompt {
		if strings.Contains(text(m), "Caio's sister is Ana.") {
			t.Errorf("message %d of the prompt tells a memory: %q", i, text(m))
		}
	}
	n := len(prompt)
	if !strings.HasPrefix(text(prompt[n-2]), "It is now ") || text(prompt[n-1]) != "you there?" {
		t.Errorf("the prompt ends with %q, %q; want the time, then the message", text(prompt[n-2]), text(prompt[n-1]))
	}
}

// A request says how much of it the next reply sends again as it was sent: all
// of it up to the time of the message she is answering. The first reply has
// only the persona to say it of.
func TestAReplySaysHowMuchOfItsPromptTheNextSendsAgain(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: says("hello")}
	r := openReply(t, f)

	r.say(t, "hey")
	r.say(t, "you there?")
	r.say(t, "hello??")

	requests := f.all()
	if len(requests) != 3 {
		t.Fatalf("%d requests were sent, want three", len(requests))
	}
	if n := requests[0].Standing; n != 1 {
		t.Errorf("the first reply says %d of its messages stand, want the persona alone", n)
	}
	for i := range len(requests) - 1 {
		was, next := requests[i], requests[i+1]
		n := was.Standing
		if n == 0 || n >= len(next.Messages) {
			t.Errorf("reply %d says %d of its messages stand, of the %d the next sends", i+1, n, len(next.Messages))
			continue
		}
		if !reflect.DeepEqual(next.Messages[:n], was.Messages[:n]) {
			t.Errorf("reply %d says %d of its messages stand, and the next changed them", i+1, n)
		}
		if !strings.HasPrefix(text(was.Messages[n]), "It is now ") || was.Messages[n].Role != api.RoleSystem {
			t.Errorf("reply %d says %d of its messages stand, and the one after them is %q, want the time now",
				i+1, n, text(was.Messages[n]))
		}
	}
}

func msg(id store.MessageID, role string, replyTo store.MessageID, text string) store.Message {
	return store.Message{
		ID: id, Role: role, ReplyTo: replyTo,
		Parts: []store.Part{{Type: store.PartText, Text: text}},
	}
}

func order(messages []store.Message) string {
	var out []string
	for _, m := range ordered(messages) {
		out = append(out, m.Text())
	}
	return strings.Join(out, ",")
}

func TestAReplyIsReadAfterWhatItAnswers(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []store.Message
		want string
	}{
		{
			"nothing happened while she wrote",
			[]store.Message{
				msg(1, store.RoleUser, 0, "a"),
				msg(2, store.RoleAssistant, 1, "A"),
				msg(3, store.RoleUser, 0, "b"),
				msg(4, store.RoleAssistant, 3, "B"),
			},
			"a,A,b,B",
		},
		{
			"something was said while she wrote",
			[]store.Message{
				msg(1, store.RoleUser, 0, "a"),
				msg(2, store.RoleUser, 0, "b"),
				msg(3, store.RoleAssistant, 1, "A"),
			},
			"a,A,b",
		},
		{
			"two things were said while she wrote",
			[]store.Message{
				msg(1, store.RoleUser, 0, "a"),
				msg(2, store.RoleUser, 0, "b"),
				msg(3, store.RoleUser, 0, "c"),
				msg(4, store.RoleAssistant, 1, "A"),
				msg(5, store.RoleAssistant, 3, "C"),
			},
			"a,A,b,c,C",
		},
		{
			"a reply answering nothing keeps its place",
			[]store.Message{
				msg(1, store.RoleUser, 0, "a"),
				msg(2, store.RoleAssistant, 0, "A"),
				msg(3, store.RoleUser, 0, "b"),
			},
			"a,A,b",
		},
		{
			"a reply whose message is gone keeps its place",
			[]store.Message{
				msg(2, store.RoleUser, 0, "b"),
				msg(3, store.RoleAssistant, 1, "A"),
				msg(4, store.RoleUser, 0, "c"),
			},
			"b,A,c",
		},
		{"nothing at all", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := order(tc.in); got != tc.want {
				t.Errorf("ordered = %q, want %q", got, tc.want)
			}
		})
	}
}
