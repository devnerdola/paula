package conversation

import (
	"context"
	"encoding/json"
	"math"
	"sync"
	"unicode/utf8"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/runners/api"
	"nerdola.dev/x/paula/internal/store"
)

const (
	// startRatio is what a character costs before a host has counted one of
	// this model's prompts: a token every 3.5 characters, which is short for
	// most text and so counts a little high.
	startRatio = 1 / 3.5
	// startImage is what a picture costs before one has been counted. A host
	// bills a picture by how big it is, and each of them by its own reckoning,
	// so there is no number that is right until one has been paid: this one is
	// high for the size Paula stores them at, and so counts a little high.
	startImage = 1000
)

// costs are what a prompt of a model comes to, by model. A host counts tokens,
// Paula counts characters and pictures, and what the two come to belongs to
// the model, so both are read back from the count a request comes home with.
type costs struct {
	mu sync.Mutex
	of map[string]cost
}

// cost is what one model charges for what a prompt is made of.
type cost struct {
	ratio float64
	image int
}

func (c *costs) at(model string) cost {
	c.mu.Lock()
	defer c.mu.Unlock()
	if v, ok := c.of[model]; ok {
		return v
	}
	return cost{ratio: startRatio, image: startImage}
}

func (c *costs) ratio(model string) float64 { return c.at(model).ratio }
func (c *costs) image(model string) int     { return c.at(model).image }

// correct reads back what a prompt the host counted came to. One with no
// picture in it says what a character costs. One with pictures says what a
// picture costs: what is left of the count once the characters are paid for is
// what the pictures came to, so the characters are paid for at the rate the
// prompts before it settled on. The tools a request offers are text the host
// counted as well.
func (c *costs) correct(model string, tokens int, messages []api.Message, offered string) {
	chars, images := measure(messages)
	chars += utf8.RuneCountInString(offered)
	if tokens <= 0 {
		return
	}
	was := c.at(model)
	now := was
	switch {
	case images == 0:
		if chars <= 0 {
			return
		}
		now.ratio = float64(tokens) / float64(chars)
	default:
		left := tokens - int(math.Ceil(float64(chars)*was.ratio))
		if left < images {
			// What is left of the count once the characters are paid for is
			// not a token for every picture, so what one costs is not in there
			// to be read.
			return
		}
		now.image = left / images
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.of == nil {
		c.of = map[string]cost{}
	}
	c.of[model] = now
}

// measure counts the characters of what is sent as text, and the pictures sent
// as pictures. The calls a reply made are text a model reads back: what each is
// called, and what it was asked with.
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
		for _, c := range m.ToolCalls {
			chars += utf8.RuneCountInString(c.Name) + utf8.RuneCountInString(c.Arguments)
		}
	}
	return chars, images
}

// toolsText is the tools a request offers as the text a model reads of them:
// what each is called, what it does, and what it takes.
func toolsText(offered []api.ToolDef) string {
	if len(offered) == 0 {
		return ""
	}
	b, err := json.Marshal(offered)
	if err != nil {
		return ""
	}
	return string(b)
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
	card := size([]api.Message{api.Text(api.RoleSystem, e.rendered)}, e.costs.ratio(m.Name), 0)
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
