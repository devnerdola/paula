package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture is one of the bodies kept beside this file. See testdata/SOURCES.md.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The bodies are two Paula sent and an API answered, kept beside this file.
// paula turns shows a prompt by reading the body back, so it has to keep reading
// the ones already recorded.
func TestReadPromptReadsBackWhatWasSent(t *testing.T) {
	p, ok := ReadPrompt(fixture(t, "chat_request.json"))
	if !ok {
		t.Fatal("ReadPrompt did not read the body")
	}
	if p.Model != "deepseek/deepseek-v4-pro-0813" || len(p.Messages) != 2 {
		t.Fatalf("prompt = %+v", p)
	}
	if p.Messages[0].Role != RoleSystem || !strings.HasPrefix(p.Messages[0].Text, "You are Ada") {
		t.Errorf("first message = %+v", p.Messages[0])
	}
	if !strings.HasSuffix(p.Messages[1].Text, "hey, how are you?") {
		t.Errorf("second message = %+v", p.Messages[1])
	}
	if p.Reasoning == nil || p.Reasoning.Enabled == nil || !*p.Reasoning.Enabled || p.Reasoning.Effort != "high" {
		t.Errorf("reasoning = %+v", p.Reasoning)
	}

	// The image of a caption comes back as its type and its size, not as the
	// bytes again.
	p, ok = ReadPrompt(fixture(t, "chat_request_image.json"))
	if !ok {
		t.Fatal("ReadPrompt did not read the body carrying an image")
	}
	if len(p.Messages) != 1 || len(p.Messages[0].Images) != 1 {
		t.Fatalf("prompt = %+v", p)
	}
	if !strings.HasPrefix(p.Messages[0].Text, "Describe this image") {
		t.Errorf("message = %+v", p.Messages[0])
	}
	if got := p.Messages[0].Images[0]; !strings.HasPrefix(got, "image/jpeg, ") {
		t.Errorf("image = %q", got)
	}

	if _, ok := ReadPrompt([]byte("not a body at all")); ok {
		t.Error("ReadPrompt read something that is not a request")
	}
	if _, ok := ReadPrompt(nil); ok {
		t.Error("ReadPrompt read nothing as a request")
	}
}
