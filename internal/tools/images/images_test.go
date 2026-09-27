package images

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/store"
	"nerdola.dev/x/paula/internal/tools/api"
)

// fakeEnv is a conversation holding the pictures a test gives it, which writes
// a time in a way of its own, and sees images when the test says it does.
type fakeEnv struct {
	images []store.Image
	sees   bool
	shown  []store.Image
}

func (f *fakeEnv) Date(t time.Time) string { return t.UTC().Format("2 Jan") }

func (f *fakeEnv) Time(t time.Time) string { return t.UTC().Format("2 Jan 15:04") }

func (f *fakeEnv) Memories(context.Context, string, int) ([]store.Memory, error) { return nil, nil }

func (f *fakeEnv) LatestMemories(context.Context, int, int) ([]store.Memory, error) { return nil, nil }

func (f *fakeEnv) Remember(context.Context, string, []store.MemoryID) (*store.Memory, error) {
	return nil, nil
}

func (f *fakeEnv) Forget(context.Context, store.MemoryID) ([]store.Memory, error) { return nil, nil }

func (f *fakeEnv) Now() time.Time { return time.Time{} }

func (f *fakeEnv) Callbacks(context.Context) ([]store.Callback, error) { return nil, nil }

func (f *fakeEnv) Schedule(context.Context, time.Time, string) (*store.Callback, error) {
	return nil, nil
}

func (f *fakeEnv) Move(context.Context, store.CallbackID, time.Time) error { return nil }

func (f *fakeEnv) Cancel(context.Context, store.CallbackID) error { return nil }

// Images are the test's pictures, which it gives newest first, from the one at
// from on.
func (f *fakeEnv) Images(_ context.Context, from, limit int) ([]store.Image, error) {
	if from >= len(f.images) {
		return nil, nil
	}
	return f.images[from:min(from+limit, len(f.images))], nil
}

func (f *fakeEnv) Image(_ context.Context, id int64) (*store.Image, error) {
	for _, img := range f.images {
		if img.ID == id {
			return &img, nil
		}
	}
	return nil, store.ErrNotFound
}

func (f *fakeEnv) Show(img store.Image) bool {
	if f.sees {
		f.shown = append(f.shown, img)
	}
	return f.sees
}

// env holds two pictures sent on 21 September 2026, the newest first, one of
// which nothing has said what it shows.
func env() *fakeEnv {
	return &fakeEnv{images: []store.Image{
		{ID: 2, SHA256: "sha-b", MessageID: 40, SentAt: time.Date(2026, 9, 21, 19, 50, 0, 0, time.UTC)},
		{ID: 1, SHA256: "sha-a", Caption: "White pixelated text on a dark screen that reads THERE IS NO KNOWLEDGE THAT IS NOT POWER.",
			MessageID: 31, SentAt: time.Date(2026, 9, 21, 19, 44, 0, 0, time.UTC)},
	}}
}

var host = api.Host{Names: api.Names{Character: "Paula", User: "Caio"}, Language: "Portuguese"}

func call(t *testing.T, name string, e *fakeEnv, args string) (string, error) {
	t.Helper()
	tools, err := Open(config.Section{}, host)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools {
		if tool.Definition().Name == name {
			return tool.Call(context.Background(), e, json.RawMessage(args))
		}
	}
	t.Fatalf("no tool is called %s", name)
	return "", nil
}

// Every picture is listed, newest first, with its number, when it was sent and
// what it showed, and a conversation that has none says so.
func TestAListIsEveryPicture(t *testing.T) {
	if got, err := call(t, "list_images", &fakeEnv{}, `{}`); err != nil || got != "no pictures yet" {
		t.Errorf("listing no pictures answered %q, %v", got, err)
	}

	got, err := call(t, "list_images", env(), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	want := "#2 (sent 21 Sep 19:50) nothing was said of what it shows\n" +
		"#1 (sent 21 Sep 19:44) White pixelated text on a dark screen that reads THERE IS NO KNOWLEDGE THAT IS NOT POWER."
	if got != want {
		t.Errorf("the list answered\n%s\nwant\n%s", got, want)
	}
}

// What a call answers goes into the prompt of the round after it, so a list of
// many pictures answers fifty at a time, saying where the older ones are.
func TestAListOfManyPicturesAnswersAPageAtATime(t *testing.T) {
	e := &fakeEnv{}
	for i := 120; i > 0; i-- {
		e.images = append(e.images, store.Image{ID: int64(i), SHA256: fmt.Sprintf("sha-%d", i),
			Caption: fmt.Sprintf("picture %d", i), SentAt: time.Date(2026, 9, 21, 19, 44, 0, 0, time.UTC)})
	}
	for _, tc := range []struct {
		args        string
		first, last string
		lines       int
		older       string
	}{
		{`{}`, "#120 ", "#71 ", 50, "list_images with from 50"},
		{`{"from":50}`, "#70 ", "#21 ", 50, "list_images with from 100"},
		{`{"from":100}`, "#20 ", "#1 ", 20, ""},
	} {
		got, err := call(t, "list_images", e, tc.args)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(got, "\n")
		pictures := lines
		if tc.older != "" {
			pictures = lines[:len(lines)-1]
			if !strings.Contains(lines[len(lines)-1], tc.older) {
				t.Errorf("%s: the list ends with %q, want it to say %q", tc.args, lines[len(lines)-1], tc.older)
			}
		}
		if len(pictures) != tc.lines || !strings.HasPrefix(pictures[0], tc.first) ||
			!strings.HasPrefix(pictures[len(pictures)-1], tc.last) {
			t.Errorf("%s: the list is %d pictures from %q to %q, want %d from %q to %q", tc.args, len(pictures),
				pictures[0], pictures[len(pictures)-1], tc.lines, tc.first, tc.last)
		}
	}
	if got, err := call(t, "list_images", e, `{"from":200}`); err != nil || got != "no pictures past the newest 200" {
		t.Errorf("listing past every picture answered %q, %v", got, err)
	}
	if _, err := call(t, "list_images", e, `{"from":-1}`); err == nil {
		t.Error("listing from below zero was answered")
	}
}

// A model that sees images is shown the picture with the answer, which says so;
// any other is told what it showed, and shown nothing.
func TestAPictureIsShownToAModelThatSees(t *testing.T) {
	line := "#1 (sent 21 Sep 19:44) White pixelated text on a dark screen that reads THERE IS NO KNOWLEDGE THAT IS NOT POWER."

	e := env()
	e.sees = true
	got, err := call(t, "get_image", e, `{"number":1}`)
	if err != nil {
		t.Fatal(err)
	}
	if got != line+"\nThe picture itself is with this answer." {
		t.Errorf("a model that sees was answered %q", got)
	}
	if len(e.shown) != 1 || e.shown[0].SHA256 != "sha-a" {
		t.Errorf("a model that sees was shown %+v, want the picture asked for", e.shown)
	}

	e = env()
	got, err = call(t, "get_image", e, `{"number":1}`)
	if err != nil {
		t.Fatal(err)
	}
	if got != line || len(e.shown) != 0 {
		t.Errorf("a model that does not see was answered %q and shown %+v", got, e.shown)
	}
}

func TestAPictureThatIsNotThereIsSaidSo(t *testing.T) {
	e := env()
	e.sees = true
	_, err := call(t, "get_image", e, `{"number":9}`)
	if err == nil || err.Error() != "no picture is numbered 9" {
		t.Errorf("a picture that is not there = %v", err)
	}
	if len(e.shown) != 0 {
		t.Errorf("shown %+v for a picture that is not there", e.shown)
	}
}

// The pictures of older messages are not in the prompt, so both tools say so,
// and each names the other.
func TestTheImageToolsSayWhereThePicturesAre(t *testing.T) {
	tools, err := Open(config.Section{}, host)
	if err != nil {
		t.Fatal(err)
	}
	notes := map[string]string{"list_images": "listing pictures", "get_image": "looking at picture #1"}
	other := map[string]string{"list_images": "get_image", "get_image": "list_images"}
	for _, tool := range tools {
		d := tool.Definition()
		for _, want := range []string{"Caio", "recent messages", other[d.Name]} {
			if !strings.Contains(d.Description, want) {
				t.Errorf("%s is described as %q, without %q", d.Name, d.Description, want)
			}
		}
		if got := tool.Note(json.RawMessage(`{"number":1}`)); got != notes[d.Name] {
			t.Errorf("%s says %q while it runs, want %q", d.Name, got, notes[d.Name])
		}
	}
}

// The section takes no settings, and a key written in it is named where the
// file has it.
func TestTheImagesSectionTakesNoSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "paula.yaml")
	file := "persona: paula.yaml\nrunners:\n  r:\n    type: venice\nmodels:\n  chat:\n    runner: r\n    id: x\n" +
		"default_models:\n  chat: chat\ntools:\n  images:\n    results: 3\n"
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(cfg.Tools[0].Section, host); err == nil || !strings.Contains(err.Error(), "tools.images") ||
		!strings.Contains(err.Error(), "results") {
		t.Errorf("a key in the section = %v, want it named", err)
	}
}
