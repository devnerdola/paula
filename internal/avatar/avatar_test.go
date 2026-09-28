package avatar

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/media"
	"nerdola.dev/x/paula/internal/persona"
	"nerdola.dev/x/paula/internal/runners"
	"nerdola.dev/x/paula/internal/runners/api"
)

// painter is a runner whose one model is what a test says, which answers a
// paint with the picture Venice painted from a prompt
// (runners/venice/testdata/SOURCES.md), and remembers what it was asked.
type painter struct {
	model api.Model
	asked []api.PaintRequest
}

func (p *painter) Name() string                                      { return "painter" }
func (p *painter) Kind() string                                      { return "painter" }
func (p *painter) URL() string                                       { return "fake:///v1" }
func (p *painter) Health(context.Context) error                      { return nil }
func (p *painter) Settings() api.Settings                            { return api.Settings{} }
func (p *painter) Models(context.Context) ([]api.Model, error)       { return []api.Model{p.model}, nil }
func (p *painter) Model(context.Context, string) (*api.Model, error) { return &p.model, nil }
func (p *painter) Check(context.Context, api.Checked) []error        { return nil }

func (p *painter) Chat(context.Context, api.ChatRequest, func(api.Chunk) error) (*api.Result, error) {
	return nil, nil
}

func (p *painter) Paint(_ context.Context, req api.PaintRequest) ([]byte, error) {
	p.asked = append(p.asked, req)
	return os.ReadFile(filepath.Join("..", "runners", "venice", "testdata", "image_generate.jpg"))
}

func (p *painter) Edit(context.Context, api.EditRequest) ([]byte, error) { return nil, nil }

// card is the example character card.
func card(t *testing.T) *persona.Card {
	t.Helper()
	c, err := persona.Load(filepath.Join("..", "..", "personas", "paula.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func setup(p *painter) (*runners.Setup, *runners.Configured) {
	m := &runners.Configured{Name: "face", ID: p.model.ID, Runner: p}
	return &runners.Setup{Runners: []runners.Runner{p}, Models: []*runners.Configured{m}}, m
}

// An avatar is found under any of the names it is looked for under, the first
// of them first, and a data directory with none of them has none.
func TestAnAvatarIsFoundUnderAnyOfItsNames(t *testing.T) {
	dir := t.TempDir()
	if got := Find(dir); got != "" {
		t.Errorf("an empty directory has the avatar %q", got)
	}
	for _, name := range []string{"avatar.webp", "avatar.png", "avatar.jpeg", "avatar.jpg"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if got := Find(dir); got != filepath.Join(dir, name) {
			t.Errorf("with %s written, the avatar is %q", name, got)
		}
	}
}

// An avatar is painted, square, from a prompt that names her and says every
// line of her appearance, and kept where one is looked for, as a JPEG no
// larger than the pictures are kept at.
func TestAnAvatarIsPaintedFromHerAppearance(t *testing.T) {
	dir := t.TempDir()
	p := &painter{model: api.Model{ID: "venice-sd35", Paint: true}}
	set, m := setup(p)
	c := card(t)
	if err := Paint(context.Background(), dir, 128, c, set, m); err != nil {
		t.Fatal(err)
	}

	if len(p.asked) != 1 {
		t.Fatalf("the model was asked %d times", len(p.asked))
	}
	req := p.asked[0]
	if req.Model != "venice-sd35" || req.Shape != api.ShapeSquare || req.Recorder != nil || !strings.Contains(req.Prompt, "Paula") {
		t.Errorf("the model was asked %+v", req)
	}
	for _, line := range c.Appearance {
		if !strings.Contains(req.Prompt, line) {
			t.Errorf("the prompt %q leaves out %q", req.Prompt, line)
		}
	}

	if got := Find(dir); got != Path(dir) {
		t.Fatalf("the avatar is %q, want it at %q", got, Path(dir))
	}
	data, err := os.ReadFile(Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	if media.Detect(data) != media.MIMEJPEG {
		t.Errorf("the avatar is %s", media.Detect(data))
	}
	if info, err := os.Stat(Path(dir)); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the avatar is kept as %v, %v", info.Mode(), err)
	}
}

// A card that says nothing of how she looks has nothing to paint, and a model
// that does not paint is not asked: neither leaves an avatar behind.
func TestAnAvatarIsNotPaintedFromNothingOrByAModelThatDoesNotPaint(t *testing.T) {
	dir := t.TempDir()
	p := &painter{model: api.Model{ID: "venice-sd35", Paint: true}}
	set, m := setup(p)
	bare := card(t)
	bare.Appearance = nil
	if err := Paint(context.Background(), dir, 128, bare, set, m); err == nil || !strings.Contains(err.Error(), "appearance") {
		t.Errorf("painting a card with no appearance = %v", err)
	}

	editor := &painter{model: api.Model{ID: "muse-image-edit", Edit: true}}
	set, m = setup(editor)
	err := Paint(context.Background(), dir, 128, card(t), set, m)
	if err == nil || !strings.Contains(err.Error(), "cannot be the "+string(config.RoleAvatar)+" model") ||
		!strings.Contains(err.Error(), "pictures from a prompt") {
		t.Errorf("painting with a model that does not paint = %v", err)
	}
	if len(p.asked)+len(editor.asked) != 0 || Find(dir) != "" {
		t.Errorf("a model was asked, or an avatar left behind")
	}
}
