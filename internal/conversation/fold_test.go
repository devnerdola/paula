package conversation

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/runners/api"
	"nerdola.dev/x/paula/internal/store"
)

// shown is the first message number a fold request lists, which is how a model
// reads which messages it is being asked about.
var shown = regexp.MustCompile(`#(\d+) `)

func firstShown(req api.ChatRequest) int64 {
	for _, m := range req.Messages {
		if found := shown.FindStringSubmatch(text(m)); found != nil {
			n, _ := strconv.ParseInt(found[1], 10, 64)
			return n
		}
	}
	return 0
}

// purpose is what a request was sent for, which its cache key carries.
func purpose(req api.ChatRequest) string {
	_, out, _ := strings.Cut(strings.TrimPrefix(req.CacheKey, "paula-"), "-")
	return out
}

// folding answers a reply with text, and the requests of a fold the way a
// model that followed the prompt would: a memory of the oldest message it is
// shown, and a summary.
func folding(reply, memory, summary string) func(context.Context, api.ChatRequest, func(api.Chunk) error) (*api.Result, error) {
	return func(_ context.Context, req api.ChatRequest, fn func(api.Chunk) error) (*api.Result, error) {
		out := reply
		switch purpose(req) {
		case store.PurposeMemories:
			out = fmt.Sprintf(`{"memories":[{"content":%q,"message":%d}]}`, memory, firstShown(req))
		case store.PurposeSummary:
			out = summary
		}
		if err := fn(api.Chunk{Kind: api.ChunkText, Text: out}); err != nil {
			return nil, err
		}
		return &api.Result{FinishReason: "stop"}, nil
	}
}

// talkPast says enough to take the conversation past the share of the context
// the messages have, and waits for the fold that follows.
func talkPast(t *testing.T, r *replyEngine) *store.Summary {
	t.Helper()
	long := strings.Repeat("a long thing to say ", 20)
	for i := range 8 {
		r.say(t, fmt.Sprintf("message %d: %s", i, long))
	}
	var out *store.Summary
	waitFor(t, "the conversation to be folded", func() bool {
		s, err := r.store.LatestSummary(context.Background())
		out = s
		return err == nil && s != nil
	})
	return out
}

func TestAConversationPastItsShareIsFolded(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: folding("hm", "Caio has a sister", "they said things")}
	r := openReplyWith(t, f, sized(f, 2000))
	ctx := context.Background()

	summary := talkPast(t, r)
	if summary.Content != "they said things" {
		t.Errorf("summary = %q", summary.Content)
	}
	if summary.UptoMessageID == 0 || summary.EntryID == 0 {
		t.Errorf("summary = %+v, want the messages it covers and the entry that wrote it", summary)
	}

	memories, err := r.store.Memories(ctx)
	if err != nil || len(memories) == 0 {
		t.Fatalf("memories = %+v, %v", memories, err)
	}
	if memories[0].Content != "Caio has a sister" || memories[0].Source == 0 {
		t.Errorf("memory = %+v", memories[0])
	}

	// The next reply is told what the fold wrote, and carries only the
	// messages the summary leaves behind.
	r.say(t, "and then?")
	req := f.replied()
	card := text(req.Messages[0])
	if !strings.Contains(card, "Earlier in your conversation with Caio:\nthey said things") {
		t.Errorf("the system message is %q, want the summary in it", card)
	}
	if !strings.Contains(card, "- (said on ") || !strings.Contains(card, "Caio has a sister") {
		t.Errorf("the system message is %q, want the memory in it", card)
	}
	for _, said := range said(req, api.RoleUser) {
		if strings.HasPrefix(said, "message 0:") {
			t.Error("a message the summary covers is still sent on its own")
		}
	}
}

func TestAPromptThatLeftSomethingOutIsFolded(t *testing.T) {
	// A context barely wider than the card: the prompt drops what will not fit
	// long before the messages it carries reach their share, so what it
	// carried never says a fold is due. What it left out does.
	rendered, err := fullCard().Render()
	if err != nil {
		t.Fatal(err)
	}
	card := size([]api.Message{api.Text(api.RoleSystem, rendered)}, startRatio, 0)

	f := &fakeRunner{model: chatModel(), chat: folding("hm", "Caio has a sister", "they said things")}
	r := openReplyWith(t, f, sized(f, 2*card))

	// Short exchanges, so none of them is a share of its own: what the prompt
	// carries stops at the context and never passes the share, however many
	// more it has to leave out.
	for i := range 10 {
		r.say(t, fmt.Sprintf("message %d", i))
	}
	waitFor(t, "the conversation to be folded", func() bool {
		s, err := r.store.LatestSummary(context.Background())
		return err == nil && s != nil
	})
}

// growing answers a fold with a summary of its own, and a compaction with
// again, so a test can say how long each of the two writes is.
func growing(reply, summary, again string) func(context.Context, api.ChatRequest, func(api.Chunk) error) (*api.Result, error) {
	return func(_ context.Context, req api.ChatRequest, fn func(api.Chunk) error) (*api.Result, error) {
		out := reply
		switch purpose(req) {
		case store.PurposeMemories:
			out = `{"memories":[]}`
		case store.PurposeSummary:
			out = summary
		case store.PurposeCompaction:
			out = again
		}
		if err := fn(api.Chunk{Kind: api.ChunkText, Text: out}); err != nil {
			return nil, err
		}
		return &api.Result{FinishReason: "stop"}, nil
	}
}

func TestASummaryPastItsRoomIsWrittenAgain(t *testing.T) {
	// A summary far longer than the system message has room for, so the fold
	// that writes it leaves it over its room.
	long := "they talked about " + strings.Repeat("all sorts of things and ", 300)
	f := &fakeRunner{model: chatModel(), chat: growing("hm", long, "they talked.")}
	r := openReplyWith(t, f, sized(f, 2000))
	ctx := context.Background()

	talkPast(t, r)
	waitFor(t, "the summary to be written again", func() bool {
		s, err := r.store.LatestSummary(ctx)
		return err == nil && s != nil && s.Content == "they talked."
	})

	// It was written again from itself, with nothing added, and it still
	// covers the messages the one before it covered.
	written := f.sentFor(store.PurposeCompaction)
	if len(written) == 0 {
		t.Fatal("the summary was never written again")
	}
	last := written[len(written)-1]
	said := text(mine(last))
	if !strings.Contains(said, "Summary so far:") {
		t.Errorf("the request said %q, want the summary it is written from", said)
	}
	if strings.Contains(said, "Messages to add:") {
		t.Errorf("the request said %q, want nothing added to it", said)
	}
	// It asks by a prompt of its own: the one a fold uses names messages that
	// a compaction is never given.
	asked := text(last.Messages[0])
	if !strings.Contains(asked, "from the summary alone") {
		t.Errorf("the prompt was %q, want the one for a summary written from itself", asked)
	}
	if strings.Contains(asked, "messages to add") {
		t.Errorf("the prompt was %q, want one that asks for no messages", asked)
	}
}

func TestASummaryWrittenAgainFoldsNothingThatFits(t *testing.T) {
	long := "they talked about " + strings.Repeat("all sorts of things and ", 300)
	f := &fakeRunner{model: chatModel(), chat: growing("hm", long, "they talked.")}
	r := openReplyWith(t, f, sized(f, 2000))
	ctx := context.Background()

	// Enough said to sit between what a fold leaves behind and what makes one
	// due: a fold takes exchanges from anything above the first, and only the
	// second says one is due at all.
	said := strings.Repeat("a long thing to say ", 20)
	for i := range 6 {
		r.say(t, fmt.Sprintf("message %d: %s", i, said))
	}
	if len(f.sentFor(store.PurposeMemories)) != 0 {
		t.Fatal("the messages were folded before the summary was there to be written again")
	}

	// A summary far past its room, covering the oldest message alone, so the
	// messages left are inside their share while it is not inside its room.
	messages, err := r.store.Messages(ctx, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	err = r.store.Fold(ctx, &store.Summary{
		UptoMessageID: messages[0].ID, Content: long, CreatedAt: r.clock.Now(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	r.say(t, "and then?")
	waitFor(t, "the summary to be written again", func() bool {
		return len(f.sentFor(store.PurposeCompaction)) > 0
	})
	// Folding here would take exchanges the prompt can still carry and add
	// them to the very summary that has outgrown its room.
	if got := f.sentFor(store.PurposeMemories); len(got) != 0 {
		t.Errorf("%d exchanges were folded, want none: only the summary was past what it has", len(got))
	}
}

func TestASummaryThatComesBackNoShorterIsDropped(t *testing.T) {
	// Written again, the summary comes back longer than it was: storing it
	// would leave the conversation carrying more than before. It is stored as
	// the model wrote it, less the space around it, so the one it is held
	// against is trimmed too.
	long := strings.TrimSpace("they talked about " + strings.Repeat("all sorts of things and ", 300))
	f := &fakeRunner{model: chatModel(), chat: growing("hm", long, long+" and then some more")}
	r := openReplyWith(t, f, sized(f, 2000))
	ctx := context.Background()

	talkPast(t, r)
	waitFor(t, "the summary to be written again", func() bool {
		return len(f.sentFor(store.PurposeCompaction)) > 0
	})
	waitFor(t, "the work to be taken as a failure", func() bool {
		return strings.Contains(r.log.String(), "keeping the conversation inside the context")
	})

	summary, err := r.store.LatestSummary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Content != long {
		t.Errorf("the summary is %d characters, want the %d it was written from",
			len(summary.Content), len(long))
	}
}

func TestAFoldAnswersNoMessage(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: folding("hm", "Caio has a sister", "they said things")}
	r := openReplyWith(t, f, sized(f, 2000))
	ctx := context.Background()

	summary := talkPast(t, r)

	// The entry of a fold names no message, so what the conversation has been
	// answered up to is what the replies say and nothing else.
	entry, err := r.store.Entry(ctx, summary.EntryID)
	if err != nil {
		t.Fatal(err)
	}
	if entry.UptoMessageID != 0 || entry.AfterMessageID != 0 {
		t.Errorf("the fold's entry = %+v, want one that answers nothing", entry)
	}
	answered, err := r.store.AnsweredUpto(ctx)
	if err != nil {
		t.Fatal(err)
	}
	reply, err := r.store.LastMessage(ctx, store.RoleAssistant)
	if err != nil {
		t.Fatal(err)
	}
	if answered >= reply.ID {
		t.Errorf("answered up to %d, want a message older than the last reply %d", answered, reply.ID)
	}
}

func TestAMemoryOfAMessageThatWasNotShownFailsTheFold(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: folding("hm", "", "")}
	f.chat = func(_ context.Context, req api.ChatRequest, fn func(api.Chunk) error) (*api.Result, error) {
		out := "hm"
		switch purpose(req) {
		case store.PurposeMemories:
			out = `{"memories":[{"content":"about nothing","message":99999}]}`
		case store.PurposeSummary:
			out = "they said things"
		}
		if err := fn(api.Chunk{Kind: api.ChunkText, Text: out}); err != nil {
			return nil, err
		}
		return &api.Result{FinishReason: "stop"}, nil
	}
	r := openReplyWith(t, f, sized(f, 2000))
	ctx := context.Background()

	long := strings.Repeat("a long thing to say ", 20)
	for i := range 8 {
		r.say(t, fmt.Sprintf("message %d: %s", i, long))
	}
	waitFor(t, "the fold to fail", func() bool {
		return strings.Contains(r.log.String(), "keeping the conversation inside the context")
	})

	// Nothing of a step that failed is kept: a summary covering messages whose
	// memories were lost would lose them for good.
	if _, err := r.store.LatestSummary(ctx); err == nil {
		t.Error("a summary was stored beside a memory that names no message")
	}
	memories, err := r.store.Memories(ctx)
	if err != nil || len(memories) != 0 {
		t.Errorf("memories = %+v, %v", memories, err)
	}
}

func TestAFoldTakesTheOldestAndLeavesTheNewest(t *testing.T) {
	// Five exchanges of a hundred each, keeping two hundred: the three oldest
	// go and the two newest stay.
	if got := chunkOf([]int{100, 100, 100, 100, 100}, 200, 1000); got != 3 {
		t.Errorf("chunk = %d, want the three oldest", got)
	}
	// The newest is never taken, whatever it takes.
	if got := chunkOf([]int{500}, 10, 1000); got != 0 {
		t.Errorf("chunk = %d, want the one she is answering left alone", got)
	}
	// A step carries no more than one prompt's worth, so a long exchange waits
	// for a step of its own.
	if got := chunkOf([]int{100, 900, 100, 100}, 50, 500); got != 1 {
		t.Errorf("chunk = %d, want only what fits the step", got)
	}
	// Nothing to do when what is there is already what a fold would leave.
	if got := chunkOf([]int{100, 100}, 500, 1000); got != 0 {
		t.Errorf("chunk = %d, want nothing folded", got)
	}
	// An exchange longer than a step may carry is taken anyway when it is the
	// oldest: leaving it would stop every fold behind it for good.
	if got := chunkOf([]int{900, 100, 100}, 50, 500); got != 1 {
		t.Errorf("chunk = %d, want the oldest taken whatever it takes", got)
	}
}

func TestASummaryCoversNothingLeftBehind(t *testing.T) {
	// A message sent while a reply was being written is stored before that
	// reply, so the chunk ends on an id newer than one left behind: a summary
	// that took it would cover a message no fold ever read.
	taken := []store.Message{
		{ID: 1, Role: store.RoleUser},
		{ID: 3, Role: store.RoleAssistant, ReplyTo: 1},
	}
	left := []store.Message{{ID: 2, Role: store.RoleUser}}
	if got := coversUpto(taken, left); got != 1 {
		t.Errorf("covered up to %d, want the newest older than everything left", got)
	}

	// Nothing arrived out of order, so it is the last of the chunk.
	taken = []store.Message{
		{ID: 1, Role: store.RoleUser},
		{ID: 2, Role: store.RoleAssistant, ReplyTo: 1},
	}
	if got := coversUpto(taken, []store.Message{{ID: 3, Role: store.RoleUser}}); got != 2 {
		t.Errorf("covered up to %d, want the last of the chunk", got)
	}
}

func TestACardThatFillsItsShareIsSaidAtStartup(t *testing.T) {
	rendered, err := fullCard().Render()
	if err != nil {
		t.Fatal(err)
	}
	card := size([]api.Message{api.Text(api.RoleSystem, rendered)}, startRatio, 0)
	f := &fakeRunner{model: chatModel(), chat: says("hey")}

	// The share of the context the system message has is the card itself, so
	// nothing is left for the memories or the summary.
	tight := openReplyWith(t, f, sized(f, 2*card))
	written := tight.log.String()
	if !strings.Contains(written, "the card leaves the system message no room") {
		t.Errorf("the log holds %q, want the card said out loud", written)
	}
	if !strings.Contains(written, "level=WARN") {
		t.Errorf("the log holds %q, want it at the level a run shows", written)
	}

	// Room enough, and nothing is said.
	roomy := openReplyWith(t, f, sized(f, 8*card))
	if written := roomy.log.String(); strings.Contains(written, "the card leaves") {
		t.Errorf("the log holds %q, want nothing said of a card that fits", written)
	}
}

func TestEveryMemoryIsToldWhenNothingBoundsThePrompt(t *testing.T) {
	cfg := config.DefaultEngine()
	// Memories may take the whole of what is left of the system message once
	// the card is written, which is the share of a room that has no size.
	cfg.MemoryRatio = 1
	e := &Engine{rendered: "You are Paula.", cfg: cfg, clock: newClock(), persona: fullCard()}
	said := time.Date(2026, 9, 14, 20, 22, 0, 0, time.UTC)

	// A model that says nothing of its context holds the system message to
	// nothing, so every memory is told whatever share memories have of it.
	got := text(e.systemMessage([]store.Memory{
		{Content: "Caio's sister is Ana", SaidAt: said},
		{Content: "Caio cooks on Saturdays", SaidAt: said},
	}, nil, 0, 1.0/3.5))
	for _, want := range []string{"Caio's sister is Ana", "Caio cooks on Saturdays"} {
		if !strings.Contains(got, want) {
			t.Errorf("the system message is %q, want %q in it", got, want)
		}
	}
}

func TestTheMemoriesAPromptTellsFitTheirRoom(t *testing.T) {
	loc := berlin
	said := time.Date(2026, 9, 14, 20, 22, 0, 0, time.UTC)
	all := []store.Memory{
		{Content: strings.Repeat("a", 70), SaidAt: said},
		{Content: strings.Repeat("b", 70), SaidAt: said},
		{Content: strings.Repeat("c", 70), SaidAt: said},
	}
	// Each line is the memory, the day it was said and the dashes around it.
	one := size([]api.Message{api.Text(api.RoleSystem, memoryLine(all[0], loc))}, 1, 0)

	if got := remembered(all, 3*one, 1, loc); len(got) != 3 {
		t.Errorf("memories told = %d, want all three", len(got))
	}
	// The newest are the ones that fit, and they are told oldest first.
	got := remembered(all, 2*one, 1, loc)
	if len(got) != 2 || got[0].Content != all[1].Content || got[1].Content != all[2].Content {
		t.Errorf("memories told = %+v, want the two newest", got)
	}
	if got := remembered(all, 0, 1, loc); len(got) != 0 {
		t.Errorf("memories told = %+v, want none", got)
	}
}
