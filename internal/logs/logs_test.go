package logs

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

func TestShortValuesAreNotRegistered(t *testing.T) {
	s := new(Secrets)
	s.Add("short7x")
	if got := s.Redact("short7x"); got != "short7x" {
		t.Errorf("Redact = %q, want the text unchanged", got)
	}
}

func TestRedact(t *testing.T) {
	s := new(Secrets)
	s.Add("sk-redact-me-please")
	got := s.Redact("Bearer sk-redact-me-please done")
	if want := "Bearer " + Mask + " done"; got != want {
		t.Errorf("Redact = %q, want %q", got, want)
	}
}

func TestLongestSecretFirst(t *testing.T) {
	s := new(Secrets)
	s.Add("tok-abcdefgh")
	s.Add("tok-abcdefgh-longer")
	if got := s.Redact("tok-abcdefgh-longer"); got != Mask {
		t.Errorf("Redact = %q, want the longer secret replaced whole", got)
	}
}

// A list is a list where the log is JSON, not a line of text that reads like
// one. A value holding a secret is the exception: it becomes the redacted text.
func TestAValueThatIsNotTextKeepsItsShape(t *testing.T) {
	s := new(Secrets)
	s.Add("sk-shape-1234567890")
	var buf bytes.Buffer
	log := New(&buf, slog.LevelInfo, FormatJSON, s)
	log.Info("asked", "hosts", []string{"novita", "parasail"},
		"keys", []string{"sk-shape-1234567890"})

	out := buf.String()
	if !strings.Contains(out, `"hosts":["novita","parasail"]`) {
		t.Errorf("the list was not written as one:\n%s", out)
	}
	if strings.Contains(out, "sk-shape-1234567890") {
		t.Errorf("the secret was written:\n%s", out)
	}
	if !strings.Contains(out, Mask) {
		t.Errorf("the list holding a secret was not redacted:\n%s", out)
	}
}

func TestLoggerRedactsMessageAndAttrs(t *testing.T) {
	s := new(Secrets)
	s.Add("env-value-12345678")
	var buf bytes.Buffer
	log := New(&buf, slog.LevelInfo, FormatText, s)
	log.With("bearer", "env-value-12345678").
		WithGroup("req").
		Info("sending env-value-12345678",
			"url", "https://x/?k=env-value-12345678",
			"err", errors.New("bad env-value-12345678"),
			slog.Group("inner", "k", "env-value-12345678"))
	out := buf.String()
	if strings.Contains(out, "env-value-12345678") {
		t.Errorf("log = %q, want no secret in it", out)
	}
	if strings.Count(out, Mask) != 5 {
		t.Errorf("log = %q, want five redactions", out)
	}
}

// A secret added after the logger was built is redacted all the same: a runner
// reads its token long after the run started logging.
func TestASecretAddedAfterTheLoggerWasBuilt(t *testing.T) {
	s := new(Secrets)
	var buf bytes.Buffer
	log := New(&buf, slog.LevelInfo, FormatText, s)
	s.Add("late-token-abcdefgh")
	log.Info("sending", "bearer", "late-token-abcdefgh")
	if strings.Contains(buf.String(), "late-token-abcdefgh") {
		t.Errorf("log = %q, want the secret replaced", buf.String())
	}
}

// A logger built with no secrets writes what it was given.
func TestALoggerWithoutSecrets(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, slog.LevelInfo, FormatText, nil)
	log.Info("sending", "bearer", "nothing-to-hide-here")
	if !strings.Contains(buf.String(), "nothing-to-hide-here") {
		t.Errorf("log = %q, want the value as it is", buf.String())
	}
}

func TestLevelAndFormat(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, slog.LevelWarn, FormatJSON, nil)
	log.Info("quiet")
	if buf.Len() != 0 {
		t.Errorf("log = %q, want nothing below the level", buf.String())
	}
	log.Warn("loud", "n", 1)
	if !strings.HasPrefix(buf.String(), "{") {
		t.Errorf("log = %q, want json", buf.String())
	}
}

func TestAFormatNobodyWrites(t *testing.T) {
	if _, err := ParseFormat("yaml"); err == nil {
		t.Error("ParseFormat read a format nothing writes")
	}
	for _, name := range []string{"text", "json"} {
		if _, err := ParseFormat(name); err != nil {
			t.Errorf("ParseFormat(%q) = %v", name, err)
		}
	}
}

// A header and a list go through the same redaction as a plain string.
func TestASecretInAnythingElse(t *testing.T) {
	s := new(Secrets)
	s.Add("test-token-abcdefgh")
	var buf bytes.Buffer
	log := New(&buf, slog.LevelInfo, FormatText, s)

	log.Info("sending",
		"headers", http.Header{"Authorization": {"Bearer test-token-abcdefgh"}},
		"models", []string{"a", "test-token-abcdefgh"})
	if strings.Contains(buf.String(), "test-token-abcdefgh") {
		t.Errorf("the log carries the secret: %s", buf.String())
	}
	if !strings.Contains(buf.String(), Mask) {
		t.Errorf("nothing was redacted: %s", buf.String())
	}
}
