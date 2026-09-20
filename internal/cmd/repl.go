package cmd

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/frontend/repl"
)

func replCommand() *command {
	return &command{
		name:  "repl",
		short: "talk to her in the terminal",
		flags: func(fs *flag.FlagSet) func(*globals, []string) error {
			return func(g *globals, args []string) error {
				if len(args) > 0 {
					return usagef("no arguments are taken")
				}
				return talk(g)
			}
		},
	}
}

func talk(g *globals) error {
	cfg, err := config.Load(g.config)
	if err != nil {
		return err
	}
	// Only the socket is read from the file: whoever is listening keeps the
	// conversation.
	socket, err := repl.Socket(cfg.FrontendSection(repl.Kind), cfg.DataDir)
	if err != nil {
		return err
	}

	// Ctrl-C is relayed rather than ending the client, since it is the reply
	// being written that it stops.
	interrupts := make(chan struct{}, 1)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt)
	defer signal.Stop(signals)
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-signals:
				select {
				case interrupts <- struct{}{}:
				default:
				}
			case <-done:
				return
			}
		}
	}()

	code := repl.Client{
		Socket:      socket,
		In:          g.stdin,
		Out:         g.stdout,
		Err:         g.stderr,
		Interactive: typedAt(g.stdin),
		Interrupts:  interrupts,
	}.Run(g.ctx)
	if code != 0 {
		return exit(code)
	}
	return nil
}

// typedAt says whether a terminal is being typed at, rather than fed a file.
// Anything that is not a file of the process is fed: a test's pipe or buffer is
// read line by line, not waited on for a prompt.
func typedAt(in io.Reader) bool {
	f, ok := in.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// exit is what a command ends with when it has said what went wrong itself and
// only the code is left to carry. Main reads the code out of it and prints
// nothing, so this reads only where an error is written by something else.
type exit int

func (e exit) Error() string { return fmt.Sprintf("the terminal ended with %d", int(e)) }
