package venice

import (
	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/runners/api"
	"nerdola.dev/x/paula/internal/runners/openai"
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

// decodeProvider reads the block only Venice documents.
func decodeProvider(s config.Section) (*provider, []error) {
	return openai.DecodeProvider[provider](s)
}

// Validate holds the block against what the API documents.
func (p *provider) Validate() []error {
	out := &api.Problems{}
	api.OneOf(out, "provider.output.verbosity", p.Output.Verbosity, verbosities)
	api.OneOf(out, "provider.cache.retention", p.Cache.Retention, retentions)
	api.OneOf(out, "provider.search.provider", p.Search.Provider, searchers)

	api.Between(out, "provider.sampling.min_temperature", p.Sampling.MinTemperature, 0, 2)
	api.Between(out, "provider.sampling.max_temperature", p.Sampling.MaxTemperature, 0, 2)
	if lo, hi := p.Sampling.MinTemperature, p.Sampling.MaxTemperature; lo != nil && hi != nil && *lo > *hi {
		out.Addf("provider.sampling.min_temperature: %v is above max_temperature %v", *lo, *hi)
	}
	return out.All()
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
