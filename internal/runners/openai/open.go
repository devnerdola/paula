package openai

import (
	"context"
	"slices"
	"sync"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/runners/api"
	"nerdola.dev/x/paula/internal/runners/transport"
)

// Config is what every OpenAI-compatible runner is configured with: its
// connection, and the settings its models take unless they say otherwise. A
// runner adds the keys only it takes.
type Config struct {
	transport.Connection `yaml:",inline"`
	api.Settings         `yaml:",inline"`
	Provider             config.Section `yaml:"provider"`
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
	cfg.Check(p)
	return cfg, p
}

// Validator is a runner's own provider block, which holds itself against what
// its API documents.
type Validator interface {
	Validate() []error
}

// DecodeProvider reads the provider block of a section into the type a runner
// keeps it in, and holds it against what that API documents. A block that was
// not written reads as one with nothing set, which every key of it then takes
// the default of.
func DecodeProvider[T any, P interface {
	*T
	Validator
}](s config.Section) (P, []error) {
	p := P(new(T))
	if s.Set() {
		if err := s.Decode(p); err != nil {
			return nil, []error{err}
		}
	}
	return p, p.Validate()
}

// Catalogue is the listing a runner serves, read once per run. Nothing of it is
// kept between runs: both APIs answer the listing no-store, and a run that
// cannot read it has no host to talk to either.
type Catalogue struct {
	// Read asks the API for the listing. A runner sets it when it opens.
	Read func(ctx context.Context) ([]api.Model, error)

	mu     sync.Mutex
	models []api.Model
	// byID is the listing by the id the API knows each model by, since looking
	// one up is what every reply, caption and fold step does and reading the
	// listing is not.
	byID   map[string]api.Model
	loaded bool
}

// Models is everything the runner serves. A read that failed is not held onto,
// so a runner that was away is asked again.
func (c *Catalogue) Models(ctx context.Context) ([]api.Model, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.read(ctx); err != nil {
		return nil, err
	}
	return slices.Clone(c.models), nil
}

// read loads the listing once, with the lock already held.
func (c *Catalogue) read(ctx context.Context) error {
	if c.loaded {
		return nil
	}
	models, err := c.Read(ctx)
	if err != nil {
		return err
	}
	c.byID = make(map[string]api.Model, len(models))
	for _, m := range models {
		c.byID[m.ID] = m
	}
	c.models, c.loaded = models, true
	return nil
}

// Find is one model of the listing, and false for one the runner does not
// serve, which the runner then says in its own words.
func (c *Catalogue) Find(ctx context.Context, id string) (*api.Model, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.read(ctx); err != nil {
		return nil, false, err
	}
	m, ok := c.byID[id]
	if !ok {
		return nil, false, nil
	}
	return &m, true, nil
}
