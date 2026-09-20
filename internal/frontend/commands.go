package frontend

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/frontend/api"
	"nerdola.dev/x/paula/internal/store"
)

// commands are the ones every frontend answers.
func (s *Session) commands() []api.Command {
	out := []api.Command{
		{Name: "models", Args: "[reset]", Short: "which model serves each role"},
		{Name: "model", Args: "ROLE NAME", Short: "have a role served by a model"},
		{Name: "summary", Short: "what she was told of the conversation before this"},
		{Name: "memory", Args: "[QUERY]", Short: "what she remembers, or what of it a question is about"},
		{Name: "forget", Args: "ID", Short: "take a memory away"},
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
	case "summary":
		return true, s.summary(ctx)
	case "memory":
		return true, s.memories(ctx, args)
	case "forget":
		return true, s.forget(ctx, args)
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

// memoriesShown is how many memories a frontend lists at once.
const memoriesShown = 10

func (s *Session) summary(ctx context.Context) error {
	summary, err := s.conv.Summary(ctx)
	if err != nil {
		return s.failed(ctx, err)
	}
	if summary == nil {
		return s.say(ctx, "no summary yet")
	}
	// What it covers is the message it was written up to, which stands where it
	// is while the summary itself is written again.
	return s.say(ctx, fmt.Sprintf("summary up to %s:\n%s",
		summary.CoversUpto.Format("Monday, 2 January 2006, 15:04"), summary.Content))
}

func (s *Session) memories(ctx context.Context, query string) error {
	found, err := s.conv.Memories(ctx, query, memoriesShown)
	if err != nil {
		return s.failed(ctx, err)
	}
	if len(found) == 0 {
		if query != "" {
			return s.say(ctx, "no memories match")
		}
		return s.say(ctx, "no memories yet")
	}
	return s.say(ctx, memoryLines(found))
}

func (s *Session) forget(ctx context.Context, args string) error {
	id, err := strconv.ParseInt(args, 10, 64)
	if err != nil || id <= 0 {
		return s.say(ctx, "/forget takes the number of a memory, such as /forget 3")
	}
	gone, err := s.conv.Forget(ctx, store.MemoryID(id))
	if err != nil {
		return s.failed(ctx, err)
	}
	return s.say(ctx, "forgot:\n"+memoryLines(gone))
}

// memoryLines is how memories are listed: the number to forget one by, the day
// it was said, and what it says.
func memoryLines(memories []store.Memory) string {
	var b strings.Builder
	for _, m := range memories {
		fmt.Fprintf(&b, "#%d (said on %s) %s\n",
			m.ID, m.SaidAt.Format("Monday, 2 January 2006"), m.Content)
	}
	return strings.TrimRight(b.String(), "\n")
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
