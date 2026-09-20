// Package cmd holds Paula's commands, their flags and usage.
package cmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"nerdola.dev/x/paula/internal/logs"
)

type globals struct {
	config    string
	logLevel  slog.Level
	logFormat logs.Format

	// secrets are the values the run must never write. The logger replaces them,
	// and a runner adds its token as it opens.
	secrets *logs.Secrets

	// ctx is what a command that runs until it is stopped is stopped by. A run
	// adds the interrupt to it; a test cancels it.
	ctx    context.Context
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

func (g *globals) logger() *slog.Logger {
	return logs.New(g.stderr, g.logLevel, g.logFormat, g.secrets)
}

type command struct {
	name  string
	args  string
	short string
	subs  []*command
	flags func(fs *flag.FlagSet) func(g *globals, args []string) error
}

// commands is the command table. Adding a command takes its file and one line
// here.
func commands() []*command {
	return []*command{
		serveCommand(),
		replCommand(),
		modelsCommand(),
		turnsCommand(),
		personaCheckCommand(),
		helpCommand(),
	}
}

type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usagef(format string, a ...any) error {
	return &usageError{msg: fmt.Sprintf(format, a...)}
}

// Main runs the command in args and returns the exit code.
func Main(args []string) int {
	return run(context.Background(), args, os.Stdin, os.Stdout, os.Stderr)
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	g := &globals{
		logLevel:  slog.LevelWarn,
		logFormat: logs.FormatText,
		secrets:   new(logs.Secrets),
		ctx:       ctx,
		stdin:     stdin,
		stdout:    stdout,
		stderr:    stderr,
	}

	fs := globalFlagSet(g, stderr)
	fs.Usage = func() { usage(stderr, nil) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	rest := fs.Args()
	if len(rest) == 0 {
		usage(stderr, nil)
		return 2
	}

	cmd, path, rest, err := lookup(commands(), nil, rest)
	if err != nil {
		fmt.Fprintf(stderr, "paula: %v\n", err)
		usage(stderr, nil)
		return 2
	}

	sub := flag.NewFlagSet("paula "+strings.Join(path, " "), flag.ContinueOnError)
	sub.SetOutput(stderr)
	sub.Usage = func() { usage(stderr, path) }
	fn := cmd.flags(sub)
	if err := sub.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if err := fn(g, sub.Args()); err != nil {
		var code exit
		if errors.As(err, &code) {
			return int(code)
		}
		var ue *usageError
		if errors.As(err, &ue) {
			if msg := ue.Error(); msg != "" {
				fmt.Fprintf(stderr, "paula %s: %s\n", strings.Join(path, " "), msg)
			}
			usage(stderr, path)
			return 2
		}
		fmt.Fprintf(stderr, "paula %s: %v\n", strings.Join(path, " "), err)
		return 1
	}
	return 0
}

func globalFlagSet(g *globals, out io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("paula", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.StringVar(&g.config, "config", "", "configuration `FILE`, default paula.yaml or $PAULA_CONFIG")
	fs.Var(levelValue{&g.logLevel}, "log-level", "`LEVEL` to log at: debug, info, warn or error (default warn)")
	fs.Var(formatValue{&g.logFormat}, "log-format", "log `FORMAT`: text or json (default text)")
	fs.Var(verboseValue{&g.logLevel}, "v", "same as -log-level info")
	return fs
}

func lookup(table []*command, path, args []string) (*command, []string, []string, error) {
	for _, c := range table {
		if c.name != args[0] {
			continue
		}
		path = append(path, c.name)
		rest := args[1:]
		if len(c.subs) > 0 && len(rest) > 0 {
			if sub, p, r, err := lookup(c.subs, path, rest); err == nil {
				return sub, p, r, nil
			}
		}
		return c, path, rest, nil
	}
	return nil, nil, nil, fmt.Errorf("unknown command %q", args[0])
}

func find(path []string) (*command, bool) {
	table := commands()
	var cmd *command
	for _, name := range path {
		cmd = nil
		for _, c := range table {
			if c.name == name {
				cmd = c
				break
			}
		}
		if cmd == nil {
			return nil, false
		}
		table = cmd.subs
	}
	return cmd, cmd != nil
}

type levelValue struct{ dst *slog.Level }

func (v levelValue) String() string {
	if v.dst == nil {
		return "warn"
	}
	return strings.ToLower(v.dst.String())
}

func (v levelValue) Set(s string) error {
	switch s {
	case "debug":
		*v.dst = slog.LevelDebug
	case "info":
		*v.dst = slog.LevelInfo
	case "warn":
		*v.dst = slog.LevelWarn
	case "error":
		*v.dst = slog.LevelError
	default:
		return fmt.Errorf("unknown level %q, want debug, info, warn or error", s)
	}
	return nil
}

type formatValue struct{ dst *logs.Format }

func (v formatValue) String() string {
	if v.dst == nil || *v.dst == "" {
		return string(logs.FormatText)
	}
	return string(*v.dst)
}

func (v formatValue) Set(s string) error {
	f, err := logs.ParseFormat(s)
	if err != nil {
		return err
	}
	*v.dst = f
	return nil
}

type verboseValue struct{ dst *slog.Level }

func (v verboseValue) String() string   { return "false" }
func (v verboseValue) IsBoolFlag() bool { return true }

func (v verboseValue) Set(s string) error {
	if s == "true" {
		*v.dst = slog.LevelInfo
	}
	return nil
}
