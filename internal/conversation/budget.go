package conversation

import (
	"context"
	"math"
	"sync"
	"unicode/utf8"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/runners/api"
	"nerdola.dev/x/paula/internal/store"
)

// startRatio is what a character costs before a host has counted one of this
// model's prompts: a token every 3.5 characters, which is short for most text
// and so counts a little high.
const startRatio = 1 / 3.5

// ratios is what a character costs, by model. A host counts tokens and Paula
// counts characters, and what the two come to belongs to the model's
// tokenizer, so it is read back from the count a request comes home with.
type ratios struct {
	mu sync.Mutex
	of map[string]float64
}

func (r *ratios) ratio(model string) float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if v, ok := r.of[model]; ok {
		return v
	}
	return startRatio
}

// correct reads the ratio back from a prompt the host counted. One that
// carried an image says nothing about characters, since the image is in the
// count and not in them.
func (r *ratios) correct(model string, tokens int, messages []api.Message) {
	chars, images := measure(messages)
	if tokens <= 0 || chars <= 0 || images > 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.of == nil {
		r.of = map[string]float64{}
	}
	r.of[model] = float64(tokens) / float64(chars)
}

// measure counts the characters of what is sent as text, and the pictures sent
// as pictures.
func measure(messages []api.Message) (chars, images int) {
	for _, m := range messages {
		for _, p := range m.Parts {
			switch p.Type {
			case api.PartText:
				chars += utf8.RuneCountInString(p.Text)
			case api.PartImage:
				images++
			}
		}
	}
	return chars, images
}

// size is what messages take of a context: their characters at what one costs,
// and imageTokens for every picture, which is what the host bills for one.
func size(messages []api.Message, ratio float64, imageTokens int) int {
	chars, images := measure(messages)
	return int(math.Ceil(float64(chars)*ratio)) + images*imageTokens
}

// share is what part of a number a ratio comes to, rounded down, and never
// below nothing.
func share(of int, ratio float64) int {
	if of <= 0 {
		return 0
	}
	return int(math.Floor(float64(of) * ratio))
}

// split is what the model's context gives the system message and what it
// leaves the messages. A model whose context nothing says holds neither to
// anything, and both come back as zero.
func (e *Engine) split(m *model) (system, history int) {
	limit := m.limit()
	if limit <= 0 {
		return 0, 0
	}
	system = share(limit, e.cfg.SystemRatio)
	return system, limit - system
}

// checkRoom says at startup when the card fills the share of the context the
// system message has, leaving nothing for what a fold writes: memories then
// take a share of nothing and the summary is never written again, however long
// it grows. The run goes on, since the prompt still holds itself to the
// context by leaving the oldest exchanges out, and a card is the owner's to
// write.
func (e *Engine) checkRoom(ctx context.Context) {
	m, err := e.roleModel(ctx, config.RoleChat)
	if err != nil {
		// A setup with no chat model is said out loud by the startup checks.
		return
	}
	system, _ := e.split(m)
	if system <= 0 {
		// Nothing says what the model holds, so nothing is divided.
		return
	}
	card := size([]api.Message{api.Text(api.RoleSystem, e.rendered)}, e.ratios.ratio(m.Name), 0)
	if card < system {
		return
	}
	e.log.Warn("the card leaves the system message no room for memories or the summary",
		"model", m.Name, "card", card, "share", system, "context", m.limit())
}

// limit is the context a prompt is held to: what the file sets for the model,
// or the largest its catalogue reports. Zero is no limit, which is what a file
// that sets none and a runner that reports none come to.
func (m *model) limit() int {
	if m.Context > 0 {
		return m.Context
	}
	if m.catalogue != nil {
		return m.catalogue.Context
	}
	return 0
}

// exchanges splits the conversation where a reply closes one: the messages
// sent since the reply before it, and the reply that answered them. They are
// dropped whole, so a reply is never left without what it answers.
func exchanges(messages []store.Message) [][]store.Message {
	var out [][]store.Message
	var current []store.Message
	var answered bool
	for _, m := range messages {
		if answered && m.Role != store.RoleAssistant {
			out = append(out, current)
			current, answered = nil, false
		}
		current = append(current, m)
		answered = answered || m.Role == store.RoleAssistant
	}
	if len(current) > 0 {
		out = append(out, current)
	}
	return out
}
