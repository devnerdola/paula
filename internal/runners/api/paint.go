package api

import (
	"context"
	"slices"
)

// Painter is a runner that makes pictures: from a prompt alone, or from a
// picture and a prompt that says what to make of it. What comes back is the
// picture's file.
type Painter interface {
	Paint(ctx context.Context, req PaintRequest) ([]byte, error)
	Edit(ctx context.Context, req EditRequest) ([]byte, error)
}

// Shapes a picture can be asked for in. An empty shape leaves it to the model.
const (
	ShapePortrait  = "portrait"
	ShapeLandscape = "landscape"
	ShapeSquare    = "square"
)

// Shapes are every shape, in the order they are offered.
var Shapes = []string{ShapePortrait, ShapeLandscape, ShapeSquare}

// ratios are the aspect ratios a shape is made in: the one it is named for,
// then the nearest to it.
var ratios = map[string][]string{
	ShapePortrait:  {"3:4", "4:5", "2:3", "9:16"},
	ShapeLandscape: {"4:3", "5:4", "3:2", "16:9"},
	ShapeSquare:    {"1:1"},
}

// Ratio is the aspect ratio a picture of a shape is asked for in: the first of
// the shape's that the model lists. It is empty for no shape, and for a model
// that lists none of them, which leaves the shape to the model.
func (m *Model) Ratio(shape string) string {
	for _, r := range ratios[shape] {
		if slices.Contains(m.Ratios, r) {
			return r
		}
	}
	return ""
}

// PaintRequest asks a model for a picture of what a prompt describes. Like a
// chat request, it carries the model's settings and what records it.
type PaintRequest struct {
	Model    string
	Prompt   string
	Shape    string
	Settings Settings
	Recorder Recorder
}

// EditRequest asks a model for a picture made from another, as a prompt says.
type EditRequest struct {
	Model    string
	Prompt   string
	Shape    string
	Picture  []byte
	Settings Settings
	Recorder Recorder
}
