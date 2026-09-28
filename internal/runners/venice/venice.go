// Package venice talks to Venice, which serves models behind an
// OpenAI-compatible API, searches the web and reads pages beside them, and
// makes pictures.
package venice

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/runners/api"
	"nerdola.dev/x/paula/internal/runners/openai"
	"nerdola.dev/x/paula/internal/runners/transport"
)

// Kind is the type written in the configuration file.
const Kind = "venice"

const (
	defaultURL      = "https://api.venice.ai/api/v1"
	defaultTokenEnv = "VENICE_API_KEY"
	defaultRetries  = 2
)

// defaultIdleTimeout is how long a request may go without a byte.
const defaultIdleTimeout = config.Duration(2 * time.Minute)

type Runner struct {
	name     string
	client   *transport.Client
	settings api.Settings
	serves   openai.Catalogue
	// searcher is the engine a web search goes to, or empty for the one
	// Venice chooses.
	searcher string
}

// Open reads a runner section and builds the runner it describes.
func Open(name string, s config.Section, h api.Host) (*Runner, error) {
	retries := defaultRetries
	cfg, p := openai.Decode(s, openai.Config{Connection: transport.Connection{
		URL:            defaultURL,
		TokenEnv:       defaultTokenEnv,
		IdleTimeout:    defaultIdleTimeout,
		RequestTimeout: config.Duration(transport.DefaultRequestTimeout),
		Retries:        &retries,
	}})
	// Venice takes a seed, which the keys every runner takes do not cover.
	if v := cfg.Settings.Sampling.Seed; v != nil && *v <= 0 {
		p.Addf("sampling.seed: %d is not above zero", *v)
	}
	prov, errs := decodeProvider(cfg.Provider)
	p.Add(errs...)
	if err := p.Err(); err != nil {
		return nil, err
	}

	r := &Runner{
		name:     name,
		settings: cfg.Settings,
		searcher: prov.Search.Provider,
	}
	r.settings.Provider = cfg.Provider
	r.serves.Read = r.listing
	r.client = cfg.Client(name, h, hooks{})
	return r, nil
}

func (r *Runner) Name() string { return r.name }
func (r *Runner) Kind() string { return Kind }

// URL is the base the runner sends its requests to.
func (r *Runner) URL() string { return r.client.BaseURL }

// Settings are the runner's own, which a model's settings are laid over.
func (r *Runner) Settings() api.Settings { return r.settings }

// Health asks what the key is allowed to do.
func (r *Runner) Health(ctx context.Context) error {
	var out struct {
		Data struct {
			AccessPermitted bool `json:"accessPermitted"`
		} `json:"data"`
	}
	if err := r.client.Get(ctx, "/api_keys/rate_limits", &out); err != nil {
		return err
	}
	if !out.Data.AccessPermitted {
		return fmt.Errorf("the key is not allowed to make requests")
	}
	return nil
}

// Chat sends a chat request and passes every chunk of the stream to fn.
func (r *Runner) Chat(ctx context.Context, req api.ChatRequest, fn func(api.Chunk) error) (*api.Result, error) {
	return openai.Chat(ctx, r.client, hooks{}, req, fn)
}

// mostResults is the most results Venice documents a search answering with.
const mostResults = 20

func (r *Runner) MostResults() int { return mostResults }

// Search asks Venice what a query finds on the web.
func (r *Runner) Search(ctx context.Context, req api.SearchRequest) ([]api.SearchResult, error) {
	var out struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
			Date    string `json:"date"`
		} `json:"results"`
	}
	in := map[string]any{"query": req.Query, "limit": req.Limit}
	if r.searcher != "" {
		in["search_provider"] = r.searcher
	}
	if err := r.client.Post(ctx, "/augment/search", in, &out, req.Recorder); err != nil {
		return nil, err
	}
	found := make([]api.SearchResult, len(out.Results))
	for i, res := range out.Results {
		found[i] = api.SearchResult{Title: res.Title, URL: res.URL, Content: res.Content, Date: res.Date}
	}
	return found, nil
}

// Read asks Venice for the text of a page, which it gives as markdown.
func (r *Runner) Read(ctx context.Context, req api.PageRequest) (string, error) {
	var out struct {
		Content string `json:"content"`
	}
	if err := r.client.Post(ctx, "/augment/scrape", map[string]any{"url": req.URL}, &out, req.Recorder); err != nil {
		return "", err
	}
	return out.Content, nil
}

// Models is everything Venice serves Paula.
func (r *Runner) Models(ctx context.Context) ([]api.Model, error) {
	return r.serves.Models(ctx)
}

// Model returns what the catalogue says about one model.
func (r *Runner) Model(ctx context.Context, id string) (*api.Model, error) {
	m, ok, err := r.serves.Find(ctx, id)
	if err != nil {
		return nil, err
	}
	if ok {
		return m, nil
	}
	return nil, fmt.Errorf("%s serves no model called %q", r.name, id)
}

type listing struct {
	Data []struct {
		ID        string `json:"id"`
		ModelSpec struct {
			AvailableContextTokens int `json:"availableContextTokens"`
			MaxCompletionTokens    int `json:"maxCompletionTokens"`
			Capabilities           struct {
				SupportsVision          bool     `json:"supportsVision"`
				SupportsFunctionCalling bool     `json:"supportsFunctionCalling"`
				SupportsReasoning       bool     `json:"supportsReasoning"`
				SupportsReasoningEffort bool     `json:"supportsReasoningEffort"`
				ReasoningEffortOptions  []string `json:"reasoningEffortOptions"`
				SupportsResponseSchema  bool     `json:"supportsResponseSchema"`
			} `json:"capabilities"`
			Constraints struct {
				AspectRatios []string `json:"aspectRatios"`
			} `json:"constraints"`
		} `json:"model_spec"`
	} `json:"data"`
}

// listing is what the API serves, as the models Paula speaks of. The listing
// with no type is the models that write text, the image listing the models
// that paint a picture from a prompt, and the inpaint listing the models that
// make one from a picture and a prompt. The voices and the embeddings are not
// asked for.
//
// No model is shown a picture in the answer of a call. Venice documents that
// answer as a string, and turns a picture put there into the text of its
// bytes: Claude, GPT and DeepSeek were each counted over ten thousand tokens
// for a picture of 256 pixels there, and none could say what it showed.
func (r *Runner) listing(ctx context.Context) ([]api.Model, error) {
	var list listing
	if err := r.client.Get(ctx, "/models", &list); err != nil {
		return nil, err
	}

	var out []api.Model
	for _, m := range list.Data {
		c := m.ModelSpec.Capabilities
		model := api.Model{
			ID:                m.ID,
			Context:           m.ModelSpec.AvailableContextTokens,
			Output:            m.ModelSpec.MaxCompletionTokens,
			Chat:              true,
			Vision:            c.SupportsVision,
			Tools:             c.SupportsFunctionCalling,
			Reasoning:         c.SupportsReasoning,
			StructuredOutputs: c.SupportsResponseSchema,
		}
		if c.SupportsReasoningEffort {
			model.Efforts = api.Order(c.ReasoningEffortOptions)
		}
		out = append(out, model)
	}

	for _, kind := range []string{"image", "inpaint"} {
		var list listing
		if err := r.client.Get(ctx, "/models?type="+kind, &list); err != nil {
			return nil, err
		}
		for _, m := range list.Data {
			out = append(out, api.Model{
				ID:     m.ID,
				Paint:  kind == "image",
				Edit:   kind == "inpaint",
				Ratios: m.ModelSpec.Constraints.AspectRatios,
			})
		}
	}
	return out, nil
}

// Paint asks Venice for a picture of what a prompt describes.
func (r *Runner) Paint(ctx context.Context, req api.PaintRequest) ([]byte, error) {
	prov, errs := decodeProvider(req.Settings.Provider)
	if len(errs) > 0 {
		return nil, errs[0]
	}
	in := map[string]any{
		"model":          req.Model,
		"prompt":         req.Prompt,
		"format":         "jpeg",
		"return_binary":  true,
		"hide_watermark": prov.Image.hideWatermark(),
		"safe_mode":      prov.Image.safeMode(),
	}
	return r.picture(ctx, "/image/generate", in, req.Model, req.Shape, req.Recorder)
}

// Edit asks Venice for a picture made from another, as a prompt says.
func (r *Runner) Edit(ctx context.Context, req api.EditRequest) ([]byte, error) {
	prov, errs := decodeProvider(req.Settings.Provider)
	if len(errs) > 0 {
		return nil, errs[0]
	}
	in := map[string]any{
		"model":         req.Model,
		"prompt":        req.Prompt,
		"image":         base64.StdEncoding.EncodeToString(req.Picture),
		"output_format": "jpeg",
		"safe_mode":     prov.Image.safeMode(),
	}
	return r.picture(ctx, "/image/edit", in, req.Model, req.Shape, req.Recorder)
}

// picture posts a request whose answer is the picture itself, in the aspect
// ratio of its shape among the ones the model lists. A JPEG is asked for,
// since that is what Paula keeps a picture as.
func (r *Runner) picture(ctx context.Context, path string, in map[string]any, id, shape string, rec api.Recorder) ([]byte, error) {
	m, err := r.Model(ctx, id)
	if err != nil {
		return nil, err
	}
	if ratio := m.Ratio(shape); ratio != "" {
		in["aspect_ratio"] = ratio
	}
	body, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	var out []byte
	err = r.client.Call(ctx, transport.Ask{
		Method:   http.MethodPost,
		Path:     path,
		Model:    id,
		Body:     body,
		Header:   http.Header{"Accept": {"*/*"}},
		Recorder: rec,
		Read: func(body io.Reader, _ *api.Record) error {
			var err error
			out, err = io.ReadAll(body)
			return err
		},
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Check reports every way a model and its settings do not fit what the
// catalogue says.
func (r *Runner) Check(ctx context.Context, m api.Checked) []error {
	model, err := r.Model(ctx, m.ID)
	if err != nil {
		return []error{err}
	}
	// A model that only makes pictures is sent none of what a chat is, so
	// none of it is held against it: the runner's settings are every model's
	// defaults, a picture's as well. Its image block is held as it is read.
	if !model.Chat {
		_, errs := decodeProvider(m.Settings.Provider)
		return errs
	}
	p := &api.Problems{}
	p.Add(m.Settings.Validate()...)
	if v := m.Settings.Sampling.Seed; v != nil && *v <= 0 {
		p.Addf("sampling.seed: %d is not above zero", *v)
	}
	api.CheckReasoning(p, model, m.Settings)

	prov, errs := decodeProvider(m.Settings.Provider)
	if prov == nil {
		// The block could not be read, and says which key it is: a model takes
		// the one its runner writes when it writes none of its own.
		return append(errs, p.All()...)
	}
	p.Add(errs...)
	p.Add(r.checkFallbacks(ctx, prov)...)
	// A web search goes through the runner rather than a model, so the engine
	// it goes to is the runner's block's alone. A model's block that names
	// another would change nothing, and says so.
	if prov.Search.Provider != r.searcher {
		p.Addf("provider.search.provider: %q is the runner's to set, since a web search goes through the runner, not a model",
			prov.Search.Provider)
	}
	// Venice lists no request parameters, so a sampling setting is not held
	// against a list.
	return p.All()
}

func (r *Runner) checkFallbacks(ctx context.Context, prov *provider) []error {
	var errs []error
	for _, id := range prov.Fallbacks {
		m, err := r.Model(ctx, id)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("provider.fallbacks: %w", err))
		case !m.Chat:
			// A reply falls back to a model that writes it, which a model that
			// only makes pictures is not.
			errs = append(errs, fmt.Errorf("provider.fallbacks: %s does not chat", id))
		}
	}
	if len(prov.Fallbacks) > maxFallbacks {
		errs = append(errs, fmt.Errorf("provider.fallbacks: %d models, at most %d are documented",
			len(prov.Fallbacks), maxFallbacks))
	}
	return errs
}
