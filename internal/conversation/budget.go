package conversation

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"sync"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/runners"
	"nerdola.dev/x/paula/internal/runners/api"
)

const (
	// startRate is what a word costs before a host has counted one of this
	// model's prompts: three tokens, more than any prompt has been counted at,
	// so a run that has counted nothing yet counts high.
	startRate = 3
	// startImage is what a picture costs before one has been counted. A host
	// bills a picture by how big it is, and each of them by its own reckoning,
	// so there is no number that is right until one has been paid: this one is
	// high for the size Paula stores them at, and so counts a little high.
	startImage = 1000
)

// costs are what a prompt of a model comes to, by model. A host counts tokens,
// Paula counts words and pictures, and what the two come to belongs to the
// model, so both are read back from the count a reply's prompt comes home
// with.
//
// A word does not cost the same in every prompt: a history of short messages,
// each with a time before it, costs more a word than the card and a summary
// do. What a word costs is the most two prompts in a row have both come to, so
// what is measured is not less than what it is, and a prompt that came to more
// once, and not again, is an exception left out of it.
type costs struct {
	mu sync.Mutex
	of map[string]cost
}

// cost is what one model charges for what a prompt is made of. counted says a
// host has counted one of its prompts; until then a word costs startRate.
// last is what a word came to in the prompt counted before: a higher rate is
// taken only when the prompt after it comes to more as well. imaged says what
// a picture costs has been read; until then one costs startImage. tokens,
// words and pictures are the prompt counted before.
type cost struct {
	rate    float64
	last    float64
	image   int
	counted bool
	imaged  bool

	tokens, words, pictures int
}

// read takes what a word came to in one prompt. The first is what a word
// costs; after it, a word costs more once two prompts in a row have come to
// more, and then as much as the lower of the two.
func (c *cost) read(rate float64) {
	if !c.counted {
		c.rate = rate
	} else if both := min(rate, c.last); both > c.rate {
		c.rate = both
	}
	c.last = rate
	c.counted = true
}

func (c *costs) at(model string) cost {
	c.mu.Lock()
	defer c.mu.Unlock()
	if v, ok := c.of[model]; ok {
		return v
	}
	return cost{rate: startRate, image: startImage}
}

func (c *costs) rate(model string) float64 { return c.at(model).rate }
func (c *costs) image(model string) int    { return c.at(model).image }

// correct reads back what the prompt of a reply came to as the host counted
// it. What a word costs is the count, less the pictures at what one costs,
// over the words. Until what a picture costs has been read, the pictures count
// as part of the words, which errs high.
//
// A prompt that carries more pictures than the one before it, and at least
// its words, says what a picture costs: each count is its words at what a word
// costs and its pictures at what a picture costs, and two counts give both. A
// prompt with fewer words has had its history compacted, and its words are
// not the same mix as the ones before. The tools a request offers are text the
// host counted as well.
func (c *costs) correct(model string, tokens int, messages []api.Message, offered string) {
	words, images := measure(messages)
	words += wordsIn(offered)
	if tokens <= 0 || words <= 0 {
		return
	}
	was := c.at(model)
	now := was
	if was.tokens > 0 && images > was.pictures && words >= was.words {
		t1, w1, p1 := float64(was.tokens), float64(was.words), float64(was.pictures)
		t2, w2, p2 := float64(tokens), float64(words), float64(images)
		if d := p2*w1 - p1*w2; d > 0 {
			if image := (t2*w1 - t1*w2) / d; image >= 1 {
				now.image, now.imaged = int(math.Round(image)), true
			}
		}
	}
	left := tokens
	if now.imaged {
		left -= images * now.image
	}
	if left > 0 {
		now.read(float64(left) / float64(words))
	}
	now.tokens, now.words, now.pictures = tokens, words, images

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.of == nil {
		c.of = map[string]cost{}
	}
	c.of[model] = now
}

// measure counts the words of what is sent as text, and the pictures sent as
// pictures. The calls a reply made are text a model reads back: what each is
// called, and what it was asked with.
func measure(messages []api.Message) (words, images int) {
	for _, m := range messages {
		for _, p := range m.Parts {
			switch p.Type {
			case api.PartText:
				words += wordsIn(p.Text)
			case api.PartImage:
				images++
			}
		}
		for _, c := range m.ToolCalls {
			words += wordsIn(c.Name) + wordsIn(c.Arguments)
		}
		// What a message thought goes with it, and is counted once: details
		// carry the same text in the shape their host sent it.
		if m.Reasoning != nil {
			words += wordsIn(m.Reasoning.Text)
		}
	}
	return words, images
}

// wordsIn is how many words a text holds.
func wordsIn(text string) int { return len(strings.Fields(text)) }

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

// size is what messages take of a context: their words at what one costs, and
// imageTokens for every picture, which is what the host bills for one.
func size(messages []api.Message, rate float64, imageTokens int) int {
	words, images := measure(messages)
	return int(math.Ceil(float64(words)*rate)) + images*imageTokens
}

// share is what part of a number a ratio comes to, rounded down, and never
// below nothing.
func share(of int, ratio float64) int {
	if of <= 0 {
		return 0
	}
	return int(math.Floor(float64(of) * ratio))
}

// reservations are what the summary and the history are held to: their ratios
// of what the model's context leaves once the persona and the tools are
// written. What the two leave of it is the user input's, which is what lets
// the turn that takes the history past its reservation go out whole. A model
// whose context nothing says reserves nothing.
func (e *Engine) reservations(m *model) (summary, history int) {
	limit := m.limit()
	if limit <= 0 {
		return 0, 0
	}
	fixed := size([]api.Message{
		api.Text(api.RoleSystem, e.card(m)),
		api.Text(api.RoleSystem, e.tools.text),
	}, e.costs.rate(m.Name), 0)
	flexible := limit - fixed
	return share(flexible, e.cfg.SummaryRatio), share(flexible, e.cfg.HistoryRatio)
}

// holdCard refuses a model that can serve the chat role and whose context
// the card and the tools fill on their own. Nothing has been counted when a
// run starts, so they are measured at a token a word, the least a word comes
// to: what this refuses fits no count a host could make of it.
func (e *Engine) holdCard(ctx context.Context) error {
	if e.runners == nil {
		return nil
	}
	// A model whose catalogue cannot be read is reported where the models
	// are checked.
	serving, _ := e.runners.Serving(ctx, config.RoleChat)
	p := &api.Problems{}
	for _, c := range serving {
		catalogue, err := e.runners.Lookup(ctx, c)
		if err != nil {
			continue
		}
		m := &model{Configured: c, catalogue: catalogue, settings: runners.WithDefaults(c.Settings, catalogue)}
		fixed := size([]api.Message{
			api.Text(api.RoleSystem, e.card(m)),
			api.Text(api.RoleSystem, e.tools.text),
		}, 1, 0)
		if limit := m.limit(); limit > 0 && fixed >= limit {
			p.Addf("%s: the card and the tools come to at least %d tokens, and its context holds %d", c.Path, fixed, limit)
		}
	}
	return p.Err()
}

// excess is how far a prompt, with the tools it offers, is past the model's
// context, and zero when it is within it or nothing says what the context
// holds. It is zero as well until a host has counted a prompt of the model:
// until then a word counts high on purpose, and a prompt held back on that
// count would never be sent to be counted.
func (e *Engine) excess(m *model, messages []api.Message) int {
	c := e.costs.at(m.Name)
	if m.limit() <= 0 || !c.counted {
		return 0
	}
	n := size(messages, c.rate, c.image) + size([]api.Message{api.Text(api.RoleSystem, e.tools.text)}, c.rate, 0)
	return max(0, n-m.limit())
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

// output is the most the model writes in one answer: what its catalogue says,
// or the max_tokens the file holds it to when that is less. Zero is no limit,
// which is what a catalogue that says nothing and a file that sets nothing
// come to.
func (m *model) output() int {
	var out int
	if m.catalogue != nil {
		out = m.catalogue.Output
	}
	if v := m.settings.Output.MaxTokens; v != nil && (out == 0 || *v < out) {
		out = *v
	}
	return out
}
