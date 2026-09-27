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

// page is how many pictures a list answers with at once. What a call answers
// goes into the prompt of the round after it, and a list of every picture of a
// long conversation would take more of it than the round has.
const page = 50

type list struct{ h api.Host }

func (t list) Definition() api.Definition {
	n := t.h.Names
	return api.Definition{
		Name: "list_images",
		Description: fmt.Sprintf("List the pictures %s has sent, newest first, %d at a time: its number, when "+
			"it was sent, and what it showed. ", n.User, page) + arrangement(n) + " List them when " + n.User +
			" brings up a picture you do not have in front of you, and look at one again with get_image, by " +
			"its number. A list with older pictures after it says so, and from lists them.",
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"from":{"type":"integer","description":"how many of the newest pictures to pass over; leave it out for the newest"}}}`),
	}
}

type listArgs struct {
	From int `json:"from"`
}

func (list) Note(json.RawMessage) string { return "listing pictures" }

func (list) LooksUp() {}

func (list) Call(ctx context.Context, env api.Env, args json.RawMessage) (string, error) {
	var a listArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	if a.From < 0 {
		return "", fmt.Errorf("from is %d, below zero", a.From)
	}
	// One past the page says whether there are older ones to list.
	images, err := env.Images(ctx, a.From, page+1)
	if err != nil {
		return "", err
	}
	if len(images) == 0 {
		if a.From > 0 {
			return fmt.Sprintf("no pictures past the newest %d", a.From), nil
		}
		return "no pictures yet", nil
	}
	older := len(images) > page
	if older {
		images = images[:page]
	}
	out := make([]string, len(images))
	for i, img := range images {
		out[i] = line(env, img)
	}
	if older {
		out = append(out, fmt.Sprintf("There are older pictures: list_images with from %d lists them.", a.From+page))
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

func (get) LooksUp() {}

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
