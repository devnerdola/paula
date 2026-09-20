package api

import (
	"fmt"
	"slices"

	"nerdola.dev/x/paula/internal/config"
)

// Reasoning modes.
const (
	ReasoningOn  = "on"
	ReasoningOff = "off"
)

// Efforts are the reasoning efforts, from the lowest to the highest.
var Efforts = []string{"minimal", "low", "medium", "high", "xhigh", "max"}

// MiddleEffort is the effort a model gets when the configuration file sets
// none: the middle of the efforts the model lists.
func MiddleEffort(efforts []string) string {
	ordered := Order(efforts)
	if len(ordered) == 0 {
		return ""
	}
	return ordered[(len(ordered)-1)/2]
}

// Order sorts efforts from the lowest to the highest, dropping any name that
// is not an effort.
func Order(efforts []string) []string {
	var out []string
	for _, e := range Efforts {
		if slices.Contains(efforts, e) {
			out = append(out, e)
		}
	}
	return out
}

// Settings are the request parameters both APIs document.
type Settings struct {
	Reasoning ReasoningSettings `yaml:"reasoning"`
	Sampling  SamplingSettings  `yaml:"sampling"`
	Output    OutputSettings    `yaml:"output"`

	// Provider holds the keys only one API has. It is filled in after
	// decoding, since only the runner knows them.
	Provider config.Section `yaml:"-"`
}

type ReasoningSettings struct {
	Mode    string `yaml:"mode"`
	Effort  string `yaml:"effort"`
	Summary string `yaml:"summary"`
}

type SamplingSettings struct {
	Temperature       *float64 `yaml:"temperature"`
	TopP              *float64 `yaml:"top_p"`
	TopK              *int     `yaml:"top_k"`
	MinP              *float64 `yaml:"min_p"`
	RepetitionPenalty *float64 `yaml:"repetition_penalty"`
	PresencePenalty   *float64 `yaml:"presence_penalty"`
	FrequencyPenalty  *float64 `yaml:"frequency_penalty"`
	Seed              *int64   `yaml:"seed"`
}

type OutputSettings struct {
	MaxTokens *int     `yaml:"max_tokens"`
	Stop      []string `yaml:"stop"`
}

// MergedOver lays s over base, key by key.
func (s Settings) MergedOver(base Settings) Settings {
	out := base
	if s.Reasoning.Mode != "" {
		out.Reasoning.Mode = s.Reasoning.Mode
	}
	if s.Reasoning.Effort != "" {
		out.Reasoning.Effort = s.Reasoning.Effort
	}
	if s.Reasoning.Summary != "" {
		out.Reasoning.Summary = s.Reasoning.Summary
	}
	over(&out.Sampling.Temperature, s.Sampling.Temperature)
	over(&out.Sampling.TopP, s.Sampling.TopP)
	over(&out.Sampling.TopK, s.Sampling.TopK)
	over(&out.Sampling.MinP, s.Sampling.MinP)
	over(&out.Sampling.RepetitionPenalty, s.Sampling.RepetitionPenalty)
	over(&out.Sampling.PresencePenalty, s.Sampling.PresencePenalty)
	over(&out.Sampling.FrequencyPenalty, s.Sampling.FrequencyPenalty)
	over(&out.Sampling.Seed, s.Sampling.Seed)
	over(&out.Output.MaxTokens, s.Output.MaxTokens)
	if s.Output.Stop != nil {
		out.Output.Stop = s.Output.Stop
	}
	return out
}

func over[T any](dst **T, v *T) {
	if v != nil {
		*dst = v
	}
}

// Fields are the API field names of the settings that are set, which is what
// a catalogue that lists request parameters is checked against.
func (s Settings) Fields() []string {
	var out []string
	add := func(name string, set bool) {
		if set {
			out = append(out, name)
		}
	}
	add("temperature", s.Sampling.Temperature != nil)
	add("top_p", s.Sampling.TopP != nil)
	add("top_k", s.Sampling.TopK != nil)
	add("min_p", s.Sampling.MinP != nil)
	add("repetition_penalty", s.Sampling.RepetitionPenalty != nil)
	add("presence_penalty", s.Sampling.PresencePenalty != nil)
	add("frequency_penalty", s.Sampling.FrequencyPenalty != nil)
	add("seed", s.Sampling.Seed != nil)
	add("max_tokens", s.Output.MaxTokens != nil)
	add("stop", len(s.Output.Stop) > 0)
	return out
}

// stopLimit is how many stop sequences both APIs document.
const stopLimit = 4

// Validate reports the settings whose value is outside what the APIs document.
// What only a catalogue can answer is checked by the runner itself.
func (s Settings) Validate() []error {
	var errs []error
	addf := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	switch s.Reasoning.Mode {
	case "", ReasoningOn, ReasoningOff:
	default:
		addf("reasoning.mode: %q is not on or off", s.Reasoning.Mode)
	}
	switch s.Reasoning.Summary {
	case "", "auto", "concise", "detailed":
	default:
		addf("reasoning.summary: %q is not auto, concise or detailed", s.Reasoning.Summary)
	}
	if e := s.Reasoning.Effort; e != "" && !slices.Contains(Efforts, e) {
		addf("reasoning.effort: %q is not one of %v", e, Efforts)
	}

	between(&errs, "sampling.temperature", s.Sampling.Temperature, 0, 2)
	between(&errs, "sampling.top_p", s.Sampling.TopP, 0, 1)
	between(&errs, "sampling.min_p", s.Sampling.MinP, 0, 1)
	between(&errs, "sampling.presence_penalty", s.Sampling.PresencePenalty, -2, 2)
	between(&errs, "sampling.frequency_penalty", s.Sampling.FrequencyPenalty, -2, 2)
	if v := s.Sampling.TopK; v != nil && *v < 0 {
		addf("sampling.top_k: %d is below zero", *v)
	}
	if v := s.Sampling.RepetitionPenalty; v != nil && *v < 0 {
		addf("sampling.repetition_penalty: %v is below zero", *v)
	}
	if s.Reasoning.Mode == ReasoningOff {
		if s.Reasoning.Effort != "" {
			addf("reasoning.effort: an effort is set with reasoning.mode off")
		}
		if s.Reasoning.Summary != "" {
			addf("reasoning.summary: a summary is asked for with reasoning.mode off")
		}
	}
	if v := s.Output.MaxTokens; v != nil && *v <= 0 {
		addf("output.max_tokens: %d is not above zero", *v)
	}
	if len(s.Output.Stop) > stopLimit {
		addf("output.stop: %d sequences, at most %d are documented", len(s.Output.Stop), stopLimit)
	}
	return errs
}

// CheckReasoning holds the reasoning settings against what a model's listing
// says about it: whether it reasons, whether it always does, and which efforts
// it takes. A setting that is not one at all is reported by Validate, and what
// the model lists adds nothing to that.
func CheckReasoning(p *Problems, m *Model, s Settings) {
	switch s.Reasoning.Mode {
	case ReasoningOff:
		if m.Mandatory {
			p.Addf("reasoning.mode: the model always reasons")
		}
	case ReasoningOn:
		if !m.Reasoning {
			p.Addf("reasoning.mode: the model does not reason")
		}
	}
	if e := s.Reasoning.Effort; e != "" && slices.Contains(Efforts, e) {
		switch {
		case !m.Reasoning:
			p.Addf("reasoning.effort: the model does not reason")
		case len(m.Efforts) == 0:
			p.Addf("reasoning.effort: the model lists no efforts")
		case !slices.Contains(m.Efforts, e):
			p.Addf("reasoning.effort: %q is not one of %v", e, m.Efforts)
		}
	}
}

func between(errs *[]error, key string, v *float64, lo, hi float64) {
	if v != nil && (*v < lo || *v > hi) {
		*errs = append(*errs, fmt.Errorf("%s: %v is outside %v to %v", key, *v, lo, hi))
	}
}
