package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPersonaCheck(t *testing.T) {
	path := filepath.Join(t.TempDir(), "paula.yaml")
	body := "persona: " + card(t, "teasing.yaml") +
		"\nrunners:\n  r:\n    type: openrouter\nmodels:\n  a:\n    runner: r\n    id: x\ndefault_models:\n  chat: a\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := exec(t, "-config", path, "persona-check")
	if code != 0 {
		t.Fatalf("code = %d, stderr %s", code, errOut)
	}
	if !strings.HasPrefix(out, "You are Ada, texting with Caio.") {
		t.Errorf("output = %q", out)
	}
	if !strings.Contains(out, "- She teases Caio.") {
		t.Errorf("output does not fill in the names: %q", out)
	}
}

func TestAConfigurationWithoutACard(t *testing.T) {
	path := filepath.Join(t.TempDir(), "paula.yaml")
	body := "runners:\n  r:\n    type: openrouter\nmodels:\n  a:\n    runner: r\n    id: x\ndefault_models:\n  chat: a\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := exec(t, "-config", path, "persona-check")
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(errOut, "persona: no character card is set") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestPersonaCheckTakesNoArguments(t *testing.T) {
	code, _, errOut := exec(t, "persona-check", "extra")
	if code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
	if !strings.Contains(errOut, "no arguments are taken") {
		t.Errorf("stderr = %q", errOut)
	}
}
