package conversation

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"nerdola.dev/x/paula/internal/store"
)

// TestLiveWeb holds the web tools to the chat model of the file PAULA_LIVE
// names, and to the runner its web tool names: asked what is on this week, she
// searches the web and answers; asked about a page she found, she reads it.
// Everything goes to a data directory of its own, with a report.md of what
// every check found.
//
//	PAULA_LIVE=live.yaml go test ./internal/conversation -run TestLiveWeb -v -timeout 20m
func TestLiveWeb(t *testing.T) {
	path := os.Getenv("PAULA_LIVE")
	if path == "" {
		t.Skip("PAULA_LIVE names no configuration file")
	}
	lv := openLive(t, path)
	defer lv.close()
	offered := make([]string, len(lv.tools))
	for i, tool := range lv.tools {
		offered[i] = tool.Definition().Name
	}
	if !slices.Contains(offered, "search_web") {
		t.Fatalf("the file offers %v, and no web tool", offered)
	}
	// She is asked in the evening of today, as a person asks about a night
	// out: at an hour she would be asleep, her instructions have her put the
	// message off rather than search.
	now := time.Now()
	clock := &pastClock{}
	clock.set(time.Date(now.Year(), now.Month(), now.Day(), 19, 30, 0, 0, now.Location()))

	r := lv.run("web", clock)
	r.send([]string{"any good concerts in Lisbon this week? I'm free friday night"}, nil)
	lv.checkRan(r.turns[0], "search_web", "asked what is on this week, she searches the web and answers")

	r.send([]string{"open the first one you found and tell me what's on friday"}, nil)
	lv.checkRan(r.turns[1], "read_page", "asked about a page she found, she reads it and answers")
	r.end()
	lv.requestsTable(r)
	lv.printf("\n%d checks passed, %d failed.", lv.passed, lv.failed)
}

// checkRan holds a turn to a reply after a call of the tool named that ran
// without an error, and that sent its request of the runner, answered 200.
func (lv *live) checkRan(turn liveTurn, tool, name string) {
	ctx := context.Background()
	calls, err := lv.st.ToolCalls(ctx, turn.entry)
	if err != nil {
		lv.t.Fatal(err)
	}
	requests, err := lv.st.Requests(ctx, turn.entry)
	if err != nil {
		lv.t.Fatal(err)
	}
	reply, _ := lv.st.ReplyOfEntry(ctx, turn.entry)
	ran := slices.ContainsFunc(calls, func(c store.ToolCall) bool {
		return c.Name == tool && c.Error == "" && slices.ContainsFunc(requests, func(r store.Request) bool {
			return r.ToolCall == c.ID && r.Status == 200
		})
	})
	var did []string
	for _, c := range calls {
		what := clip(oneLine(c.Result), 100)
		if c.Error != "" {
			what = "error: " + c.Error
		}
		did = append(did, fmt.Sprintf("%s %s → %s", c.Name, c.Arguments, what))
	}
	lv.check(name, ran && reply != nil && turn.failed == "",
		"a "+tool+" call that ran and sent a request answered 200, and a reply",
		fmt.Sprintf("calls [%s]; reply %q; %s", strings.Join(did, "; "), clip(oneLine(textOfMessage(reply)), 300),
			errorOf(turn.failed)), lv.ids(turn.entry)...)
}
