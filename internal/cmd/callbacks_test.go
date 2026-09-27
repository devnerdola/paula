package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"nerdola.dev/x/paula/internal/store"
)

// scheduled is a data directory holding two call backs that have not come,
// the sooner scheduled after the later one and its reason written over two
// lines, and one that already fired.
func scheduled(t *testing.T) (dir string, sooner, later, fired store.Callback) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "data")
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx := context.Background()
	entry := &store.Entry{Status: store.StatusDone, StartedAt: when}
	if err := s.StartEntry(ctx, entry); err != nil {
		t.Fatal(err)
	}
	schedule := func(due time.Time, reason string) store.Callback {
		c := &store.Callback{DueAt: due, Reason: reason, Entry: entry.ID}
		if err := s.Schedule(ctx, c); err != nil {
			t.Fatal(err)
		}
		return *c
	}
	fired = schedule(when.Add(time.Hour), "say good night")
	later = schedule(when.Add(26*time.Hour), "ask how the interview went")
	sooner = schedule(when.Add(3*time.Hour), "check that the cake\ncame out")
	msg := &store.Message{Role: store.RoleCallback, Parts: []store.Part{{Type: store.PartText, Text: fired.Reason}},
		CreatedAt: fired.DueAt}
	if err := s.Fire(ctx, fired.ID, msg); err != nil {
		t.Fatal(err)
	}
	return dir, sooner, later, fired
}

// due is when a call back is due as the machine's clock reads it.
func due(c store.Callback) string { return c.DueAt.Local().Format("2006-01-02 15:04") }

// The call backs that have not come are listed soonest first, each on a line
// of its own with its number, when it is due and why; one that fired is not.
func TestCallbacksList(t *testing.T) {
	dir, sooner, later, fired := scheduled(t)
	code, out, errOut := exec(t, "-config", memoryConfig(t, dir), "callbacks", "list")
	if code != 0 {
		t.Fatalf("code = %d, stderr %s", code, errOut)
	}
	header := line(t, out, "ID ")
	for _, want := range []string{"DUE", "REASON"} {
		if !strings.Contains(header, want) {
			t.Errorf("header = %q, want %q in it", header, want)
		}
	}
	first := strings.Fields(line(t, out, strconv.FormatInt(int64(sooner.ID), 10)+" "))
	second := strings.Fields(line(t, out, strconv.FormatInt(int64(later.ID), 10)+" "))
	if got := strings.Join(first[1:], " "); got != due(sooner)+" check that the cake came out" {
		t.Errorf("the sooner reads %q", got)
	}
	if got := strings.Join(second[1:], " "); got != due(later)+" ask how the interview went" {
		t.Errorf("the later reads %q", got)
	}
	if strings.Index(out, "cake") > strings.Index(out, "interview") {
		t.Errorf("out = %q, want the sooner first", out)
	}
	if strings.Contains(out, fired.Reason) {
		t.Errorf("out = %q, want nothing of the call back that fired", out)
	}
}

// A call back is cancelled by its number, and said to be; one that is not
// there to cancel, having fired or been cancelled already, is not.
func TestCallbacksCancel(t *testing.T) {
	dir, sooner, later, fired := scheduled(t)
	cfg := memoryConfig(t, dir)
	id := strconv.FormatInt(int64(sooner.ID), 10)

	code, out, errOut := exec(t, "-config", cfg, "callbacks", "cancel", id)
	if code != 0 {
		t.Fatalf("code = %d, stderr %s", code, errOut)
	}
	if !strings.HasPrefix(out, "cancelled:") || !strings.Contains(out, "check that the cake came out") {
		t.Errorf("out = %q, want the call back that was cancelled", out)
	}
	if _, out, _ := exec(t, "-config", cfg, "callbacks", "list"); strings.Contains(out, "cake") || !strings.Contains(out, "interview") {
		t.Errorf("out = %q, want the cancelled one gone and the other left", out)
	}
	for _, gone := range []store.Callback{sooner, fired} {
		code, _, errOut := exec(t, "-config", cfg, "callbacks", "cancel", strconv.FormatInt(int64(gone.ID), 10))
		if code != 1 || !strings.Contains(errOut, "no call back that has not come yet is numbered") {
			t.Errorf("cancelling %q = %d, stderr %q", gone.Reason, code, errOut)
		}
	}
	if code, _, _ := exec(t, "-config", cfg, "callbacks", "cancel", "none"); code != 2 {
		t.Error("cancelling something that is not a number was not a usage error")
	}
	if _, out, _ := exec(t, "-config", cfg, "callbacks", "cancel", strconv.FormatInt(int64(later.ID), 10)); !strings.HasPrefix(out, "cancelled:") {
		t.Errorf("out = %q, want the other cancelled", out)
	}
	if _, out, _ := exec(t, "-config", cfg, "callbacks", "list"); strings.TrimSpace(out) != "no call backs" {
		t.Errorf("out = %q, want none left", out)
	}
}

// Cancelling writes, but it does not start a conversation: a data_dir with a
// typo in it says so rather than being left holding an empty database.
func TestCancellingWhereThereIsNoConversation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	code, _, errOut := exec(t, "-config", memoryConfig(t, dir), "callbacks", "cancel", "1")
	if code != 1 || !strings.Contains(errOut, "holds no conversation yet") {
		t.Errorf("code = %d, stderr %q", code, errOut)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s was left behind: %v", dir, err)
	}
}

func TestCallbacksTakesOneOfItsCommands(t *testing.T) {
	dir, _, _, _ := scheduled(t)
	code, _, errOut := exec(t, "-config", memoryConfig(t, dir), "callbacks")
	if code != 2 {
		t.Errorf("code = %d, want a usage error", code)
	}
	for _, want := range []string{"list", "cancel"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr = %q, want %q among the commands", errOut, want)
		}
	}
}
