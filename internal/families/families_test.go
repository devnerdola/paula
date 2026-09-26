package families

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"nerdola.dev/x/paula/internal/runners/openrouter"
	"nerdola.dev/x/paula/internal/runners/venice"
)

func listed(t *testing.T, name string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(b))
}

// With tools in a request, only DeepSeek's chat template keeps the thinking of
// earlier turns, so only DeepSeek is sent what earlier replies thought.
func TestOnlyDeepSeekIsSentWhatEarlierRepliesThought(t *testing.T) {
	for name, e := range table {
		x, err := e.open(true)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := x.PastThought(), strings.HasSuffix(name, "/deepseek"); got != want {
			t.Errorf("%s asks for what earlier replies thought: %v, want %v", name, got, want)
		}
	}
}

func TestEveryExtensionIsForARunnerKind(t *testing.T) {
	for name, e := range table {
		if e.runner != openrouter.Kind && e.runner != venice.Kind {
			t.Errorf("%s is for %q, which is no runner kind", name, e.runner)
		}
	}
}

// An extension changes what only its own models are sent, so no model a host
// lists is served by two.
func TestNoModelIsServedByTwoExtensions(t *testing.T) {
	for runner, file := range map[string]string{
		openrouter.Kind: "openrouter-ids.txt",
		venice.Kind:     "venice-ids.txt",
	} {
		for _, id := range listed(t, file) {
			if names := Match(runner, id); len(names) > 1 {
				t.Errorf("the %s model %q is served by %v", runner, id, names)
			}
		}
	}
}

// Claude, GPT and DeepSeek are each served on both hosts, under the ids each
// host lists them by. A model none of them is, and a GPT model from before 5.6,
// whose cache follows other rules, are served by none.
func TestEachFamilyIsServedOnEachHost(t *testing.T) {
	ids := map[string][]string{
		openrouter.Kind: listed(t, "openrouter-ids.txt"),
		venice.Kind:     listed(t, "venice-ids.txt"),
	}
	for _, tc := range []struct {
		runner, id, want string
	}{
		{openrouter.Kind, "anthropic/claude-opus-5.5", "openrouter/claude"},
		{openrouter.Kind, "~anthropic/claude-opus-latest", "openrouter/claude"},
		{openrouter.Kind, "openai/gpt-6-astra", "openrouter/gpt"},
		{openrouter.Kind, "openai/gpt-5.6-luna", "openrouter/gpt"},
		{openrouter.Kind, "~openai/gpt-sol-latest", "openrouter/gpt"},
		{openrouter.Kind, "openai/gpt-5.5", ""},
		{openrouter.Kind, "~openai/gpt-mini-latest", ""},
		{openrouter.Kind, "deepseek/deepseek-v4-pro-0813", "openrouter/deepseek"},
		{openrouter.Kind, "~deepseek/deepseek-flash-latest", "openrouter/deepseek"},
		{venice.Kind, "claude-opus-5-5", "venice/claude"},
		{venice.Kind, "openai-gpt-56-luna", "venice/gpt"},
		{venice.Kind, "openai-gpt-6-astra", "venice/gpt"},
		{venice.Kind, "openai-gpt-55", ""},
		{venice.Kind, "openai-gpt-oss-120b", ""},
		{venice.Kind, "deepseek-v4-pro-0813", "venice/deepseek"},
		{venice.Kind, "e2ee-deepseek-v4-flash", ""},
		{venice.Kind, "z-ai-glm-5-3", ""},
	} {
		if !slices.Contains(ids[tc.runner], tc.id) {
			t.Errorf("%s lists no %q", tc.runner, tc.id)
			continue
		}
		var want []string
		if tc.want != "" {
			want = []string{tc.want}
		}
		if got := Match(tc.runner, tc.id); !slices.Equal(got, want) {
			t.Errorf("the %s model %q is served by %v, want %v", tc.runner, tc.id, got, want)
		}
	}
}
