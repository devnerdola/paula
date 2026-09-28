// Package openrouter talks to OpenRouter, which serves many models behind one
// OpenAI-compatible API and routes each request to a host, and makes pictures.
package openrouter

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/runners/api"
	"nerdola.dev/x/paula/internal/runners/openai"
	"nerdola.dev/x/paula/internal/runners/transport"
)

// Kind is the type written in the configuration file.
const Kind = "openrouter"

const (
	defaultURL      = "https://openrouter.ai/api/v1"
	defaultTokenEnv = "OPENROUTER_API_KEY"
	defaultRetries  = 2
)

// defaultIdleTimeout is how long a request may go without a byte. OpenRouter
// sends keep-alive comments while a host works, so silence means the
// connection is gone.
const defaultIdleTimeout = config.Duration(2 * time.Minute)

type Runner struct {
	name     string
	client   *transport.Client
	settings api.Settings
	serves   openai.Catalogue
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
	// The block is read to hold it against what the API takes. What reaches a
	// request is the section, which every model's own settings are merged over.
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

// Health asks for the key the runner is using.
func (r *Runner) Health(ctx context.Context) error {
	var out struct {
		Data struct {
			Label string `json:"label"`
		} `json:"data"`
	}
	return r.client.Get(ctx, "/key", &out)
}

// Chat sends a chat request and passes every chunk of the stream to fn.
func (r *Runner) Chat(ctx context.Context, req api.ChatRequest, fn func(api.Chunk) error) (*api.Result, error) {
	return openai.Chat(ctx, r.client, hooks{}, req, fn)
}

// Models is everything OpenRouter serves.
func (r *Runner) Models(ctx context.Context) ([]api.Model, error) {
	return r.serves.Models(ctx)
}

// Model returns what the catalogue says about one model.
func (r *Runner) Model(ctx context.Context, id string) (*api.Model, error) {
	m, ok, err := r.serves.Find(ctx, id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%s serves no model called %q", r.name, id)
	}
	return m, nil
}

// sizeOf is a size a listing gives, a context or the most a model writes, and 0
// when it gives none, which is how a size the catalogue does not say reads.
func sizeOf(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

type listing struct {
	Data []struct {
		ID            string `json:"id"`
		ContextLength *int   `json:"context_length"`
		TopProvider   struct {
			MaxCompletionTokens *int `json:"max_completion_tokens"`
		} `json:"top_provider"`
		Architecture struct {
			InputModalities  []string `json:"input_modalities"`
			OutputModalities []string `json:"output_modalities"`
		} `json:"architecture"`
		SupportedParameters []string `json:"supported_parameters"`
		Reasoning           *struct {
			Mandatory        bool     `json:"mandatory"`
			SupportedEfforts []string `json:"supported_efforts"`
		} `json:"reasoning"`
	} `json:"data"`
}

// pictures is the listing of the models that make pictures, whose parameters
// are described rather than named.
type pictures struct {
	Data []struct {
		ID                  string `json:"id"`
		SupportedParameters struct {
			InputReferences *struct {
				Min int `json:"min"`
				Max int `json:"max"`
			} `json:"input_references"`
			AspectRatio struct {
				Values []string `json:"values"`
			} `json:"aspect_ratio"`
		} `json:"supported_parameters"`
	} `json:"data"`
}

// read is the listing as the models Paula speaks of: the models listing, and
// the image models listing, which alone says what makes a picture. A model on
// both is one model.
func (r *Runner) read(ctx context.Context) ([]api.Model, error) {
	var chat listing
	if err := r.client.Get(ctx, "/models", &chat); err != nil {
		return nil, err
	}

	out := make([]api.Model, 0, len(chat.Data))
	for _, m := range chat.Data {
		model := api.Model{
			ID:                m.ID,
			Context:           sizeOf(m.ContextLength),
			Output:            sizeOf(m.TopProvider.MaxCompletionTokens),
			Chat:              slices.Contains(m.Architecture.OutputModalities, "text"),
			Vision:            slices.Contains(m.Architecture.InputModalities, "image"),
			Tools:             slices.Contains(m.SupportedParameters, "tools"),
			Reasoning:         slices.Contains(m.SupportedParameters, "reasoning"),
			StructuredOutputs: slices.Contains(m.SupportedParameters, "structured_outputs"),
			Parameters:        m.SupportedParameters,
		}
		if model.Reasoning && m.Reasoning != nil {
			model.Mandatory = m.Reasoning.Mandatory
			model.Efforts = api.Order(m.Reasoning.SupportedEfforts)
		}
		// OpenRouter hands a picture in the answer of a call to the model as a
		// picture: Claude and GPT were counted a hundred tokens or so for one
		// there, and said what it showed.
		model.AnswerVision = model.Vision
		out = append(out, model)
	}

	var images pictures
	if err := r.client.Get(ctx, "/images/models", &images); err != nil {
		return nil, err
	}
	at := make(map[string]int, len(out))
	for i, m := range out {
		at[m.ID] = i
	}
	for _, m := range images.Data {
		i, ok := at[m.ID]
		if !ok {
			out = append(out, api.Model{ID: m.ID})
			i = len(out) - 1
		}
		// A model that takes a picture as a reference makes one from another,
		// and one that must be given one does nothing else.
		refs := m.SupportedParameters.InputReferences
		out[i].Paint = refs == nil || refs.Min == 0
		out[i].Edit = refs != nil && refs.Max > 0
		out[i].Ratios = m.SupportedParameters.AspectRatio.Values
	}
	return out, nil
}

// Paint asks OpenRouter for a picture of what a prompt describes.
func (r *Runner) Paint(ctx context.Context, req api.PaintRequest) ([]byte, error) {
	in := map[string]any{"model": req.Model, "prompt": req.Prompt}
	return r.picture(ctx, in, req.Model, req.Shape, req.Recorder)
}

// Edit asks OpenRouter for a picture made from another, which goes as the one
// reference of the request.
func (r *Runner) Edit(ctx context.Context, req api.EditRequest) ([]byte, error) {
	url := "data:" + http.DetectContentType(req.Picture) + ";base64," + base64.StdEncoding.EncodeToString(req.Picture)
	in := map[string]any{
		"model":  req.Model,
		"prompt": req.Prompt,
		"input_references": []map[string]any{
			{"type": "image_url", "image_url": map[string]any{"url": url}},
		},
	}
	return r.picture(ctx, in, req.Model, req.Shape, req.Recorder)
}

// picture posts a request for a picture, in the aspect ratio of its shape
// among the ones the model lists. The picture comes back as base64, beside
// what it cost.
func (r *Runner) picture(ctx context.Context, in map[string]any, id, shape string, rec api.Recorder) ([]byte, error) {
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
		Path:     "/images",
		Model:    id,
		Body:     body,
		Recorder: rec,
		Read: func(body io.Reader, rec *api.Record) error {
			var answer struct {
				Data []struct {
					B64JSON string `json:"b64_json"`
				} `json:"data"`
				Usage *struct {
					Cost float64 `json:"cost"`
				} `json:"usage"`
			}
			if err := json.NewDecoder(body).Decode(&answer); err != nil {
				return err
			}
			if answer.Usage != nil {
				rec.Usage = &api.Usage{Cost: answer.Usage.Cost}
			}
			if len(answer.Data) == 0 {
				return transport.ErrNoAnswer
			}
			var err error
			out, err = base64.StdEncoding.DecodeString(answer.Data[0].B64JSON)
			return err
		},
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Check reports every way a model and its settings do not fit what the
// catalogue says, and what the endpoints allowed by the routing settings
// support.
func (r *Runner) Check(ctx context.Context, m api.Checked) []error {
	model, err := r.Model(ctx, m.ID)
	if err != nil {
		return []error{err}
	}
	// A model that only makes pictures is sent none of what a chat is, and
	// nothing of the provider block, so none of it is held against it: the
	// runner's settings are every model's defaults, a picture's as well.
	if !model.Chat {
		_, errs := decodeProvider(m.Settings.Provider)
		return errs
	}

	p := &api.Problems{}
	p.Add(m.Settings.Validate()...)
	api.CheckReasoning(p, model, m.Settings)
	r.checkParameters(ctx, p, model, m.Settings)

	prov, errs := decodeProvider(m.Settings.Provider)
	if prov == nil {
		// The block could not be read, and says which key it is: a model takes
		// the one its runner writes when it writes none of its own.
		return append(errs, p.All()...)
	}
	p.Add(errs...)
	if len(prov.Routing.Only) > 0 {
		p.Add(r.checkEndpoints(ctx, m, prov)...)
	}
	return p.All()
}

// checkParameters holds a setting against the catalogue only when the
// catalogue names its field for some model, which is how the names OpenRouter
// uses are found without guessing them.
func (r *Runner) checkParameters(ctx context.Context, p *api.Problems, m *api.Model, s api.Settings) {
	known, err := r.known(ctx)
	if err != nil {
		p.Add(err)
		return
	}
	if m.Parameters == nil {
		// The listing says nothing about what this model takes, which is not
		// the same as saying it takes nothing.
		return
	}
	for _, field := range s.Fields() {
		if known[field] && !slices.Contains(m.Parameters, field) {
			p.Addf("the model does not take %s", field)
		}
	}
}

func (r *Runner) known(ctx context.Context) (map[string]bool, error) {
	models, err := r.Models(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, m := range models {
		for _, p := range m.Parameters {
			out[p] = true
		}
	}
	return out, nil
}

type endpoints struct {
	Data struct {
		Endpoints []struct {
			Name                string   `json:"name"`
			ProviderName        string   `json:"provider_name"`
			Tag                 string   `json:"tag"`
			ContextLength       *int     `json:"context_length"`
			SupportedParameters []string `json:"supported_parameters"`
		} `json:"endpoints"`
	} `json:"data"`
}

// checkEndpoints reports that no host the routing settings allow takes
// everything Paula sends the model.
func (r *Runner) checkEndpoints(ctx context.Context, m api.Checked, prov *provider) []error {
	var list endpoints
	if err := r.client.Get(ctx, "/models/"+m.ID+"/endpoints", &list); err != nil {
		return []error{err}
	}

	wanted := m.Settings.Fields()
	if m.Needs.Tools {
		wanted = append(wanted, "tools")
	}
	if m.Settings.Reasoning.Mode != api.ReasoningOff {
		wanted = append(wanted, "reasoning")
	}

	var allowed, missing []string
	for _, e := range list.Data.Endpoints {
		if !matches(prov.Routing.Only, e.Tag) {
			continue
		}
		allowed = append(allowed, e.Tag)
		var gaps []string
		for _, w := range wanted {
			if !slices.Contains(e.SupportedParameters, w) {
				gaps = append(gaps, w)
			}
		}
		// A host serves the model with a context of its own, which may be
		// below the model's.
		if held := sizeOf(e.ContextLength); m.Needs.Context > 0 && held > 0 && held < m.Needs.Context {
			gaps = append(gaps, fmt.Sprintf("a context of %d, holding %d", m.Needs.Context, held))
		}
		if len(gaps) == 0 {
			return nil
		}
		missing = append(missing, fmt.Sprintf("%s takes no %s", e.Tag, strings.Join(gaps, ", ")))
	}
	if len(allowed) == 0 {
		return []error{fmt.Errorf("provider.routing.only: no endpoint of the model is one of %v", prov.Routing.Only)}
	}
	return []error{fmt.Errorf("provider.routing.only: %s", strings.Join(missing, "; "))}
}

// matches reports whether an endpoint tag is one of the slugs, where a base
// slug covers its variants, such as novita and novita/fp8.
func matches(only []string, tag string) bool {
	base, _, _ := strings.Cut(tag, "/")
	for _, o := range only {
		if o == tag || o == base {
			return true
		}
	}
	return false
}
