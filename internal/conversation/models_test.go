package conversation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/runners"
	"nerdola.dev/x/paula/internal/runners/api"
)

func TestTheModelsMenuSaysWhatServesEachRole(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: says("hey you")}
	eyes := &fakeRunner{model: visionModel(), chat: says("a red square")}
	r := openReplyWith(t, f, withVision(f, eyes))

	models, err := r.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	roles := map[config.Role]RoleModels{}
	for _, role := range models.Roles {
		roles[role.Role] = role
	}
	chat, ok := roles[config.RoleChat]
	if !ok {
		t.Fatalf("roles = %+v, want one for chat", models.Roles)
	}
	if chat.Current != "chat" || chat.Default != "chat" || chat.Saved {
		t.Errorf("chat = %+v, want the file's model and nothing saved", chat)
	}
	if len(chat.Options) != 1 || chat.Options[0] != "chat" {
		t.Errorf("chat options = %v, want only the model that does chat", chat.Options)
	}
	vision, ok := roles[config.RoleVision]
	if !ok || vision.Current != "eyes" {
		t.Errorf("vision = %+v", vision)
	}
}

func TestAModelChosenForARoleIsKept(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: says("hey you")}
	eyes := &fakeRunner{model: visionModel(), chat: says("a red square")}
	r := openReplyWith(t, f, withVision(f, eyes))
	ctx := context.Background()

	// The eyes see images, so they cannot write a reply.
	if err := r.SetModel(ctx, config.RoleChat, "eyes"); err == nil {
		t.Error("a model that does no chat was set for chat")
	}
	if err := r.SetModel(ctx, "audio", "chat"); err == nil {
		t.Error("a role that does not exist was set")
	}
	if err := r.SetModel(ctx, config.RoleChat, "nope"); err == nil {
		t.Error("a model that is not configured was set")
	}

	// A model of its own for vision.
	chat := r.Engine.runners.Model("chat")
	chat.Runner.(*fakeRunner).model.Vision = true
	if err := r.SetModel(ctx, config.RoleVision, "chat"); err != nil {
		t.Fatal(err)
	}
	models, err := r.Models(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range models.Roles {
		if role.Role == config.RoleVision && (!role.Saved || role.Current != "chat" || role.Default != "eyes") {
			t.Errorf("vision = %+v, want the saved model beside the file's", role)
		}
	}

	// A reply now asks the model that was chosen.
	sendPhoto(t, r, "look at this", photo(t))
	if eyes.count() != 0 {
		t.Error("the model the file names was asked, not the one that was chosen")
	}

	if err := r.ResetModels(ctx); err != nil {
		t.Fatal(err)
	}
	models, err = r.Models(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range models.Roles {
		if role.Saved {
			t.Errorf("%s is still saved", role.Role)
		}
	}
}

// A model the conversation was given serves the role wherever the file's
// default stands, and where the file names none.
func TestWhatServesARoleIsWhatTheConversationWasGiven(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: says("hey you")}
	set := withVision(f, &fakeRunner{model: visionModel()})
	// Another model that sees, which the role can be given.
	also := &fakeRunner{model: api.Model{ID: "some/others", Context: 100000, Chat: true, Vision: true}}
	other := &runners.Configured{Name: "others", ID: also.model.ID, Runner: also}
	set.Runners = append(set.Runners, also)
	set.Models = append(set.Models, other)
	r := openReplyWith(t, f, set)
	ctx := context.Background()

	m, err := RoleModel(ctx, r.store, set, config.RoleVision)
	if err != nil {
		t.Fatal(err)
	}
	if m == nil || m.Name != set.Defaults[config.RoleVision].Name {
		t.Fatalf("vision is served by %+v, want the file's default", m)
	}

	if err := r.SetModel(ctx, config.RoleVision, other.Name); err != nil {
		t.Fatal(err)
	}
	if m, err := RoleModel(ctx, r.store, set, config.RoleVision); err != nil || m == nil || m.Name != other.Name {
		t.Fatalf("vision is served by %+v, %v, want the model it was given", m, err)
	}

	// The file naming no default for the role leaves what it was given, which
	// is what serves it.
	delete(set.Defaults, config.RoleVision)
	if m, err := RoleModel(ctx, r.store, set, config.RoleVision); err != nil || m == nil || m.Name != other.Name {
		t.Fatalf("vision is served by %+v, %v, want the model it was given", m, err)
	}
	// A role nothing serves is answered as nothing, for whoever asked to say
	// what it means.
	if err := r.ResetModels(ctx); err != nil {
		t.Fatal(err)
	}
	if m, err := RoleModel(ctx, r.store, set, config.RoleVision); err != nil || m != nil {
		t.Errorf("vision is served by %+v, %v, want nothing", m, err)
	}
}

func TestTheModelsMenuOfASetupWithNoRunners(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: says("hey you")}
	r := openReplyWith(t, f, nil)

	models, err := r.Models(context.Background())
	if err != nil || len(models.Roles) != 0 {
		t.Errorf("models = %+v, %v", models, err)
	}
	if err := r.SetModel(context.Background(), config.RoleChat, "chat"); err == nil {
		t.Error("a model was set with no runner")
	}
}

func TestWhatWasSaidSinceAMessage(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: says("hey you")}
	r := openReply(t, f)
	ctx := context.Background()

	r.say(t, "one")
	r.say(t, "two")

	all, err := r.Since(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("messages = %d, want two of each", len(all))
	}
	after, err := r.Since(ctx, all[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 2 || after[0].ID != all[2].ID {
		t.Errorf("messages after %d = %+v", all[1].ID, after)
	}
	if none, err := r.Since(ctx, all[3].ID); err != nil || len(none) != 0 {
		t.Errorf("messages after the last = %+v, %v", none, err)
	}
}

// The loop needs one thing of a timer that the standard library's does not do:
// it waits for nothing until the loop resets it.
func TestAWallTimerStartsStopped(t *testing.T) {
	var w Wall
	timer := w.NewTimer(time.Millisecond)
	select {
	case <-timer.Chan():
		t.Error("the timer went off before it was set")
	case <-time.After(10 * time.Millisecond):
	}

	timer.Reset(time.Millisecond)
	select {
	case <-timer.Chan():
	case <-time.After(time.Second):
		t.Error("the timer never went off")
	}
	timer.Stop()
}

func TestEveryMessageOfAPromptIsRead(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: says("hey you")}
	r := openReply(t, f)

	for i := range 12 {
		r.say(t, fmt.Sprintf("message %d", i))
	}

	stored, err := r.Since(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}

	// Twelve lines, each answered, are 24 messages.
	if len(stored) != 24 {
		t.Fatalf("the conversation holds %d messages, want 24", len(stored))
	}
	// The prompt of the last reply carries the card, every message there was
	// bar that reply, and the time before each of the twelve I sent.
	if asked := f.asked().Messages; len(asked) != 36 {
		t.Errorf("prompt = %d messages, want the card, the 23 before the reply and twelve times",
			len(asked))
	}
}

// The role is in the menu with the reason, rather than missing from it.
func TestARoleNothingCanServeSaysWhy(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: says("hey")}
	away := &fakeRunner{model: visionModel()}
	away.chat = func(context.Context, api.ChatRequest, func(api.Chunk) error) (*api.Result, error) {
		return nil, errors.New("not asked")
	}
	away.models = func() ([]api.Model, error) {
		return nil, &api.APIError{Status: 503, Message: "the runner is away"}
	}
	r := openReplyWith(t, f, withVision(f, away))

	models, err := r.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var vision *RoleModels
	for i := range models.Roles {
		if models.Roles[i].Role == config.RoleVision {
			vision = &models.Roles[i]
		}
	}
	if vision == nil {
		t.Fatalf("roles = %+v, want vision among them", models.Roles)
	}
	if !strings.Contains(vision.Problem, "the runner is away") {
		t.Errorf("problem = %q, want why it could not be asked", vision.Problem)
	}
}
