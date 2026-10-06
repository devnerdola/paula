package conversation

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/media"
	"nerdola.dev/x/paula/internal/runners/api"
	"nerdola.dev/x/paula/internal/store"
)

// captionPrompt is what a model with vision is asked of an image.
const captionPrompt = "Describe this image in detail, including any abnormal features."

// described is the line that stands for an image that is not sent as one. The
// reply that needs it asks for it, and an image is asked about once.
func (e *Engine) described(ctx context.Context, a *attempt, sha256 string) string {
	if line, ok := e.captions.Load(sha256); ok {
		return line.(string)
	}
	m, err := e.store.Media(ctx, sha256)
	if err != nil {
		e.log.Warn("reading what an image shows", "sha256", sha256, "error", err)
		return "[photo]"
	}
	if m.Caption != "" {
		return e.caption(sha256, m.Caption)
	}
	if m.CaptionError != "" {
		// One that could not be described is never asked about again, so
		// what it reads as is settled too.
		e.captions.Store(sha256, "[photo]")
		return "[photo]"
	}
	caption, err := e.ask(ctx, a, sha256)
	if err != nil {
		// No vision model, and a look that was cut short, are both asked again.
		if !errors.Is(err, errNoModel) && !api.Gone(err) {
			e.log.Warn("describing an image", "sha256", sha256, "error", err)
		}
		return "[photo]"
	}
	return e.caption(sha256, caption)
}

// caption is the line a described picture reads as, kept for the prompts and
// the compactions that follow: what a picture shows is written down once.
func (e *Engine) caption(sha256, caption string) string {
	line := "[photo: " + caption + "]"
	e.captions.Store(sha256, line)
	return line
}

// known is what a picture is written as, as far as anything has been asked. It
// asks nothing itself: it is what weighing an exchange reads, and weighing one
// is not what makes a model look at a picture.
func (e *Engine) known(ctx context.Context, sha256 string) string {
	if line, ok := e.captions.Load(sha256); ok {
		return line.(string)
	}
	m, err := e.store.Media(ctx, sha256)
	if err != nil || m.Caption == "" {
		return "[photo]"
	}
	return e.caption(sha256, m.Caption)
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
	empty := err == nil && strings.TrimSpace(text.String()) == ""
	if empty {
		err = errors.New("the model described nothing")
	}
	if err != nil {
		// What is kept is what the host said of the picture: nothing, or a
		// refusal of the request itself. A look that was cut short, a host that
		// was busy or down and a connection that went say nothing about the
		// image, so nothing is kept of them and the next reply asks again.
		if !empty && !settled(err) {
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

// settled reports whether an error is the host refusing the request itself: a
// status in the 400s, but for the two that ask for the request again later, a
// request timeout and a rate limit.
func settled(err error) bool {
	var e *api.APIError
	if !errors.As(err, &e) {
		return false
	}
	return e.Status >= 400 && e.Status < 500 &&
		e.Status != http.StatusRequestTimeout && e.Status != http.StatusTooManyRequests
}
