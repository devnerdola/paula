package conversation

import (
	"context"
	"fmt"
	"slices"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/media"
	"nerdola.dev/x/paula/internal/runners/api"
	"nerdola.dev/x/paula/internal/store"
)

// Photo has the image model make a picture from the reference, as the prompt
// says. The reference goes as every picture Paula keeps does, upright and no
// larger than image_max_px, whatever file it came from. The picture is kept
// under the call that took it, and described the way a picture she is sent is,
// so every model after it knows what it shows.
func (v env) Photo(ctx context.Context, prompt, shape string, reference []byte) (*store.Image, error) {
	m, err := v.e.roleModel(ctx, config.RoleImage)
	if err != nil {
		return nil, err
	}
	painter, ok := m.Runner.(api.Painter)
	if !ok {
		return nil, fmt.Errorf("%s makes no pictures", m.Runner.Name())
	}
	reference, err = media.Process(reference, v.e.cfg.ImageMaxPx)
	if err != nil {
		return nil, fmt.Errorf("the avatar: %w", err)
	}
	data, err := painter.Edit(ctx, api.EditRequest{
		Model:    m.ID,
		Prompt:   prompt,
		Shape:    shape,
		Picture:  reference,
		Settings: m.settings,
		Recorder: v.Recorder(),
	})
	if err != nil {
		return nil, err
	}
	sha, err := v.e.media.Store(data)
	if err != nil {
		return nil, err
	}
	// A picture that was paid for is kept, even when the reply is stopped as
	// it arrives.
	keep := context.WithoutCancel(ctx)
	id, err := v.e.store.AddPhoto(keep, sha, v.call)
	if err != nil {
		return nil, err
	}
	v.e.described(ctx, v.a, sha)
	taken, err := v.e.store.Photo(keep, id)
	if err != nil {
		return nil, err
	}
	return &store.Image{ID: id, SHA256: sha, Caption: taken.Caption}, nil
}

// SendPhoto has a photo she took go with the reply, after what she writes.
func (v env) SendPhoto(ctx context.Context, id int64) error {
	taken, err := v.e.store.Photo(ctx, id)
	if err != nil {
		return err
	}
	if slices.Contains(v.a.photos, taken.SHA256) {
		return fmt.Errorf("photo #%d already goes with your reply", id)
	}
	v.a.photos = append(v.a.photos, taken.SHA256)
	return nil
}
