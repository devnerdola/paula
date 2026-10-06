// Package photos is the tools by which she sends photos of herself: taking
// one, which the image model makes from her avatar and what she says of the
// photo, and sending one she took with her reply.
package photos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"nerdola.dev/x/paula/internal/config"
	runnersapi "nerdola.dev/x/paula/internal/runners/api"
	"nerdola.dev/x/paula/internal/store"
	"nerdola.dev/x/paula/internal/tools/api"
)

// Kind is the key written in the configuration file.
const Kind = "photos"

// Open reads the photos section, which takes no settings, and builds the two
// tools. A photo of her is made from her avatar by the image model, so there
// has to be both.
func Open(s config.Section, h api.Host) ([]api.Tool, error) {
	var none struct{}
	if err := s.Decode(&none); err != nil {
		return nil, err
	}
	p := &config.Problems{Path: s.Path()}
	if h.Avatar == "" {
		p.Addf("there is no avatar to take photos from: put avatar.jpg, avatar.jpeg, avatar.png or avatar.webp " +
			"in data_dir, or name a model for default_models.avatar")
	}
	if !h.Image {
		p.Addf("there is no model to take photos with: name one for default_models.image")
	}
	if err := p.Err(); err != nil {
		return nil, err
	}
	return []api.Tool{take{h}, send{h}}, nil
}

type take struct{ h api.Host }

func (t take) Definition() api.Definition {
	return api.Definition{
		Name: "take_photo",
		Description: "Take a photo of yourself, as your phone would. It is made from your avatar, the " +
			"picture that says how you look, and from what your prompt says of the photo. You are told its " +
			"number, and the answer carries the photo itself when it can, and says so; when it cannot, it says " +
			"what the photo shows. It is not sent: send_photo puts it in your reply.",
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"prompt":{"type":"string","description":"the photo as its camera sees it: where you are, what you are ` +
			`doing and wearing, the light, and where the camera is, such as held at arm's length or propped on a ` +
			`shelf. Say what the photo shows, not the taking of it. How you look is the avatar's to say."},` +
			`"shape":{"type":"string","enum":["` + strings.Join(runnersapi.Shapes, `","`) + `"],` +
			`"description":"the shape of the photo; leave it out for the one the model chooses"}},` +
			`"required":["prompt"]}`),
	}
}

func (t take) Instructions() string {
	n := t.h.Names
	return "You can take photos of yourself with take_photo and send them to " + n.User + " with send_photo, " +
		"as you would from your phone: when " + n.User + " asks for one, or when a photo says what words " +
		"would not. Look at what comes back before you send it: one that is not what you meant, or does not " +
		"look like you, is taken again with a clearer prompt. A photo you send goes with " +
		"your reply, after what you write. After a message of yours that carried a photo, a message from the " +
		"app you and " + n.User + " text through says what the photo showed; it is for you to know, never " +
		"to answer."
}

type takeArgs struct {
	Prompt string `json:"prompt"`
	Shape  string `json:"shape"`
}

// reference is what the image model is told of the avatar it is given, before
// her prompt: that it says how she looks and nothing more, and that the photo
// is what its camera sees. A model given a picture to make another from keeps
// what it is not told to change: told only of a place, it made the avatar again
// over another background. Told of a selfie, it drew her holding a phone.
const reference = "The picture shows what this person looks like: their face, hair and build. Nothing else " +
	"of it matters. Make a new photo of the same person, not an edit of this one: its own place, pose, " +
	"framing, clothes and light, as described here. The photo is what its camera sees, so neither the " +
	"camera nor the phone that took it is in the photo, unless it is taken in a mirror.\n\n"

func (take) Note(json.RawMessage) string { return "taking a photo" }

// LooksUp leaves a photo untaken in the last round of a reply, where nothing
// could send it.
func (take) LooksUp() {}

func (t take) Call(ctx context.Context, env api.Env, args json.RawMessage) (string, error) {
	var a takeArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	prompt := strings.TrimSpace(a.Prompt)
	if prompt == "" {
		return "", errors.New("no prompt was given")
	}
	if a.Shape != "" && !slices.Contains(runnersapi.Shapes, a.Shape) {
		return "", fmt.Errorf("the shape %q is not one of %v", a.Shape, runnersapi.Shapes)
	}
	avatar, err := os.ReadFile(t.h.Avatar)
	if err != nil {
		return "", err
	}
	photo, err := env.Photo(ctx, reference+prompt, a.Shape, avatar)
	if err != nil {
		return "", err
	}
	if env.Show(*photo) {
		return fmt.Sprintf("#%d\nThe photo itself is with this answer.", photo.ID), nil
	}
	caption := photo.Caption
	if caption == "" {
		caption = "nothing was said of what it shows"
	}
	return fmt.Sprintf("#%d %s", photo.ID, caption), nil
}

type send struct{ h api.Host }

func (t send) Definition() api.Definition {
	return api.Definition{
		Name: "send_photo",
		Description: "Send " + t.h.Names.User + " a photo you took with take_photo, by its number. It goes " +
			"with your reply, after what you write.",
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"number":{"type":"integer","description":"the number of the photo"}},` +
			`"required":["number"]}`),
	}
}

type sendArgs struct {
	Number int64 `json:"number"`
}

func (send) Note(json.RawMessage) string { return "sending a photo" }

func (send) Call(ctx context.Context, env api.Env, args json.RawMessage) (string, error) {
	var a sendArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	err := env.SendPhoto(ctx, a.Number)
	if errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("no photo you took is numbered %d", a.Number)
	}
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("photo #%d goes with your reply", a.Number), nil
}
