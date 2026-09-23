package conversation

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"nerdola.dev/x/paula/internal/runners"
	"nerdola.dev/x/paula/internal/runners/api"
	"nerdola.dev/x/paula/internal/store"
)

// sized is a setup whose model is held to a context of its own, the way a
// models entry that writes a context is.
func sized(f *fakeRunner, tokens int) *runners.Setup {
	set := setup(f)
	set.Models[0].Context = tokens
	return set
}

// said is what the model was sent of each message, in order.
func said(req api.ChatRequest, role string) []string {
	var out []string
	for _, m := range req.Messages {
		if m.Role == role {
			out = append(out, text(m))
		}
	}
	return out
}

func TestWhatAPromptTakesIsItsCharactersAndItsPictures(t *testing.T) {
	messages := []api.Message{
		// Four characters written as five bytes, which are four to a model.
		api.Text(api.RoleSystem, "café"),
		{Role: api.RoleUser, Parts: []api.Part{
			{Type: api.PartText, Text: "abcdefg"},
			{Type: api.PartImage, Data: []byte("a picture")},
		}},
	}

	chars, images := measure(messages)
	if chars != 11 || images != 1 {
		t.Errorf("measure = %d characters and %d pictures, want 11 and 1", chars, images)
	}
	// Eleven characters at half a token each is 5.5, which costs six, and the
	// picture costs what the host bills for one.
	if got := size(messages, 0.5, 1000); got != 1006 {
		t.Errorf("size = %d, want 1006", got)
	}
}

func TestWhatACharacterCostsIsReadBackFromTheCount(t *testing.T) {
	var r costs
	// A token every 3.5 characters, until a host says otherwise.
	if got := r.ratio("chat"); got != 1.0/3.5 {
		t.Errorf("ratio = %v, want a token every 3.5 characters", got)
	}

	// Twenty characters counted as ten tokens is a token every two.
	r.correct("chat", 10, []api.Message{api.Text(api.RoleUser, strings.Repeat("a", 20))}, "")
	if got := r.ratio("chat"); got != 0.5 {
		t.Errorf("ratio = %v, want 0.5", got)
	}
	if got := r.ratio("eyes"); got != 1.0/3.5 {
		t.Errorf("another model's ratio = %v, want the one nothing has counted", got)
	}

	// A count of nothing is a host that reported no usage.
	r.correct("chat", 0, []api.Message{api.Text(api.RoleUser, strings.Repeat("a", 20))}, "")
	if got := r.ratio("chat"); got != 0.5 {
		t.Errorf("ratio = %v, want the one that was counted", got)
	}

	// What a reply thought goes back with it, and is text the host counted:
	// ten characters written and thirty thought, counted as ten tokens, is a
	// token every four.
	thought := api.Text(api.RoleAssistant, strings.Repeat("a", 10))
	thought.Reasoning = &api.Reasoning{Text: strings.Repeat("b", 30)}
	r.correct("chat", 10, []api.Message{thought}, "")
	if got := r.ratio("chat"); got != 0.25 {
		t.Errorf("ratio = %v, want 0.25", got)
	}
}

// A host bills a picture by how big it is, and each of them by its own
// reckoning, so what one costs is read back the way what a character costs is:
// from the count a prompt carrying one came home with.
func TestWhatAPictureCostsIsReadBackFromTheCount(t *testing.T) {
	var r costs
	if got := r.image("chat"); got != startImage {
		t.Errorf("a picture costs %d, want the one nothing has counted", got)
	}

	withPicture := func(chars, pictures int) []api.Message {
		parts := []api.Part{{Type: api.PartText, Text: strings.Repeat("a", chars)}}
		for range pictures {
			parts = append(parts, api.Part{Type: api.PartImage, Data: []byte("a picture")})
		}
		return []api.Message{{Role: api.RoleUser, Parts: parts}}
	}

	// Twenty characters at a token every 3.5 come to 6, and the rest of the
	// count is what the picture cost.
	r.correct("chat", 1000, withPicture(20, 1), "")
	if got := r.image("chat"); got != 994 {
		t.Errorf("a picture costs %d, want 994", got)
	}
	// The picture is in the count and not in the characters, so it still says
	// nothing about what one of those costs.
	if got := r.ratio("chat"); got != 1.0/3.5 {
		t.Errorf("ratio = %v, want the one nothing has counted", got)
	}
	if got := r.image("eyes"); got != startImage {
		t.Errorf("another model's picture costs %d, want the one nothing has counted", got)
	}

	// Two of them share what is left of the count.
	r.correct("chat", 406, withPicture(20, 2), "")
	if got := r.image("chat"); got != 200 {
		t.Errorf("a picture costs %d, want 200", got)
	}
	// A count the characters alone come to has no picture in it to read.
	r.correct("chat", 6, withPicture(20, 1), "")
	if got := r.image("chat"); got != 200 {
		t.Errorf("a picture costs %d, want the one that was counted", got)
	}
}

// counting answers with one chunk of text, and reports what the host counted
// the prompt it was sent as.
func counting(text string, prompt int) func(context.Context, api.ChatRequest, func(api.Chunk) error) (*api.Result, error) {
	return func(_ context.Context, _ api.ChatRequest, fn func(api.Chunk) error) (*api.Result, error) {
		if err := fn(api.Chunk{Kind: api.ChunkText, Text: text}); err != nil {
			return nil, err
		}
		return &api.Result{FinishReason: "stop", Usage: api.Usage{PromptTokens: prompt}}, nil
	}
}

func TestTheCountOfAReplysPromptSetsWhatACharacterCosts(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: counting("ok", 4000)}
	r := openReply(t, f)
	r.say(t, "hey")

	var chars int
	for _, m := range f.asked().Messages {
		chars += utf8.RuneCountInString(text(m))
	}
	if chars == 0 {
		t.Fatal("the prompt was sent with no text in it")
	}
	if want, got := 4000/float64(chars), r.costs.ratio("chat"); got != want {
		t.Errorf("ratio = %v, want %v: what the host counted over what she sent", got, want)
	}
}

func TestAnExchangeEndsWhereItsReplyDoes(t *testing.T) {
	// A burst is answered once, so both messages belong to the one exchange.
	// The last messages are the ones being answered now.
	got := exchanges([]store.Message{
		{ID: 1, Role: store.RoleUser},
		{ID: 2, Role: store.RoleUser},
		{ID: 3, Role: store.RoleAssistant, ReplyTo: 2},
		{ID: 4, Role: store.RoleUser},
		{ID: 5, Role: store.RoleAssistant, ReplyTo: 4},
		{ID: 6, Role: store.RoleUser},
	})

	var ids [][]store.MessageID
	for _, group := range got {
		var in []store.MessageID
		for _, m := range group {
			in = append(in, m.ID)
		}
		ids = append(ids, in)
	}
	want := [][]store.MessageID{{1, 2, 3}, {4, 5}, {6}}
	if len(ids) != len(want) {
		t.Fatalf("exchanges = %v, want %v", ids, want)
	}
	for i := range want {
		if !slices.Equal(ids[i], want[i]) {
			t.Errorf("exchange %d = %v, want %v", i, ids[i], want[i])
		}
	}
}

func TestTheOldestOfAConversationPastTheContextIsLeftOut(t *testing.T) {
	long := strings.Repeat("a long thing to say ", 20)
	f := &fakeRunner{model: chatModel(), chat: says(long)}
	r := openReplyWith(t, f, sized(f, 1000))
	for i := range 12 {
		r.say(t, fmt.Sprintf("message %d: %s", i, long))
	}

	req := f.replied()
	mine := said(req, api.RoleUser)
	if len(mine) == 0 || len(mine) >= 12 {
		t.Fatalf("the prompt holds %d of my messages, want some of the twelve", len(mine))
	}
	// The newest is what she is answering, and the oldest went.
	if !strings.HasPrefix(mine[len(mine)-1], "message 11:") {
		t.Errorf("the last message of the prompt is %.20q, want the newest", mine[len(mine)-1])
	}
	for _, said := range mine {
		if strings.HasPrefix(said, "message 0:") {
			t.Error("the oldest message is still in the prompt")
		}
	}
	// What is left opens with the card and then an exchange of its own: a
	// reply is never left without the message it answers.
	if !strings.HasPrefix(text(req.Messages[0]), "You are Paula") {
		t.Fatalf("the prompt opens with %.30q, want the card", text(req.Messages[0]))
	}
	if req.Messages[1].Role != api.RoleSystem {
		t.Errorf("the card is followed by a %s message, want the time before a message of mine",
			req.Messages[1].Role)
	}
}

func TestTheExchangeSheIsAnsweringGoesWhateverItTakes(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: says("ok")}
	r := openReplyWith(t, f, sized(f, 1))
	r.say(t, "one")
	r.say(t, "two")
	r.say(t, "three")

	// A context that holds nothing still holds what she is answering, since
	// leaving it out would answer nothing.
	if mine := said(f.replied(), api.RoleUser); len(mine) != 1 || mine[0] != "three" {
		t.Errorf("the prompt holds %q of my messages, want the one she is answering", mine)
	}
}

func TestWhatIsLeftOutOfAPromptIsSaid(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: says("ok")}
	r := openReplyWith(t, f, sized(f, 1))
	r.say(t, "one")
	r.say(t, "two")

	// Losing the oldest of a conversation is not something to do quietly, so
	// it is logged at the level a run shows by default, with how much went.
	written := r.log.String()
	if !strings.Contains(written, "level=WARN") ||
		!strings.Contains(written, "left out of the prompt") {
		t.Errorf("the log holds %q, want a warning about the prompt", written)
	}
	if !strings.Contains(written, "exchanges=1") {
		t.Errorf("the log holds %q, want the one exchange that went", written)
	}
}

func TestAModelWithNoContextLeavesNothingOut(t *testing.T) {
	catalogue := chatModel()
	catalogue.Context = 0
	f := &fakeRunner{model: catalogue, chat: says("ok")}
	r := openReply(t, f)
	for i := range 5 {
		r.say(t, fmt.Sprintf("message %d", i))
	}

	// Neither the file nor the catalogue says what the model holds, so there
	// is nothing to hold the prompt to.
	if mine := said(f.asked(), api.RoleUser); len(mine) != 5 {
		t.Errorf("the prompt holds %q, want all five", mine)
	}
}
