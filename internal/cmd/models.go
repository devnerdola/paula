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
		args:  "[-available]",
		short: "ask every runner about the models the configuration file names",
		flags: func(fs *flag.FlagSet) func(*globals, []string) error {
			available := fs.Bool("available", false, "add the catalogue of every runner")
			return func(g *globals, args []string) error {
				if len(args) > 0 {
					return usagef("no arguments are taken")
				}
				return models(g, *available)
			}
		},
	}
}

func models(g *globals, available bool) error {
	cfg, err := config.Load(g.config)
	if err != nil {
		return err
	}
	set, err := runners.Configure(cfg, runners.Host{Log: g.logger(), Secrets: g.secrets})
	if err != nil {
		return err
	}

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

	if available {
		for _, st := range report.Runners {
			fmt.Fprintln(w)
			catalogue(ctx, w, st.Runner)
		}
	}

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
// reports.
func budget(configured int, m *api.Model) string {
	switch {
	case m == nil && configured == 0:
		return "-"
	case m == nil:
		return strconv.Itoa(configured)
	case configured == 0:
		return strconv.Itoa(m.Context)
	}
	return fmt.Sprintf("%d of %d", configured, m.Context)
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
		s := "reasoning"
		if len(m.Efforts) > 0 {
			s += "(" + strings.Join(m.Efforts, ",") + ")"
		}
		if m.Mandatory {
			s += " always"
		}
		out = append(out, s)
	}
	if m.StructuredOutputs {
		out = append(out, "schema")
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

func catalogue(ctx context.Context, w io.Writer, r runners.Runner) {
	models, err := r.Models(ctx)
	if err != nil {
		fmt.Fprintf(w, "%s: %v\n", r.Name(), err)
		return
	}
	slices.SortFunc(models, func(a, b api.Model) int { return strings.Compare(a.ID, b.ID) })
	fmt.Fprintf(w, "%s serves:\n", r.Name())
	table(w, []string{"ID", "CONTEXT", "CAPABILITIES"}, func(row func(...string)) {
		for _, m := range models {
			row(m.ID, strconv.Itoa(m.Context), capabilities(&m))
		}
	})
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
