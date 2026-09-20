package runners

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/runners/api"
)

// Configured is a model of the configuration file, with the settings of its
// runner already under its own.
type Configured struct {
	Name string
	// Path is the key the configuration file writes it under, which is what a
	// problem with it is reported as.
	Path     string
	ID       string
	Runner   Runner
	Context  int
	Settings api.Settings
}

// Setup is every runner and model a configuration file names.
type Setup struct {
	Runners  []Runner
	Models   []*Configured
	Defaults map[config.Role]*Configured
}

// Configure opens every runner and lays each model's settings over its
// runner's. It reports every problem it finds at once.
func Configure(cfg *config.Config, host Host) (*Setup, error) {
	s := &Setup{Defaults: map[config.Role]*Configured{}}
	p := &api.Problems{}

	byName := map[string]Runner{}
	for _, rc := range cfg.Runners {
		r, err := Open(rc.Name, rc.Type, rc.Section, host)
		if err != nil {
			p.Add(err)
			continue
		}
		byName[rc.Name] = r
		s.Runners = append(s.Runners, r)
	}

	for _, mc := range cfg.Models {
		r, ok := byName[mc.Runner]
		if !ok {
			continue
		}
		m, err := configure(mc, r)
		if err != nil {
			p.Add(err)
			continue
		}
		s.Models = append(s.Models, m)
	}

	for _, role := range config.Roles {
		name := cfg.DefaultModels.Get(role)
		if name == "" {
			continue
		}
		if m := s.Model(name); m != nil {
			s.Defaults[role] = m
		}
	}
	if err := p.Err(); err != nil {
		return nil, err
	}
	return s, nil
}

func configure(mc config.Model, r Runner) (*Configured, error) {
	// The settings of a model are read here, and the keys config already read
	// are named so that a section is decoded whole: it reports any key it does
	// not know, and these three it does.
	var own struct {
		Runner       string `yaml:"runner"`
		ID           string `yaml:"id"`
		Context      int    `yaml:"context"`
		api.Settings `yaml:",inline"`
		Provider     config.Section `yaml:"provider"`
	}
	if err := mc.Section.Decode(&own); err != nil {
		return nil, err
	}

	base := r.Settings()
	merged := own.Settings.MergedOver(base)
	merged.Provider = config.MergeSections(base.Provider, own.Provider)

	return &Configured{
		Name:     mc.Name,
		Path:     mc.Section.Path(),
		ID:       mc.ID,
		Runner:   r,
		Context:  mc.Context,
		Settings: merged,
	}, nil
}

// Model returns the configured model of that name, or nil.
func (s *Setup) Model(name string) *Configured {
	for _, m := range s.Models {
		if m.Name == name {
			return m
		}
	}
	return nil
}

// RoleNeeds is what a model has to be able to do to serve a role, which is the
// same whatever is configured.
func RoleNeeds(role config.Role) api.Needs {
	switch role {
	case config.RoleChat:
		return api.Needs{Chat: true, Tools: true}
	case config.RoleVision:
		return api.Needs{Vision: true}
	}
	return api.Needs{}
}

// Lookup returns what the runner's catalogue says about a configured model.
func (s *Setup) Lookup(ctx context.Context, m *Configured) (*api.Model, error) {
	return m.Runner.Model(ctx, m.ID)
}

// Serving returns the models that can serve a role, in the order the
// configuration file writes them, and why any of the others could not be
// asked.
func (s *Setup) Serving(ctx context.Context, role config.Role) ([]*Configured, error) {
	serving, err := s.ServingRoles(ctx, []config.Role{role})
	return serving[role], err
}

// ServingRoles returns the models that can serve each of the roles. Every
// catalogue is read once, so a runner that is away is not asked again for
// every role it could have served.
func (s *Setup) ServingRoles(ctx context.Context, roles []config.Role) (map[config.Role][]*Configured, error) {
	out := make(map[config.Role][]*Configured, len(roles))
	var problems []error
	for _, m := range s.Models {
		c, err := s.Lookup(ctx, m)
		if err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", m.Name, err))
			continue
		}
		for _, role := range roles {
			if len(c.Missing(RoleNeeds(role))) == 0 {
				out[role] = append(out[role], m)
			}
		}
	}
	return out, errors.Join(problems...)
}

// Report is what every runner, model and role of a configuration file
// answered.
type Report struct {
	Runners []RunnerStatus
	Models  []ModelStatus
	Roles   []RoleStatus
}

type RunnerStatus struct {
	Runner Runner
	Err    error
}

type ModelStatus struct {
	Model *Configured
	// Catalogue is what the runner says about the model, and is nil when the
	// runner does not serve it.
	Catalogue *api.Model
	Err       error
	// Skipped says the runner did not answer, so nothing was asked of it.
	Skipped bool
}

type RoleStatus struct {
	Role    config.Role
	Model   *Configured
	Err     error
	Skipped bool
}

// Check asks every runner whether it answers and holds every model and role
// against what the catalogues say. It reports every problem at once.
func (s *Setup) Check(ctx context.Context) error {
	report := s.Report(ctx)
	p := &api.Problems{}
	for _, st := range report.Runners {
		p.Add(st.Err)
	}
	for _, st := range report.Models {
		p.Add(st.Err)
	}
	for _, st := range report.Roles {
		p.Add(st.Err)
	}
	return p.Err()
}

// Report runs the same checks and keeps what each runner, model and role
// answered. The models command prints it.
func (s *Setup) Report(ctx context.Context) *Report {
	runners := s.checkRunners(ctx)
	models := s.checkModels(ctx, runners)
	return &Report{
		Runners: runners,
		Models:  models,
		Roles:   s.checkRoles(models),
	}
}

// checkRunners asks every runner whether it answers. They are asked at the
// same time, since one that is slow to answer would hold up the rest.
func (s *Setup) checkRunners(ctx context.Context) []RunnerStatus {
	out := make([]RunnerStatus, len(s.Runners))
	var wg sync.WaitGroup
	for i, r := range s.Runners {
		out[i].Runner = r
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := r.Health(ctx); err != nil {
				out[i].Err = fmt.Errorf("runners.%s: %w", r.Name(), err)
			}
		}()
	}
	wg.Wait()
	return out
}

// checkModels holds every model against the catalogue of its runner. A model
// whose runner never answered is not asked about.
func (s *Setup) checkModels(ctx context.Context, runners []RunnerStatus) []ModelStatus {
	answered := make(map[string]bool, len(runners))
	for _, st := range runners {
		answered[st.Runner.Name()] = st.Err == nil
	}

	out := make([]ModelStatus, len(s.Models))
	var wg sync.WaitGroup
	for i, m := range s.Models {
		out[i] = ModelStatus{Model: m, Skipped: !answered[m.Runner.Name()]}
		if out[i].Skipped {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i].Catalogue, out[i].Err = s.checkModel(ctx, m)
		}()
	}
	wg.Wait()
	return out
}

// checkModel holds one model and its settings against what its runner says
// about it.
func (s *Setup) checkModel(ctx context.Context, m *Configured) (*api.Model, error) {
	catalogue, err := m.Runner.Model(ctx, m.ID)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", m.Path, err)
	}

	// A request carries a prompt as long as the context the file configures,
	// or the one the catalogue reports when the file configures none. Holding
	// that much is part of what the model has to be able to do, so a host that
	// serves it with less is caught here.
	needs := s.needsOf(m)
	needs.Context = m.Context
	if needs.Context == 0 {
		needs.Context = catalogue.Context
	}

	// The runner reports what it finds under the key it is about, and the model
	// is named here, once, above the lot.
	p := &api.Problems{Path: m.Path}
	p.Add(m.Runner.Check(ctx, api.Checked{
		ID:       m.ID,
		Settings: WithDefaults(m.Settings, catalogue),
		Needs:    needs,
	})...)
	if catalogue.Context > 0 && m.Context > catalogue.Context {
		p.Addf("context: %d is above the %d the catalogue reports",
			m.Context, catalogue.Context)
	}
	return catalogue, p.Err()
}

// checkRoles holds the model of every role against what the role asks of it. A
// model its runner said nothing about is left to its own row.
func (s *Setup) checkRoles(models []ModelStatus) []RoleStatus {
	catalogues := make(map[string]*api.Model, len(models))
	for _, st := range models {
		catalogues[st.Model.Name] = st.Catalogue
	}

	var out []RoleStatus
	for _, role := range config.Roles {
		m, ok := s.Defaults[role]
		if !ok {
			continue
		}
		catalogue := catalogues[m.Name]
		st := RoleStatus{Role: role, Model: m, Skipped: catalogue == nil}
		if catalogue != nil {
			p := &api.Problems{}
			for _, err := range catalogue.Missing(RoleNeeds(role)) {
				p.Addf("default_models.%s: %s: %w", role, m.Name, err)
			}
			st.Err = p.Err()
		}
		out = append(out, st)
	}
	return out
}

// needsOf is everything the roles a model is the default of ask of it.
func (s *Setup) needsOf(m *Configured) api.Needs {
	var out api.Needs
	for role, d := range s.Defaults {
		if d == m {
			out = out.With(RoleNeeds(role))
		}
	}
	return out
}

// WithDefaults returns the settings a request to a model carries: what the
// configuration file says, with what only the catalogue can decide filled in.
// A model that reasons does so, at the middle of the efforts it lists.
func WithDefaults(s api.Settings, catalogue *api.Model) api.Settings {
	if s.Reasoning.Mode == "" {
		s.Reasoning.Mode = api.ReasoningOff
		if catalogue.Reasoning {
			s.Reasoning.Mode = api.ReasoningOn
		}
	}
	if s.Reasoning.Effort == "" && s.Reasoning.Mode == api.ReasoningOn {
		s.Reasoning.Effort = api.MiddleEffort(catalogue.Efforts)
	}
	return s
}

// ContextSize is the context a model is used with: what the configuration file
// says, or what the catalogue reports when the file says nothing.
func (s *Setup) ContextSize(ctx context.Context, m *Configured) (int, error) {
	if m.Context > 0 {
		return m.Context, nil
	}
	catalogue, err := m.Runner.Model(ctx, m.ID)
	if err != nil {
		return 0, err
	}
	return catalogue.Context, nil
}
