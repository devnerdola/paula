package conversation

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"nerdola.dev/x/paula/internal/store"
)

// TestLiveCallbacks holds a call back to being scheduled, moved and cancelled
// by the chat model of the file PAULA_LIVE names with its tools, to firing as
// a message she answers when it comes due, and to firing when a run starts
// after it came due while none was on. It runs on the clock of the machine,
// so it takes a few minutes. Everything goes to a data directory of its own,
// with a report.md of what every check found.
//
//	PAULA_LIVE=live.yaml go test ./internal/conversation -run TestLiveCallbacks -v -timeout 20m
func TestLiveCallbacks(t *testing.T) {
	path := os.Getenv("PAULA_LIVE")
	if path == "" {
		t.Skip("PAULA_LIVE names no configuration file")
	}
	lv := openLive(t, path)
	defer lv.close()
	ctx := context.Background()
	clock := &pastClock{}
	clock.set(time.Now())

	lv.printf("\n## The first run")
	r := lv.run("first run", clock)
	r.send([]string{"hey, I'm starting a meeting. ping me in 2 minutes to remind me to drink some water, ok?"}, nil)
	pending := lv.pending(ctx)
	// It is moved next, so it has to be still pending.
	scheduled, found := lv.dueAbout(r.turns[0], pending, 2*time.Minute)
	scheduled = scheduled && len(pending) == 1
	lv.check("asked to ping in 2 minutes, she schedules a call back for then", scheduled,
		"one call back due about 2 minutes after the message", found, lv.ids(r.turns[0].entry)...)
	if !scheduled {
		r.end()
		lv.printf("\n%d checks passed, %d failed.", lv.passed, lv.failed)
		return
	}
	was := pending[0]

	r.send([]string{"actually make it 4 minutes, the meeting runs long"}, nil)
	pending = lv.pending(ctx)
	moved, found := lv.dueAbout(r.turns[1], pending, 4*time.Minute)
	moved = moved && len(pending) == 1 && pending[0].DueAt.After(was.DueAt)
	lv.check("asked to make it 4 minutes, she moves the call back", moved,
		"the call back due later, about 4 minutes after the message", found, lv.ids(r.turns[1].entry)...)

	r.send([]string{"never mind, I'll just drink now. cancel that reminder"}, nil)
	pending = lv.pending(ctx)
	lv.check("asked to cancel, she cancels the call back", len(pending) == 0,
		"no call back pending", callbackTexts(pending), lv.ids(r.turns[2].entry)...)

	r.send([]string{"ok one more: in 1 minute, tell me something nice, I need it before this call"}, nil)
	pending = lv.pending(ctx)
	again, found := lv.dueAbout(r.turns[3], pending, time.Minute)
	lv.check("asked for a call back in 1 minute, she schedules one", again,
		"one call back due about a minute after the message", found, lv.ids(r.turns[3].entry)...)
	if !again {
		r.end()
		lv.printf("\n%d checks passed, %d failed.", lv.passed, lv.failed)
		return
	}
	fired := lv.awaitCallback(r, 3*time.Minute)
	lv.checkCameDue(r, fired, "the call back fires when it comes due, and she answers it")

	r.send([]string{"thanks. ping me again in 1 minute, I want to see something"}, nil)
	pending = lv.pending(ctx)
	lv.check("she schedules another call back", len(pending) == 1, "one call back pending", callbackTexts(pending),
		lv.ids(r.turns[len(r.turns)-1].entry)...)
	r.end()
	lv.requestsTable(r)

	// The run is over while the call back comes due, so the next run finds
	// it overdue and fires it as it starts.
	lv.printf("\n## The second run, %s later", 75*time.Second)
	time.Sleep(75 * time.Second)
	second := lv.run("second run", clock)
	fired = lv.awaitCallback(second, time.Minute)
	lv.checkCameDue(second, fired, "a call back that came due while no run was on fires when the next run starts")

	// In the small hours she is asleep, so a message then is one she puts
	// off: she writes nothing and schedules a call back for the morning.
	night := clock.Now().AddDate(0, 0, 1)
	night = time.Date(night.Year(), night.Month(), night.Day(), 3, 20, 0, 0, night.Location())
	clock.set(night)
	lv.printf("\n## The small hours, %s", night.Format(time.DateTime))
	second.send([]string{"just got home, the party went on forever. are you up?"}, nil)
	turn := second.turns[len(second.turns)-1]
	reply, _ := lv.st.ReplyOfEntry(ctx, turn.entry)
	pending = lv.pending(ctx)
	morning := len(pending) == 1 && pending[0].DueAt.After(night.Add(3*time.Hour)) && pending[0].DueAt.Before(night.Add(12*time.Hour))
	lv.check("a message at 03:20 is put off: she writes nothing and schedules a call back for the morning",
		reply == nil && turn.failed == "" && morning,
		"no reply, the turn done, and one call back due 3 to 12 hours later",
		fmt.Sprintf("reply %q; %s; %s", clip(oneLine(textOfMessage(reply)), 80), errorOf(turn.failed), callbackTexts(pending)),
		lv.ids(turn.entry)...)
	second.end()
	lv.requestsTable(second)
	lv.printf("\n%d checks passed, %d failed.", lv.passed, lv.failed)
}

func (lv *live) pending(ctx context.Context) []store.Callback {
	pending, err := lv.st.Callbacks(ctx)
	if err != nil {
		lv.t.Fatal(err)
	}
	return pending
}

func callbackTexts(pending []store.Callback) string {
	if len(pending) == 0 {
		return "no call back pending"
	}
	var out []string
	for _, c := range pending {
		out = append(out, fmt.Sprintf("#%d due %s: %s", c.ID, c.DueAt.Format(time.TimeOnly), c.Reason))
	}
	return strings.Join(out, "; ")
}

// dueAbout says the one call back she scheduled in a turn is due about so long
// after its message, and what was found. She is told the time to the minute,
// so it is counted from the minute the message was sent to when the turn
// ended, a minute either way: "in 4 minutes" said a few seconds after a minute
// begins may be read from the minute before. For the same reason "in 1 minute"
// may be due seconds after the message, and fire as the turn ends, before the
// call backs pending are read: then it is the message it fired as, at the time
// it fired.
func (lv *live) dueAbout(turn liveTurn, pending []store.Callback, after time.Duration) (bool, string) {
	ctx := context.Background()
	found := callbackTexts(pending)
	msg, err := lv.st.Message(ctx, turn.message)
	if err != nil {
		return false, found + "; the message: " + err.Error()
	}
	entry, err := lv.st.Entry(ctx, turn.entry)
	if err != nil {
		return false, found + "; the turn: " + err.Error()
	}
	found += fmt.Sprintf("; sent %s, answered %s", msg.CreatedAt.Format(time.TimeOnly), entry.EndedAt.Format(time.TimeOnly))
	var due time.Time
	switch len(pending) {
	case 0:
		fired, err := lv.st.LastAsked(ctx)
		if err != nil || fired.Role != store.RoleCallback || fired.ID < turn.message {
			return false, found
		}
		due = fired.CreatedAt
		found += fmt.Sprintf("; fired at %s as message %d", due.Format(time.TimeOnly), fired.ID)
	case 1:
		due = pending[0].DueAt
	default:
		return false, found
	}
	from := msg.CreatedAt.Truncate(time.Minute).Add(after - time.Minute)
	to := entry.EndedAt.Add(after + time.Minute)
	return !due.Before(from) && !due.After(to), found
}

// awaitCallback waits for a call back to fire as a message and for the turn
// that answers it to end, and writes the turn down as one of the run's.
func (lv *live) awaitCallback(r *liveRun, patience time.Duration) *store.Message {
	ctx := context.Background()
	deadline := time.Now().Add(patience)
	var fired *store.Message
	for time.Now().Before(deadline) {
		last, err := lv.st.LastAsked(ctx)
		if err == nil && last.Role == store.RoleCallback {
			fired = last
			break
		}
		time.Sleep(time.Second)
	}
	if fired == nil {
		return nil
	}
	// The turn began as the call back fired, so the wait is for it.
	if _, err := r.e.Wait(ctx); err != nil {
		lv.t.Fatal(err)
	}
	entries := r.entries()
	turn := liveTurn{sent: []string{"(call back: " + fired.Text() + ")"}, message: fired.ID}
	if len(entries) > 0 {
		turn.entry = entries[len(entries)-1].ID
	}
	r.turns = append(r.turns, turn)
	reply, _ := lv.st.ReplyOfEntry(ctx, turn.entry)
	lv.printf("- %q → entry %d: %s", turn.said(), turn.entry, clip(oneLine(textOfMessage(reply)), 120))
	return fired
}

// checkCameDue holds a call back that fired to having been answered by a
// reply whose prompt ended with the note that it came due, and nothing else.
func (lv *live) checkCameDue(r *liveRun, fired *store.Message, name string) {
	ctx := context.Background()
	if fired == nil {
		lv.check(name, false, "a call back message and a reply to it", "no call back fired in time")
		return
	}
	turn := r.turns[len(r.turns)-1]
	entry, err := lv.st.Entry(ctx, turn.entry)
	reply, _ := lv.st.ReplyOfEntry(ctx, turn.entry)
	answers := err == nil && entry.UptoMessageID == fired.ID && entry.Status == store.StatusDone && reply != nil
	req, ok := lv.replied(turn.entry)
	sent := sentMessages(req.RequestBody)
	// The note ends with the reason, given a stop when the model wrote none.
	told := ok && len(sent) > 0 && strings.HasPrefix(sent[len(sent)-1].text, "A call back you scheduled came due at ") &&
		(strings.HasSuffix(sent[len(sent)-1].text, ": "+fired.Text()) ||
			strings.HasSuffix(sent[len(sent)-1].text, ": "+fired.Text()+"."))
	lv.check(name, answers && told,
		"a done entry answering the call back message, whose prompt ends with the note saying it came due",
		fmt.Sprintf("entry %d %s answers message %d (call back %d); the prompt ends with %q", turn.entry, statusOf(entry),
			uptoOf(entry), fired.ID, lastText(sent)), lv.ids(turn.entry)...)
}

func statusOf(e *store.Entry) string {
	if e == nil {
		return "missing"
	}
	return e.Status
}

func uptoOf(e *store.Entry) store.MessageID {
	if e == nil {
		return 0
	}
	return e.UptoMessageID
}

func lastText(sent []sentMessage) string {
	if len(sent) == 0 {
		return ""
	}
	return clip(sent[len(sent)-1].text, 120)
}
