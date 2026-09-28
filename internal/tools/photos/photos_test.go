package photos

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/store"
	"nerdola.dev/x/paula/internal/tools/api"
)

// fakeEnv is a conversation whose image model answers with the photo a test
// gives it, which holds the photos she took by number, and sees images when
// the test says it does. A tool of this kind reaches for nothing else.
type fakeEnv struct {
	api.Env
	photo *store.Image
	taken map[int64]bool
	sees  bool

	prompt, shape string
	reference     []byte
	sent          []int64
}

func (f *fakeEnv) Photo(_ context.Context, prompt, shape string, reference []byte) (*store.Image, error) {
	f.prompt, f.shape, f.reference = prompt, shape, reference
	return f.photo, nil
}

func (f *fakeEnv) SendPhoto(_ context.Context, id int64) error {
	if !f.taken[id] {
		return store.ErrNotFound
	}
	f.sent = append(f.sent, id)
	return nil
}

func (f *fakeEnv) Show(store.Image) bool { return f.sees }

// avatar is a picture Venice made, standing for hers.
var avatar = filepath.Join("..", "..", "runners", "venice", "testdata", "image_generate.jpg")

var host = api.Host{Names: api.Names{Character: "Paula", User: "Caio"}, Avatar: avatar, Image: true}

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

// A photo is made from her avatar and what the prompt says, in the shape
// asked for, and answered with its number and what it shows; a model that
// sees images is told the photo itself is with the answer. The image model is
// told the avatar is how she looks, and the photo a new one seen by its
// camera: told only of a place, a model made the avatar again over another
// background, and told of a selfie, it drew her holding a phone.
func TestAPhotoIsMadeFromTheAvatarAndThePrompt(t *testing.T) {
	want, err := os.ReadFile(avatar)
	if err != nil {
		t.Fatal(err)
	}
	e := &fakeEnv{photo: &store.Image{ID: 7, SHA256: "sha-7", Caption: "A woman smiling at a window in the morning."}}
	got, err := call(t, "take_photo", e, `{"prompt":" a selfie at the kitchen window, morning light ","shape":"portrait"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.prompt, "not an edit of this one") || !strings.Contains(e.prompt, "neither the camera nor the phone") ||
		!strings.HasSuffix(e.prompt, "\n\na selfie at the kitchen window, morning light") ||
		e.shape != "portrait" || !bytes.Equal(e.reference, want) {
		t.Errorf("the photo was asked for with %q in %q from %d bytes, want the avatar told as a reference, her prompt, the shape and the avatar",
			e.prompt, e.shape, len(e.reference))
	}
	if got != "#7 A woman smiling at a window in the morning." {
		t.Errorf("the tool answered %q", got)
	}

	e.sees = true
	got, err = call(t, "take_photo", e, `{"prompt":"a selfie"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, "\nThe photo itself is with this answer.") || e.shape != "" {
		t.Errorf("the tool answered %q in the shape %q, want the photo with it in the model's own shape", got, e.shape)
	}
}

// A photo with nothing to show, or in a shape that is not offered, is not
// taken.
func TestAPhotoOfNothingOrOfNoShapeIsNotTaken(t *testing.T) {
	for _, args := range []string{`{"prompt":"  "}`, `{"prompt":"a selfie","shape":"round"}`} {
		e := &fakeEnv{photo: &store.Image{ID: 7}}
		if _, err := call(t, "take_photo", e, args); err == nil || e.reference != nil {
			t.Errorf("%s was taken: %v", args, err)
		}
	}
}

// A photo she took goes with her reply by its number, and a number that is no
// photo of hers is refused.
func TestAPhotoIsSentByItsNumber(t *testing.T) {
	e := &fakeEnv{taken: map[int64]bool{7: true}}
	if got, err := call(t, "send_photo", e, `{"number":7}`); err != nil || got != "photo #7 goes with your reply" {
		t.Errorf("sending photo 7 answered %q, %v", got, err)
	}
	if _, err := call(t, "send_photo", e, `{"number":3}`); err == nil || err.Error() != "no photo you took is numbered 3" {
		t.Errorf("sending a photo that is not there = %v", err)
	}
	if len(e.sent) != 1 || e.sent[0] != 7 {
		t.Errorf("sent %v, want photo 7 alone", e.sent)
	}
}

// A photo is not taken in the last round of a reply, where nothing could send
// it, and one she took is sent in any round.
func TestAPhotoIsTakenOnlyWhereItCanBeSent(t *testing.T) {
	tools, err := Open(config.Section{}, host)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools {
		_, left := tool.(api.Lookup)
		if name := tool.Definition().Name; left != (name == "take_photo") {
			t.Errorf("%s is left in the last round: %v", name, left)
		}
	}
}

// A photo is made from her avatar by the image model, so the kind does not
// open without either, and says where each comes from.
func TestPhotosNeedAnAvatarAndAnImageModel(t *testing.T) {
	_, err := Open(config.Section{}, api.Host{Names: host.Names, Image: true})
	if err == nil || !strings.Contains(err.Error(), "avatar.jpg") || !strings.Contains(err.Error(), "default_models.avatar") {
		t.Errorf("opening with no avatar = %v", err)
	}
	_, err = Open(config.Section{}, api.Host{Names: host.Names, Avatar: avatar})
	if err == nil || !strings.Contains(err.Error(), "default_models.image") {
		t.Errorf("opening with no image model = %v", err)
	}
}

// The section takes no settings, and a key written in it is named where the
// file has it.
func TestThePhotosSectionTakesNoSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "paula.yaml")
	file := "persona: paula.yaml\nrunners:\n  r:\n    type: venice\nmodels:\n  chat:\n    runner: r\n    id: x\n" +
		"default_models:\n  chat: chat\ntools:\n  photos:\n    model: x\n"
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(cfg.Tools[0].Section, host); err == nil || !strings.Contains(err.Error(), "tools.photos") ||
		!strings.Contains(err.Error(), "model") {
		t.Errorf("a key in the section = %v, want it named", err)
	}
}
