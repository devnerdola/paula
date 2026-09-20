// Package venice talks to Venice, which serves models that keep nothing of a
// request behind an OpenAI-compatible API.
package venice

import (
	"context"
	"fmt"
	"sync"
	"time"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/runners/api"
	"nerdola.dev/x/paula/internal/runners/openai"
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

// private is the only privacy Paula serves a model with, so nothing of a
// conversation is kept by the host.
const private = "private"

// embedding is the type the catalogue gives a model that turns text into a
// vector rather than into more text.
const embedding = "embedding"

type Runner struct {
	name     string
	client   *openai.Client
	settings api.Settings
	serves   openai.Catalogue

	// refused are the models Venice serves with a privacy Paula does not, and
	// why, so a model named in the file is refused in those words rather than
	// as one that is not there. The listing fills it in.
	mu      sync.Mutex
	refused map[string]string
}

// Open reads a runner section and builds the runner it describes.
func Open(name string, s config.Section, h api.Host) (*Runner, error) {
	retries := defaultRetries
	cfg, p := openai.Decode(s, openai.Config{
		URL:            defaultURL,
		TokenEnv:       defaultTokenEnv,
		IdleTimeout:    defaultIdleTimeout,
		RequestTimeout: config.Duration(openai.DefaultRequestTimeout),
		Retries:        &retries,
	})
	// Venice takes a seed, which the keys every runner takes do not cover.
	if v := cfg.Settings.Sampling.Seed; v != nil && *v <= 0 {
		p.Addf("sampling.seed: %d is not above zero", *v)
	}
	_, errs := decodeProvider(cfg.Provider)
	p.Add(errs...)
	if err := p.Err(); err != nil {
		return nil, err
	}

	r := &Runner{
		name:     name,
		settings: cfg.Settings,
	}
	r.settings.Provider = cfg.Provider
	r.serves.Read = r.read
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
	return r.client.Chat(ctx, req, fn)
}

func (r *Runner) Embed(ctx context.Context, req api.EmbedRequest) (*api.EmbedResult, error) {
	return r.client.Embed(ctx, req)
}

// Models is everything Venice serves Paula.
func (r *Runner) Models(ctx context.Context) ([]api.Model, error) {
	return r.serves.Models(ctx)
}

// Model returns what the catalogue says about one model, and why a model Venice
// serves is not one Paula does.
func (r *Runner) Model(ctx context.Context, id string) (*api.Model, error) {
	m, ok, err := r.serves.Find(ctx, id)
	if err != nil {
		return nil, err
	}
	if ok {
		return m, nil
	}
	r.mu.Lock()
	why, refused := r.refused[id]
	r.mu.Unlock()
	if refused {
		return nil, fmt.Errorf("%s serves %q with privacy %q, and Paula serves only %q", r.name, id, why, private)
	}
	return nil, fmt.Errorf("%s serves no model called %q", r.name, id)
}

// read is the listing, and remembers the models it named that Paula does not
// serve, for Model to say why.
func (r *Runner) read(ctx context.Context) ([]api.Model, error) {
	models, refused, err := r.listing(ctx)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.refused = refused
	r.mu.Unlock()
	return models, nil
}

type listing struct {
	Data []struct {
		ID        string `json:"id"`
		Type      string `json:"type"`
		ModelSpec struct {
			Privacy                string `json:"privacy"`
			AvailableContextTokens int    `json:"availableContextTokens"`
			MaxInputTokens         int    `json:"maxInputTokens"`
			EmbeddingDimensions    int    `json:"embeddingDimensions"`
			Capabilities           struct {
				SupportsVision          bool     `json:"supportsVision"`
				SupportsFunctionCalling bool     `json:"supportsFunctionCalling"`
				SupportsReasoning       bool     `json:"supportsReasoning"`
				SupportsReasoningEffort bool     `json:"supportsReasoningEffort"`
				ReasoningEffortOptions  []string `json:"reasoningEffortOptions"`
				SupportsResponseSchema  bool     `json:"supportsResponseSchema"`
			} `json:"capabilities"`
		} `json:"model_spec"`
	} `json:"data"`
}

// listing is what the API serves, as the models Paula speaks of and the ones it
// serves with another privacy.
func (r *Runner) listing(ctx context.Context) ([]api.Model, map[string]string, error) {
	var list listing
	if err := r.client.Get(ctx, "/models?type=all", &list); err != nil {
		return nil, nil, err
	}

	var out []api.Model
	refused := map[string]string{}
	for _, m := range list.Data {
		// The models that write text, and the ones that embed it. Nothing
		// Paula does asks anything of the rest.
		if m.Type != "text" && m.Type != embedding {
			continue
		}
		if m.ModelSpec.Privacy != private {
			refused[m.ID] = m.ModelSpec.Privacy
			continue
		}
		if m.Type == embedding {
			// A model that embeds writes nothing, so it is the whole of what
			// the catalogue says about it.
			out = append(out, api.Model{
				ID:         m.ID,
				Context:    m.ModelSpec.MaxInputTokens,
				Embeddings: true,
			})
			continue
		}
		c := m.ModelSpec.Capabilities
		model := api.Model{
			ID:                m.ID,
			Context:           m.ModelSpec.AvailableContextTokens,
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
	return out, refused, nil
}

// Check reports every way a model and its settings do not fit what the
// catalogue says.
func (r *Runner) Check(ctx context.Context, m api.Checked) []error {
	model, err := r.Model(ctx, m.ID)
	if err != nil {
		return []error{err}
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
	// Venice lists no request parameters, so a sampling setting is not held
	// against a list.
	return p.All()
}

func (r *Runner) checkFallbacks(ctx context.Context, prov *provider) []error {
	var errs []error
	for _, id := range prov.Fallbacks {
		if _, err := r.Model(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("provider.fallbacks: %w", err))
		}
	}
	if len(prov.Fallbacks) > maxFallbacks {
		errs = append(errs, fmt.Errorf("provider.fallbacks: %d models, at most %d are documented",
			len(prov.Fallbacks), maxFallbacks))
	}
	return errs
}
