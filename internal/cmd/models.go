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
	"text/tabwriter"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/conversation"
	"nerdola.dev/x/paula/internal/runners"
	"nerdola.dev/x/paula/internal/runners/api"
	"nerdola.dev/x/paula/internal/store"
)

func modelsCommand() *command {
	return &command{
		name:  "models",
		args:  "list|available|set|reset",
		short: "the models the configuration file names, and which serves each role",
		subs: []*command{
			modelsListCommand(),
			modelsAvailableCommand(),
			modelsSetCommand(),
			modelsResetCommand(),
		},
		flags: oneOfItsCommands,
	}
}

func modelsListCommand() *command {
	return &command{
		name:  "list",
		short: "ask every runner about the models the file names, and show which serves each role",
		flags: func(fs *flag.FlagSet) func(*globals, []string) error {
			return func(g *globals, args []string) error {
				if len(args) > 0 {
					return usagef("list takes no arguments")
				}
				cfg, err := config.Load(g.config)
				if err != nil {
					return err
				}
				set, err := configured(g, cfg)
				if err != nil {
					return err
				}
				return models(g, cfg, set)
			}
		},
	}
}

func modelsAvailableCommand() *command {
	return &command{
		name:  "available",
		short: "everything the runners serve, which is where a model's id comes from",
		flags: func(fs *flag.FlagSet) func(*globals, []string) error {
			return func(g *globals, args []string) error {
				if len(args) > 0 {
					return usagef("available takes no arguments")
				}
				cfg, err := config.Load(g.config)
				if err != nil {
					return err
				}
				set, err := configured(g, cfg)
				if err != nil {
					return err
				}
				ctx := context.Background()
				var problems []error
				for i, r := range set.Runners {
					if i > 0 {
						fmt.Fprintln(g.stdout)
					}
					if err := catalogue(ctx, g.stdout, r); err != nil {
						problems = append(problems, err)
					}
				}
				return errors.Join(problems...)
			}
		},
	}
}

func modelsSetCommand() *command {
	return &command{
		name:  "set",
		args:  "ROLE NAME",
		short: "have a role served by a model the file names, and remember it",
		flags: func(fs *flag.FlagSet) func(*globals, []string) error {
			return func(g *globals, args []string) error {
				if len(args) != 2 {
					return usagef("set takes a role and a model, such as set chat fast")
				}
				// The choice is written beside a run that is serving, which
				// reads it before every reply. It takes the conversation that
				// is there rather than starting one: before the first, the
				// file's default is the one to change.
				cfg, s, err := reading(g)
				if err != nil {
					return err
				}
				defer s.Close()
				set, err := configured(g, cfg)
				if err != nil {
					return err
				}
				if err := conversation.SetModel(context.Background(), s, set, config.Role(args[0]), args[1]); err != nil {
					return err
				}
				fmt.Fprintf(g.stdout, "%s: %s\n", args[0], args[1])
				return nil
			}
		},
	}
}

func modelsResetCommand() *command {
	return &command{
		name:  "reset",
		short: "forget every choice, so the file decides again",
		flags: func(fs *flag.FlagSet) func(*globals, []string) error {
			return func(g *globals, args []string) error {
				if len(args) > 0 {
					return usagef("reset takes no arguments")
				}
				_, s, err := reading(g)
				if err != nil {
					return err
				}
				defer s.Close()
				if err := conversation.ResetModels(context.Background(), s); err != nil {
					return err
				}
				fmt.Fprintln(g.stdout, "models reset")
				return nil
			}
		},
	}
}

// configured sets the runners of a configuration file up.
func configured(g *globals, cfg *config.Config) (*runners.Setup, error) {
	return runners.Configure(cfg, runners.Host{Log: g.logger(), Secrets: g.secrets})
}

func models(g *globals, cfg *config.Config, set *runners.Setup) error {
	ctx := context.Background()
	var problems []error
	report := set.Report(ctx)
	saved := savedModels(ctx, g.stderr, cfg)

	note := func(err error, skipped bool) string {
		switch {
		case skipped:
			return "not asked"
		case err == nil:
			return "ok"
		}
		problems = append(problems, err)
		return "problem"
	}

	w := g.stdout
	table(w, []string{"RUNNER", "TYPE", "URL", "STATUS"}, func(row func(...string)) {
		for _, st := range report.Runners {
			row(st.Runner.Name(), st.Runner.Kind(), st.Runner.URL(), note(st.Err, false))
		}
	})

	fmt.Fprintln(w)
	table(w, []string{"MODEL", "RUNNER", "ID", "CONTEXT", "CAPABILITIES", "ROLES", "STATUS"},
		func(row func(...string)) {
			for _, st := range report.Models {
				m := st.Model
				row(m.Name, m.Runner.Name(), m.ID, budget(m.Context, st.Catalogue),
					capabilities(st.Catalogue), roles(set, m), note(st.Err, st.Skipped))
			}
		})

	fmt.Fprintln(w)
	table(w, []string{"ROLE", "MODEL", "CHOICE", "TOKENS", "STATUS"}, func(row func(...string)) {
		for _, st := range report.Roles {
			// A runner that never answered is not asked again for the prompt
			// its model is written within.
			prompt := "-"
			if st.Err == nil && !st.Skipped {
				prompt = tokens(ctx, set, st.Model)
			}
			row(string(st.Role), st.Model.Name, choice(saved, st.Role), prompt,
				note(st.Err, st.Skipped))
		}
	})

	return errors.Join(problems...)
}

func table(w io.Writer, header []string, rows func(row func(...string))) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(header, "\t"))
	rows(func(cells ...string) {
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	})
	tw.Flush()
}

// budget is the context column: what the file sets, and what the catalogue
// reports. A model that makes pictures has no context, and neither says one.
func budget(configured int, m *api.Model) string {
	listed := 0
	if m != nil {
		listed = m.Context
	}
	switch {
	case configured == 0 && listed == 0:
		return "-"
	case listed == 0:
		return strconv.Itoa(configured)
	case configured == 0:
		return strconv.Itoa(listed)
	}
	return fmt.Sprintf("%d of %d", configured, listed)
}

func capabilities(m *api.Model) string {
	if m == nil {
		return "-"
	}
	var out []string
	if m.Chat {
		out = append(out, "chat")
	}
	if m.Vision {
		out = append(out, "vision")
	}
	if m.Tools {
		out = append(out, "tools")
	}
	if m.Reasoning {
		// Reasoning a model cannot be asked to leave off is one entry of the
		// list like the rest, so the column reads as the list it is.
		s := "reasoning"
		if m.Mandatory {
			s = "always-reasoning"
		}
		if len(m.Efforts) > 0 {
			s += "(" + strings.Join(m.Efforts, ",") + ")"
		}
		out = append(out, s)
	}
	if m.StructuredOutputs {
		out = append(out, "schema")
	}
	if m.Paint {
		out = append(out, "paint")
	}
	if m.Edit {
		out = append(out, "edit")
	}
	if len(out) == 0 {
		return "-"
	}
	return strings.Join(out, ",")
}

func roles(set *runners.Setup, m *runners.Configured) string {
	var out []string
	for _, role := range config.Roles {
		if set.Defaults[role] == m {
			out = append(out, string(role))
		}
	}
	if len(out) == 0 {
		return "-"
	}
	return strings.Join(out, ",")
}

func choice(saved map[config.Role]string, role config.Role) string {
	if name, ok := saved[role]; ok {
		return name
	}
	return "-"
}

// tokens is the context a role's model is used with.
func tokens(ctx context.Context, set *runners.Setup, m *runners.Configured) string {
	budget, err := set.ContextSize(ctx, m)
	if err != nil || budget == 0 {
		return "-"
	}
	return strconv.Itoa(budget)
}

// catalogue writes everything a runner serves, and reports a listing that
// could not be read.
func catalogue(ctx context.Context, w io.Writer, runner runners.Runner) error {
	r, ok := runner.(runners.Server)
	if !ok {
		fmt.Fprintf(w, "%s serves no models\n", runner.Name())
		return nil
	}
	models, err := r.Models(ctx)
	if err != nil {
		return fmt.Errorf("%s: %w", r.Name(), err)
	}
	slices.SortFunc(models, func(a, b api.Model) int { return strings.Compare(a.ID, b.ID) })
	fmt.Fprintf(w, "%s serves:\n", r.Name())
	table(w, []string{"ID", "CONTEXT", "CAPABILITIES"}, func(row func(...string)) {
		for _, m := range models {
			row(m.ID, budget(0, &m), capabilities(&m))
		}
	})
	return nil
}

// savedModels reads the model saved for each role. A conversation that has not
// started yet has none; a database that cannot be read is said out loud, since
// every role would otherwise read as one nothing was ever chosen for.
func savedModels(ctx context.Context, w io.Writer, cfg *config.Config) map[config.Role]string {
	s, err := store.Read(cfg.DataDir)
	if err != nil {
		if !errors.Is(err, store.ErrNoConversation) {
			fmt.Fprintf(w, "paula models: %v\n", err)
		}
		return nil
	}
	defer s.Close()
	saved, err := conversation.SavedModels(ctx, s)
	if err != nil {
		fmt.Fprintf(w, "paula models: %v\n", err)
		return nil
	}
	return saved
}
