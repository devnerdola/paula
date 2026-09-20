package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// Prompt is a chat request read back from the bytes that were recorded. Every
// runner sends an OpenAI-compatible body, so what one of them recorded is read
// here rather than by the runner that happens to be configured today.
type Prompt struct {
	Model    string
	Messages []PromptMessage
	// Reasoning is the object a runner's hook adds, which both of them write
	// as whether it is on and at what effort.
	Reasoning *PromptReasoning
}

type PromptMessage struct {
	Role string
	Text string
	// Images say what was sent, without the bytes themselves.
	Images []string
}

type PromptReasoning struct {
	Enabled *bool
	Effort  string
}

// ReadPrompt reads a recorded chat request, and reports false for bytes that
// are not one.
func ReadPrompt(b []byte) (*Prompt, bool) {
	if len(b) == 0 {
		return nil, false
	}
	var sent struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Reasoning *struct {
			Enabled *bool  `json:"enabled"`
			Effort  string `json:"effort"`
		} `json:"reasoning"`
	}
	if err := json.Unmarshal(b, &sent); err != nil {
		return nil, false
	}
	out := &Prompt{Model: sent.Model}
	if r := sent.Reasoning; r != nil {
		out.Reasoning = &PromptReasoning{Enabled: r.Enabled, Effort: r.Effort}
	}
	for _, m := range sent.Messages {
		text, images := content(m.Content)
		out.Messages = append(out.Messages, PromptMessage{Role: m.Role, Text: text, Images: images})
	}
	return out, true
}

// content reads a message's content, which is a string or a list of parts.
func content(raw json.RawMessage) (string, []string) {
	if len(raw) == 0 {
		return "", nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return string(raw), nil
	}
	var b strings.Builder
	var images []string
	for _, p := range parts {
		switch p.Type {
		case "text":
			b.WriteString(p.Text)
		case "image_url":
			images = append(images, describe(p.ImageURL.URL))
		}
	}
	return b.String(), images
}

// describe says what an image was, without the whole of it. The size is the
// picture's own, not the length of the base64 it was sent as.
func describe(url string) string {
	head, rest, ok := strings.Cut(url, ",")
	if !ok || !strings.HasPrefix(head, "data:") {
		return url
	}
	mime := strings.TrimSuffix(strings.TrimPrefix(head, "data:"), ";base64")
	size := base64.StdEncoding.DecodedLen(len(rest))
	if b, err := base64.StdEncoding.DecodeString(rest); err == nil {
		size = len(b)
	}
	return fmt.Sprintf("%s, %d bytes", mime, size)
}
