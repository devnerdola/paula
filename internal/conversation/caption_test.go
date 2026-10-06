package conversation

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/runners"
	"nerdola.dev/x/paula/internal/runners/api"
	"nerdola.dev/x/paula/internal/store"
)

// withVision gives the setup a second model, which sees images.
func withVision(f *fakeRunner, eyes *fakeRunner) *runners.Setup {
	s := setup(f)
	m := &runners.Configured{Name: "eyes", ID: eyes.model.ID, Runner: eyes}
	s.Runners = append(s.Runners, eyes)
	s.Models = append(s.Models, m)
	s.Defaults[config.RoleVision] = m
	return s
}

func visionModel() api.Model {
	return api.Model{ID: "some/eyes", Context: 100000, Chat: true, Vision: true}
}

// openSeeing builds an engine whose chat model is blind and whose vision model
// describes images.
func openSeeing(t *testing.T, f, eyes *fakeRunner) *replyEngine {
	t.Helper()
	return openReplyWith(t, f, withVision(f, eyes))
}

func TestAReplyWaitsForAnImageToBeDescribed(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: says("nice one")}
	eyes := &fakeRunner{model: visionModel(), chat: says("a red square")}
	r := openSeeing(t, f, eyes)

	sendPhoto(t, r, "look at this", photo(t))

	if got := text(mine(f.asked())); !strings.HasSuffix(got, "[photo: a red square]") {
		t.Errorf("message = %q", got)
	}
	if eyes.count() == 0 {
		t.Fatal("nothing was asked of the vision model")
	}
	asked := eyes.asked()
	if len(asked.Messages) != 1 || asked.Messages[0].Role != api.RoleUser {
		t.Fatalf("the vision model was asked %+v", asked.Messages)
	}
	if got := text(asked.Messages[0]); got != captionPrompt {
		t.Errorf("asked %q, want %q", got, captionPrompt)
	}
	if len(images(asked.Messages[0])) != 1 {
		t.Error("the image was not sent to the vision model")
	}
}

func TestADescriptionIsKept(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: says("nice one")}
	eyes := &fakeRunner{model: visionModel(), chat: says("a red square")}
	r := openSeeing(t, f, eyes)

	sha := sendPhoto(t, r, "look at this", photo(t))
	asked := eyes.count()

	m, err := r.store.Media(context.Background(), sha)
	if err != nil {
		t.Fatal(err)
	}
	if m.Caption != "a red square" {
		t.Errorf("caption = %q", m.Caption)
	}

	// The next reply reads what is already there.
	r.say(t, "and this one?")
	if eyes.count() != asked {
		t.Errorf("the vision model was asked again")
	}
}

func TestAnImageThatCannotBeDescribed(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: says("nice one")}
	eyes := &fakeRunner{model: visionModel()}
	eyes.chat = func(context.Context, api.ChatRequest, func(api.Chunk) error) (*api.Result, error) {
		return nil, &api.APIError{Status: 400, Message: "the image could not be decoded"}
	}
	r := openSeeing(t, f, eyes)

	sha := sendPhoto(t, r, "look at this", photo(t))

	if got := text(mine(f.asked())); !strings.HasSuffix(got, "[photo]") {
		t.Errorf("message = %q, want the photo left undescribed", got)
	}
	m, err := r.store.Media(context.Background(), sha)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m.CaptionError, "could not be decoded") {
		t.Errorf("why it failed = %q", m.CaptionError)
	}
}

// A look the host did not make says nothing about the image: the connection
// went, the host was busy or down, or asked for the request again later, none
// of which is the model saying it cannot describe it.
func TestALookThatWasNotMadeIsAskedAgain(t *testing.T) {
	for _, c := range []struct {
		what string
		err  error
	}{
		{"the connection went", api.ErrIdle},
		{"a rate limit", &api.APIError{Status: 429, Message: "slow down"}},
		{"a request timeout", &api.APIError{Status: 408, Message: "too slow"}},
		{"a host that is down", &api.APIError{Status: 503, Message: "over capacity"}},
		{"a connection refused", errors.New("the host went away")},
	} {
		f := &fakeRunner{model: chatModel(), chat: says("nice one")}
		eyes := &fakeRunner{model: visionModel()}
		eyes.chat = func(context.Context, api.ChatRequest, func(api.Chunk) error) (*api.Result, error) {
			return nil, c.err
		}
		r := openSeeing(t, f, eyes)

		sha := sendPhoto(t, r, "look at this", photo(t))

		m, err := r.store.Media(context.Background(), sha)
		if err != nil {
			t.Fatal(err)
		}
		if m.CaptionError != "" {
			t.Errorf("%s: why it failed = %q, want nothing kept of a look that was not made", c.what, m.CaptionError)
		}

		// The next reply asks again, and this time it is answered.
		eyes.chat = says("a red square")
		r.say(t, "and now?")
		m, err = r.store.Media(context.Background(), sha)
		if err != nil {
			t.Fatal(err)
		}
		if m.Caption != "a red square" {
			t.Errorf("%s: caption = %q, want the image described on the next try", c.what, m.Caption)
		}
	}
}

// An image the host refused to look at is answered without a description, and
// not sent again.
func TestAnImageThatFailedIsNotTriedAgain(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: says("nice one")}
	eyes := &fakeRunner{model: visionModel()}
	eyes.chat = func(context.Context, api.ChatRequest, func(api.Chunk) error) (*api.Result, error) {
		return nil, &api.APIError{Status: 400, Message: "the image could not be decoded"}
	}
	r := openSeeing(t, f, eyes)

	sendPhoto(t, r, "look at this", photo(t))
	tried := eyes.count()
	if tried == 0 {
		t.Fatal("the vision model was never asked")
	}

	r.say(t, "still there?")
	r.clock.Advance(2 * time.Hour)
	r.say(t, "and now?")
	if eyes.count() != tried {
		t.Errorf("the image was asked about again")
	}
	if got := text(mine(f.asked())); !strings.HasSuffix(got, "[photo]") {
		t.Errorf("message = %q, want the photo left undescribed", got)
	}
}

// An image a model can see is sent as it is and described all the same, so what
// it shows is in the conversation from the turn it arrived in.
func TestAnImageIsUnderstoodWhenItArrives(t *testing.T) {
	model := chatModel()
	model.Vision = true
	f := &fakeRunner{model: model, chat: says("nice one")}
	eyes := &fakeRunner{model: visionModel(), chat: says("a red square")}
	r := openSeeing(t, f, eyes)

	sha := sendPhoto(t, r, "look at this", photo(t))

	if len(images(mine(f.asked()))) != 1 {
		t.Error("the image was not sent to a model that sees images")
	}
	m, err := r.store.Media(context.Background(), sha)
	if err != nil {
		t.Fatal(err)
	}
	if m.Caption != "a red square" {
		t.Errorf("media = %+v, want what it shows kept", m)
	}

	// It is asked about once, however many turns it is sent in.
	asked := eyes.count()
	r.say(t, "and now?")
	if eyes.count() != asked {
		t.Errorf("the vision model was asked again")
	}
}

func TestWithoutAVisionModelNothingIsDescribed(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: says("nice one")}
	r := openReply(t, f)

	sha := sendPhoto(t, r, "look at this", photo(t))

	if got := text(mine(f.asked())); !strings.HasSuffix(got, "[photo]") {
		t.Errorf("message = %q", got)
	}
	m, err := r.store.Media(context.Background(), sha)
	if err != nil {
		t.Fatal(err)
	}
	if m.CaptionError != "" {
		t.Errorf("a failure was kept with no vision model: %q", m.CaptionError)
	}
}

func TestTheDescriptionRequestIsRecorded(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: says("nice one")}
	eyes := &fakeRunner{model: visionModel(), chat: says("a red square")}
	r := openSeeing(t, f, eyes)
	sendPhoto(t, r, "look at this", photo(t))

	ctx := context.Background()
	entries, err := r.store.Entries(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	var purposes []string
	for _, e := range entries {
		requests, err := r.store.Requests(ctx, e.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, req := range requests {
			purposes = append(purposes, req.Purpose)
		}
	}
	var caption, reply int
	for _, p := range purposes {
		switch p {
		case store.PurposeCaption:
			caption++
		case store.PurposeReply:
			reply++
		}
	}
	if caption == 0 || reply == 0 {
		t.Errorf("requests = %v, want one of each", purposes)
	}
}

// The same image arriving three times in a burst is described once, for the
// reply that answers them.
func TestOneImageIsAskedAboutOnce(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: says("nice one")}
	eyes := &fakeRunner{model: visionModel(), chat: says("a red square")}
	r := openSeeing(t, f, eyes)

	for _, text := range []string{"one", "two", "three"} {
		err := r.Post(context.Background(), NewMessage{
			Channel: "repl", Text: text, Images: [][]byte{photo(t)},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	r.clock.Advance(config.DefaultEngine().Debounce.Duration())
	if _, err := r.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}

	if eyes.count() != 1 {
		t.Errorf("the vision model was asked %d times about one image", eyes.count())
	}
	if got := text(mine(f.asked())); !strings.HasSuffix(got, "[photo: a red square]") {
		t.Errorf("message = %q", got)
	}
}

// The image is asked about again the next time.
func TestADescriptionCutShortIsNotKept(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: says("nice one")}
	eyes := &fakeRunner{model: visionModel()}
	looking := make(chan struct{})
	eyes.chat = func(ctx context.Context, _ api.ChatRequest, _ func(api.Chunk) error) (*api.Result, error) {
		select {
		case <-looking:
		default:
			close(looking)
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	r := openSeeing(t, f, eyes)

	if err := r.Post(context.Background(), NewMessage{
		Channel: "repl", Text: "look at this", Images: [][]byte{photo(t)},
	}); err != nil {
		t.Fatal(err)
	}
	r.clock.Advance(config.DefaultEngine().Debounce.Duration())
	<-looking

	// He stops the reply while the vision model is still looking.
	if _, err := r.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}

	history, _, err := r.History(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	sha := history[0].Images()[0].SHA256
	m, err := r.store.Media(context.Background(), sha)
	if err != nil {
		t.Fatal(err)
	}
	if m.CaptionError != "" {
		t.Errorf("why it failed = %q, want a stop kept as nothing", m.CaptionError)
	}

	// The next reply asks about it again.
	eyes.chat = says("a red square")
	r.say(t, "well?")
	if got := text(mine(f.asked())); !strings.Contains(got, "[photo: a red square]") {
		t.Errorf("message = %q, want the photo described", got)
	}
}
