// Package callbacks is the tools by which she writes on her own: scheduling a
// time to, and moving or cancelling one she scheduled. A call back that comes
// due is a message she answers, so she writes then the way she answers one.
package callbacks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/store"
	"nerdola.dev/x/paula/internal/tools/api"
)

// Kind is the key written in the configuration file.
const Kind = "callbacks"

// Open reads the callbacks section, which takes no settings, and builds the
// four tools.
func Open(s config.Section, h api.Host) ([]api.Tool, error) {
	var none struct{}
	if err := s.Decode(&none); err != nil {
		return nil, err
	}
	return []api.Tool{list{h}, schedule{h}, move{h}, cancel{h}}, nil
}

// arrangement is what every tool tells a model of how a call back works.
func arrangement(n api.Names) string {
	return "A call back is a time you chose to write to " + n.User + " on your own: when it comes, you are " +
		"told it is due and why, and you write. Your call backs are not in your prompt; list_callbacks " +
		"is how you see them, each with a number that move_callback and cancel_callback take."
}

type list struct{ h api.Host }

func (t list) Definition() api.Definition {
	return api.Definition{
		Name: "list_callbacks",
		Description: "List the call backs you scheduled that have not come yet, soonest first: its number, " +
			"when it is due, and why. " + arrangement(t.h.Names) + " List them before moving or cancelling " +
			"one, and when " + t.h.Names.User + " asks what you have planned.",
		Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
	}
}

func (list) Note(json.RawMessage) string { return "listing call backs" }

func (list) LooksUp() {}

func (list) Call(ctx context.Context, env api.Env, _ json.RawMessage) (string, error) {
	pending, err := env.Callbacks(ctx)
	if err != nil {
		return "", err
	}
	if len(pending) == 0 {
		return "no call backs scheduled", nil
	}
	out := make([]string, len(pending))
	for i, c := range pending {
		out[i] = line(env, c)
	}
	return strings.Join(out, "\n"), nil
}

type schedule struct{ h api.Host }

func (t schedule) Definition() api.Definition {
	n := t.h.Names
	return api.Definition{
		Name: "schedule_callback",
		Description: "Schedule a time to write to " + n.User + " on your own: when you said you would get " +
			"back, when something of " + n.User + "'s is due to have happened, or when you want to check in. " +
			arrangement(n) + " A time in the past is refused. To change the time of one you already " +
			"scheduled, move it with move_callback rather than schedule another: both would come.",
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"at":{"type":"string","description":"when, as an RFC 3339 time with its offset, such as 2026-09-29T21:30:00+02:00"},` +
			`"reason":{"type":"string","description":"why you want to write then: it is what you are told when the time comes"}},` +
			`"required":["at","reason"]}`),
	}
}

// Instructions is what she is told about her call backs: the message that
// says one came due is the app's, and a message need not be answered the
// moment it arrives, when the person she is would not be writing then.
func (t schedule) Instructions() string {
	n := t.h.Names
	return "When a call back you scheduled comes due, a message saying so, and why, comes from the app you and " +
		n.User + " text through, not from " + n.User + ": you answer it by writing to " + n.User + ". " +
		"You need not answer a message the moment it arrives. When you would not be writing right then, " +
		"asleep at that hour, at work, or out, send nothing and schedule a call back with schedule_callback " +
		"for when you would, saying in its reason what " + n.User + " wrote. The message waits: when the call " +
		"back comes due you are shown it again, and you write then, as you would have. Before you schedule a " +
		"call back, list the ones you have with list_callbacks: one you have already is moved with " +
		"move_callback, never scheduled again, since every one you have will come."
}

type scheduleArgs struct {
	At     string `json:"at"`
	Reason string `json:"reason"`
}

func (schedule) Note(args json.RawMessage) string {
	var a scheduleArgs
	json.Unmarshal(args, &a)
	return strings.TrimSpace("scheduling a call back: " + a.Reason)
}

func (schedule) Call(ctx context.Context, env api.Env, args json.RawMessage) (string, error) {
	var a scheduleArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	at, err := when(env, a.At)
	if err != nil {
		return "", err
	}
	reason := strings.TrimSpace(a.Reason)
	if reason == "" {
		return "", errors.New("no reason was given")
	}
	c, err := env.Schedule(ctx, at, reason)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("call back #%d set for %s", c.ID, env.Time(c.DueAt)), nil
}

type move struct{ h api.Host }

func (t move) Definition() api.Definition {
	return api.Definition{
		Name:        "move_callback",
		Description: "Move a call back you scheduled to another time, by its number. " + arrangement(t.h.Names),
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"id":{"type":"integer","description":"the number of the call back"},` +
			`"at":{"type":"string","description":"the new time, as an RFC 3339 time with its offset"}},` +
			`"required":["id","at"]}`),
	}
}

type moveArgs struct {
	ID int64  `json:"id"`
	At string `json:"at"`
}

func (move) Note(args json.RawMessage) string {
	var a moveArgs
	json.Unmarshal(args, &a)
	return fmt.Sprintf("moving call back #%d", a.ID)
}

func (move) Call(ctx context.Context, env api.Env, args json.RawMessage) (string, error) {
	var a moveArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	at, err := when(env, a.At)
	if err != nil {
		return "", err
	}
	err = env.Move(ctx, store.CallbackID(a.ID), at)
	if errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("no call back is numbered %d", a.ID)
	}
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("call back #%d moved to %s", a.ID, env.Time(at)), nil
}

type cancel struct{ h api.Host }

func (t cancel) Definition() api.Definition {
	return api.Definition{
		Name:        "cancel_callback",
		Description: "Cancel a call back you scheduled, by its number. " + arrangement(t.h.Names),
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"id":{"type":"integer","description":"the number of the call back"}},` +
			`"required":["id"]}`),
	}
}

type cancelArgs struct {
	ID int64 `json:"id"`
}

func (cancel) Note(args json.RawMessage) string {
	var a cancelArgs
	json.Unmarshal(args, &a)
	return fmt.Sprintf("cancelling call back #%d", a.ID)
}

func (cancel) Call(ctx context.Context, env api.Env, args json.RawMessage) (string, error) {
	var a cancelArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	err := env.Cancel(ctx, store.CallbackID(a.ID))
	if errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("no call back is numbered %d", a.ID)
	}
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("call back #%d cancelled", a.ID), nil
}

// when reads a time a model gave, which has to be ahead: a call back for a
// time that has passed would fire at once, for nothing she meant. A time is
// told to her by the minute, so one in the minute it is now is not past to
// her, however many seconds of it have gone.
func when(env api.Env, text string) (time.Time, error) {
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(text))
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is not an RFC 3339 time", text)
	}
	if now := env.Now(); at.Before(now.Truncate(time.Minute)) {
		return time.Time{}, fmt.Errorf("%s is not ahead: it is now %s", env.Time(at), env.Time(now))
	}
	return at, nil
}

// line is a call back as a model reads it: its number, when it is due, and
// why.
func line(env api.Env, c store.Callback) string {
	return fmt.Sprintf("#%d (due %s) %s", c.ID, env.Time(c.DueAt), c.Reason)
}
