// Package images is the tools that reach the pictures of the conversation,
// the ones she was sent and the photos she sent: listing them, and looking at
// one again. The pictures of messages older than the recent ones are not in
// the prompt, so these tools are how she has them.
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

// defaultPage is how many pictures a list answers with when the file says
// nothing.
const defaultPage = 50

type settings struct {
	// Page is how many pictures a list answers with, unless the call asks for
	// another number.
	Page int `yaml:"page"`
}

// Open reads the images section and builds the two tools.
func Open(s config.Section, h api.Host) ([]api.Tool, error) {
	cfg := settings{Page: defaultPage}
	if err := s.Decode(&cfg); err != nil {
		return nil, err
	}
	if cfg.Page < 1 {
		p := &config.Problems{Path: s.Path()}
		p.Addf("page: %d is below one", cfg.Page)
		return nil, p.Err()
	}
	return []api.Tool{list{h: h, page: cfg.Page}, get{h}}, nil
}

// arrangement is what both tools tell a model of where the pictures are.
func arrangement(n api.Names) string {
	return "The pictures " + n.User + " sent and the photos you sent are kept for good, each with what it " +
		"showed. Only the pictures of the recent messages are in your prompt: older messages survive as the " +
		"summary of your conversation, which mentions a picture at most."
}

type list struct {
	h    api.Host
	page int
}

func (t list) Definition() api.Definition {
	n := t.h.Names
	return api.Definition{
		Name: "list_images",
		Description: fmt.Sprintf("List the pictures %s has sent and the photos you have sent, newest first: its "+
			"number, when it was sent and by whom, and what it showed, %d at a time, or as many as limit asks "+
			"for. The answer says how many pictures there are in all. ", n.User, t.page) +
			arrangement(n) + " List them when " + n.User +
			" brings up a picture you do not have in front of you, and look at one again with get_image, by " +
			"its number. A list with older pictures after it says so, and from lists them.",
		Parameters: json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{`+
			`"from":{"type":"integer","description":"how many of the newest pictures to pass over; leave it out for the newest"},`+
			`"limit":{"type":"integer","description":"how many pictures to list; leave it out for %d"}}}`, t.page)),
	}
}

type listArgs struct {
	From  int `json:"from"`
	Limit int `json:"limit"`
}

func (list) Note(json.RawMessage) string { return "listing pictures" }

func (list) LooksUp() {}

func (t list) Call(ctx context.Context, env api.Env, args json.RawMessage) (string, error) {
	var a listArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	if a.From < 0 {
		return "", fmt.Errorf("from is %d, below zero", a.From)
	}
	limit, err := api.Count("limit", a.Limit, t.page)
	if err != nil {
		return "", err
	}
	images, total, err := env.Images(ctx, a.From, limit)
	if err != nil {
		return "", err
	}
	if len(images) == 0 {
		if total > 0 {
			return fmt.Sprintf("no pictures past the newest %d: there are %d in all", a.From, total), nil
		}
		return "no pictures yet", nil
	}
	shown := a.From + len(images)
	pictures := fmt.Sprintf("%d pictures", total)
	if total == 1 {
		pictures = "1 picture"
	}
	out := []string{fmt.Sprintf("%d to %d of %s, newest first:", a.From+1, shown, pictures)}
	for _, img := range images {
		out = append(out, line(env, t.h.Names, img))
	}
	if shown < total {
		out = append(out, fmt.Sprintf("There are older pictures: list_images with from %d lists them.", shown))
	}
	return strings.Join(out, "\n"), nil
}

type get struct{ h api.Host }

func (t get) Definition() api.Definition {
	n := t.h.Names
	return api.Definition{
		Name: "get_image",
		Description: "Look at a picture " + n.User + " sent, or a photo you sent, by the number list_images gives it. " +
			arrangement(n) + " The answer carries the picture itself when it can, and says so; when it cannot, " +
			"it says what the picture showed.",
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

func (t get) Call(ctx context.Context, env api.Env, args json.RawMessage) (string, error) {
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
	if env.Show(*img) {
		return sent(env, t.h.Names, *img) + "\nThe picture itself is with this answer.", nil
	}
	return line(env, t.h.Names, *img), nil
}

// line is a picture as a model reads it when it is not shown the picture: its
// number, when it was sent and by whom, and what it showed.
func line(env api.Env, n api.Names, img store.Image) string {
	caption := img.Caption
	if caption == "" {
		caption = "nothing was said of what it shows"
	}
	return sent(env, n, img) + " " + caption
}

// sent is a picture's number, when it was sent and by whom.
func sent(env api.Env, n api.Names, img store.Image) string {
	by := n.User
	if img.Role == store.RoleAssistant {
		by = "you"
	}
	return fmt.Sprintf("#%d (sent %s by %s)", img.ID, env.Time(img.SentAt), by)
}
