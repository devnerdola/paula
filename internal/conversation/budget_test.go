package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"nerdola.dev/x/paula/internal/config"
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

func TestWhatAPromptTakesIsItsWordsAndItsPictures(t *testing.T) {
	messages := []api.Message{
		api.Text(api.RoleSystem, "a café"),
		{Role: api.RoleUser, Parts: []api.Part{
			{Type: api.PartText, Text: "one  two\nthree"},
			{Type: api.PartImage, Data: []byte("a picture")},
		}},
	}

	words, images := measure(messages)
	if words != 5 || images != 1 {
		t.Errorf("measure = %d words and %d pictures, want 5 and 1", words, images)
	}
	// Five words at half a token each is 2.5, which costs three, and the
	// picture costs what the host bills for one.
	if got := size(messages, 0.5, 1000); got != 1003 {
		t.Errorf("size = %d, want 1003", got)
	}
}

// ofWords is a message of n words.
func ofWords(role string, n int) api.Message { return api.Text(role, manyWords(n)) }

// What a word costs is the most two prompts in a row have come to, so a prompt
// that comes to less leaves it where it was, and one that comes to more once
// is an exception until the next comes to more as well.
func TestWhatAWordCostsIsTheMostTwoPromptsInARowCameTo(t *testing.T) {
	var r costs
	// Three tokens a word, until a host says otherwise.
	if got := r.rate("chat"); got != 3 {
		t.Errorf("rate = %v, want three tokens a word", got)
	}

	// The first count is what a word costs, less than what nothing counted
	// said or not: twenty words counted as forty tokens is two a word.
	r.correct("chat", 40, []api.Message{ofWords(api.RoleUser, 20)}, "")
	if got := r.rate("chat"); got != 2 {
		t.Errorf("rate = %v, want 2", got)
	}
	if got := r.rate("eyes"); got != 3 {
		t.Errorf("another model's rate = %v, want the one nothing has counted", got)
	}

	// A count of nothing is a host that reported no usage.
	r.correct("chat", 0, []api.Message{ofWords(api.RoleUser, 20)}, "")
	// A prompt that came to less says nothing a word is sure to cost less for.
	r.correct("chat", 30, []api.Message{ofWords(api.RoleUser, 20)}, "")
	if got := r.rate("chat"); got != 2 {
		t.Errorf("rate = %v, want the most a prompt came to, 2", got)
	}

	// A prompt that came to far more, once, is an exception: the next came to
	// two a word again.
	r.correct("chat", 200, []api.Message{ofWords(api.RoleUser, 20)}, "")
	r.correct("chat", 40, []api.Message{ofWords(api.RoleUser, 20)}, "")
	if got := r.rate("chat"); got != 2 {
		t.Errorf("rate = %v, want 2, the ten a word of one prompt left out", got)
	}

	// What a reply thought goes back with it, and is text the host counted:
	// ten words written and thirty thought, counted as a hundred tokens, is
	// 2.5 a word. Two prompts in a row at 2.5 and then 3 a word take the rate
	// to the lower of the two.
	thought := ofWords(api.RoleAssistant, 10)
	thought.Reasoning = &api.Reasoning{Text: strings.TrimSpace(strings.Repeat("thought ", 30))}
	r.correct("chat", 100, []api.Message{thought}, "")
	if got := r.rate("chat"); got != 2 {
		t.Errorf("rate = %v after one prompt at 2.5, want 2 until another comes to more", got)
	}
	r.correct("chat", 60, []api.Message{ofWords(api.RoleUser, 20)}, "")
	if got := r.rate("chat"); got != 2.5 {
		t.Errorf("rate = %v, want 2.5, the lower of two prompts in a row", got)
	}
}

// withPictures is a prompt of so many words and pictures.
func withPictures(words, pictures int) []api.Message {
	m := ofWords(api.RoleUser, words)
	for range pictures {
		m.Parts = append(m.Parts, api.Part{Type: api.PartImage, Data: []byte("a picture")})
	}
	return []api.Message{m}
}

// A host bills a picture by how big it is, and each host by its own reckoning.
// A prompt that carries a picture more than the one before it says what one
// costs: each count is its words and its pictures at what each costs, and two
// counts give both. What a word costs is then every count less its pictures,
// over its words, the way it is read off a prompt of words alone: what the
// words added between two prompts came to is not it.
func TestWhatAPictureCostsIsReadWhenAPromptCarriesOneMore(t *testing.T) {
	var r costs
	// A host that counts the prompt at two tokens a word, and a picture at 800.
	r.correct("eyes", 2*3000, withPictures(3000, 0), "")
	r.correct("eyes", 2*3100+800, withPictures(3100, 1), "")
	if got := r.image("eyes"); got != 800 {
		t.Errorf("a picture costs %d, want 800", got)
	}
	if got := r.rate("eyes"); got != 2 {
		t.Errorf("rate = %v, want 2", got)
	}
	if got := r.image("chat"); got != startImage {
		t.Errorf("another model's picture costs %d, want the one nothing has counted", got)
	}

	// The replies added between prompts come to four tokens a word, with what
	// they thought, and the prompts as a whole to a little over two.
	r.correct("eyes", 2*3100+800+4*50, withPictures(3150, 1), "")
	r.correct("eyes", 2*3100+800+4*100, withPictures(3200, 1), "")
	if got, want := r.rate("eyes"), float64(2*3100+4*50)/3150; got != want {
		t.Errorf("rate = %v, want %v, the lower of the last two prompts less the picture", got, want)
	}
}

// A run whose history holds a picture carries it in every prompt from the
// first, so no two counts tell a word from a picture yet. Until they do, the
// pictures count as part of the words, which puts a word high rather than low.
func TestUntilAPictureIsReadItCountsAsWords(t *testing.T) {
	var r costs
	r.correct("eyes", 2*3000+800, withPictures(3000, 1), "")
	r.correct("eyes", 2*3100+800, withPictures(3100, 1), "")
	if got, want := r.rate("eyes"), float64(2*3000+800)/3000; got != want {
		t.Errorf("rate = %v, want %v, the words and the picture over the words", got, want)
	}
	if got := r.image("eyes"); got != startImage {
		t.Errorf("a picture costs %d, want the one nothing has counted", got)
	}
}

// A prompt with fewer words than the one before has had its history compacted,
// and a summary costs more a word than the history it replaced. The two counts
// say nothing of what a picture costs.
func TestACompactedPromptSaysNothingOfWhatAPictureCosts(t *testing.T) {
	var r costs
	r.correct("eyes", 2*3000, withPictures(3000, 0), "")
	// 2,000 words at 2.2 tokens each, and a picture at 800.
	r.correct("eyes", 4400+800, withPictures(2000, 1), "")
	if got := r.image("eyes"); got != startImage {
		t.Errorf("a picture costs %d, want the one nothing has counted", got)
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

func TestTheCountOfAReplysPromptSetsWhatAWordCosts(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: counting("ok", 4000)}
	r := openReply(t, f)
	r.say(t, "hey")

	var words int
	for _, m := range f.asked().Messages {
		words += len(strings.Fields(text(m)))
	}
	if words == 0 {
		t.Fatal("the prompt was sent with no text in it")
	}
	if want, got := 4000/float64(words), r.costs.rate("chat"); got != want {
		t.Errorf("rate = %v, want %v: what the host counted over what she sent", got, want)
	}
}

// The rounds after a reply's first carry the calls it made and what they
// answered, which the history never does, so the count of one says nothing of
// what a word of the history costs, even when two of them in a row come to
// far more.
func TestOnlyTheFirstRoundOfAReplySaysWhatAWordCosts(t *testing.T) {
	rounds := answering(
		round{calls: []api.ToolCall{lookup("call_1", `{"query":"Ana"}`)}},
		round{calls: []api.ToolCall{lookup("call_2", `{"query":"Lisbon"}`)}},
		round{text: "Ana lives in Lisbon, you told me"},
	)
	counted := func(ctx context.Context, req api.ChatRequest, fn func(api.Chunk) error) (*api.Result, error) {
		res, err := rounds(ctx, req, fn)
		if res != nil {
			res.Usage.PromptTokens = 4000
			if last := req.Messages[len(req.Messages)-1]; last.Role == api.RoleTool {
				res.Usage.PromptTokens = 1000000
			}
		}
		return res, err
	}
	look := &fakeTool{name: "search_memories", answer: "Ana lives in Lisbon"}
	f := &fakeRunner{model: chatModel(), chat: counted}
	r := openReplyOffering(t, f, setup(f), config.DefaultEngine(), look)
	r.say(t, "where does Ana live?")

	requests := f.all()
	if len(requests) != 3 {
		t.Fatalf("%d rounds, want the two that asked and the one that answered", len(requests))
	}
	words := len(strings.Fields(r.tools.text))
	for _, m := range requests[0].Messages {
		words += len(strings.Fields(text(m)))
	}
	if want, got := 4000/float64(words), r.costs.rate("chat"); got != want {
		t.Errorf("rate = %v, want %v: what the host counted the first round at", got, want)
	}
}

// The summary and the history are reserved their ratios of what the context
// leaves once the persona, what she is told after it, and the tools are
// written, counted at three tokens a word until a host has counted a prompt of
// the model.
func TestTheReservationsAreTheirRatiosOfWhatThePersonaAndToolsLeave(t *testing.T) {
	ctx := context.Background()
	look := &fakeTool{name: "search_memories", answer: "found"}
	f := &fakeRunner{model: chatModel(), chat: says("ok")}
	r := openReplyOffering(t, f, sized(f, 10000), config.DefaultEngine(), look)
	m, err := r.roleModel(ctx, config.RoleChat)
	if err != nil {
		t.Fatal(err)
	}

	rendered, err := fullCard().Render()
	if err != nil {
		t.Fatal(err)
	}
	offered, err := json.Marshal([]api.ToolDef{api.ToolDef(look.Definition())})
	if err != nil {
		t.Fatal(err)
	}
	written := len(strings.Fields(rendered)) + len(strings.Fields(plain)) + len(strings.Fields(string(offered)))
	fixed := int(math.Ceil(float64(written) * 3))
	flexible := float64(10000 - fixed)

	summary, history := r.reservations(m)
	if want := int(math.Floor(flexible * 0.3)); summary != want {
		t.Errorf("the summary is reserved %d, want %d", summary, want)
	}
	if want := int(math.Floor(flexible * 0.6)); history != want {
		t.Errorf("the history is reserved %d, want %d", history, want)
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

// countedAtThree answers with chat, and has the prompt of every reply counted
// at three tokens a word, the way a host counts what it was sent.
func countedAtThree(chat chatFunc) chatFunc {
	return func(ctx context.Context, req api.ChatRequest, fn func(api.Chunk) error) (*api.Result, error) {
		res, err := chat(ctx, req, fn)
		if res != nil && purpose(req) == store.PurposeReply {
			var words int
			for _, m := range req.Messages {
				for _, p := range m.Parts {
					words += len(strings.Fields(p.Text))
				}
			}
			res.Usage.PromptTokens = 3 * words
		}
		return res, err
	}
}

// talkedAndCounted is a conversation held to 20,000 tokens, of five exchanges
// of 400 words each, whose prompts a host has counted: a history within its
// reservation, with room left for a message of about a thousand words.
func talkedAndCounted(t *testing.T) (*fakeRunner, *replyEngine) {
	t.Helper()
	f := &fakeRunner{model: chatModel(), chat: countedAtThree(compacting("hm", "they said things"))}
	r := openReplyWith(t, f, sized(f, 20000))
	for i := range 5 {
		r.say(t, fmt.Sprintf("message %d: %s", i, manyWords(400)))
		settle(t, r)
	}
	if _, err := r.store.LatestSummary(context.Background()); err == nil {
		t.Fatal("the history was compacted, want it within its reservation")
	}
	return f, r
}

// A prompt past the context is one the host refuses. A turn that would send
// one waits for the history to be compacted to make room for its messages,
// and goes out with the summary in place of the history.
func TestATurnPastTheContextWaitsForTheHistoryToMakeRoom(t *testing.T) {
	f, r := talkedAndCounted(t)

	long := "and this: " + manyWords(5000)
	r.say(t, long)
	if seen(r.Engine, ReplyFailed) {
		t.Error("the turn failed, want it answered once the history made room")
	}
	req := f.replied()
	if got := said(req, api.RoleUser); len(got) != 1 || got[0] != long {
		t.Errorf("the turn sent %d messages of its own, want only the long one", len(got))
	}
	if card := text(req.Messages[0]); !strings.Contains(card, "they said things") {
		t.Errorf("the turn went out with %q, want the summary in it", card)
	}
}

// A turn whose messages are past the context once the history has made room
// fails, saying why, and sends nothing. The room the history makes is what
// the summary leaves, which here fills its reservation, as one written to
// does.
func TestATurnPastTheContextOnceTheHistoryMadeRoomFails(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: countedAtThree(keeping)}
	r := openReplyWith(t, f, sized(f, 20000))
	for i := range 5 {
		r.say(t, fmt.Sprintf("message %d: %s", i, manyWords(400)))
		settle(t, r)
	}
	replies := len(f.sentFor(store.PurposeReply))

	r.say(t, "and this: "+manyWords(5000))
	if n := len(f.sentFor(store.PurposeReply)); n != replies {
		t.Errorf("%d replies went out past the context, want none", n-replies)
	}
	if _, err := r.store.LatestSummary(context.Background()); err != nil {
		t.Errorf("the history made no room before the turn failed: %v", err)
	}
	var failure string
	for _, ev := range published(r.Engine) {
		if ev.Kind == ReplyFailed {
			failure = ev.Text
		}
	}
	if !strings.Contains(failure, "context") {
		t.Errorf("the failure was said as %q, want it to say the prompt is past the context", failure)
	}
}

// A turn whose prompt is past the context needs the room a compaction makes,
// so when that compaction fails the turn is dropped and the failure is said
// the way a failed reply is: sent as it is, its prompt would be refused again.
func TestATurnThatNeedsRoomACompactionCannotMakeIsDropped(t *testing.T) {
	f, r := talkedAndCounted(t)
	f.chat = countedAtThree(func(ctx context.Context, req api.ChatRequest, fn func(api.Chunk) error) (*api.Result, error) {
		if purpose(req) == store.PurposeSummary {
			return nil, errors.New("the host is away")
		}
		return compacting("hm", "they said things")(ctx, req, fn)
	})
	replies := len(f.sentFor(store.PurposeReply))

	r.say(t, "and this: "+manyWords(5000))
	if n := len(f.sentFor(store.PurposeReply)); n != replies {
		t.Errorf("%d replies went out past the context, want none", n-replies)
	}
	var failure string
	for _, ev := range published(r.Engine) {
		if ev.Kind == ReplyFailed {
			failure = ev.Text
		}
	}
	if !strings.Contains(failure, "the host is away") {
		t.Errorf("the failure was said as %q, want the host's error", failure)
	}
}

// Until a host has counted a prompt of the model, a word counts high on
// purpose, so a prompt that count puts past the context goes out all the
// same: held back, it would never be counted.
func TestAPromptGoesOutUntilAHostHasCountedOne(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: compacting("hm", "they said things")}
	r := openReplyWith(t, f, sized(f, 20000))

	r.say(t, "hello: "+manyWords(7000))
	if n := len(f.sentFor(store.PurposeReply)); n != 1 || seen(r.Engine, ReplyFailed) {
		t.Errorf("%d replies went out, and one failed: %v, want the one", n, seen(r.Engine, ReplyFailed))
	}
}

// A message the model's context cannot hold even once the history is
// compacted away could never be answered, and stored it would fail every turn
// after it: it is refused as it is sent, and nothing of it is stored. It is
// measured at a token a word, the least a word comes to, until a host has
// counted a prompt of the model, and at what the host counted after.
func TestAMessageTooLongForTheContextIsRefusedAsItIsSent(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: countedAtThree(compacting("ok", "they said things"))}
	r := openReplyWith(t, f, sized(f, 2000))
	ctx := context.Background()

	err := r.Post(ctx, NewMessage{Channel: "repl", Text: "read this: " + manyWords(2000)})
	if err == nil || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("a message of more words than the context = %v, want it refused", err)
	}
	if history, _ := r.History(ctx, 0, 10); len(history) != 0 {
		t.Errorf("history = %+v, want nothing of the message stored", history)
	}

	// Before a host has counted, a message that fits at a token a word goes
	// out. The host counts three a word, so one that fit at one no longer does.
	r.say(t, "hello: "+manyWords(1000))
	if seen(r.Engine, ReplyFailed) {
		t.Fatal("a message that fits at a token a word failed")
	}
	err = r.Post(ctx, NewMessage{Channel: "repl", Text: "and this: " + manyWords(1000)})
	if err == nil || !strings.Contains(err.Error(), "too long") {
		t.Errorf("a message past the context at what the host counted = %v, want it refused", err)
	}
}

// refusingPast answers with chat, and refuses a prompt of more words than the
// context holds, the way a host refuses one past its context, at a token a
// word, the least a word comes to.
func refusingPast(limit int, chat chatFunc) chatFunc {
	return func(ctx context.Context, req api.ChatRequest, fn func(api.Chunk) error) (*api.Result, error) {
		var words int
		for _, m := range req.Messages {
			words += len(strings.Fields(text(m)))
		}
		if words > limit {
			return nil, &api.APIError{Status: 400, Message: "the prompt is past the context"}
		}
		return chat(ctx, req, fn)
	}
}

// A prompt the host refused went out uncounted, so nothing held it back, and
// nothing counts it either. The next turn measures the history first, and
// goes out with the summary in place of what the host refused.
func TestAPromptTheHostRefusedIsCompactedBeforeTheNextTurn(t *testing.T) {
	ctx := context.Background()
	big := &fakeRunner{model: chatModel(), chat: compacting("hm", "they said things")}
	small := &fakeRunner{
		model: api.Model{ID: "small/model", Context: 2000, Chat: true, Tools: true},
		chat:  refusingPast(2000, compacting("ok", "they said things")),
	}
	set := setup(big)
	set.Runners = append(set.Runners, small)
	set.Models = append(set.Models, &runners.Configured{Name: "small", ID: small.model.ID, Runner: small})
	r := openReplyWith(t, big, set)
	for i := range 3 {
		r.say(t, fmt.Sprintf("message %d: %s", i, manyWords(800)))
	}

	// A history the big model held is past the small one's context.
	if err := r.SetModel(ctx, config.RoleChat, "small"); err != nil {
		t.Fatal(err)
	}
	r.say(t, "and now?")
	if !seen(r.Engine, ReplyFailed) {
		t.Fatal("the turn was answered, want it refused by the host")
	}

	r.say(t, "still there?")
	var failed int
	for _, ev := range published(r.Engine) {
		if ev.Kind == ReplyFailed {
			failed++
		}
	}
	if failed != 1 {
		t.Errorf("%d turns failed, want only the one the host refused", failed)
	}
	if _, err := r.store.LatestSummary(ctx); err != nil {
		t.Errorf("the history was not compacted before the next turn: %v", err)
	}
	if card := text(small.replied().Messages[0]); !strings.Contains(card, "they said things") {
		t.Errorf("the turn went out with %q, want the summary in it", card)
	}
}

// A model whose context the card and the tools fill on their own is refused
// when the conversation opens, named by its key. Nothing has been counted
// then, so they are measured at a token a word, the least a word comes to: a
// context that holds them at that count is taken, however much more a host
// counts them at.
func TestAModelTheCardFillsIsRefusedWhenTheConversationOpens(t *testing.T) {
	rendered, err := fullCard().Render()
	if err != nil {
		t.Fatal(err)
	}
	words := len(strings.Fields(rendered))
	open := func(tokens int) error {
		t.Helper()
		st, err := store.Open(filepath.Join(t.TempDir(), "data"))
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		f := &fakeRunner{model: chatModel(), chat: says("ok")}
		set := sized(f, tokens)
		set.Models[0].Path = "models.chat"
		e, err := Open(context.Background(), Options{Store: st, Runners: set, Persona: fullCard(), Engine: config.DefaultEngine()})
		if err == nil {
			e.Close()
		}
		return err
	}
	if err := open(words); err == nil || !strings.Contains(err.Error(), "models.chat") {
		t.Errorf("a context of %d tokens for a card of %d words = %v, want it refused under the model's key", words, words, err)
	}
	if err := open(2 * words); err != nil {
		t.Errorf("a context of %d tokens for a card of %d words = %v, want it taken", 2*words, words, err)
	}
}
