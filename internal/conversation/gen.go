//go:build ignore

// Command gen writes testdata/past.json.gz, the conversation the live test
// starts from: months of texting between the two people of the example card,
// written by a model playing both of them. It runs by hand from this
// directory,
//
//	OPENROUTER_API_KEY=... go run gen.go
//
// and asks the model for about a hundred answers, which cost money, so nothing
// runs it on its own. It speaks to the API directly rather than through
// Paula's runners, which the live test is there to try. See
// testdata/SOURCES.md.
package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	model = "openai/gpt-5-mini"
	url   = "https://openrouter.ai/api/v1/chat/completions"
	days  = 100
)

// message is one text of the past: when it was sent in its day, who sent it,
// "user" or "character", and what it said.
type message struct {
	At   string `json:"at"`
	From string `json:"from"`
	Text string `json:"text"`
}

type past struct {
	Model   string      `json:"model"`
	Written string      `json:"written"`
	Caio    string      `json:"caio"`
	Plan    []string    `json:"plan"`
	Days    [][]message `json:"days"`
}

const planPrompt = `Here is the character card of Paula, who texts with her partner Caio:

%s
Invent Caio: a job, a routine, friends and what Caio cares about. Do not give any of Caio's relatives a name. Then plan %d consecutive days of their lives: work, plans, small events, moods, a trip, a cold, a fight and making up, and the ordinary things a couple texts about. Never name a weekday, a month or a date.

Write everything in the third person. First one line, "Who Caio is: ...", and then one line per day, "Day N: what happens that day".`

const dayPrompt = `Here is the character card of Paula, who texts with her partner Caio:

%s
%s

Day %d of their story: %s
The day before: %s
The day after: %s

Write every text message Caio and Paula send each other on day %d, between 100 and 150 messages, the way a couple really texts through a day: a few conversations, when Caio wakes up, around lunch, after work and at night, each of them messages a minute or a few apart, with hours of silence between the conversations. Caio texts first, and every message of Paula's answers something Caio sent. Either of them sometimes sends two or three messages in a row. Paula texts the way her card says. Messages are short and casual, like real texts. Never name a weekday, a month or a date.

Write one message per line, as HH:MM Name: text, and nothing else. The times are between 06:00 and 23:59, and each is the same as the one before it or later.`

var (
	dayLine  = regexp.MustCompile(`^\**Day (\d+)\**:\**\s*(.+)$`)
	textLine = regexp.MustCompile(`^(\d{1,2}):(\d{2}) (Caio|Paula): (.+)$`)
)

func main() {
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		fail(errors.New("OPENROUTER_API_KEY is not set"))
	}
	card, err := os.ReadFile("../../personas/paula.example.yaml")
	if err != nil {
		fail(err)
	}

	answer, err := ask(key, fmt.Sprintf(planPrompt, card, days))
	if err != nil {
		fail(err)
	}
	out := past{Model: model, Written: time.Now().UTC().Format(time.DateOnly), Plan: make([]string, days)}
	for line := range strings.SplitSeq(answer, "\n") {
		line = strings.TrimSpace(line)
		if who, ok := strings.CutPrefix(line, "Who Caio is:"); ok {
			out.Caio = "Caio: " + strings.TrimSpace(who)
			continue
		}
		if m := dayLine.FindStringSubmatch(line); m != nil {
			n, _ := strconv.Atoi(m[1])
			if n >= 1 && n <= days {
				out.Plan[n-1] = m[2]
			}
		}
	}
	for i, p := range out.Plan {
		if p == "" || out.Caio == "" {
			fail(fmt.Errorf("the plan has no Caio or no day %d:\n%s", i+1, answer))
		}
	}

	out.Days = make([][]message, days)
	var wg sync.WaitGroup
	errs := make([]error, days)
	slots := make(chan struct{}, 8)
	for i := range days {
		wg.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			before, after := "nothing yet", "the story goes on"
			if i > 0 {
				before = out.Plan[i-1]
			}
			if i < days-1 {
				after = out.Plan[i+1]
			}
			prompt := fmt.Sprintf(dayPrompt, card, out.Caio, i+1, out.Plan[i], before, after, i+1)
			for range 4 {
				answer, err := ask(key, prompt)
				if err == nil {
					out.Days[i], err = parse(answer)
					if err == nil && len(out.Days[i]) < 60 {
						err = fmt.Errorf("%d messages", len(out.Days[i]))
					}
					if err == nil {
						fmt.Fprintf(os.Stderr, "day %d: %d messages\n", i+1, len(out.Days[i]))
						errs[i] = nil
						return
					}
				}
				errs[i] = fmt.Errorf("day %d: %w", i+1, err)
				fmt.Fprintln(os.Stderr, errs[i])
			}
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		fail(err)
	}

	var b bytes.Buffer
	z := gzip.NewWriter(&b)
	if err := json.NewEncoder(z).Encode(out); err != nil {
		fail(err)
	}
	if err := z.Close(); err != nil {
		fail(err)
	}
	if err := os.WriteFile("testdata/past.json.gz", b.Bytes(), 0o644); err != nil {
		fail(err)
	}
}

// parse reads the messages of a day off the lines the model wrote, and leaves
// out any line that is not one. A day whose times are not times of a day, or go
// back, is not one that happened, and is asked for again.
func parse(answer string) ([]message, error) {
	var out []message
	for line := range strings.SplitSeq(answer, "\n") {
		m := textLine.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		from := "user"
		if m[3] == "Paula" {
			from = "character"
		}
		hour, _ := strconv.Atoi(m[1])
		minute, _ := strconv.Atoi(m[2])
		at := fmt.Sprintf("%02d:%02d", hour, minute)
		if hour > 23 || minute > 59 {
			return nil, fmt.Errorf("the time %s", at)
		}
		if n := len(out); n > 0 && at < out[n-1].At {
			return nil, fmt.Errorf("%s after %s", at, out[n-1].At)
		}
		out = append(out, message{At: at, From: from, Text: m[4]})
	}
	return out, nil
}

func ask(key, prompt string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"model":     model,
		"messages":  []map[string]string{{"role": "user", "content": prompt}},
		"provider":  map[string]any{"zdr": true},
		"reasoning": map[string]any{"effort": "low"},
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	res, err := (&http.Client{Timeout: 10 * time.Minute}).Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return "", err
	}
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s", res.Status, raw)
	}
	var answer struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		return "", err
	}
	if len(answer.Choices) == 0 {
		return "", fmt.Errorf("no answer: %s", raw)
	}
	return answer.Choices[0].Message.Content, nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
