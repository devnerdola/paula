package cmd

import (
	"flag"
	"fmt"
	"runtime/debug"
)

func versionCommand() *command {
	return &command{
		name:  "version",
		short: "print the version this build was made from",
		flags: func(fs *flag.FlagSet) func(*globals, []string) error {
			return func(g *globals, args []string) error {
				if len(args) > 0 {
					return usagef("version takes no arguments")
				}
				info, ok := debug.ReadBuildInfo()
				fmt.Fprintln(g.stdout, version(info, ok))
				return nil
			}
		},
	}
}

// version is the module version the Go toolchain stamps a build with: a tag
// when the build is of one, and otherwise a pseudo-version naming the time and
// the commit, with +dirty after it when the checkout had changes not
// committed.
func version(info *debug.BuildInfo, ok bool) string {
	if !ok {
		return "paula (unknown)"
	}
	return "paula " + info.Main.Version
}
