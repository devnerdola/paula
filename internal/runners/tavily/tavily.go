// Package tavily talks to Tavily, which searches the web and reads pages for a
// model to use, and serves no models of its own.
package tavily

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/runners/api"
	"nerdola.dev/x/paula/internal/runners/transport"
)

// Kind is the type written in the configuration file.
const Kind = "tavily"

const (
	defaultURL      = "https://api.tavily.com"
	defaultTokenEnv = "TAVILY_API_KEY"
	defaultRetries  = 2
)

// defaultIdleTimeout is how long a request may go without a byte.
const defaultIdleTimeout = config.Duration(2 * time.Minute)

// mostResults is the most results Tavily documents a search answering with.
const mostResults = 20

type Runner struct {
	name   string
	client *transport.Client
}

// Open reads a runner section and builds the runner it describes. It takes
// the keys of its connection and nothing else, since it serves no models to
// lay settings over.
func Open(name string, s config.Section, h api.Host) (*Runner, error) {
	retries := defaultRetries
	c, p := transport.DecodeConnection(s, transport.Connection{
		URL:            defaultURL,
		TokenEnv:       defaultTokenEnv,
		IdleTimeout:    defaultIdleTimeout,
		RequestTimeout: config.Duration(transport.DefaultRequestTimeout),
		Retries:        &retries,
	})
	if err := p.Err(); err != nil {
		return nil, err
	}
	return &Runner{name: name, client: c.Client(name, h, answers{})}, nil
}

func (r *Runner) Name() string { return r.name }
func (r *Runner) Kind() string { return Kind }

// URL is the base the runner sends its requests to.
func (r *Runner) URL() string { return r.client.BaseURL }

// Health asks what the key has spent of its credits, which Tavily answers
// only for a key it takes.
func (r *Runner) Health(ctx context.Context) error {
	return r.client.Get(ctx, "/usage", new(struct{}))
}

func (r *Runner) MostResults() int { return mostResults }

// Search asks Tavily what a query finds on the web, with the day each page was
// published when Tavily knows it.
func (r *Runner) Search(ctx context.Context, req api.SearchRequest) ([]api.SearchResult, error) {
	var out struct {
		Results []struct {
			Title         string `json:"title"`
			URL           string `json:"url"`
			Content       string `json:"content"`
			PublishedDate string `json:"published_date"`
		} `json:"results"`
	}
	in := map[string]any{"query": req.Query, "max_results": req.Limit, "include_published_date": true}
	if err := r.client.Post(ctx, "/search", in, &out, req.Recorder); err != nil {
		return nil, err
	}
	found := make([]api.SearchResult, len(out.Results))
	for i, res := range out.Results {
		found[i] = api.SearchResult{Title: res.Title, URL: res.URL, Content: res.Content, Date: res.PublishedDate}
	}
	return found, nil
}

// Read asks Tavily for the text of a page, which it gives as markdown. A page
// it could not read is answered with a 200 all the same, named among the
// failed results with why.
func (r *Runner) Read(ctx context.Context, req api.PageRequest) (string, error) {
	var out struct {
		Results []struct {
			RawContent string `json:"raw_content"`
		} `json:"results"`
		FailedResults []struct {
			Error string `json:"error"`
		} `json:"failed_results"`
	}
	if err := r.client.Post(ctx, "/extract", map[string]any{"urls": []string{req.URL}}, &out, req.Recorder); err != nil {
		return "", err
	}
	switch {
	case len(out.FailedResults) > 0:
		return "", errors.New(out.FailedResults[0].Error)
	case len(out.Results) == 0:
		return "", errors.New("the answer holds no page")
	}
	return out.Results[0].RawContent, nil
}

// answers are how Tavily answers a request.
type answers struct{}

var _ transport.Answers = answers{}

// Retry sends a 429 again after the seconds Tavily documents in Retry-After.
func (answers) Retry(_ time.Time, status int, h http.Header) (time.Duration, bool) {
	if status != http.StatusTooManyRequests {
		return 0, false
	}
	if n, err := strconv.Atoi(h.Get("Retry-After")); err == nil && n >= 0 {
		return time.Duration(n) * time.Second, true
	}
	return 0, true
}

// Error reads the two shapes Tavily documents: a detail that holds the error,
// and a detail that lists every field that failed validation.
func (answers) Error(status int, body []byte) *api.APIError {
	var text struct {
		Detail struct {
			Error string `json:"error"`
		} `json:"detail"`
	}
	if err := json.Unmarshal(body, &text); err == nil && text.Detail.Error != "" {
		return &api.APIError{Status: status, Message: text.Detail.Error}
	}
	// Where a field failed is a path of names, and of positions in a list.
	var list struct {
		Detail []struct {
			Loc []json.RawMessage `json:"loc"`
			Msg string            `json:"msg"`
		} `json:"detail"`
	}
	if err := json.Unmarshal(body, &list); err != nil || len(list.Detail) == 0 {
		return nil
	}
	fields := make([]string, len(list.Detail))
	for i, d := range list.Detail {
		path := make([]string, len(d.Loc))
		for j, step := range d.Loc {
			var name string
			if json.Unmarshal(step, &name) != nil {
				name = string(step)
			}
			path[j] = name
		}
		fields[i] = strings.Join(path, ".") + ": " + d.Msg
	}
	return &api.APIError{Status: status, Message: strings.Join(fields, "; ")}
}
