package cmd

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"nerdola.dev/x/paula/internal/media"
	"nerdola.dev/x/paula/internal/store"
)

// pictured is a data directory holding two pictures: one Caio sent, which was
// described, and one Ada sent an hour later, which the vision host refused to
// look at.
func pictured(t *testing.T) (dir string, caios, adas store.Image) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "data")
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	add := func(role, sha string, at time.Time) {
		if err := s.AddMedia(ctx, sha); err != nil {
			t.Fatal(err)
		}
		m := &store.Message{
			Role:      role,
			Channel:   "repl",
			Parts:     []store.Part{{Type: store.PartImage, SHA256: sha, MIME: media.MIMEJPEG}},
			CreatedAt: at,
		}
		if err := s.AddMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	add(store.RoleUser, "abc123", when)
	if err := s.SetCaption(ctx, "abc123", "A red apple on a table."); err != nil {
		t.Fatal(err)
	}
	add(store.RoleAssistant, "def456", when.Add(time.Hour))
	if err := s.SetCaptionError(ctx, "def456", "502: bad gateway"); err != nil {
		t.Fatal(err)
	}

	images, err := s.Images(ctx)
	if err != nil || len(images) != 2 {
		t.Fatalf("images = %+v, %v", images, err)
	}
	return dir, images[1], images[0]
}

func TestImagesList(t *testing.T) {
	dir, caios, adas := pictured(t)
	cfg := memoryConfig(t, dir)
	code, out, errOut := exec(t, "-config", cfg, "images", "list")
	if code != 0 {
		t.Fatalf("code = %d, stderr %s", code, errOut)
	}

	header := line(t, out, "ID ")
	for _, want := range []string{"SENT", "BY", "MESSAGE", "SHOWED"} {
		if !strings.Contains(header, want) {
			t.Errorf("the header has no %q: %s", want, header)
		}
	}
	// The newest comes first, sent by Ada, and nothing has said what it
	// shows.
	rows := strings.Split(strings.TrimSpace(out), "\n")
	if !strings.HasPrefix(rows[1], strconv.FormatInt(adas.ID, 10)+" ") {
		t.Errorf("the first row is %q, want the newest picture", rows[1])
	}
	if row := rows[1]; !strings.Contains(row, "Ada") || !strings.Contains(row, "nothing was said of what it shows") {
		t.Errorf("Ada's picture reads %q", row)
	}
	row := line(t, out, strconv.FormatInt(caios.ID, 10)+" ")
	for _, want := range []string{"Caio", strconv.FormatInt(int64(caios.MessageID), 10), "A red apple on a table."} {
		if !strings.Contains(row, want) {
			t.Errorf("Caio's picture has no %q: %s", want, row)
		}
	}

	// A list ends with which of how many pictures it holds.
	if !strings.HasSuffix(out, "\n\n1 to 2 of 2 pictures\n") {
		t.Errorf("the list ends\n%s\nwant which of how many pictures it holds", out)
	}

	// A list is paged from the newest, and says when there is nothing past
	// what was passed over.
	for _, c := range []struct {
		flags      []string
		who, which string
	}{
		{[]string{"-n", "1"}, "Ada", "1 to 1 of 2 pictures"},
		{[]string{"-n", "1", "-from", "1"}, "Caio", "2 to 2 of 2 pictures"},
	} {
		_, out, _ := exec(t, append([]string{"-config", cfg, "images", "list"}, c.flags...)...)
		rows := strings.Split(strings.TrimSpace(out), "\n")
		if len(rows) != 4 || !strings.Contains(rows[1], c.who) || rows[3] != c.which {
			t.Errorf("%v listed:\n%s\nwant %s's picture alone, and %q", c.flags, out, c.who, c.which)
		}
	}
	if _, out, _ := exec(t, "-config", cfg, "images", "list", "-from", "5"); strings.TrimSpace(out) != "no pictures past the newest 5: there are 2 in all" {
		t.Errorf("-from 5 listed %q", out)
	}
	if code, _, _ := exec(t, "-config", cfg, "images", "list", "extra"); code != 2 {
		t.Error("list took an argument")
	}
	for _, flags := range [][]string{{"-n", "0"}, {"-from", "-1"}} {
		if code, _, _ := exec(t, append([]string{"-config", cfg, "images", "list"}, flags...)...); code != 2 {
			t.Errorf("%v was taken", flags)
		}
	}
}

// A conversation with no pictures says so, wherever the list is asked from.
func TestImagesListWithNoPictures(t *testing.T) {
	dir, _, _, _ := remembered(t)
	cfg := memoryConfig(t, dir)
	for _, flags := range [][]string{nil, {"-from", "5"}} {
		if _, out, _ := exec(t, append([]string{"-config", cfg, "images", "list"}, flags...)...); strings.TrimSpace(out) != "no pictures" {
			t.Errorf("%v listed %q, want none", flags, out)
		}
	}
}

// A picture is shown with where its file is, so it can be opened, and with
// why it was not described when the host said so.
func TestImagesShow(t *testing.T) {
	dir, caios, adas := pictured(t)
	cfg := memoryConfig(t, dir)
	code, out, errOut := exec(t, "-config", cfg, "images", "show", strconv.FormatInt(caios.ID, 10))
	if code != 0 {
		t.Fatalf("code = %d, stderr %s", code, errOut)
	}
	for _, want := range []string{
		"picture " + strconv.FormatInt(caios.ID, 10) + "\n",
		// The time is shown in the machine's zone, whichever that is.
		"sent    " + when.Local().Format("2006-01-02 15:04:05"),
		" by Caio, in message " + strconv.FormatInt(int64(caios.MessageID), 10) + "\n",
		"file    " + media.New(dir, 0).Path("abc123") + "\n",
		"shows   A red apple on a table.\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output has no %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "error") {
		t.Errorf("a picture that was described shows an error:\n%s", out)
	}

	_, out, _ = exec(t, "-config", cfg, "images", "show", strconv.FormatInt(adas.ID, 10))
	for _, want := range []string{" by Ada, ", "shows   nothing was said of what it shows\n", "error   502: bad gateway\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("output has no %q:\n%s", want, out)
		}
	}
}

func TestImagesShowOfAPictureThatIsNotThere(t *testing.T) {
	dir, _, _ := pictured(t)
	code, _, errOut := exec(t, "-config", memoryConfig(t, dir), "images", "show", "99")
	if code != 1 || !strings.Contains(errOut, "not found") {
		t.Errorf("code = %d, stderr %q, want the picture not found", code, errOut)
	}
}

func TestImagesTakesOneOfItsCommands(t *testing.T) {
	dir, _, _ := pictured(t)
	cfg := memoryConfig(t, dir)
	if code, _, errOut := exec(t, "-config", cfg, "images"); code != 2 || !strings.Contains(errOut, "usage: paula images") {
		t.Errorf("images alone = %d, %q", code, errOut)
	}
	if code, _, errOut := exec(t, "-config", cfg, "images", "show"); code != 2 || !strings.Contains(errOut, "which picture") {
		t.Errorf("show with no picture = %d, %q", code, errOut)
	}
	if code, _, errOut := exec(t, "-config", cfg, "images", "show", "1", "2"); code != 2 || !strings.Contains(errOut, "one picture at a time") {
		t.Errorf("show of two pictures = %d, %q", code, errOut)
	}
	if code, _, errOut := exec(t, "-config", cfg, "images", "show", "x"); code != 2 || !strings.Contains(errOut, `"x" is no picture`) {
		t.Errorf("show of x = %d, %q", code, errOut)
	}
}
