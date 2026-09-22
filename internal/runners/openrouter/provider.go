package openrouter

import (
	"slices"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/runners/api"
	"nerdola.dev/x/paula/internal/runners/openai"
)

// provider holds the request parameters only OpenRouter documents.
type provider struct {
	Routing   routing   `yaml:"routing"`
	Reasoning reasoning `yaml:"reasoning"`
	Sampling  sampling  `yaml:"sampling"`
	Cache     cache     `yaml:"cache"`

	ServiceTier string `yaml:"service_tier"`
}

type routing struct {
	Order                  []string           `yaml:"order"`
	Only                   []string           `yaml:"only"`
	Ignore                 []string           `yaml:"ignore"`
	AllowFallbacks         *bool              `yaml:"allow_fallbacks"`
	RequireParameters      *bool              `yaml:"require_parameters"`
	DataCollection         string             `yaml:"data_collection"`
	ZDR                    *bool              `yaml:"zdr"`
	EnforceDistillableText *bool              `yaml:"enforce_distillable_text"`
	Quantizations          []string           `yaml:"quantizations"`
	Sort                   sort               `yaml:"sort"`
	PreferredMinThroughput map[string]float64 `yaml:"preferred_min_throughput"`
	PreferredMaxLatency    map[string]float64 `yaml:"preferred_max_latency"`
	MaxPrice               map[string]float64 `yaml:"max_price"`
}

type sort struct {
	By        string `yaml:"by"`
	Partition string `yaml:"partition"`
}

type reasoning struct {
	MaxTokens *int `yaml:"max_tokens"`
}

type sampling struct {
	TopA      *float64           `yaml:"top_a"`
	LogitBias map[string]float64 `yaml:"logit_bias"`
}

type cache struct {
	Control control `yaml:"control"`
}

type control struct {
	Type string `yaml:"type"`
	TTL  string `yaml:"ttl"`
}

var (
	dataCollections = []string{"allow", "deny"}
	quantizations   = []string{"int4", "int8", "fp4", "mxfp4", "nvfp4", "fp6",
		"fp8", "mxfp8", "fp16", "bf16", "fp32", "unknown"}
	sortsBy     = []string{"price", "throughput", "latency", "exacto"}
	partitions  = []string{"model", "none"}
	percentiles = []string{"p50", "p75", "p90", "p95", "p99"}
	tiers       = []string{"auto", "default", "fast", "flex", "priority", "scale"}
	prices      = []string{"prompt", "completion", "image", "audio", "request"}
	ttls        = []string{"5m", "1h"}
)

// decodeProvider reads the block only OpenRouter documents.
func decodeProvider(s config.Section) (*provider, []error) {
	return openai.DecodeProvider[provider](s)
}

// Validate holds the block against what the API documents.
func (p *provider) Validate() []error {
	out := &api.Problems{}
	addf := out.Addf

	api.OneOf(out, "provider.routing.data_collection", p.Routing.DataCollection, dataCollections)
	api.OneOf(out, "provider.routing.sort.by", p.Routing.Sort.By, sortsBy)
	api.OneOf(out, "provider.routing.sort.partition", p.Routing.Sort.Partition, partitions)
	api.OneOf(out, "provider.service_tier", p.ServiceTier, tiers)
	api.OneOf(out, "provider.cache.control.ttl", p.Cache.Control.TTL, ttls)
	if t := p.Cache.Control.Type; t != "" && t != "ephemeral" {
		addf("provider.cache.control.type: %q is not ephemeral", t)
	}
	if p.Routing.Sort.Partition != "" && p.Routing.Sort.By == "" {
		addf("provider.routing.sort.partition: no sort.by is set")
	}
	for _, q := range p.Routing.Quantizations {
		if !slices.Contains(quantizations, q) {
			addf("provider.routing.quantizations: %q is not one of %v", q, quantizations)
		}
	}
	for k := range p.Routing.MaxPrice {
		if !slices.Contains(prices, k) {
			addf("provider.routing.max_price.%s: not one of %v", k, prices)
		}
	}
	check := func(key string, v map[string]float64) {
		for k := range v {
			if !slices.Contains(percentiles, k) {
				addf("provider.routing.%s.%s: not one of %v", key, k, percentiles)
			}
		}
	}
	check("preferred_min_throughput", p.Routing.PreferredMinThroughput)
	check("preferred_max_latency", p.Routing.PreferredMaxLatency)

	if p.Reasoning.MaxTokens != nil && *p.Reasoning.MaxTokens <= 0 {
		addf("provider.reasoning.max_tokens: %d is not above zero", *p.Reasoning.MaxTokens)
	}
	return out.All()
}

// object is the API's provider object, with the keys that are set.
func (r routing) object() map[string]any {
	out := map[string]any{}
	put := func(key string, v any) {
		if v != nil {
			out[key] = v
		}
	}
	if len(r.Order) > 0 {
		out["order"] = r.Order
	}
	if len(r.Only) > 0 {
		out["only"] = r.Only
	}
	if len(r.Ignore) > 0 {
		out["ignore"] = r.Ignore
	}
	if len(r.Quantizations) > 0 {
		out["quantizations"] = r.Quantizations
	}
	if len(r.MaxPrice) > 0 {
		out["max_price"] = r.MaxPrice
	}
	if r.AllowFallbacks != nil {
		put("allow_fallbacks", *r.AllowFallbacks)
	}
	if r.RequireParameters != nil {
		put("require_parameters", *r.RequireParameters)
	}
	if r.ZDR != nil {
		put("zdr", *r.ZDR)
	}
	if r.EnforceDistillableText != nil {
		put("enforce_distillable_text", *r.EnforceDistillableText)
	}
	if r.DataCollection != "" {
		out["data_collection"] = r.DataCollection
	}
	if r.Sort.By != "" {
		if r.Sort.Partition != "" {
			out["sort"] = map[string]any{"by": r.Sort.By, "partition": r.Sort.Partition}
		} else {
			out["sort"] = r.Sort.By
		}
	}
	if len(r.PreferredMinThroughput) > 0 {
		out["preferred_min_throughput"] = r.PreferredMinThroughput
	}
	if len(r.PreferredMaxLatency) > 0 {
		out["preferred_max_latency"] = r.PreferredMaxLatency
	}
	return out
}
