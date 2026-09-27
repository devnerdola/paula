package callbacks

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"nerdola.dev/x/paula/internal/config"
	runnersapi "nerdola.dev/x/paula/internal/runners/api"
	"nerdola.dev/x/paula/internal/store"
	"nerdola.dev/x/paula/internal/tools/api"
)

// now is the conversation's time in the tests: a Tuesday evening in Lisbon,
// twenty seconds into the minute.
var now = time.Date(2026, 9, 29, 20, 0, 20, 0, time.FixedZone("UTC+01:00", 3600))

// fakeEnv is a conversation holding the call backs a test gives it, at a
// time of its own, which writes a time in a way of its own.
type fakeEnv struct {
	pending   []store.Callback
	scheduled *store.Callback
	moved     store.CallbackID
	movedTo   time.Time
	cancelled store.CallbackID
}

func (f *fakeEnv) Date(t time.Time) string { return t.In(now.Location()).Format("2 Jan") }

func (f *fakeEnv) Time(t time.Time) string { return t.In(now.Location()).Format("2 Jan 15:04") }

func (f *fakeEnv) Now() time.Time { return now }

func (f *fakeEnv) Memories(context.Context, string, int) ([]store.Memory, error) { return nil, nil }

func (f *fakeEnv) LatestMemories(context.Context, int, int) ([]store.Memory, error) { return nil, nil }

func (f *fakeEnv) Remember(context.Context, string, []store.MemoryID) (*store.Memory, error) {
	return nil, nil
}

func (f *fakeEnv) Forget(context.Context, store.MemoryID) ([]store.Memory, error) { return nil, nil }

func (f *fakeEnv) Images(context.Context, int, int) ([]store.Image, error) { return nil, nil }

func (f *fakeEnv) Image(context.Context, int64) (*store.Image, error) { return nil, store.ErrNotFound }

func (f *fakeEnv) Show(store.Image) bool { return false }

func (f *fakeEnv) Callbacks(context.Context) ([]store.Callback, error) { return f.pending, nil }

func (f *fakeEnv) Schedule(_ context.Context, at time.Time, reason string) (*store.Callback, error) {
	f.scheduled = &store.Callback{ID: 4, DueAt: at, Reason: reason}
	return f.scheduled, nil
}

func (f *fakeEnv) Move(_ context.Context, id store.CallbackID, at time.Time) error {
	if f.find(id) == nil {
		return store.ErrNotFound
	}
	f.moved, f.movedTo = id, at
	return nil
}

func (f *fakeEnv) Cancel(_ context.Context, id store.CallbackID) error {
	if f.find(id) == nil {
		return store.ErrNotFound
	}
	f.cancelled = id
	return nil
}

func (f *fakeEnv) Recorder() runnersapi.Recorder { return nil }

func (f *fakeEnv) find(id store.CallbackID) *store.Callback {
	for i := range f.pending {
		if f.pending[i].ID == id {
			return &f.pending[i]
		}
	}
	return nil
}

// env holds two call backs, the soonest first.
func env() *fakeEnv {
	return &fakeEnv{pending: []store.Callback{
		{ID: 2, DueAt: now.Add(90 * time.Minute), Reason: "say good night"},
		{ID: 1, DueAt: now.Add(26 * time.Hour), Reason: "ask how the interview went"},
	}}
}

var host = api.Host{Names: api.Names{Character: "Paula", User: "Caio"}, Language: "Portuguese"}

func call(t *testing.T, name string, e *fakeEnv, args string) (string, error) {
	t.Helper()
	tools, err := Open(config.Section{}, host)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools {
		if tool.Definition().Name == name {
			return tool.Call(context.Background(), e, json.RawMessage(args))
		}
	}
	t.Fatalf("no tool is called %s", name)
	return "", nil
}

// The pending call backs are listed as the conversation gives them, each with
// its number, when it is due and why, and none says so.
func TestAListIsEveryPendingCallback(t *testing.T) {
	if got, err := call(t, "list_callbacks", &fakeEnv{}, `{}`); err != nil || got != "no call backs scheduled" {
		t.Errorf("listing none answered %q, %v", got, err)
	}
	got, err := call(t, "list_callbacks", env(), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	want := "#2 (due 29 Sep 21:30) say good night\n#1 (due 30 Sep 22:00) ask how the interview went"
	if got != want {
		t.Errorf("the list answered\n%s\nwant\n%s", got, want)
	}
}

// A call back is kept for the time given, whatever offset it is written
// with, and the answer says its number and when it is due. A time is told to
// her by the minute, so one in the minute it is now is taken, though seconds
// of it have gone.
func TestACallbackIsScheduledForTheTimeGiven(t *testing.T) {
	e := env()
	got, err := call(t, "schedule_callback", e, `{"at":"2026-09-29T22:30:00+02:00","reason":" check that the cake came out "}`)
	if err != nil {
		t.Fatal(err)
	}
	if got != "call back #4 set for 29 Sep 21:30" {
		t.Errorf("scheduling answered %q", got)
	}
	if e.scheduled == nil || !e.scheduled.DueAt.Equal(now.Truncate(time.Minute).Add(90*time.Minute)) || e.scheduled.Reason != "check that the cake came out" {
		t.Errorf("scheduled %+v, want the time given and the reason as written", e.scheduled)
	}

	e = env()
	if _, err := call(t, "schedule_callback", e, `{"at":"2026-09-29T20:00:00+01:00","reason":"this minute"}`); err != nil || e.scheduled == nil {
		t.Errorf("scheduling for the minute it is now = %v, want it taken", err)
	}
}

// A time before the minute it is now, one that is not a time, and no reason
// are refused, and nothing is scheduled.
func TestACallbackForThePastOrForNothingIsRefused(t *testing.T) {
	for _, args := range []string{
		`{"at":"2026-09-29T19:59:00+01:00","reason":"too late"}`,
		`{"at":"2026-09-29T19:59:59+01:00","reason":"a second before the minute"}`,
		`{"at":"tomorrow evening","reason":"vague"}`,
		`{"at":"2026-09-29T22:30:00+02:00","reason":"  "}`,
	} {
		e := env()
		if got, err := call(t, "schedule_callback", e, args); err == nil {
			t.Errorf("scheduling with %s answered %q, want it refused", args, got)
		}
		if e.scheduled != nil {
			t.Errorf("scheduling with %s kept %+v", args, e.scheduled)
		}
	}
}

// Moving a call back gives it the time given, and one that is not there is
// said so by its number.
func TestACallbackIsMovedToTheTimeGiven(t *testing.T) {
	e := env()
	got, err := call(t, "move_callback", e, `{"id":2,"at":"2026-09-30T09:00:00+01:00"}`)
	if err != nil {
		t.Fatal(err)
	}
	if got != "call back #2 moved to 30 Sep 09:00" || e.moved != 2 || !e.movedTo.Equal(now.Truncate(time.Minute).Add(13*time.Hour)) {
		t.Errorf("moving answered %q and moved #%d to %v", got, e.moved, e.movedTo)
	}
	e = env()
	if _, err := call(t, "move_callback", e, `{"id":9,"at":"2026-09-30T09:00:00+01:00"}`); err == nil || err.Error() != "no call back is numbered 9" {
		t.Errorf("moving one that is not there = %v", err)
	}
	if _, err := call(t, "move_callback", e, `{"id":2,"at":"2026-09-29T08:00:00+01:00"}`); err == nil || e.moved != 0 {
		t.Errorf("moving into the past = %v, and moved #%d", err, e.moved)
	}
}

func TestACallbackIsCancelledByItsNumber(t *testing.T) {
	e := env()
	got, err := call(t, "cancel_callback", e, `{"id":1}`)
	if err != nil {
		t.Fatal(err)
	}
	if got != "call back #1 cancelled" || e.cancelled != 1 {
		t.Errorf("cancelling answered %q and cancelled #%d", got, e.cancelled)
	}
	if _, err := call(t, "cancel_callback", env(), `{"id":9}`); err == nil || err.Error() != "no call back is numbered 9" {
		t.Errorf("cancelling one that is not there = %v", err)
	}
}

// Every tool says what she is doing while it runs, and tells her where her
// call backs are and how the tools reach them.
func TestEachToolSaysWhatItDoesAndWhereTheCallbacksAre(t *testing.T) {
	tools, err := Open(config.Section{}, host)
	if err != nil {
		t.Fatal(err)
	}
	args := json.RawMessage(`{"id":2,"at":"2026-09-30T09:00:00+01:00","reason":"say good night"}`)
	notes := map[string]string{
		"list_callbacks":    "listing call backs",
		"schedule_callback": "scheduling a call back: say good night",
		"move_callback":     "moving call back #2",
		"cancel_callback":   "cancelling call back #2",
	}
	for _, tool := range tools {
		d := tool.Definition()
		for _, want := range []string{"Caio", "not in your prompt", "list_callbacks"} {
			if !strings.Contains(d.Description, want) {
				t.Errorf("%s is described as %q, without %q", d.Name, d.Description, want)
			}
		}
		if got := tool.Note(args); got != notes[d.Name] {
			t.Errorf("%s says %q while it runs, want %q", d.Name, got, notes[d.Name])
		}
	}
}
