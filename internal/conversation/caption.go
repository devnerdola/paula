package conversation

import (
	"context"
	"errors"
	"strings"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/media"
	"nerdola.dev/x/paula/internal/runners/api"
	"nerdola.dev/x/paula/internal/store"
)

// captionPrompt is what a model with vision is asked of an image.
const captionPrompt = "Describe this image in one or two sentences."

// described is the line that stands for an image that is not sent as one. The
// reply that needs it asks for it, and an image is asked about once.
func (e *Engine) described(ctx context.Context, a *attempt, sha256 string) string {
	m, err := e.store.Media(ctx, sha256)
	if err != nil {
		e.log.Warn("reading what an image shows", "sha256", sha256, "error", err)
		return "[photo]"
	}
	if m.Caption != "" {
		return "[photo: " + m.Caption + "]"
	}
	if m.CaptionError != "" {
		return "[photo]"
	}
	caption, err := e.ask(ctx, a, sha256)
	if err != nil {
		// No vision model, and a look cut short by a stop, are both asked for.
		switch {
		case errors.Is(err, errNoModel), errors.Is(err, context.Canceled),
			errors.Is(err, context.DeadlineExceeded):
		default:
			e.log.Warn("describing an image", "sha256", sha256, "error", err)
		}
		return "[photo]"
	}
	return "[photo: " + caption + "]"
}

// ask asks the vision model what an image shows and keeps the answer.
func (e *Engine) ask(ctx context.Context, a *attempt, sha256 string) (string, error) {
	m, err := e.roleModel(ctx, config.RoleVision)
	if err != nil {
		return "", err
	}
	data, err := e.media.Load(sha256)
	if err != nil {
		return "", err
	}

	var text strings.Builder
	_, err = m.Runner.Chat(ctx, api.ChatRequest{
		Model:    m.ID,
		Settings: m.settings,
		CacheKey: e.cacheKey(store.PurposeCaption),
		Recorder: e.recorder(a, store.PurposeCaption),
		Messages: []api.Message{{
			Role: api.RoleUser,
			Parts: []api.Part{
				{Type: api.PartText, Text: captionPrompt},
				{Type: api.PartImage, MIME: media.MIMEJPEG, Data: data},
			},
		}},
	}, func(c api.Chunk) error {
		if c.Kind == api.ChunkText {
			text.WriteString(c.Text)
		}
		return nil
	})

	// What was learned has to outlive a context a stop cancelled.
	keep := context.WithoutCancel(ctx)
	if err == nil && strings.TrimSpace(text.String()) == "" {
		err = errors.New("the model described nothing")
	}
	if err != nil {
		// A look that was cut short says nothing about the image, so nothing is
		// kept of it and the next reply asks again.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", err
		}
		if serr := e.store.SetCaptionError(keep, sha256, err.Error()); serr != nil {
			e.log.Error("keeping why an image was not described", "error", serr)
		}
		return "", err
	}

	caption := strings.TrimSpace(text.String())
	if err := e.store.SetCaption(keep, sha256, caption); err != nil {
		return "", err
	}
	return caption, nil
}
