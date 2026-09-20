package cmd

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func exec(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	return execWith(t, context.Background(), strings.NewReader(""), args...)
}

// execWith runs a command that reads what is typed or is stopped by a context
// of the test's own.
func execWith(t *testing.T, ctx context.Context, stdin io.Reader, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(ctx, args, stdin, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestUsageOfEveryCommand(t *testing.T) {
	var walk func(path []string, table []*command)
	walk = func(path []string, table []*command) {
		for _, c := range table {
			p := append(append([]string{}, path...), c.name)
			var buf bytes.Buffer
			usage(&buf, p)
			got := buf.String()
			if want := "usage: paula " + strings.Join(p, " "); !strings.HasPrefix(got, want) {
				t.Errorf("usage(%v) = %q, want prefix %q", p, got, want)
			}
			if c.short == "" {
				t.Errorf("command %v has no description", p)
			}
			if !strings.Contains(got, c.short) {
				t.Errorf("usage(%v) does not describe the command", p)
			}
			for _, s := range c.subs {
				if !strings.Contains(got, s.name) {
					t.Errorf("usage(%v) does not list %q", p, s.name)
				}
			}
			walk(p, c.subs)
		}
	}
	walk(nil, commands())
}

func TestHelpFlagOfEveryCommand(t *testing.T) {
	var walk func(path []string, table []*command)
	walk = func(path []string, table []*command) {
		for _, c := range table {
			p := append(append([]string{}, path...), c.name)
			code, _, errOut := exec(t, append(p, "-h")...)
			if code != 0 {
				t.Errorf("paula %s -h = %d, want 0", strings.Join(p, " "), code)
			}
			if want := "usage: paula " + strings.Join(p, " "); !strings.Contains(errOut, want) {
				t.Errorf("paula %s -h stderr = %q, want %q", strings.Join(p, " "), errOut, want)
			}
			walk(p, c.subs)
		}
	}
	walk(nil, commands())
}

func TestHelp(t *testing.T) {
	code, out, _ := exec(t, "help")
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	for _, c := range commands() {
		if !strings.Contains(out, c.name) {
			t.Errorf("help does not list %q", c.name)
		}
	}
}

func TestHelpOfUnknownCommand(t *testing.T) {
	code, _, errOut := exec(t, "help", "nope")
	if code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
	if !strings.Contains(errOut, `unknown command "nope"`) {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestNoCommand(t *testing.T) {
	code, _, errOut := exec(t)
	if code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
	if !strings.HasPrefix(errOut, "usage: paula ") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestUnknownCommand(t *testing.T) {
	code, _, errOut := exec(t, "nope")
	if code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
	if !strings.Contains(errOut, `unknown command "nope"`) {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestLogLevelFlags(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want slog.Level
	}{
		{[]string{"help"}, slog.LevelWarn},
		{[]string{"-v", "help"}, slog.LevelInfo},
		{[]string{"-log-level", "debug", "help"}, slog.LevelDebug},
		{[]string{"-v", "-log-level", "error", "help"}, slog.LevelError},
		{[]string{"-log-level", "error", "-v", "help"}, slog.LevelInfo},
	} {
		g := &globals{logLevel: slog.LevelWarn}
		fs := globalFlagSet(g, new(bytes.Buffer))
		if err := fs.Parse(tc.args); err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if g.logLevel != tc.want {
			t.Errorf("%v: level = %v, want %v", tc.args, g.logLevel, tc.want)
		}
	}
}

func TestBadLogFlags(t *testing.T) {
	if code, _, _ := exec(t, "-log-level", "loud", "help"); code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
	code, _, errOut := exec(t, "-log-format", "yaml", "help")
	if code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
	if !strings.Contains(errOut, "unknown format") {
		t.Errorf("stderr = %q", errOut)
	}
}
