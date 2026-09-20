package api

import (
	"slices"
	"strings"
	"testing"
)

func TestMiddleEffort(t *testing.T) {
	for _, tc := range []struct {
		efforts []string
		want    string
	}{
		{nil, ""},
		{[]string{"low"}, "low"},
		{[]string{"low", "high"}, "low"},
		{[]string{"low", "medium", "high"}, "medium"},
		{[]string{"high", "low", "medium"}, "medium"},
		{[]string{"minimal", "low", "medium", "high"}, "low"},
		{[]string{"minimal", "low", "medium", "high", "xhigh"}, "medium"},
		{[]string{"none", "low", "medium", "high"}, "medium"},
		{[]string{"nonsense"}, ""},
	} {
		if got := MiddleEffort(tc.efforts); got != tc.want {
			t.Errorf("MiddleEffort(%v) = %q, want %q", tc.efforts, got, tc.want)
		}
	}
}

func TestOrderLeavesOutWhatIsNotNamed(t *testing.T) {
	got := Order([]string{"max", "none", "low", "made-up"})
	if !slices.Equal(got, []string{"low", "max"}) {
		t.Errorf("Order = %v", got)
	}
}

func TestMergedOver(t *testing.T) {
	base := Settings{
		Reasoning: ReasoningSettings{Mode: ReasoningOn, Effort: "high", Summary: "auto"},
		Sampling:  SamplingSettings{Temperature: new(0.7), TopP: new(0.9)},
		Output:    OutputSettings{MaxTokens: new(100), Stop: []string{"a"}},
	}
	over := Settings{
		Reasoning: ReasoningSettings{Effort: "low"},
		Sampling:  SamplingSettings{Temperature: new(1.2)},
		Output:    OutputSettings{Stop: []string{"b", "c"}},
	}
	got := over.MergedOver(base)

	if got.Reasoning.Mode != ReasoningOn || got.Reasoning.Summary != "auto" {
		t.Errorf("reasoning = %+v, want the runner's kept", got.Reasoning)
	}
	if got.Reasoning.Effort != "low" {
		t.Errorf("effort = %q, want the model's", got.Reasoning.Effort)
	}
	if *got.Sampling.Temperature != 1.2 || *got.Sampling.TopP != 0.9 {
		t.Errorf("sampling = %+v", got.Sampling)
	}
	if *got.Output.MaxTokens != 100 || !slices.Equal(got.Output.Stop, []string{"b", "c"}) {
		t.Errorf("output = %+v", got.Output)
	}
	if *base.Sampling.Temperature != 0.7 {
		t.Error("the runner's settings were changed")
	}
}

func TestFields(t *testing.T) {
	s := Settings{
		Sampling: SamplingSettings{Temperature: new(0.7), Seed: new(int64(3))},
		Output:   OutputSettings{MaxTokens: new(10)},
	}
	got := s.Fields()
	if !slices.Equal(got, []string{"temperature", "seed", "max_tokens"}) {
		t.Errorf("Fields = %v", got)
	}
	if len(Settings{}.Fields()) != 0 {
		t.Errorf("Fields of nothing = %v", Settings{}.Fields())
	}
}

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    Settings
		want string
	}{
		{"mode", Settings{Reasoning: ReasoningSettings{Mode: "yes"}}, "reasoning.mode"},
		{"summary", Settings{Reasoning: ReasoningSettings{Summary: "long"}}, "reasoning.summary"},
		{"effort", Settings{Reasoning: ReasoningSettings{Effort: "enormous"}}, "reasoning.effort"},
		{"temperature", Settings{Sampling: SamplingSettings{Temperature: new(2.5)}}, "sampling.temperature"},
		{"top_p", Settings{Sampling: SamplingSettings{TopP: new(1.5)}}, "sampling.top_p"},
		{"min_p", Settings{Sampling: SamplingSettings{MinP: new(-0.1)}}, "sampling.min_p"},
		{"presence", Settings{Sampling: SamplingSettings{PresencePenalty: new(3.0)}}, "sampling.presence_penalty"},
		{"frequency", Settings{Sampling: SamplingSettings{FrequencyPenalty: new(-3.0)}}, "sampling.frequency_penalty"},
		{"top_k", Settings{Sampling: SamplingSettings{TopK: new(-1)}}, "sampling.top_k"},
		{"repetition", Settings{Sampling: SamplingSettings{RepetitionPenalty: new(-1.0)}}, "sampling.repetition_penalty"},
		{"max_tokens", Settings{Output: OutputSettings{MaxTokens: new(0)}}, "output.max_tokens"},
		{"stop", Settings{Output: OutputSettings{Stop: []string{"a", "b", "c", "d", "e"}}}, "output.stop"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs := tc.s.Validate()
			if len(errs) != 1 {
				t.Fatalf("errors = %v, want one", errs)
			}
			if !strings.HasPrefix(errs[0].Error(), tc.want) {
				t.Errorf("error = %q, want it to start with %q", errs[0], tc.want)
			}
		})
	}
}

func TestValidateAcceptsWhatIsDocumented(t *testing.T) {
	s := Settings{
		Reasoning: ReasoningSettings{Mode: ReasoningOn, Effort: "max", Summary: "detailed"},
		Sampling: SamplingSettings{
			Temperature: new(2.0), TopP: new(1.0), TopK: new(0), MinP: new(0.0),
			RepetitionPenalty: new(0.0), PresencePenalty: new(-2.0),
			FrequencyPenalty: new(2.0), Seed: new(int64(-1)),
		},
		Output: OutputSettings{MaxTokens: new(1), Stop: []string{"a", "b", "c", "d"}},
	}
	if errs := s.Validate(); len(errs) != 0 {
		t.Errorf("errors = %v", errs)
	}
}

func TestAPIError(t *testing.T) {
	e := &APIError{Status: 429, Code: "rate_limit", Type: "requests", Message: "slow down"}
	if want := "429 requests rate_limit: slow down"; e.Error() != want {
		t.Errorf("Error = %q, want %q", e, want)
	}
	if got := (&APIError{Status: 500}).Error(); got != "500" {
		t.Errorf("Error = %q", got)
	}
}

// An error the stream carried has no status of its own, and reads without a 0
// in front of it.
func TestAPIErrorWithoutAStatus(t *testing.T) {
	e := &APIError{Code: "rate_limited", Message: "slow down"}
	if want := "rate_limited: slow down"; e.Error() != want {
		t.Errorf("Error = %q, want %q", e.Error(), want)
	}
	if got := (&APIError{Message: "nothing else"}).Error(); got != "nothing else" {
		t.Errorf("Error = %q", got)
	}
}
