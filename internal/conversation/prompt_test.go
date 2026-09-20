package conversation

import (
	"strings"
	"testing"

	"nerdola.dev/x/paula/internal/store"
)

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

func TestOrdered(t *testing.T) {
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
