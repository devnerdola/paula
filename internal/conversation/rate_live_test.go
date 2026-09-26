package conversation

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"nerdola.dev/x/paula/internal/runners"
	"nerdola.dev/x/paula/internal/runners/api"
)

// TestLiveTheRate holds what a word costs to the counts a real host gives the
// prompts of a conversation: only the first round of a reply is read, and a
// higher rate is taken once two replies in a row come to more, at the lower of
// the two. It texts the chat model of the file PAULA_LIVE names, from an empty
// conversation, and asks her once to keep something and once for everything
// she kept, which only the memory tools can tell her, so replies run tools. A
// model that sees is sent a picture with the second text: that prompt and the
// one before it say what a picture costs, and every prompt after it is read
// less the picture. Everything goes to a data directory of its own, with a
// report.md of what every round was counted at.
//
//	PAULA_LIVE=live.yaml go test ./internal/conversation -run TestLiveTheRate -v -timeout 20m
func TestLiveTheRate(t *testing.T) {
	path := os.Getenv("PAULA_LIVE")
	if path == "" {
		t.Skip("PAULA_LIVE names no configuration file")
	}
	lv := openLive(t, path)
	defer lv.close()

	// A picture large enough for a model to see what it shows, as
	// testdata/SOURCES.md of the media package says.
	photo, err := os.ReadFile(filepath.Join("..", "media", "testdata", "red-blue-800x400.png"))
	if err != nil {
		t.Fatal(err)
	}

	// Every request is kept with what its host counted it at, so each round's
	// rate is read from the messages it carried.
	counted := &countedRounds{}
	for _, m := range lv.set.Models {
		m.Runner = &countingRunner{Runner: m.Runner, rounds: counted}
	}
	clock := &pastClock{}
	clock.set(time.Now())
	r := lv.run("rate", clock)
	name := r.model().Name
	sees := r.model().catalogue.Vision

	lv.printf("\n## Replies")
	// want is the rate a word costs after the readings so far, and last the
	// latest of them. image is what a picture costs once it has been read,
	// and zero before. before is the first round of the reply before.
	want, last, read := float64(startRate), 0.0, false
	var before *countedRound
	image, withTools := 0, 0
	for i, text := range []string{
		"hey, how was your day?",
		"I just got home, the traffic was awful",
		"Please remember this: my sister Beatriz moved to Recife last month.",
		"what are you making for dinner tonight?",
		"I think I'll just order something, too tired to cook",
		"Tell me everything you have saved in your memories about me.",
		"she says it's hot there all year, I'm jealous",
		"anyway, what are you up to this weekend?",
		"maybe we could go to that pasta place on saturday",
		"ok I need to sleep, talk tomorrow",
	} {
		var photos [][]byte
		if i == 1 && sees {
			photos = [][]byte{photo}
		}
		n := counted.len()
		r.send([]string{text}, nil, photos...)
		rounds := counted.since(n)
		entry := int64(r.turns[len(r.turns)-1].entry)
		if len(rounds) == 0 {
			lv.check("every reply is counted", false, "a counted round", "none", entry)
			continue
		}
		c := r.e.costs.at(name)
		var at []string
		for _, round := range rounds {
			at = append(at, fmt.Sprintf("%d tokens, %d words, %d pictures", round.tokens, round.words(r.e), round.pictures()))
		}
		lv.printf("  - rounds: %v; the rate read %.3f, kept %.3f; a picture costs %d", at, c.last, c.rate, c.image)

		// A first round with a picture more than the one before, and at least
		// its words, says what a picture costs: each count is its words and
		// its pictures at what each costs, and the two give both.
		first := rounds[0]
		if before != nil && first.pictures() > before.pictures() && first.words(r.e) >= before.words(r.e) {
			t1, w1, p1 := float64(before.tokens), float64(before.words(r.e)), float64(before.pictures())
			t2, w2, p2 := float64(first.tokens), float64(first.words(r.e)), float64(first.pictures())
			image = int(math.Round((t2*w1 - t1*w2) / (p2*w1 - p1*w2)))
			lv.check(fmt.Sprintf("reply %d reads what a picture costs", i+1), c.image == image,
				fmt.Sprintf("%d", image), fmt.Sprintf("%d", c.image), entry)
		}
		// What a word costs is the count less the pictures, over the words;
		// until a picture's cost is read, the pictures count as words.
		left := first.tokens - first.pictures()*image
		reading := float64(left) / float64(first.words(r.e))
		if !read {
			want = reading
		} else if both := min(reading, last); both > want {
			want = both
		}
		last, read = reading, true
		lv.check(fmt.Sprintf("reply %d reads its first round", i+1), c.last == last,
			fmt.Sprintf("%.4f", last), fmt.Sprintf("%.4f", c.last), entry)
		lv.check(fmt.Sprintf("reply %d keeps the most two readings in a row came to", i+1), c.rate == want,
			fmt.Sprintf("%.4f", want), fmt.Sprintf("%.4f", c.rate), entry)
		before = &first
		if len(rounds) > 1 {
			withTools++
		}
	}
	r.end()

	lv.check("a reply ran a tool", withTools > 0, "at least one reply of more than one round",
		fmt.Sprintf("%d", withTools))
	if sees {
		lv.check("a picture's cost is read", image > 0, "a cost read when the picture came", fmt.Sprintf("%d", image))
	}
	lv.printf("\n%d checks passed, %d failed.", lv.passed, lv.failed)
}

// countedRound is one request a reply sent, and what its host counted it at.
type countedRound struct {
	messages []api.Message
	tokens   int
}

// words are the words of the round's messages and of the tools offered.
func (c countedRound) words(e *Engine) int {
	words, _ := measure(c.messages)
	return words + wordsIn(e.tools.text)
}

// rate is what a word of the round came to: the host's count over its words.
func (c countedRound) rate(e *Engine) float64 {
	return float64(c.tokens) / float64(c.words(e))
}

// pictures are how many pictures the round carried.
func (c countedRound) pictures() int {
	_, images := measure(c.messages)
	return images
}

type countedRounds struct {
	mu     sync.Mutex
	rounds []countedRound
}

func (c *countedRounds) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.rounds)
}

func (c *countedRounds) since(n int) []countedRound {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]countedRound(nil), c.rounds[n:]...)
}

// countingRunner is a runner that keeps every request of a reply it sends with
// what the host counted it at. A reply is the one request that says how much
// of it the next sends again.
type countingRunner struct {
	runners.Runner
	rounds *countedRounds
}

func (r *countingRunner) Chat(ctx context.Context, req api.ChatRequest, fn func(api.Chunk) error) (*api.Result, error) {
	res, err := r.Runner.Chat(ctx, req, fn)
	if res != nil && req.Standing > 0 {
		r.rounds.mu.Lock()
		r.rounds.rounds = append(r.rounds.rounds, countedRound{messages: req.Messages, tokens: res.Usage.PromptTokens})
		r.rounds.mu.Unlock()
	}
	return res, err
}
