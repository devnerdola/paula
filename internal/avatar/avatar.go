// Package avatar is her picture: the file in the data directory that says how
// she looks, which the photos she takes are made from and a frontend shows
// where a face goes. A data directory with none has one painted from what the
// character card says of her appearance.
package avatar

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/media"
	"nerdola.dev/x/paula/internal/persona"
	"nerdola.dev/x/paula/internal/runners"
	"nerdola.dev/x/paula/internal/runners/api"
)

// names are the files an avatar is looked for under, in order. A painted one
// is kept under the first.
var names = []string{"avatar.jpg", "avatar.jpeg", "avatar.png", "avatar.webp"}

// Find is the avatar of a data directory, and empty when it has none.
func Find(dataDir string) string {
	for _, name := range names {
		path := filepath.Join(dataDir, name)
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return ""
}

// Path is where a painted avatar is kept.
func Path(dataDir string) string { return filepath.Join(dataDir, names[0]) }

// Prompt is what an avatar is painted from: a portrait of her as the card's
// appearance describes her, in the words the card says it in. It is her head
// and shoulders against nothing, since every photo she takes is made from it:
// a photo made from a picture keeps what it is not told to change, and made
// from a selfie, a selfie she asked for came back as the avatar again.
func Prompt(card *persona.Card) (string, error) {
	if len(card.Appearance) == 0 {
		return "", errors.New("the character card says nothing under appearance to paint an avatar from")
	}
	lines := []string{"A realistic head-and-shoulders portrait photo of " + card.Name + ", looking at the camera, " +
		"against a plain light background, in soft daylight. " + card.Name + " is described below, addressed as \"you\":"}
	for _, line := range card.Appearance {
		lines = append(lines, "- "+line)
	}
	return strings.Join(lines, "\n"), nil
}

// Paint has the model paint her avatar from the card, and keeps it at Path
// as a JPEG no larger than maxPx on its longest side. A request that belongs
// to no turn is not recorded.
func Paint(ctx context.Context, dataDir string, maxPx int, card *persona.Card, set *runners.Setup, m *runners.Configured) error {
	prompt, err := Prompt(card)
	if err != nil {
		return err
	}
	catalogue, err := set.Lookup(ctx, m)
	if err != nil {
		return err
	}
	if missing := catalogue.Missing(runners.RoleNeeds(config.RoleAvatar)); len(missing) > 0 {
		return fmt.Errorf("%s cannot be the %s model: %w", m.Name, config.RoleAvatar, errors.Join(missing...))
	}
	painter, ok := m.Runner.(api.Painter)
	if !ok {
		return fmt.Errorf("%s makes no pictures", m.Runner.Name())
	}
	data, err := painter.Paint(ctx, api.PaintRequest{
		Model:    m.ID,
		Prompt:   prompt,
		Shape:    api.ShapeSquare,
		Settings: runners.WithDefaults(m.Settings, catalogue),
	})
	if err != nil {
		return err
	}
	out, err := media.Process(data, maxPx)
	if err != nil {
		return err
	}
	return media.Keep(Path(dataDir), out)
}
