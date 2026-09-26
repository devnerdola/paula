// Package images is the tools that reach the pictures of the conversation:
// listing them, and looking at one again. The pictures of messages older than
// the recent ones are not in the prompt, so these tools are how she has them.
package images

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/store"
	"nerdola.dev/x/paula/internal/tools/api"
)

// Kind is the key written in the configuration file.
const Kind = "images"

// Open reads the images section, which takes no settings, and builds the two
// tools.
func Open(s config.Section, h api.Host) ([]api.Tool, error) {
	var none struct{}
	if err := s.Decode(&none); err != nil {
		return nil, err
	}
	return []api.Tool{list{h}, get{h}}, nil
}

// arrangement is what both tools tell a model of where the pictures are.
func arrangement(n api.Names) string {
	return "The pictures " + n.User + " sent are kept for good, each with what it showed. Only the pictures " +
		"of the recent messages are in your prompt: older messages survive as the summary of your " +
		"conversation, which mentions a picture at most."
}

type list struct{ h api.Host }

func (t list) Definition() api.Definition {
	n := t.h.Names
	return api.Definition{
		Name: "list_images",
		Description: "List every picture " + n.User + " has sent, newest first: its number, when it was sent, " +
			"and what it showed. " + arrangement(n) + " List them when " + n.User + " brings up a picture you " +
			"do not have in front of you, and look at one again with get_image, by its number.",
		Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
	}
}

func (list) Note(json.RawMessage) string { return "listing pictures" }

func (list) Call(ctx context.Context, env api.Env, _ json.RawMessage) (string, error) {
	images, err := env.Images(ctx)
	if err != nil {
		return "", err
	}
	if len(images) == 0 {
		return "no pictures yet", nil
	}
	out := make([]string, len(images))
	for i, img := range images {
		out[i] = line(env, img)
	}
	return strings.Join(out, "\n"), nil
}

type get struct{ h api.Host }

func (t get) Definition() api.Definition {
	n := t.h.Names
	return api.Definition{
		Name: "get_image",
		Description: "Look at a picture " + n.User + " sent, by the number list_images gives it. " +
			arrangement(n) + " You are shown the picture itself when you can see images, and told what it " +
			"showed otherwise.",
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"number":{"type":"integer","description":"the number of the picture"}},` +
			`"required":["number"]}`),
	}
}

type getArgs struct {
	Number int64 `json:"number"`
}

func (get) Note(args json.RawMessage) string {
	var a getArgs
	json.Unmarshal(args, &a)
	return fmt.Sprintf("looking at picture #%d", a.Number)
}

func (get) Call(ctx context.Context, env api.Env, args json.RawMessage) (string, error) {
	var a getArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	img, err := env.Image(ctx, a.Number)
	if errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("no picture is numbered %d", a.Number)
	}
	if err != nil {
		return "", err
	}
	out := line(env, *img)
	if env.Show(*img) {
		out += "\nThe picture itself is with this answer."
	}
	return out, nil
}

// line is a picture as a model reads it: its number, when it was sent, and
// what it showed.
func line(env api.Env, img store.Image) string {
	caption := img.Caption
	if caption == "" {
		caption = "nothing was said of what it shows"
	}
	return fmt.Sprintf("#%d (sent %s) %s", img.ID, env.Time(img.SentAt), caption)
}
