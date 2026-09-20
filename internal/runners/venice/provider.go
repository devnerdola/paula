package venice

import (
	"fmt"
	"slices"

	"nerdola.dev/x/paula/internal/config"
)

// maxFallbacks is the ten models a request may fall back to.
const maxFallbacks = 10

// provider holds the request parameters only Venice documents.
type provider struct {
	SystemPrompt *bool    `yaml:"system_prompt"`
	Character    string   `yaml:"character"`
	E2EE         *bool    `yaml:"e2ee"`
	Fallbacks    []string `yaml:"fallbacks"`

	Sampling sampling `yaml:"sampling"`
	Output   output   `yaml:"output"`
	Cache    cache    `yaml:"cache"`
	Search   search   `yaml:"search"`
}

type sampling struct {
	MinTemperature *float64 `yaml:"min_temperature"`
	MaxTemperature *float64 `yaml:"max_temperature"`
}

type output struct {
	StopTokenIDs []int  `yaml:"stop_token_ids"`
	Verbosity    string `yaml:"verbosity"`
}

type cache struct {
	Retention string `yaml:"retention"`
}

type search struct {
	Provider string `yaml:"provider"`
}

var (
	verbosities = []string{"low", "medium", "high", "auto"}
	retentions  = []string{"default", "extended", "24h"}
	searchers   = []string{"brave", "google"}
)

func decodeProvider(s config.Section) (*provider, []error) {
	var p provider
	if s.Set() {
		if err := s.Decode(&p); err != nil {
			return nil, []error{err}
		}
	}
	return &p, p.validate()
}

func (p *provider) validate() []error {
	var errs []error
	addf := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	oneOf := func(key, v string, allowed []string) {
		if v != "" && !slices.Contains(allowed, v) {
			addf("provider.%s: %q is not one of %v", key, v, allowed)
		}
	}
	oneOf("output.verbosity", p.Output.Verbosity, verbosities)
	oneOf("cache.retention", p.Cache.Retention, retentions)
	oneOf("search.provider", p.Search.Provider, searchers)

	between := func(key string, v *float64) {
		if v != nil && (*v < 0 || *v > 2) {
			addf("provider.sampling.%s: %v is outside 0 to 2", key, *v)
		}
	}
	between("min_temperature", p.Sampling.MinTemperature)
	between("max_temperature", p.Sampling.MaxTemperature)
	if lo, hi := p.Sampling.MinTemperature, p.Sampling.MaxTemperature; lo != nil && hi != nil && *lo > *hi {
		addf("provider.sampling.min_temperature: %v is above max_temperature %v", *lo, *hi)
	}
	return errs
}

// object is venice_parameters, with the keys that are set. The system prompt
// is switched off unless it is asked for.
func (p *provider) object() map[string]any {
	out := map[string]any{"include_venice_system_prompt": false}
	if p.SystemPrompt != nil {
		out["include_venice_system_prompt"] = *p.SystemPrompt
	}
	if p.Character != "" {
		out["character_slug"] = p.Character
	}
	if p.E2EE != nil {
		out["enable_e2ee"] = *p.E2EE
	}
	return out
}
