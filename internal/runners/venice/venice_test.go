package venice

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
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

// listed answers each listing with the one captured of its type.
func listed(t *testing.T, w http.ResponseWriter, r *http.Request) {
	file := "models.json"
	if kind := r.URL.Query().Get("type"); kind != "" {
		file = "models_" + kind + ".json"
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(read(t, file))
}

// answers serves one captured file for every path but the listings.
func answers(t *testing.T, kind, file string) *httptest.Server {
	t.Helper()
	body := read(t, file)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			listed(t, w, r)
			return
		}
		w.Header().Set("Content-Type", kind)
		w.Write(body)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func runner(t *testing.T, url string, body string) *Runner {
	t.Helper()
	t.Setenv("VENICE_API_KEY", "test-token-abcdefgh")
	path := filepath.Join(t.TempDir(), "paula.yaml")
	file := "persona: paula.yaml\nrunners:\n  venice:\n    type: venice\n    url: " + url + "/v1\n"
	for line := range strings.SplitSeq(body, "\n") {
		if line != "" {
			file += "    " + line + "\n"
		}
	}
	file += "models:\n  chat:\n    runner: venice\n    id: x\ndefault_models:\n  chat: chat\n"
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Open("venice", cfg.Runners[0].Section, api.Host{})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func model(t *testing.T, r *Runner, id string) api.Model {
	t.Helper()
	m, err := r.Model(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return *m
}

// The stream is the one a reply that asked for two tools came back in, as
// Venice sent it: each call opened by its id and name, then built from
// fragments of its arguments that name the id and the name as null.
func TestTheCallsOfACapturedStreamComeBackWhole(t *testing.T) {
	res := streamed(t, "stream_tool_calls.sse")
	want := []api.ToolCall{
		{ID: "chatcmpl-tool-8764b7316a7d866f", Name: "search_memories", Arguments: `{"query": "Caio's sister name"}`},
		{ID: "chatcmpl-tool-a3fb1b90fafb4d7a", Name: "search_memories", Arguments: `{"query": "Caio family sister"}`},
	}
	if !slices.Equal(res.ToolCalls, want) {
		t.Errorf("calls = %+v, want %+v", res.ToolCalls, want)
	}
	if res.FinishReason != "tool_calls" {
		t.Errorf("finish reason = %q", res.FinishReason)
	}
}

// What a model thought on its way to its calls goes back with them in the
// field the stream gave it in.
func TestTheReasoningOfACallGoesBackInTheFieldItCameIn(t *testing.T) {
	res := streamed(t, "stream_tool_calls.sse")
	if res.Reasoning == "" {
		t.Fatal("the stream gave no reasoning")
	}
	out := map[string]any{}
	hooks{}.Message(out, api.Message{
		Role:      api.RoleAssistant,
		ToolCalls: res.ToolCalls,
		Reasoning: &api.Reasoning{Text: res.Reasoning},
	})
	if got := out["reasoning_content"]; got != res.Reasoning {
		t.Errorf("reasoning_content = %v, want what the model thought, %q", got, res.Reasoning)
	}

	none := map[string]any{}
	hooks{}.Message(none, api.Text(api.RoleUser, "hey"))
	if len(none) != 0 {
		t.Errorf("a message with no reasoning carries %+v", none)
	}
}

// A reply of several rounds is kept as one message, with what every round
// thought, and goes back with what its last round thought, as it came: what a
// GPT model thinks comes as an encrypted item OpenAI parses only whole and on
// its own, and three rounds' items handed back together were refused.
func TestAReplyGoesBackWithWhatItsLastRoundThought(t *testing.T) {
	earlier := streamed(t, "stream_tool_calls.sse")
	last := streamed(t, "stream_cache_write.sse")
	if !strings.HasPrefix(last.Reasoning, "__ENCRYPTED_REASONING__") {
		t.Fatalf("the captured round thought %q, want the encrypted item it came as", last.Reasoning)
	}
	out := map[string]any{}
	hooks{}.Message(out, api.Message{
		Role:  api.RoleAssistant,
		Parts: []api.Part{{Type: api.PartText, Text: "hey love"}},
		Reasoning: &api.Reasoning{
			Text:    earlier.Reasoning + "\n\n" + last.Reasoning,
			Details: last.ReasoningDetails,
		},
	})
	if got := out["reasoning_content"]; got != last.Reasoning {
		t.Errorf("reasoning_content = %q, want what the last round thought, %q", got, last.Reasoning)
	}
}

// streamed is what the runner makes of a captured stream.
func streamed(t *testing.T, name string) *api.Result {
	t.Helper()
	r := runner(t, answers(t, "text/event-stream", name).URL, "")
	res, err := r.Chat(context.Background(), api.ChatRequest{Model: "m"},
		func(api.Chunk) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// What Paula makes of each entry of the captured listing.
func TestCatalogue(t *testing.T) {
	r := runner(t, answers(t, "application/json", "models.json").URL, "")

	// A model that reasons at a chosen effort, sees images, but not in the
	// answer of a call, takes tools and answers a schema.
	m := model(t, r, "z-ai-glm-5-3-flash")
	if !m.Chat || !m.Vision || m.AnswerVision || !m.Tools || !m.Reasoning || !m.StructuredOutputs {
		t.Errorf("%s = %+v", m.ID, m)
	}
	if m.Context != 1048576 || m.Output != 131072 {
		t.Errorf("%s = %+v, want the context and the output the listing gives", m.ID, m)
	}
	if len(m.Efforts) == 0 {
		t.Errorf("%s lists no efforts", m.ID)
	}
	if slices.Contains(m.Efforts, "none") {
		t.Errorf("%s efforts = %v, want none left out", m.ID, m.Efforts)
	}

	// A model that reasons but takes no effort.
	if m := model(t, r, "z-ai-glm-5-3"); !m.Reasoning || len(m.Efforts) != 0 {
		t.Errorf("%s = %+v, want reasoning with no efforts", m.ID, m)
	}

	// A model that does not reason.
	if m := model(t, r, "venice-uncensored-1-2"); m.Reasoning || len(m.Efforts) != 0 {
		t.Errorf("%s = %+v, want no reasoning", m.ID, m)
	}

	// A model that writes makes no picture, one of the image listing paints,
	// and one of the inpaint listing makes a picture from another. Neither
	// writes, and each is made in the aspect ratios it lists, or none.
	if m := model(t, r, "z-ai-glm-5-3"); m.Paint || m.Edit {
		t.Errorf("%s = %+v, want no pictures", m.ID, m)
	}
	if m := model(t, r, "venice-sd35"); !m.Paint || m.Edit || m.Chat || m.Ratios != nil {
		t.Errorf("%s = %+v, want a model that paints in no listed ratio", m.ID, m)
	}
	if m := model(t, r, "nano-banana-2-edit"); !m.Edit || m.Paint || m.Chat || !slices.Contains(m.Ratios, "3:4") {
		t.Errorf("%s = %+v, want a model that edits in the ratios it lists", m.ID, m)
	}

	if _, err := r.Model(context.Background(), "nope"); err == nil {
		t.Error("Model of an unknown id succeeded")
	}
}

// The catalogue is the models that write text, the ones that paint and the
// ones that make a picture from another: the voices and the embeddings are
// not asked for.
func TestTheCatalogueIsTheTextImageAndInpaintListings(t *testing.T) {
	var asked []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.RequestURI())
		listed(t, w, r)
	}))
	t.Cleanup(ts.Close)

	if _, err := runner(t, ts.URL, "").Models(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"/v1/models", "/v1/models?type=image", "/v1/models?type=inpaint"}; !slices.Equal(asked, want) {
		t.Errorf("asked %q, want %q", asked, want)
	}
}

// A listing that cannot be read leaves the catalogue unread, whichever it is.
func TestAListingThatFailsFailsTheCatalogue(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("type") == "inpaint" {
			http.Error(w, `{"error":"the host is away"}`, http.StatusBadGateway)
			return
		}
		listed(t, w, r)
	}))
	t.Cleanup(ts.Close)

	if _, err := runner(t, ts.URL, "retries: 0").Models(context.Background()); err == nil {
		t.Error("the catalogue was read without the inpaint listing")
	}
}

func TestCatalogueIsReadOnce(t *testing.T) {
	var reads int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads++
		listed(t, w, r)
	}))
	t.Cleanup(ts.Close)

	r := runner(t, ts.URL, "")
	for range 3 {
		if _, err := r.Models(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if reads != 3 {
		t.Errorf("reads = %d, want the three listings read once", reads)
	}
}

// The answer is served no-store, and the models it names are what every
// configured model is held to, so a second runner over the same data directory
// asks the API again.
func TestEveryRunReadsTheListing(t *testing.T) {
	var (
		mu    sync.Mutex
		reads int
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if r.URL.RequestURI() == "/v1/models" {
			reads++
		}
		mu.Unlock()
		listed(t, w, r)
	}))
	t.Cleanup(ts.Close)

	for range 2 {
		r := runner(t, ts.URL, "")
		models, err := r.Models(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(models) == 0 {
			t.Fatal("the run read an empty catalogue")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if reads != 2 {
		t.Errorf("the listing was read %d times, want it read by each run", reads)
	}
}

// The stream is the one a reply with reasoning came back in, and its last chunk
// carries the usage with no choice beside it.
func TestStreamOfACapturedReply(t *testing.T) {
	r := runner(t, answers(t, "text/event-stream", "stream_reply.sse").URL, "")

	var text, reasoning strings.Builder
	res, err := r.Chat(context.Background(), api.ChatRequest{Model: "m"}, func(c api.Chunk) error {
		switch c.Kind {
		case api.ChunkText:
			text.WriteString(c.Text)
		case api.ChunkReasoning:
			reasoning.WriteString(c.Text)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if text.Len() == 0 {
		t.Error("no text came out of the stream")
	}
	if res.Reasoning == "" || reasoning.String() != res.Reasoning {
		t.Errorf("reasoning = %q, chunks = %q", res.Reasoning, reasoning.String())
	}
	if res.FinishReason != "stop" {
		t.Errorf("finish reason = %q", res.FinishReason)
	}
	if res.Usage.PromptTokens == 0 || res.Usage.CompletionTokens == 0 || res.Usage.ReasoningTokens == 0 {
		t.Errorf("usage = %+v", res.Usage)
	}
	if res.Usage.Cost == 0 {
		t.Errorf("usage = %+v, want the cost", res.Usage)
	}
}

// The first request of a prompt wrote 3796 of its 3799 tokens to the cache and
// read none, which is what paula turns has to show of it.
func TestWhatARequestWroteToTheCacheIsRead(t *testing.T) {
	r := runner(t, answers(t, "text/event-stream", "stream_cache_write.sse").URL, "")
	res, err := r.Chat(context.Background(), api.ChatRequest{Model: "m"}, func(api.Chunk) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if res.Usage.PromptTokens != 3799 || res.Usage.CachedTokens != 0 || res.Usage.CacheWriteTokens != 3796 {
		t.Errorf("usage = %+v, want 3799 prompt tokens, none read and 3796 written", res.Usage)
	}
}

func TestCapturedErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		file   string
		status int
		want   string
	}{
		{"a model that does not exist", "error_bad_model.json", 404, "Specified model not found"},
		{"a key that does not exist", "error_unauthorized.json", 401, "Authentication failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := read(t, tc.file)
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				w.Write(body)
			}))
			t.Cleanup(ts.Close)
			r := runner(t, ts.URL, "")

			_, err := r.Chat(context.Background(), api.ChatRequest{Model: "m"}, func(api.Chunk) error { return nil })
			var e *api.APIError
			if !errors.As(err, &e) {
				t.Fatalf("error = %v, want an APIError", err)
			}
			if e.Status != tc.status {
				t.Errorf("status = %d", e.Status)
			}
			if !strings.Contains(e.Message, tc.want) {
				t.Errorf("message = %q, want %q in it", e.Message, tc.want)
			}
		})
	}
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

// A search is sent the query and how many pages to answer with, is kept by the
// recorder it carries, and is what Venice found: each page's title, address
// and passage, and a date Venice leaves empty when it does not know one.
func TestASearchIsWhatVeniceFound(t *testing.T) {
	ts, path, sent := posted(t, http.StatusOK, "search.json")
	r := runner(t, ts.URL, "")
	kept := &records{}
	found, err := r.Search(context.Background(), api.SearchRequest{Query: "concertos em Lisboa esta semana", Limit: 5, Recorder: kept})
	if err != nil {
		t.Fatal(err)
	}
	if *path != "/v1/augment/search" || (*sent)["query"] != "concertos em Lisboa esta semana" || (*sent)["limit"] != 5.0 ||
		(*sent)["search_provider"] != nil {
		t.Errorf("sent %v to %s", *sent, *path)
	}
	if len(kept.ended) != 1 || kept.ended[0].Status != http.StatusOK {
		t.Errorf("records = %+v, want the search kept", kept.ended)
	}
	if len(found) != 5 {
		t.Fatalf("found %d pages, want the five Venice answered with", len(found))
	}
	first := found[0]
	if first.Title != "Os concertos em Lisboa que vai querer ver esta semana" ||
		first.URL != "https://www.timeout.pt/lisboa/pt/musica/os-melhores-concertos-em-lisboa-esta-semana" ||
		!strings.HasPrefix(first.Content, "Este fim-de-semana, o último, pode ver <strong>FF (25 Set)") || first.Date != "" {
		t.Errorf("the first page = %+v", first)
	}
}

// The engine the runner's provider block names is the one a search goes to.
func TestASearchGoesToTheEngineTheRunnerNames(t *testing.T) {
	ts, _, sent := posted(t, http.StatusOK, "search.json")
	r := runner(t, ts.URL, "provider:\n  search:\n    provider: google")
	if _, err := r.Search(context.Background(), api.SearchRequest{Query: "fado", Limit: 5}); err != nil {
		t.Fatal(err)
	}
	if (*sent)["search_provider"] != "google" {
		t.Errorf("sent %v, want the search sent to google", *sent)
	}
}

// A page is read as the markdown Venice made of it, and one Venice will not
// read is refused in its words.
func TestAPageIsWhatVeniceRead(t *testing.T) {
	ts, path, sent := posted(t, http.StatusOK, "scrape.json")
	r := runner(t, ts.URL, "")
	url := "https://www.timeout.pt/lisboa/pt/musica/os-melhores-concertos-em-lisboa-esta-semana"
	got, err := r.Read(context.Background(), api.PageRequest{URL: url})
	if err != nil {
		t.Fatal(err)
	}
	if *path != "/v1/augment/scrape" || (*sent)["url"] != url {
		t.Errorf("sent %v to %s", *sent, *path)
	}
	if !strings.HasPrefix(got, "[Ir para o conteúdo](https://www.timeout.pt/") {
		t.Errorf("the page reads %q", got[:min(len(got), 200)])
	}

	ts, _, _ = posted(t, http.StatusBadRequest, "error_scrape_blocked.json")
	_, err = runner(t, ts.URL, "").Read(context.Background(), api.PageRequest{URL: "https://www.reddit.com/r/lisboa/"})
	var e *api.APIError
	if !errors.As(err, &e) || e.Status != http.StatusBadRequest ||
		e.Message != "Reddit blocks automated access to their content. Unable to scrape this URL." {
		t.Errorf("a page Venice will not read = %v", err)
	}
}

// painted serves the listings, and one captured file with a status for any
// other path, keeping the path and the body of what it was sent.
func painted(t *testing.T, status int, kind, file string) (*httptest.Server, *string, *map[string]any) {
	t.Helper()
	body := read(t, file)
	var path string
	var sent map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			listed(t, w, r)
			return
		}
		path = r.URL.Path
		json.NewDecoder(r.Body).Decode(&sent)
		w.Header().Set("Content-Type", kind)
		w.WriteHeader(status)
		w.Write(body)
	}))
	t.Cleanup(ts.Close)
	return ts, &path, &sent
}

// A picture is the file Venice answered with, asked for as a JPEG with no
// watermark and not blurred, and kept by the recorder the request carries. A
// model that lists no aspect ratio is sent none.
func TestAPaintIsThePictureVeniceMade(t *testing.T) {
	ts, path, sent := painted(t, http.StatusOK, "image/jpeg", "image_generate.jpg")
	r := runner(t, ts.URL, "")
	kept := &records{}
	got, err := r.Paint(context.Background(), api.PaintRequest{
		Model:    "venice-sd35",
		Prompt:   "A red apple on a wooden kitchen table, morning light",
		Shape:    api.ShapeSquare,
		Settings: r.Settings(),
		Recorder: kept,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, read(t, "image_generate.jpg")) {
		t.Errorf("the picture is %d bytes, want the %d Venice answered with", len(got), len(read(t, "image_generate.jpg")))
	}
	want := map[string]any{
		"model":          "venice-sd35",
		"prompt":         "A red apple on a wooden kitchen table, morning light",
		"format":         "jpeg",
		"return_binary":  true,
		"hide_watermark": true,
		"safe_mode":      false,
	}
	if *path != "/v1/image/generate" || !maps.Equal(*sent, want) {
		t.Errorf("sent %v to %s, want %v", *sent, *path, want)
	}
	if len(kept.ended) != 1 || kept.ended[0].Model != "venice-sd35" || kept.ended[0].Status != http.StatusOK {
		t.Errorf("records = %+v, want the picture kept", kept.ended)
	}
}

// An edit carries the picture as base64 and asks for a JPEG, in the aspect
// ratio of its shape among the ones the model lists: the one the shape is
// named for, the nearest when the model lists another, and none when it lists
// none of them or no shape is asked for.
func TestAnEditCarriesThePictureInTheShapeAsked(t *testing.T) {
	picture := read(t, "image_generate.jpg")
	for _, tc := range []struct{ id, shape, ratio string }{
		{"muse-image-edit", api.ShapePortrait, "3:4"},
		{"nano-banana-2-edit", api.ShapeLandscape, "3:2"},
		{"wan-2-7-pro-edit", api.ShapePortrait, ""},
		{"muse-image-edit", "", ""},
	} {
		ts, path, sent := painted(t, http.StatusOK, "image/jpeg", "image_edit.jpg")
		r := runner(t, ts.URL, "")
		got, err := r.Edit(context.Background(), api.EditRequest{
			Model:    tc.id,
			Prompt:   "The same apple, cut in half, on the same table",
			Shape:    tc.shape,
			Picture:  picture,
			Settings: r.Settings(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, read(t, "image_edit.jpg")) {
			t.Errorf("%s: the picture is %d bytes, want the one Venice answered with", tc.id, len(got))
		}
		want := map[string]any{
			"model":         tc.id,
			"prompt":        "The same apple, cut in half, on the same table",
			"image":         base64.StdEncoding.EncodeToString(picture),
			"output_format": "jpeg",
			"safe_mode":     false,
		}
		if tc.ratio != "" {
			want["aspect_ratio"] = tc.ratio
		}
		if *path != "/v1/image/edit" || !maps.Equal(*sent, want) {
			t.Errorf("%s as %q: sent %s with aspect_ratio %v, want %q", tc.id, tc.shape, *path, (*sent)["aspect_ratio"], tc.ratio)
		}
	}
}

// What the provider block sets of a picture is what it is asked for with.
func TestAPictureIsAskedForAsTheBlockSays(t *testing.T) {
	ts, _, sent := painted(t, http.StatusOK, "image/jpeg", "image_generate.jpg")
	r := runner(t, ts.URL, "provider:\n  image:\n    hide_watermark: false\n    safe_mode: true")
	if _, err := r.Paint(context.Background(), api.PaintRequest{Model: "venice-sd35", Prompt: "an apple", Settings: r.Settings()}); err != nil {
		t.Fatal(err)
	}
	if (*sent)["hide_watermark"] != false || (*sent)["safe_mode"] != true {
		t.Errorf("sent %v, want the watermark kept and safe mode on", *sent)
	}
}

// A model that only makes pictures is sent none of what a chat is, so the
// settings its runner writes for every model are not held against it; a
// model that chats is held to the same ones.
func TestAModelThatOnlyMakesPicturesIsHeldToNoChatSetting(t *testing.T) {
	r := runner(t, answers(t, "application/json", "models.json").URL, "")
	s := api.Settings{
		Reasoning: api.ReasoningSettings{Mode: api.ReasoningOn, Effort: "high"},
		Sampling:  api.SamplingSettings{Seed: new(int64(0))},
	}
	ctx := context.Background()
	if err := check(ctx, r, api.Checked{ID: "muse-image-edit", Settings: s, Needs: api.Needs{Edit: true}}); err != nil {
		t.Errorf("a model that only edits was held to the chat settings: %v", err)
	}
	if err := check(ctx, r, api.Checked{ID: "venice-uncensored-1-2", Settings: s}); err == nil {
		t.Error("a model that chats took reasoning it does not do and a seed at zero")
	}
}

// A picture Venice will not make is refused in its words.
func TestAPictureVeniceWillNotMakeIsRefusedInItsWords(t *testing.T) {
	ts, _, _ := painted(t, http.StatusNotFound, "application/json", "error_image_generate.json")
	_, err := runner(t, ts.URL, "").Paint(context.Background(), api.PaintRequest{Model: "venice-sd35", Prompt: "A red apple"})
	var e *api.APIError
	if !errors.As(err, &e) || e.Status != http.StatusNotFound || e.Message != "Specified model not found: no-such-painter." {
		t.Errorf("a paint Venice refused = %v", err)
	}

	ts, _, _ = painted(t, http.StatusBadRequest, "application/json", "error_image_edit.json")
	_, err = runner(t, ts.URL, "").Edit(context.Background(), api.EditRequest{Model: "muse-image-edit", Prompt: "The same apple", Picture: read(t, "image_generate.jpg")})
	if !errors.As(err, &e) || e.Status != http.StatusBadRequest ||
		e.Message != "Prompt exceeds 1500 character limit for model 'firered-image-edit'" {
		t.Errorf("an edit Venice refused = %v", err)
	}
}

func TestRequestBody(t *testing.T) {
	var sent []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write(read(t, "stream_reply.sse"))
	}))
	t.Cleanup(ts.Close)

	r := runner(t, ts.URL, "provider:\n  character: some-slug\n  output:\n    verbosity: low")
	_, err := r.Chat(context.Background(), api.ChatRequest{
		Model:    "m",
		Messages: []api.Message{api.Text(api.RoleUser, "hey")},
		Settings: api.Settings{
			Reasoning: api.ReasoningSettings{Mode: api.ReasoningOn, Effort: "high"},
			Provider:  r.Settings().Provider,
		},
	}, func(api.Chunk) error { return nil })
	if err != nil {
		t.Fatal(err)
	}

	var body map[string]any
	if err := json.Unmarshal(sent, &body); err != nil {
		t.Fatal(err)
	}
	options, _ := body["stream_options"].(map[string]any)
	if options == nil || options["include_usage"] != true {
		t.Errorf("stream_options = %v, want the usage asked for", body["stream_options"])
	}
	params, _ := body["venice_parameters"].(map[string]any)
	if params == nil {
		t.Fatalf("venice_parameters = %v", body["venice_parameters"])
	}
	if params["include_venice_system_prompt"] != false {
		t.Errorf("include_venice_system_prompt = %v, want it off", params["include_venice_system_prompt"])
	}
	if params["character_slug"] != "some-slug" {
		t.Errorf("character_slug = %v", params["character_slug"])
	}
	if body["verbosity"] != "low" {
		t.Errorf("verbosity = %v", body["verbosity"])
	}
	reasoning, _ := body["reasoning"].(map[string]any)
	if reasoning == nil || reasoning["enabled"] != true || reasoning["effort"] != "high" {
		t.Errorf("reasoning = %v", body["reasoning"])
	}
}

// check is what the runner found wrong with a model, as one error to read.
func check(ctx context.Context, r *Runner, m api.Checked) error {
	return errors.Join(r.Check(ctx, m)...)
}

func TestSystemPromptStaysOffUnlessAskedFor(t *testing.T) {
	// A runner section that names no provider keys.
	p, errs := decodeProvider(config.Section{})
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	if p.object()["include_venice_system_prompt"] != false {
		t.Errorf("object = %v, want the system prompt off", p.object())
	}
}

func TestCheckHoldsTheSettingsAgainstTheCatalogue(t *testing.T) {
	r := runner(t, answers(t, "application/json", "models.json").URL, "")
	ctx := context.Background()

	for _, tc := range []struct {
		name  string
		id    string
		s     api.Settings
		needs api.Needs
		want  string
	}{
		{
			"reasoning on a model that does not reason",
			"venice-uncensored-1-2",
			api.Settings{Reasoning: api.ReasoningSettings{Mode: api.ReasoningOn}},
			api.Needs{}, "the model does not reason",
		},
		{
			"an effort on a model that takes none",
			"z-ai-glm-5-3",
			api.Settings{Reasoning: api.ReasoningSettings{Effort: "high"}},
			api.Needs{}, "lists no efforts",
		},
		{
			"an effort the model does not list",
			"z-ai-glm-5-3-flash",
			api.Settings{Reasoning: api.ReasoningSettings{Effort: "max"}},
			api.Needs{}, "is not one of",
		},
		{
			"a seed at zero",
			"z-ai-glm-5-3-flash",
			api.Settings{Sampling: api.SamplingSettings{Seed: new(int64(0))}},
			api.Needs{}, "sampling.seed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := check(ctx, r, api.Checked{ID: tc.id, Settings: tc.s, Needs: tc.needs})
			if err == nil {
				t.Fatalf("Check passed, want %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want %q in it", err, tc.want)
			}
		})
	}

	s := api.Settings{
		Reasoning: api.ReasoningSettings{Mode: api.ReasoningOn, Effort: "high"},
		Sampling:  api.SamplingSettings{Temperature: new(0.8), Seed: new(int64(7))},
	}
	needs := api.Needs{Chat: true, Vision: true, Tools: true}
	if err := check(ctx, r, api.Checked{ID: "z-ai-glm-5-3-flash", Settings: s, Needs: needs}); err != nil {
		t.Errorf("Check = %v, want it to pass", err)
	}
}

// A web search goes through the runner, so only the runner's block names the
// engine it goes to. A model whose own block names another is reported under
// the key; one that names the runner's, or none, changes nothing.
func TestOnlyTheRunnerNamesTheSearchEngine(t *testing.T) {
	ctx := context.Background()
	url := answers(t, "application/json", "models.json").URL
	for _, tc := range []struct {
		runner, model string
		refused       bool
	}{
		{"", "search:\n  provider: google", true},
		{"provider:\n  search:\n    provider: brave", "search:\n  provider: google", true},
		{"provider:\n  search:\n    provider: brave", "search:\n  provider: brave", false},
		{"provider:\n  search:\n    provider: brave", "", false},
	} {
		r := runner(t, url, tc.runner)
		own, err := config.Root([]byte(tc.model))
		if err != nil {
			t.Fatal(err)
		}
		s := api.Settings{Provider: config.MergeSections(r.Settings().Provider, own)}
		err = check(ctx, r, api.Checked{ID: "z-ai-glm-5-3-flash", Settings: s})
		if refused := err != nil && strings.Contains(err.Error(), "provider.search.provider"); refused != tc.refused {
			t.Errorf("a runner of %q and a model of %q = %v, want it refused: %v", tc.runner, tc.model, err, tc.refused)
		}
	}
}

// A reply falls back to a model the catalogue serves that writes it: one the
// catalogue does not hold, and one that only makes pictures, are each named.
func TestFallbacksMustBeServed(t *testing.T) {
	r := runner(t, answers(t, "application/json", "models.json").URL,
		"provider:\n  fallbacks: [z-ai-glm-5-3, nope, seedream-v5-lite]")
	s := api.Settings{Provider: r.Settings().Provider}
	err := check(context.Background(), r, api.Checked{ID: "z-ai-glm-5-3-flash", Settings: s})
	if err == nil {
		t.Fatal("Check passed")
	}
	if !strings.Contains(err.Error(), `provider.fallbacks`) || !strings.Contains(err.Error(), "nope") ||
		!strings.Contains(err.Error(), "seedream-v5-lite does not chat") || strings.Contains(err.Error(), "z-ai-glm-5-3 ") {
		t.Errorf("error = %v", err)
	}
}

func TestOpenWithoutAToken(t *testing.T) {
	t.Setenv("VENICE_API_KEY", "")
	path := filepath.Join(t.TempDir(), "paula.yaml")
	os.WriteFile(path, []byte("persona: paula.yaml\nrunners:\n  venice:\n    type: venice\nmodels:\n  chat:\n    runner: venice\n    id: x\ndefault_models:\n  chat: chat\n"), 0o600)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open("venice", cfg.Runners[0].Section, api.Host{}); err == nil {
		t.Fatal("Open succeeded")
	} else if !strings.Contains(err.Error(), "VENICE_API_KEY is not set") {
		t.Errorf("error = %v", err)
	}
}

func TestOpenRejectsUnknownProviderKeys(t *testing.T) {
	t.Setenv("VENICE_API_KEY", "test-token-abcdefgh")
	path := filepath.Join(t.TempDir(), "paula.yaml")
	os.WriteFile(path, []byte("persona: paula.yaml\nrunners:\n  venice:\n    type: venice\n    provider:\n      enable_web_search: true\nmodels:\n  chat:\n    runner: venice\n    id: x\ndefault_models:\n  chat: chat\n"), 0o600)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open("venice", cfg.Runners[0].Section, api.Host{}); err == nil {
		t.Fatal("Open succeeded")
	} else if !strings.Contains(err.Error(), "enable_web_search: unknown key") {
		t.Errorf("error = %v", err)
	}
}

func TestACatalogueThatFailedIsAskedAgain(t *testing.T) {
	var reads int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads++
		if reads == 1 {
			http.Error(w, `{"error":"the host is away"}`, http.StatusBadGateway)
			return
		}
		listed(t, w, r)
	}))
	t.Cleanup(ts.Close)

	r := runner(t, ts.URL, "retries: 0")
	ctx := context.Background()
	if _, err := r.Models(ctx); err == nil {
		t.Fatal("the first read succeeded")
	}
	models, err := r.Models(ctx)
	if err != nil {
		t.Fatalf("the catalogue was never read again: %v", err)
	}
	if len(models) == 0 {
		t.Error("the catalogue is empty")
	}
}

func TestTheResetHeaderIsCountedFromTheGivenTime(t *testing.T) {
	var hook hooks
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	h := http.Header{}
	h.Set(resetHeader, strconv.FormatInt(now.Add(90*time.Second).Unix(), 10))

	d, ok := hook.Retry(now, http.StatusTooManyRequests, h)
	if !ok || d != 90*time.Second {
		t.Errorf("retry = %s, %v, want a minute and a half", d, ok)
	}
	// A reset that has already passed is no delay at all.
	if d, ok := hook.Retry(now.Add(2*time.Minute), http.StatusTooManyRequests, h); !ok || d != 0 {
		t.Errorf("retry = %s, %v, want no delay", d, ok)
	}
	if _, ok := hook.Retry(now, http.StatusBadGateway, h); ok {
		t.Error("a status Venice does not name is sent again")
	}
}

func TestHealthAsksWhatTheKeyMayDo(t *testing.T) {
	var permitted bool
	var asked string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":{"accessPermitted":%t}}`, permitted)
	}))
	t.Cleanup(ts.Close)
	r := runner(t, ts.URL, "")
	ctx := context.Background()

	if err := r.Health(ctx); err == nil {
		t.Error("a key that may not make requests passed")
	}
	if asked != "/v1/api_keys/rate_limits" {
		t.Errorf("asked %q", asked)
	}
	permitted = true
	if err := r.Health(ctx); err != nil {
		t.Errorf("Health = %v", err)
	}
}

func TestTheRunnerSaysWhereItSends(t *testing.T) {
	ts := answers(t, "application/json", "models.json")
	r := runner(t, ts.URL, "")
	if got := r.URL(); got != ts.URL+"/v1" {
		t.Errorf("URL = %q", got)
	}
	if r.Name() != "venice" || r.Kind() != Kind {
		t.Errorf("runner = %s, %s", r.Name(), r.Kind())
	}
}

// A timeout that is no wait at all would give up on a request before it was
// sent, so it is refused where it is written.
func TestOpenRejectsATimeoutThatIsNoWait(t *testing.T) {
	for _, key := range []string{"idle_timeout", "request_timeout"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv("VENICE_API_KEY", "test-token-abcdefgh")
			path := filepath.Join(t.TempDir(), "paula.yaml")
			os.WriteFile(path, []byte("persona: paula.yaml\nrunners:\n  venice:\n    type: venice\n    "+
				key+": 0s\nmodels:\n  chat:\n    runner: venice\n    id: x\ndefault_models:\n  chat: chat\n"), 0o600)
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Open("venice", cfg.Runners[0].Section, api.Host{}); err == nil {
				t.Fatal("Open succeeded")
			} else if !strings.Contains(err.Error(), key+": 0s is not above zero") {
				t.Errorf("error = %v", err)
			}
		})
	}
}

// The whole a listing may take is the runner's to set, since a catalogue that
// takes longer than the client's own default is not one that failed.
func TestTheRequestTimeoutAsItIsWritten(t *testing.T) {
	r := runner(t, answers(t, "application/json", "models.json").URL, "request_timeout: 3m")
	if got := r.client.RequestTimeout; got != 3*time.Minute {
		t.Errorf("request timeout = %s, want the one the file writes", got)
	}
}
