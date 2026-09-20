package cmd

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/conversation"
	"nerdola.dev/x/paula/internal/runners"
	"nerdola.dev/x/paula/internal/runners/api"
	"nerdola.dev/x/paula/internal/store"
)

// defaultMemories is how many memories are listed when no number is asked for.
const defaultMemories = 20

func memoryCommand() *command {
	return &command{
		name:  "memory",
		args:  "list|search|forget",
		short: "what she remembers of your conversations",
		subs: []*command{
			memoryListCommand(),
			memorySearchCommand(),
			memoryForgetCommand(),
		},
		flags: oneOfItsCommands,
	}
}

func memoryListCommand() *command {
	return &command{
		name:  "list",
		args:  "[-n N]",
		short: "the newest memories",
		flags: func(fs *flag.FlagSet) func(*globals, []string) error {
			n := fs.Int("n", defaultMemories, "how many memories to list")
			return func(g *globals, args []string) error {
				if len(args) > 0 {
					return usagef("list takes no arguments")
				}
				if *n < 1 {
					return usagef("-n takes a count above zero")
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

				found, err := s.LatestMemories(context.Background(), *n)
				if err != nil {
					return err
				}
				return listMemories(g.stdout, found)
			}
		},
	}
}

func memorySearchCommand() *command {
	return &command{
		name:  "search",
		args:  "[-n N] QUERY",
		short: "the memories a question is about",
		flags: func(fs *flag.FlagSet) func(*globals, []string) error {
			n := fs.Int("n", defaultMemories, "how many memories to list")
			return func(g *globals, args []string) error {
				if len(args) == 0 {
					return usagef("what to search for")
				}
				if *n < 1 {
					return usagef("-n takes a count above zero")
				}
				cfg, err := config.Load(g.config)
				if err != nil {
					return err
				}
				// Memories are searched by what they mean, so the words to
				// look for are turned into a vector by the model that turned
				// the memories into theirs.
				set, err := runners.Configure(cfg, runners.Host{Log: g.logger(), Secrets: g.secrets})
				if err != nil {
					return err
				}
				s, err := store.Read(cfg.DataDir)
				if err != nil {
					return err
				}
				defer s.Close()

				ctx := context.Background()
				// The role is served by the model the conversation was given
				// for it, which is the one that embedded the memories: a vector
				// of another model measures nothing against them.
				m, err := conversation.RoleModel(ctx, s, set, config.RoleEmbed)
				if err != nil {
					return err
				}
				if m == nil {
					return fmt.Errorf("%s: default_models.embed: no model is set", cfg.Path)
				}
				out, err := m.Runner.Embed(ctx, api.EmbedRequest{
					Model:    m.ID,
					Input:    []string{strings.Join(args, " ")},
					Settings: m.Settings,
				})
				if err != nil {
					return err
				}
				if len(out.Vectors) != 1 {
					return fmt.Errorf("the query was embedded as %d vectors", len(out.Vectors))
				}
				by := store.Embedded{Runner: m.Runner.Name(), Model: m.ID}
				found, err := s.NearestMemories(ctx, by, out.Vectors[0], *n)
				if err != nil {
					return err
				}
				return listMemories(g.stdout, found)
			}
		},
	}
}

func memoryForgetCommand() *command {
	return &command{
		name:  "forget",
		args:  "ID",
		short: "take a memory away, and the ones it replaced",
		flags: func(fs *flag.FlagSet) func(*globals, []string) error {
			return func(g *globals, args []string) error {
				if len(args) != 1 {
					return usagef("one memory at a time")
				}
				id, err := strconv.ParseInt(args[0], 10, 64)
				if err != nil || id < 1 {
					return usagef("%q is not a memory", args[0])
				}
				cfg, err := config.Load(g.config)
				if err != nil {
					return err
				}
				// Forgetting writes, and waits its turn beside a run that is
				// serving. It takes the conversation that is there rather than
				// starting one: there is nothing to forget in a conversation
				// that was never had.
				s, err := store.Read(cfg.DataDir)
				if err != nil {
					return err
				}
				defer s.Close()

				gone, err := s.Forget(context.Background(), store.MemoryID(id))
				if err != nil {
					return err
				}
				fmt.Fprintln(g.stdout, "forgot:")
				return listMemories(g.stdout, gone)
			}
		},
	}
}

// listMemories writes the memories out, the number each is forgotten by first.
func listMemories(w io.Writer, memories []store.Memory) error {
	if len(memories) == 0 {
		fmt.Fprintln(w, "no memories")
		return nil
	}
	table(w, []string{"ID", "SAID", "MEMORY"}, func(row func(...string)) {
		for _, m := range memories {
			// A memory is one line of a table, so what it holds is written as
			// one: a fold that wrote a line of its own inside one would break
			// the row it is part of.
			said := strings.Join(strings.Fields(m.Content), " ")
			row(strconv.FormatInt(int64(m.ID), 10), m.SaidAt.Format(time.DateOnly), said)
		}
	})
	return nil
}
