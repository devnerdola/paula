package openai

import (
	"context"
	"os"
	"slices"
	"sync"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/logs"
	"nerdola.dev/x/paula/internal/runners/api"
)

// Config is what every OpenAI-compatible runner is configured with. A runner
// decodes it with the defaults of its own API, and adds the keys only it takes.
type Config struct {
	// Type is what the kind table read to know which runner to open. It is
	// named here so that a section is decoded whole, since a section reports
	// any key it does not know.
	Type           string          `yaml:"type"`
	URL            string          `yaml:"url"`
	TokenEnv       string          `yaml:"token_env"`
	IdleTimeout    config.Duration `yaml:"idle_timeout"`
	RequestTimeout config.Duration `yaml:"request_timeout"`
	// Retries is a pointer so that nothing tells zero retries from a file that
	// asks for none. The defaults a runner passes set it.
	Retries *int `yaml:"retries"`

	api.Settings `yaml:",inline"`
	Provider     config.Section `yaml:"provider"`

	token string
}

// Decode reads a runner section over the defaults it is given, and reports what
// is wrong with the keys every runner takes. A runner adds its own problems to
// what comes back before asking for the client.
func Decode(s config.Section, defaults Config) (Config, *api.Problems) {
	cfg := defaults
	p := &api.Problems{Path: s.Path()}
	if err := s.Decode(&cfg); err != nil {
		p.Add(err)
		return cfg, p
	}

	p.Add(cfg.Settings.Validate()...)
	cfg.token = os.Getenv(cfg.TokenEnv)
	switch {
	case cfg.token == "":
		p.Addf("token_env: environment variable %s is not set", cfg.TokenEnv)
	case len(cfg.token) < logs.MinSecret:
		// A value this short is no key, and it is too short to be redacted, so
		// it would be written to the log as it is.
		p.Addf("token_env: the value of %s is %d bytes, too short to be a key",
			cfg.TokenEnv, len(cfg.token))
	}
	if *cfg.Retries < 0 {
		p.Addf("retries: %d is below zero", *cfg.Retries)
	}
	if cfg.IdleTimeout <= 0 {
		p.Addf("idle_timeout: %s is not above zero", cfg.IdleTimeout)
	}
	if cfg.RequestTimeout <= 0 {
		p.Addf("request_timeout: %s is not above zero", cfg.RequestTimeout)
	}
	return cfg, p
}

// Client is the client the configuration describes. The token is registered as a
// secret of the run, since from here on it is sent with every request.
func (c Config) Client(name string, h api.Host, hooks Hooks) *Client {
	h.Secrets.Add(c.token)
	return &Client{
		Runner:         name,
		BaseURL:        c.URL,
		Token:          c.token,
		IdleTimeout:    c.IdleTimeout.Duration(),
		RequestTimeout: c.RequestTimeout.Duration(),
		Retries:        *c.Retries,
		Hooks:          hooks,
		Log:            h.Log,
		Secrets:        h.Secrets,
	}
}

// Catalogue is the listing a runner serves, read once per run. Nothing of it is
// kept between runs: both APIs answer the listing no-store, and a run that
// cannot read it has no host to talk to either.
type Catalogue struct {
	// Read asks the API for the listing. A runner sets it when it opens.
	Read func(ctx context.Context) ([]api.Model, error)

	mu     sync.Mutex
	models []api.Model
	loaded bool
}

// Models is everything the runner serves. A read that failed is not held onto,
// so a runner that was away is asked again.
func (c *Catalogue) Models(ctx context.Context) ([]api.Model, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.loaded {
		models, err := c.Read(ctx)
		if err != nil {
			return nil, err
		}
		c.models, c.loaded = models, true
	}
	return slices.Clone(c.models), nil
}

// Find is one model of the listing, and false for one the runner does not
// serve, which the runner then says in its own words.
func (c *Catalogue) Find(ctx context.Context, id string) (*api.Model, bool, error) {
	models, err := c.Models(ctx)
	if err != nil {
		return nil, false, err
	}
	for i := range models {
		if models[i].ID == id {
			return &models[i], true, nil
		}
	}
	return nil, false, nil
}
