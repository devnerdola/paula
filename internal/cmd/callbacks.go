package cmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/store"
)

func callbacksCommand() *command {
	return &command{
		name:  "callbacks",
		args:  "list|cancel",
		short: "the times she scheduled to write to you on her own",
		subs: []*command{
			callbacksListCommand(),
			callbacksCancelCommand(),
		},
		flags: oneOfItsCommands,
	}
}

func callbacksListCommand() *command {
	return &command{
		name:  "list",
		short: "the call backs that have not come yet, soonest first",
		flags: func(fs *flag.FlagSet) func(*globals, []string) error {
			return func(g *globals, args []string) error {
				if len(args) > 0 {
					return usagef("list takes no arguments")
				}
				cfg, err := config.Load(g.config)
				if err != nil {
					return err
				}
				s, err := store.Read(cfg.DataDir)
				if err != nil {
					return err
				}
				defer s.Close()

				pending, err := s.Callbacks(context.Background())
				if err != nil {
					return err
				}
				return listCallbacks(g.stdout, pending)
			}
		},
	}
}

func callbacksCancelCommand() *command {
	return &command{
		name:  "cancel",
		args:  "ID",
		short: "take a call back away before it comes",
		flags: func(fs *flag.FlagSet) func(*globals, []string) error {
			return func(g *globals, args []string) error {
				if len(args) != 1 {
					return usagef("one call back at a time")
				}
				id, err := strconv.ParseInt(args[0], 10, 64)
				if err != nil || id < 1 {
					return usagef("%q is not a call back", args[0])
				}
				cfg, err := config.Load(g.config)
				if err != nil {
					return err
				}
				// Cancelling writes, and waits its turn beside a run that is
				// serving, which reads what is pending again before it fires
				// one. It takes the conversation that is there rather than
				// starting one: there is nothing to cancel in a conversation
				// that was never had.
				s, err := store.Read(cfg.DataDir)
				if err != nil {
					return err
				}
				defer s.Close()

				ctx := context.Background()
				pending, err := s.Callbacks(ctx)
				if err != nil {
					return err
				}
				at := slices.IndexFunc(pending, func(c store.Callback) bool { return c.ID == store.CallbackID(id) })
				// One that fired between the two is no longer there to cancel.
				if at >= 0 {
					err = s.CancelCallback(ctx, store.CallbackID(id))
				}
				if at < 0 || errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("no call back that has not come yet is numbered %d", id)
				}
				if err != nil {
					return err
				}
				fmt.Fprintln(g.stdout, "cancelled:")
				return listCallbacks(g.stdout, pending[at:at+1])
			}
		},
	}
}

// listCallbacks writes the call backs out, the number each is cancelled by
// first, and when it is due in the time zone of the machine.
func listCallbacks(w io.Writer, pending []store.Callback) error {
	if len(pending) == 0 {
		fmt.Fprintln(w, "no call backs")
		return nil
	}
	table(w, []string{"ID", "DUE", "REASON"}, func(row func(...string)) {
		for _, c := range pending {
			// A reason is one line of a table, so what it holds is written as
			// one: a model that wrote a line of its own inside one would break
			// the row it is part of.
			reason := strings.Join(strings.Fields(c.Reason), " ")
			row(strconv.FormatInt(int64(c.ID), 10), c.DueAt.Local().Format("2006-01-02 15:04"), reason)
		}
	})
	return nil
}
