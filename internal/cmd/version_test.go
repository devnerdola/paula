package cmd

import (
	"runtime/debug"
	"strings"
	"testing"
)

// The version is the one the toolchain stamped the build with, tag or
// pseudo-version, and a binary with no build information says so.
func TestTheVersionIsTheOneTheBuildWasStampedWith(t *testing.T) {
	for _, c := range []struct {
		what string
		info *debug.BuildInfo
		ok   bool
		want string
	}{
		{"no build information", nil, false, "paula (unknown)"},
		{"a tag", &debug.BuildInfo{Main: debug.Module{Version: "v0.3.1"}}, true, "paula v0.3.1"},
		{"a checkout", &debug.BuildInfo{Main: debug.Module{Version: "v0.0.0-20260928090838-f3c89d3f3ffa"}}, true,
			"paula v0.0.0-20260928090838-f3c89d3f3ffa"},
		{"a checkout with changes", &debug.BuildInfo{Main: debug.Module{Version: "v0.0.0-20260928090838-f3c89d3f3ffa+dirty"}}, true,
			"paula v0.0.0-20260928090838-f3c89d3f3ffa+dirty"},
	} {
		if got := version(c.info, c.ok); got != c.want {
			t.Errorf("%s: version = %q, want %q", c.what, got, c.want)
		}
	}
}

// paula version prints the version and nothing else, and takes nothing.
func TestVersionCommand(t *testing.T) {
	code, out, errOut := exec(t, "version")
	if code != 0 || !strings.HasPrefix(out, "paula ") || strings.Count(out, "\n") != 1 || errOut != "" {
		t.Errorf("paula version = %d, %q, %q, want one line of the version", code, out, errOut)
	}
	if code, _, errOut := exec(t, "version", "now"); code != 2 || !strings.Contains(errOut, "takes no arguments") {
		t.Errorf("paula version now = %d, %q, want it refused", code, errOut)
	}
}
