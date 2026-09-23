package conversation

import (
	"context"
	"fmt"
	"reflect"
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

// A picture weighs what a host bills for it, which is what a prompt carries
// and so what a fold is deciding about. One that measured only the words would
// find nothing worth folding while the prompt was already too full for them.
func TestAFoldWeighsThePicturesAnExchangeCarries(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: folding("hm", "Caio sent a picture", "they looked at pictures")}
	f.model.Vision = true
	r := openReplyWith(t, f, sized(f, 3000))
	ctx := context.Background()

	// Three short messages, each carrying a picture. Their words come to
	// almost nothing; the pictures come to more than the messages' share.
	for _, said := range []string{"one", "two", "three"} {
		sendPhoto(t, r, said, photo(t))
	}

	waitFor(t, "the conversation to be folded", func() bool {
		s, err := r.store.LatestSummary(ctx)
		return err == nil && s != nil
	})
	summary, err := r.store.LatestSummary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Content != "they looked at pictures" {
		t.Errorf("summary = %q", summary.Content)
	}
}

func TestAConversationPastItsShareIsFolded(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: folding("hm", "Caio has a sister", "they said things")}
	r := openReplyWith(t, f, sized(f, 2000))
	ctx := context.Background()

	summary := talkPast(t, r)
	if summary.Content != "they said things" {
		t.Errorf("summary = %q", summary.Content)
	}
	if summary.UptoMessageID == 0 {
		t.Errorf("summary = %+v, want the messages it covers", summary)
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
	told := text(req.Messages[len(req.Messages)-3])
	if !strings.Contains(told, "- (said on ") || !strings.Contains(told, "Caio has a sister") {
		t.Errorf("what she remembers is told as %q, want the memory in it", told)
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
		UptoMessageID: messages[0].ID, Content: long,
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

	talkPast(t, r)

	// The entry of a fold names no message, so what the conversation has been
	// answered up to is what the replies say and nothing else. The fold is the
	// last thing that ran, so its entry is the newest.
	entries, err := r.store.Entries(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("nothing was written down of the fold")
	}
	if entry := entries[0]; entry.UptoMessageID != 0 || entry.AfterMessageID != 0 {
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

// A fold runs beside a reply, so the exchange being answered is still
// arriving: one folded away while it was would be summarised as a message that
// went unanswered, and the reply would land with nothing before it.
func TestAFoldLeavesWhatIsStillBeingAnswered(t *testing.T) {
	groups := [][]store.Message{
		{spoken(1, store.RoleUser), spoken(2, store.RoleAssistant)},
		{spoken(3, store.RoleUser), spoken(4, store.RoleAssistant)},
		{spoken(5, store.RoleUser)},
	}
	if got := foldable(groups, 3, 3); got != 2 {
		t.Errorf("foldable = %d, want the two that were answered", got)
	}
	// The reply to the second is still being written, so only the first may go.
	if got := foldable(groups, 1, 3); got != 1 {
		t.Errorf("foldable = %d, want only the one that was answered", got)
	}
}

// A message sent while a reply was being written is stored before that reply,
// so the two exchanges hold ids that interleave. Cutting between them would
// put a reply on the far side of the summary from the message it answers.
func TestAFoldCutsWhereTheIdsDo(t *testing.T) {
	// Message 3 answered by 5, message 4 answered by 6.
	groups := [][]store.Message{
		{spoken(1, store.RoleUser), spoken(2, store.RoleAssistant)},
		{spoken(3, store.RoleUser), spoken(5, store.RoleAssistant)},
		{spoken(4, store.RoleUser), spoken(6, store.RoleAssistant)},
	}
	// The sizes say to take the two oldest; the ids say only the first may go.
	if got := foldable(groups, 6, 2); got != 1 {
		t.Errorf("foldable = %d, want the cut before the ids interleave", got)
	}
	// Taking the interleaved pair whole is a cut the ids do fall apart at.
	if got := foldable(groups, 6, 3); got != 3 {
		t.Errorf("foldable = %d, want both of them when both may go", got)
	}
}

// A fold runs beside a reply, so it reads a conversation whose newest
// exchanges are still being answered. Taking one of those would write a
// summary saying a message went unanswered, and leave the reply that is about
// to be stored with nothing before it.
func TestAFoldOverAConversationStillBeingAnswered(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: folding("hm", "Caio said something", "they talked")}
	r := openReplyWith(t, f, sized(f, 2000))
	ctx := context.Background()
	// Short enough that two exchanges fit one step, long enough that three of
	// them are past what a fold leaves behind.
	long := strings.Repeat("a long thing to say ", 35)

	// One exchange that was answered, then one whose reply is written but
	// which no entry covers, then the newest message of all.
	answered := add(t, r, store.RoleUser, long)
	add(t, r, store.RoleAssistant, long)
	ended(t, r, answered)
	add(t, r, store.RoleUser, long)
	add(t, r, store.RoleAssistant, long)
	add(t, r, store.RoleUser, long)

	if _, err := r.foldStep(ctx); err != nil {
		t.Fatal(err)
	}
	summary, err := r.store.LatestSummary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.UptoMessageID != 2 {
		t.Errorf("the summary covers up to %d, want only the exchange that was answered",
			summary.UptoMessageID)
	}
}

// add stores one message of the conversation, and answers with its number.
func add(t *testing.T, r *replyEngine, role, text string) store.MessageID {
	t.Helper()
	m := &store.Message{Role: role, Channel: "repl", CreatedAt: r.clock.Now(),
		Parts: []store.Part{{Type: store.PartText, Text: text}}}
	if err := r.store.AddMessage(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	return m.ID
}

// ended writes down an entry that answered up to a message, which is what says
// the conversation has been answered that far.
func ended(t *testing.T, r *replyEngine, upto store.MessageID) {
	t.Helper()
	ctx := context.Background()
	entry := &store.Entry{Channel: "repl", UptoMessageID: upto, StartedAt: r.clock.Now()}
	if err := r.store.StartEntry(ctx, entry); err != nil {
		t.Fatal(err)
	}
	entry.Status, entry.EndedAt = store.StatusDone, r.clock.Now()
	if err := r.store.EndEntry(ctx, entry); err != nil {
		t.Fatal(err)
	}
}

func spoken(id store.MessageID, role string) store.Message {
	return store.Message{ID: id, Role: role,
		Parts: []store.Part{{Type: store.PartText, Text: "something"}}}
}

// A model told to reply with an object often writes it inside a code fence,
// the way it would to a person. A fold that could not read that would stop for
// the rest of the run.
func TestJSONInsideAFenceIsRead(t *testing.T) {
	for _, c := range []struct{ what, text, want string }{
		{"bare", `{"memories": []}`, `{"memories": []}`},
		{"fenced", "```\n{\"memories\": []}\n```", `{"memories": []}`},
		{"fenced as json", "```json\n{\"memories\": []}\n```", `{"memories": []}`},
		{"fenced with words after", "```json\n{\"a\": 1}\n```\nthat is all", `{"a": 1}`},
		{"an opening fence alone", "```json", "```json"},
	} {
		if got := unfenced(c.text); got != c.want {
			t.Errorf("%s = %q, want %q", c.what, got, c.want)
		}
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

// What she remembers changes whenever she keeps or forgets something, so it is
// told just before the message she is answering rather than beside the card:
// a memory kept between two replies leaves the second prompt what the first
// was up to where its memories stood, which is everything but its newest.
func TestWhatSheRemembersIsToldBeforeTheMessageSheIsAnswering(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: says("hello")}
	r := openReply(t, f)
	ctx := context.Background()
	keep := func(content string) {
		t.Helper()
		first, err := r.store.MessagesAfter(ctx, 0)
		if err != nil || len(first) == 0 {
			t.Fatalf("messages = %+v, %v", first, err)
		}
		if err := r.store.Remember(ctx, &store.Memory{Content: content, Source: first[0].ID}); err != nil {
			t.Fatal(err)
		}
	}

	r.say(t, "hey")
	keep("Caio's sister is Ana.")
	r.say(t, "you there?")
	keep("Caio cooks on Saturdays.")
	r.say(t, "hello??")

	requests := f.all()
	before, now := requests[1].Messages, requests[2].Messages
	for i, prompt := range [][]api.Message{before, now} {
		n := len(prompt)
		if strings.Contains(text(prompt[0]), "Caio's sister is Ana.") {
			t.Errorf("prompt %d tells the memories beside the card", i+2)
		}
		if !strings.Contains(text(prompt[n-3]), "Caio's sister is Ana.") ||
			!strings.HasPrefix(text(prompt[n-2]), "It is now ") || prompt[n-1].Role != api.RoleUser {
			t.Errorf("prompt %d ends with %q, %q, %q; want the memories, the time, then the message",
				i+2, text(prompt[n-3]), text(prompt[n-2]), text(prompt[n-1]))
		}
	}
	if !strings.Contains(text(now[len(now)-3]), "Caio cooks on Saturdays.") {
		t.Errorf("the memory kept since is not told: %q", text(now[len(now)-3]))
	}
	// Everything before where the first prompt told its memories stands, as
	// it was sent.
	kept := len(before) - 3
	if !reflect.DeepEqual(now[:kept], before[:kept]) {
		t.Errorf("the second prompt changed what the first sent before its memories")
	}
}

// A request says how much of it the next reply sends again as it was sent: all
// of it up to what she remembers and the time of the message she is answering,
// whether or not she remembers anything yet. The first reply has only the card
// to say it of.
func TestAReplySaysHowMuchOfItsPromptTheNextSendsAgain(t *testing.T) {
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
	r.say(t, "hello??")

	requests := f.all()
	if len(requests) != 3 {
		t.Fatalf("%d requests were sent, want three", len(requests))
	}
	if n := requests[0].Standing; n != 1 {
		t.Errorf("the first reply says %d of its messages stand, want the card alone", n)
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
		if reflect.DeepEqual(next.Messages[n], was.Messages[n]) {
			t.Errorf("reply %d says %d of its messages stand, and the next sends %q again after them",
				i+1, n, text(was.Messages[n]))
		}
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
	told := e.remembering([]store.Memory{
		{Content: "Caio's sister is Ana", SaidAt: said},
		{Content: "Caio cooks on Saturdays", SaidAt: said},
	}, 0, 1.0/3.5)
	if len(told) != 1 {
		t.Fatalf("what she remembers is %d messages, want one", len(told))
	}
	for _, want := range []string{"Caio's sister is Ana", "Caio cooks on Saturdays"} {
		if !strings.Contains(text(told[0]), want) {
			t.Errorf("what she remembers is %q, want %q in it", text(told[0]), want)
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
