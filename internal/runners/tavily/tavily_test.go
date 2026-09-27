package tavily

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/runners/api"
)

func read(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// section reads a configuration file whose tavily runner holds what is given.
func section(t *testing.T, body string) config.Section {
	t.Helper()
	path := filepath.Join(t.TempDir(), "paula.yaml")
	file := "persona: paula.yaml\nrunners:\n  tavily:\n    type: tavily\n"
	for line := range strings.SplitSeq(body, "\n") {
		if line != "" {
			file += "    " + line + "\n"
		}
	}
	file += "models:\n  chat:\n    runner: tavily\n    id: x\ndefault_models:\n  chat: chat\n"
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Runners[0].Section
}

func runner(t *testing.T, url string) *Runner {
	t.Helper()
	t.Setenv("TAVILY_API_KEY", "test-token-abcdefgh")
	r, err := Open("tavily", section(t, "url: "+url), api.Host{})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// posted serves one captured file with a status, and keeps the path and the
// body of what it was sent.
func posted(t *testing.T, status int, file string) (*httptest.Server, *string, *map[string]any) {
	t.Helper()
	body := read(t, file)
	var path string
	var sent map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		json.NewDecoder(r.Body).Decode(&sent)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write(body)
	}))
	t.Cleanup(ts.Close)
	return ts, &path, &sent
}

type records struct{ ended []*api.Record }

func (r *records) StartRequest(context.Context, *api.Record) error { return nil }

func (r *records) EndRequest(_ context.Context, rec *api.Record) error {
	r.ended = append(r.ended, rec)
	return nil
}

// A search is sent the query, how many pages to answer with, and a request for
// the day each was published; it is kept by the recorder it carries, and is
// what Tavily found, with no date where Tavily knows none.
func TestASearchIsWhatTavilyFound(t *testing.T) {
	ts, path, sent := posted(t, http.StatusOK, "search.json")
	r := runner(t, ts.URL)
	kept := &records{}
	found, err := r.Search(context.Background(), api.SearchRequest{Query: "concertos em Lisboa esta semana", Limit: 5, Recorder: kept})
	if err != nil {
		t.Fatal(err)
	}
	if *path != "/search" || (*sent)["query"] != "concertos em Lisboa esta semana" || (*sent)["max_results"] != 5.0 ||
		(*sent)["include_published_date"] != true {
		t.Errorf("sent %v to %s", *sent, *path)
	}
	if len(kept.ended) != 1 || kept.ended[0].Status != http.StatusOK {
		t.Errorf("records = %+v, want the search kept", kept.ended)
	}
	if len(found) != 5 {
		t.Fatalf("found %d pages, want the five Tavily answered with", len(found))
	}
	first := found[0]
	if first.Title != "Concertos em Lisboa: a agenda desta semana (21-27 Set)" ||
		first.URL != "https://www.timeout.pt/lisboa/pt/musica/os-melhores-concertos-em-lisboa-esta-semana" ||
		!strings.Contains(first.Content, "\n\nO B.leza acolhe uma nova edição da noite B.Leza Atómica") ||
		first.Date != "Mon, 14 Sep 2026 16:00:00 GMT" {
		t.Errorf("the first page = %+v", first)
	}
	if found[2].URL != "https://www.agendalx.pt" || found[2].Date != "" {
		t.Errorf("the third page = %+v, want no date", found[2])
	}
}

// A page is read as the markdown Tavily made of it, and one Tavily could not
// read, which it answers with a 200 all the same, is refused in its words.
func TestAPageIsWhatTavilyRead(t *testing.T) {
	ts, path, sent := posted(t, http.StatusOK, "extract.json")
	url := "https://www.timeout.pt/lisboa/pt/musica/os-melhores-concertos-em-lisboa-esta-semana"
	got, err := runner(t, ts.URL).Read(context.Background(), api.PageRequest{URL: url})
	if err != nil {
		t.Fatal(err)
	}
	urls, _ := (*sent)["urls"].([]any)
	if *path != "/extract" || len(urls) != 1 || urls[0] != url {
		t.Errorf("sent %v to %s", *sent, *path)
	}
	if !strings.Contains(got, "Cais do Gás, 1 (Cais do Sodré). 24 Set (Qui) 21.30. 10€*") {
		t.Errorf("the page reads %q", got[:min(len(got), 200)])
	}

	ts, _, _ = posted(t, http.StatusOK, "extract_failed.json")
	if _, err := runner(t, ts.URL).Read(context.Background(), api.PageRequest{URL: "https://www.reddit.com/r/lisboa/"}); err == nil ||
		err.Error() != "Failed to fetch url" {
		t.Errorf("a page Tavily could not read = %v", err)
	}
}

// Both shapes Tavily writes an error in are read: a detail that holds it, and
// a detail that lists every field that failed.
func TestCapturedErrors(t *testing.T) {
	for _, tc := range []struct {
		file   string
		status int
		want   string
	}{
		{"error_unauthorized.json", http.StatusUnauthorized, "Unauthorized: missing or invalid API key."},
		{"error_invalid.json", http.StatusUnprocessableEntity, "body.max_results.literal['auto']: Input should be 'auto'; " +
			"body.max_results.int: Input should be a valid integer, unable to parse string as an integer"},
		{"error_invalid_item.json", http.StatusUnprocessableEntity, "body.include_domains.1: Input should be a valid string"},
	} {
		ts, _, _ := posted(t, tc.status, tc.file)
		_, err := runner(t, ts.URL).Search(context.Background(), api.SearchRequest{Query: "concertos em Lisboa esta semana", Limit: 5})
		var e *api.APIError
		if !errors.As(err, &e) || e.Status != tc.status || e.Message != tc.want {
			t.Errorf("%s = %v, want %d: %s", tc.file, err, tc.status, tc.want)
		}
	}
}

// A 429 is sent again after the seconds Tavily names in Retry-After, and at
// once when it names none; no other status is.
func TestATooManyRequestsIsSentAgainAfterTheWaitItNames(t *testing.T) {
	now := time.Now()
	h := http.Header{"Retry-After": {"3"}}
	if d, ok := (answers{}).Retry(now, http.StatusTooManyRequests, h); !ok || d != 3*time.Second {
		t.Errorf("retry = %s, %v, want 3s", d, ok)
	}
	if d, ok := (answers{}).Retry(now, http.StatusTooManyRequests, http.Header{}); !ok || d != 0 {
		t.Errorf("retry = %s, %v, want no delay", d, ok)
	}
	if _, ok := (answers{}).Retry(now, http.StatusInternalServerError, h); ok {
		t.Error("a status Tavily does not name is sent again")
	}
}

// A tavily runner takes the keys of its connection and nothing else, since it
// serves no models to lay settings over, and needs its key.
func TestOpenTakesTheConnectionAndNothingElse(t *testing.T) {
	if _, err := Open("tavily", section(t, ""), api.Host{}); err == nil ||
		!strings.Contains(err.Error(), "runners.tavily: token_env: environment variable TAVILY_API_KEY is not set") {
		t.Errorf("a runner with no key = %v", err)
	}
	t.Setenv("TAVILY_API_KEY", "test-token-abcdefgh")
	if _, err := Open("tavily", section(t, "sampling:\n  temperature: 0.7"), api.Host{}); err == nil ||
		!strings.Contains(err.Error(), "sampling: unknown key") {
		t.Errorf("a runner with a model setting = %v", err)
	}
	if _, err := Open("tavily", section(t, "retries: 4\nrequest_timeout: 1m"), api.Host{}); err != nil {
		t.Errorf("a runner with its own connection = %v", err)
	}
}

// Health asks for the credits the key has spent, which Tavily refuses a key
// it does not take.
func TestHealthAsksForTheCredits(t *testing.T) {
	refusal := read(t, "error_unauthorized.json")
	var asked string
	taken := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.Method + " " + r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		if !taken {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write(refusal)
			return
		}
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(ts.Close)
	r := runner(t, ts.URL)
	if err := r.Health(context.Background()); err == nil {
		t.Error("a key Tavily refused passed")
	}
	if asked != "GET /usage" {
		t.Errorf("asked %q", asked)
	}
	taken = true
	if err := r.Health(context.Background()); err != nil {
		t.Errorf("Health = %v", err)
	}
}
