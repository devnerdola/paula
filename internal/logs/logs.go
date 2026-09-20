// Package logs builds Paula's logger and keeps secrets out of what it writes.
package logs

import (
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
)

// MinSecret is the shortest value that can be redacted: a shorter one would
// mangle ordinary text.
const MinSecret = 8

// Mask replaces a secret wherever it is written.
const Mask = "[redacted]"

// Secrets are the values that must never be written. A runner adds the token it
// was given, and the logger the same Secrets was built with replaces it wherever
// it turns up in what is logged. No Secrets at all holds nothing and replaces
// nothing, which is what a caller with nothing to hide is given.
type Secrets struct {
	mu   sync.RWMutex
	list []string
}

// Add registers a value to be replaced wherever Paula writes it. A value shorter
// than MinSecret is not registered.
func (s *Secrets) Add(v string) {
	if s == nil || len(v) < MinSecret {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if slices.Contains(s.list, v) {
		return
	}
	s.list = append(s.list, v)
	// The longest first, so a secret that holds a shorter one is replaced whole.
	slices.SortFunc(s.list, func(a, b string) int { return len(b) - len(a) })
}

// Redact replaces every registered value in text.
func (s *Secrets) Redact(text string) string {
	if s == nil {
		return text
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, v := range s.list {
		text = strings.ReplaceAll(text, v, Mask)
	}
	return text
}

// Format is how a log is written. The zero Format is text.
type Format string

// The formats a log may be written in.
const (
	FormatText Format = "text"
	FormatJSON Format = "json"
)

// ParseFormat reads the name of a format.
func ParseFormat(s string) (Format, error) {
	switch f := Format(s); f {
	case FormatText, FormatJSON:
		return f, nil
	}
	return "", fmt.Errorf("unknown format %q, want %s or %s", s, FormatText, FormatJSON)
}

// New builds a logger that writes to w in the given format, with every secret
// replaced in the message and in every value it carries.
func New(w io.Writer, level slog.Level, format Format, secrets *Secrets) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level, ReplaceAttr: secrets.attr}
	if format == FormatJSON {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

// attr is what the handler writes in place of one attribute. It is called for
// every value of a record, the message among them, and for the leaves of a
// group, so nothing written goes past it.
func (s *Secrets) attr(_ []string, a slog.Attr) slog.Attr {
	switch v := a.Value; v.Kind() {
	case slog.KindString:
		a.Value = slog.StringValue(s.Redact(v.String()))
	case slog.KindAny:
		switch x := v.Any().(type) {
		case error:
			a.Value = slog.StringValue(s.Redact(x.Error()))
		case fmt.Stringer:
			a.Value = slog.StringValue(s.Redact(x.String()))
		default:
			// Anything else keeps its shape, so a list stays a list in JSON.
			// One that holds a secret becomes the redacted text of it: the
			// shape is worth less than the secret.
			if text := fmt.Sprint(x); s.Redact(text) != text {
				a.Value = slog.StringValue(s.Redact(text))
			}
		}
	}
	return a
}
