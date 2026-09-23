package cmd

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/runners/api"
	"nerdola.dev/x/paula/internal/store"
)

// defaultTurns is how many entries are listed when no number is asked for.
const defaultTurns = 20

func turnsCommand() *command {
	return &command{
		name:  "turns",
		args:  "[-n N] [-dump] [ID]",
		short: "show what was sent to a model and what came back",
		flags: func(fs *flag.FlagSet) func(*globals, []string) error {
			n := fs.Int("n", defaultTurns, "how many entries to list")
			dump := fs.Bool("dump", false, "print the whole snapshot of an entry")
			return func(g *globals, args []string) error {
				if len(args) > 1 {
					return usagef("one entry at a time")
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

				ctx := context.Background()
				if len(args) == 0 {
					if *dump {
						return usagef("which entry to dump")
					}
					return listTurns(ctx, g.stdout, s, *n)
				}
				n, err := strconv.ParseInt(args[0], 10, 64)
				if err != nil {
					return usagef("%q is not an entry", args[0])
				}
				id := store.EntryID(n)
				if *dump {
					return dumpTurn(ctx, g.stdout, s, id)
				}
				return showTurn(ctx, g.stdout, s, id)
			}
		},
	}
}

// totals are what the requests of an entry add up to.
type totals struct {
	models     []string
	requests   int
	prompt     int
	cached     int
	completion int
	reasoning  int
	cost       float64
}

func sum(requests []store.Request) totals {
	t := totals{requests: len(requests)}
	for _, r := range requests {
		if r.Model != "" && !slices.Contains(t.models, r.Model) {
			t.models = append(t.models, r.Model)
		}
		t.cost += r.Cost
		if r.Usage == nil {
			continue
		}
		t.prompt += r.Usage.PromptTokens
		t.cached += r.Usage.CachedTokens
		t.completion += r.Usage.CompletionTokens
		t.reasoning += r.Usage.ReasoningTokens
	}
	return t
}

func listTurns(ctx context.Context, w io.Writer, s *store.Store, n int) error {
	entries, err := s.Entries(ctx, n)
	if err != nil {
		return err
	}
	var rows [][]string
	for _, e := range entries {
		requests, err := s.Requests(ctx, e.ID)
		if err != nil {
			return err
		}
		t := sum(requests)
		rows = append(rows, []string{
			strconv.FormatInt(int64(e.ID), 10),
			e.StartedAt.Local().Format("2006-01-02 15:04:05"),
			e.Status,
			orDash(strings.Join(t.models, ",")),
			strconv.Itoa(t.requests),
			strconv.Itoa(t.prompt), strconv.Itoa(t.cached),
			strconv.Itoa(t.completion), strconv.Itoa(t.reasoning),
			money(t.cost), took(e),
		})
	}
	table(w, []string{"ID", "TIME", "STATUS", "MODELS", "REQUESTS",
		"PROMPT", "CACHED", "COMPLETION", "REASONING", "COST", "DURATION"},
		func(row func(...string)) {
			for _, r := range rows {
				row(r...)
			}
		})
	return nil
}

func showTurn(ctx context.Context, w io.Writer, s *store.Store, id store.EntryID) error {
	e, err := s.Entry(ctx, id)
	if err != nil {
		return err
	}
	requests, err := s.Requests(ctx, id)
	if err != nil {
		return err
	}
	// The reply names the entry it belongs to, so it is read back by the entry
	// rather than pointed at from it.
	reply, err := s.ReplyOfEntry(ctx, id)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if err := header(ctx, w, s, e, reply, sum(requests)); err != nil {
		return err
	}

	if len(requests) > 0 {
		fmt.Fprintln(w)
		table(w, []string{"REQUEST", "PURPOSE", "RUNNER", "MODEL", "PROVIDER", "STATUS",
			"ATTEMPTS", "PROMPT", "CACHED", "COMPLETION", "REASONING", "COST",
			"FIRST BYTE", "DURATION", "FINISH"}, func(row func(...string)) {
			for i, r := range requests {
				u := r.Usage
				if u == nil {
					u = &store.Usage{}
				}
				row(strconv.Itoa(i+1), r.Purpose, r.Runner, orDash(r.Model),
					orDash(r.Provider), strconv.Itoa(r.Status),
					strconv.Itoa(len(r.Attempts)),
					strconv.Itoa(u.PromptTokens), strconv.Itoa(u.CachedTokens),
					strconv.Itoa(u.CompletionTokens), strconv.Itoa(u.ReasoningTokens),
					money(r.Cost), since(r.StartedAt, r.FirstByteAt),
					since(r.StartedAt, r.EndedAt), orDash(r.FinishReason))
			}
		})
	}

	calls, err := s.ToolCalls(ctx, id)
	if err != nil {
		return err
	}
	if len(calls) > 0 {
		// A call names the request of the round that asked for it, which is
		// numbered here the way the table above numbers it.
		request := numbered(requests)
		fmt.Fprintln(w)
		table(w, []string{"TOOL CALL", "REQUEST", "NAME", "DURATION", "ERROR"}, func(row func(...string)) {
			for i, c := range calls {
				row(strconv.Itoa(i+1), request[c.RequestID], c.Name,
					since(c.StartedAt, c.EndedAt), orDash(c.Error))
			}
		})
	}

	// The reply's own request shows what was sent; an entry with no reply
	// shows every request it made.
	asked := lastOf(requests, store.PurposeReply)
	if asked == nil {
		for i := range requests {
			fmt.Fprintf(w, "\n--- request %d, %s\n\n", i+1, requests[i].Purpose)
			writePrompt(w, requests[i])
		}
		return nil
	}

	if settings := readSettings(*asked); settings != "" {
		fmt.Fprintf(w, "\nsettings  %s\n", settings)
	}
	fmt.Fprintln(w)
	writePrompt(w, *asked)
	writeReply(w, reply)
	return nil
}

func header(ctx context.Context, w io.Writer, s *store.Store, e *store.Entry, reply *store.Message, t totals) error {
	fmt.Fprintf(w, "entry %d  %s", e.ID, e.Status)
	if e.Channel != "" {
		fmt.Fprintf(w, "  %s", e.Channel)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "started   %s\n", stamp(e.StartedAt))
	if !e.EndedAt.IsZero() {
		fmt.Fprintf(w, "ended     %s (%s)\n", stamp(e.EndedAt), took(*e))
	}
	if e.Error != "" {
		fmt.Fprintf(w, "error     %s\n", e.Error)
	}

	answered, err := s.MessagesAnsweredBy(ctx, e.ID)
	if err != nil {
		return err
	}
	if len(answered) > 0 {
		var ids []string
		for _, m := range answered {
			ids = append(ids, strconv.FormatInt(int64(m.ID), 10))
		}
		fmt.Fprintf(w, "answers   messages %s\n", strings.Join(ids, ", "))
	}
	if reply != nil {
		fmt.Fprintf(w, "reply     message %d\n", reply.ID)
	}
	if t.cost > 0 {
		fmt.Fprintf(w, "cost      %s\n", money(t.cost))
	}
	return nil
}

func writeReply(w io.Writer, reply *store.Message) {
	if reply == nil {
		return
	}
	if reply.Reasoning != "" {
		fmt.Fprintf(w, "\n--- reasoning\n%s\n", reply.Reasoning)
	}
	fmt.Fprintf(w, "\n--- reply, message %d\n%s\n", reply.ID, reply.Text())
}

func lastOf(requests []store.Request, purpose string) *store.Request {
	for i := len(requests) - 1; i >= 0; i-- {
		if requests[i].Purpose == purpose {
			return &requests[i]
		}
	}
	return nil
}

func readSettings(r store.Request) string {
	p, ok := api.ReadPrompt(r.RequestBody)
	if !ok || p.Reasoning == nil {
		return ""
	}
	var out []string
	if p.Reasoning.Enabled != nil {
		out = append(out, "reasoning "+onOff(*p.Reasoning.Enabled))
	}
	if p.Reasoning.Effort != "" {
		out = append(out, "effort "+p.Reasoning.Effort)
	}
	return strings.Join(out, ", ")
}

func onOff(v bool) string {
	if v {
		return "on"
	}
	return "off"
}

func writePrompt(w io.Writer, r store.Request) {
	if r.Pruned {
		fmt.Fprintln(w, pruned)
		return
	}
	// What an embedding was asked to turn into vectors is not a prompt, and is
	// shown as it was sent. What it is for says so; the shape of the body only
	// says whether it could be read.
	p, ok := api.ReadPrompt(r.RequestBody)
	if !ok || r.Purpose == store.PurposeEmbedding {
		if len(r.RequestBody) > 0 {
			fmt.Fprintf(w, "%s\n", r.RequestBody)
		}
		return
	}
	for i, m := range p.Messages {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "--- %s\n", m.Role)
		// A message that ends with a newline of its own would otherwise leave
		// two blank lines before the next section.
		if text := strings.TrimRight(m.Text, "\n"); text != "" {
			fmt.Fprintln(w, text)
		}
		for _, image := range m.Images {
			fmt.Fprintf(w, "image %s\n", image)
		}
	}
}

// pruned is what an entry shows in place of bodies that were dropped.
const pruned = "bodies pruned (engine.log_keep)"

func dumpTurn(ctx context.Context, w io.Writer, s *store.Store, id store.EntryID) error {
	e, err := s.Entry(ctx, id)
	if err != nil {
		return err
	}
	requests, err := s.Requests(ctx, id)
	if err != nil {
		return err
	}
	reply, err := s.ReplyOfEntry(ctx, id)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if err := header(ctx, w, s, e, reply, sum(requests)); err != nil {
		return err
	}

	answered, err := s.MessagesAnsweredBy(ctx, id)
	if err != nil {
		return err
	}
	for _, m := range answered {
		fmt.Fprintf(w, "\n== message %d  %s  %s  %s\n", m.ID, m.Role, orDash(m.Channel), stamp(m.CreatedAt))
		fmt.Fprintln(w, m.Text())
		for _, p := range m.Images() {
			fmt.Fprintf(w, "image %s %s\n", p.SHA256, p.MIME)
		}
	}

	for i, r := range requests {
		fmt.Fprintf(w, "\n== request %d  %s  %s  %s\n", i+1, r.Purpose, r.Runner, orDash(r.Model))
		fmt.Fprintf(w, "%s %s\n", r.Method, r.URL)
		for i, a := range r.Attempts {
			fmt.Fprintf(w, "attempt %d  started %s", i+1, clock(a.StartedAt))
			if !a.FirstByteAt.IsZero() {
				fmt.Fprintf(w, "  first byte %s", clock(a.FirstByteAt))
			}
			fmt.Fprintf(w, "  ended %s  status %d", clock(a.EndedAt), a.Status)
			if a.RetryAfter > 0 {
				fmt.Fprintf(w, "  sent again after %s", a.RetryAfter)
			}
			if a.Error != "" {
				fmt.Fprintf(w, "  %s", a.Error)
			}
			fmt.Fprintln(w)
		}
		if r.Pruned {
			fmt.Fprintf(w, "\n%s\n", pruned)
		} else {
			writeHeaders(w, "request headers", r.RequestHeaders)
			writeBody(w, "request body", r.RequestBody)
			writeHeaders(w, "response headers", r.ResponseHeaders)
			writeBody(w, "response body", r.ResponseBody)
		}
		writeUsage(w, r)
	}

	calls, err := s.ToolCalls(ctx, id)
	if err != nil {
		return err
	}
	request := numbered(requests)
	for i, c := range calls {
		fmt.Fprintf(w, "\n== tool call %d  %s  %s  asked by request %s  started %s",
			i+1, c.Name, c.CallID, request[c.RequestID], clock(c.StartedAt))
		if !c.EndedAt.IsZero() {
			fmt.Fprintf(w, "  ended %s", clock(c.EndedAt))
		}
		fmt.Fprintln(w)
		writeBody(w, "arguments", []byte(c.Arguments))
		if c.Error != "" {
			writeBody(w, "error", []byte(c.Error))
		}
		writeBody(w, "result", []byte(c.Result))
	}

	if reply != nil {
		fmt.Fprintf(w, "\n== message %d  %s  %s\n", reply.ID, reply.Role, orDash(reply.Channel))
		if reply.Reasoning != "" {
			writeBody(w, "reasoning", []byte(reply.Reasoning))
		}
		writeBody(w, "text", []byte(reply.Text()))
	}
	return nil
}

// numbered is the requests of an entry numbered by their place in it, a search
// between two rounds included, which is how a tool call names the request of
// the round that asked for it.
func numbered(requests []store.Request) map[int64]string {
	out := make(map[int64]string, len(requests))
	for i, r := range requests {
		out[r.ID] = strconv.Itoa(i + 1)
	}
	return out
}

// writeBody prints one block of a snapshot under its own name, leaving out
// what a request did not carry.
func writeBody(w io.Writer, title string, body []byte) {
	if len(body) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%s\n%s\n", title, bytes.TrimRight(body, "\n"))
}

func writeHeaders(w io.Writer, title string, h http.Header) {
	if len(h) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%s\n", title)
	for _, k := range slices.Sorted(maps.Keys(h)) {
		for _, v := range h[k] {
			fmt.Fprintf(w, "%s: %s\n", k, v)
		}
	}
}

func writeUsage(w io.Writer, r store.Request) {
	u := r.Usage
	if u == nil {
		u = &store.Usage{}
	}
	fmt.Fprintf(w, "\nprovider %s  finish %s  prompt %d  cached %d  cache write %d  completion %d  reasoning %d  cost %s\n",
		orDash(r.Provider), orDash(r.FinishReason), u.PromptTokens, u.CachedTokens,
		u.CacheWriteTokens, u.CompletionTokens, u.ReasoningTokens, money(r.Cost))
}

func stamp(t time.Time) string {
	t = t.Local()
	return t.Format("2006-01-02 15:04:05.000 ") + "UTC" + t.Format("-07:00")
}

func clock(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("15:04:05.000")
}

func took(e store.Entry) string {
	if e.EndedAt.IsZero() {
		return "-"
	}
	return e.EndedAt.Sub(e.StartedAt).Round(time.Millisecond).String()
}

func since(from, to time.Time) string {
	if from.IsZero() || to.IsZero() {
		return "-"
	}
	return to.Sub(from).Round(time.Millisecond).String()
}

func money(v float64) string {
	if v == 0 {
		return "-"
	}
	return fmt.Sprintf("$%.6f", v)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
