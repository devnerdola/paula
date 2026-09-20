package frontend

import (
	"context"
	"fmt"
	"strings"
	"text/tabwriter"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/frontend/api"
)

// commands are the ones every frontend answers.
func (s *Session) commands() []api.Command {
	out := []api.Command{
		{Name: "models", Args: "[reset]", Short: "which model serves each role"},
		{Name: "model", Args: "ROLE NAME", Short: "have a role served by a model"},
		{Name: "stop", Short: "stop the reply being written"},
	}
	out = append(out, s.features.Commands...)
	out = append(out, api.Command{Name: "help", Short: "what you can type"})
	return out
}

// command answers something that starts with a slash, and says whether it was
// one.
func (s *Session) command(ctx context.Context, text string) (bool, error) {
	if !strings.HasPrefix(text, "/") {
		return false, nil
	}
	name, args, _ := strings.Cut(strings.TrimPrefix(text, "/"), " ")
	args = strings.TrimSpace(args)

	switch name {
	case "models":
		return true, s.models(ctx, args)
	case "model":
		return true, s.setModel(ctx, args)
	case "stop":
		return true, s.stop(ctx)
	case "help":
		return true, s.help(ctx)
	}

	return true, s.say(ctx, fmt.Sprintf("unknown command /%s, try /help", name))
}

func (s *Session) models(ctx context.Context, args string) error {
	if args == "reset" {
		if err := s.conv.ResetModels(ctx); err != nil {
			return s.failed(ctx, err)
		}
		if err := s.say(ctx, "models reset"); err != nil {
			return err
		}
	} else if args != "" {
		return s.say(ctx, "/models takes nothing, or reset")
	}
	return s.showModels(ctx)
}

func (s *Session) setModel(ctx context.Context, args string) error {
	role, name, ok := strings.Cut(args, " ")
	name = strings.TrimSpace(name)
	if !ok || role == "" || name == "" {
		return s.say(ctx, "/model takes a role and a model, such as /model chat fast")
	}
	if err := s.conv.SetModel(ctx, config.Role(role), name); err != nil {
		return s.failed(ctx, err)
	}
	return s.say(ctx, fmt.Sprintf("%s: %s", role, name))
}

func (s *Session) stop(ctx context.Context) error {
	stopped, err := s.conv.Stop(ctx)
	if err != nil {
		return s.failed(ctx, err)
	}
	if stopped {
		return nil
	}
	return s.say(ctx, "nothing to stop")
}

func (s *Session) help(ctx context.Context) error {
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	for _, c := range s.commands() {
		fmt.Fprintf(tw, "/%s\t%s\n", strings.TrimRight(c.Name+" "+c.Args, " "), c.Short)
	}
	tw.Flush()
	return s.say(ctx, strings.TrimRight(b.String(), "\n"))
}

// showModels says which model serves each role.
func (s *Session) showModels(ctx context.Context) error {
	models, err := s.conv.Models(ctx)
	if err != nil {
		return s.failed(ctx, err)
	}
	if len(models.Roles) == 0 {
		return s.say(ctx, "no models are set up")
	}
	var b strings.Builder
	for _, r := range models.Roles {
		fmt.Fprintf(&b, "%s: %s", r.Role, orNone(r.Current))
		if r.Saved {
			b.WriteString(", saved")
		}
		if len(r.Options) > 1 {
			fmt.Fprintf(&b, " (%s)", strings.Join(r.Options, ", "))
		}
		b.WriteString("\n")
		if r.Problem != "" {
			fmt.Fprintf(&b, "  %s\n", r.Problem)
		}
	}
	return s.say(ctx, strings.TrimRight(b.String(), "\n"))
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// failed says what went wrong, the same way everywhere.
func (s *Session) failed(ctx context.Context, err error) error {
	return s.say(ctx, "error: "+err.Error())
}
