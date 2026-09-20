package cmd

import (
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

func helpCommand() *command {
	return &command{
		name:  "help",
		args:  "[COMMAND]",
		short: "show the usage of a command",
		flags: func(fs *flag.FlagSet) func(*globals, []string) error {
			return func(g *globals, args []string) error {
				if len(args) > 0 {
					if _, ok := find(args); !ok {
						return usagef("unknown command %q", strings.Join(args, " "))
					}
				}
				usage(g.stdout, args)
				return nil
			}
		},
	}
}

func usage(w io.Writer, path []string) {
	cmd, table := (*command)(nil), commands()
	if len(path) > 0 {
		var ok bool
		if cmd, ok = find(path); !ok {
			return
		}
		table = cmd.subs
	}

	if cmd == nil {
		fmt.Fprintln(w, "usage: paula [-config FILE] [-log-level LEVEL] [-log-format FORMAT] [-v] COMMAND [ARGS]")
	} else {
		fmt.Fprintln(w, strings.TrimRight("usage: paula "+strings.Join(path, " ")+" "+cmd.args, " "))
		fmt.Fprintf(w, "\n%s\n", cmd.short)
	}

	if len(table) > 0 {
		fmt.Fprintln(w, "\ncommands:")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, c := range table {
			fmt.Fprintf(tw, "  %s\t%s\n", strings.TrimRight(c.name+" "+c.args, " "), c.short)
		}
		tw.Flush()
	}

	if cmd == nil {
		writeFlags(w, "global flags:", globalFlagSet(&globals{}, io.Discard))
		return
	}
	fs := flag.NewFlagSet(strings.Join(path, " "), flag.ContinueOnError)
	cmd.flags(fs)
	writeFlags(w, "flags:", fs)
}

func writeFlags(w io.Writer, title string, fs *flag.FlagSet) {
	var lines [][2]string
	fs.VisitAll(func(f *flag.Flag) {
		name, help := flag.UnquoteUsage(f)
		lines = append(lines, [2]string{strings.TrimRight("-"+f.Name+" "+name, " "), help})
	})
	if len(lines) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%s\n", title)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, l := range lines {
		fmt.Fprintf(tw, "  %s\t%s\n", l[0], l[1])
	}
	tw.Flush()
}
