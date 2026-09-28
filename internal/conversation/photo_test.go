package conversation

import (
	"bytes"
	"context"
	"image"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/runners"
	"nerdola.dev/x/paula/internal/runners/api"
	"nerdola.dev/x/paula/internal/store"
	toolsapi "nerdola.dev/x/paula/internal/tools/api"
	"nerdola.dev/x/paula/internal/tools/photos"
)

// avatarFile stands for her picture, and made for a picture an image model
// made from it: a picture Venice painted from a prompt, and one it made from
// that picture as a second prompt said (runners/venice/testdata/SOURCES.md).
var (
	avatarFile = filepath.Join("..", "runners", "venice", "testdata", "image_generate.jpg")
	made       = filepath.Join("..", "runners", "venice", "testdata", "image_edit.jpg")
)

func read(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fakePainter is a runner whose model makes a picture from another: it answers
// every edit with the picture a test gives it, reports it to the recorder the
// request carries, and remembers what it was asked.
type fakePainter struct {
	*fakeRunner
	picture []byte
	edits   []api.EditRequest
}

func (p *fakePainter) Paint(context.Context, api.PaintRequest) ([]byte, error) { return p.picture, nil }

func (p *fakePainter) Edit(ctx context.Context, req api.EditRequest) ([]byte, error) {
	p.mu.Lock()
	p.edits = append(p.edits, req)
	p.mu.Unlock()
	rec := api.Recording(req.Recorder)
	record := &api.Record{Runner: p.Name(), Model: req.Model, Method: "POST", URL: p.URL() + "/image/edit", StartedAt: time.Now()}
	if err := rec.StartRequest(ctx, record); err != nil {
		return nil, err
	}
	record.Status, record.EndedAt = 200, time.Now()
	if err := rec.EndRequest(ctx, record); err != nil {
		return nil, err
	}
	return p.picture, nil
}

// withPainter gives a setup whose chat model is blind a model that sees, which
// describes pictures, and one that makes a picture from another, for the image
// role.
func withPainter(f, eyes *fakeRunner, painter *fakePainter) *runners.Setup {
	s := withVision(f, eyes)
	m := &runners.Configured{Name: "painter", ID: painter.model.ID, Runner: painter}
	s.Runners = append(s.Runners, painter)
	s.Models = append(s.Models, m)
	s.Defaults[config.RoleImage] = m
	return s
}

// openTakingPhotos opens an engine offering the photos tools over her avatar,
// whose chat model answers with the rounds given.
func openTakingPhotos(t *testing.T, rounds ...round) (*replyEngine, *fakePainter) {
	t.Helper()
	return openTakingPhotosFrom(t, avatarFile, rounds...)
}

// openTakingPhotosFrom is openTakingPhotos over an avatar of the test's own.
func openTakingPhotosFrom(t *testing.T, avatar string, rounds ...round) (*replyEngine, *fakePainter) {
	t.Helper()
	f := &fakeRunner{model: chatModel(), chat: answering(rounds...)}
	eyes := &fakeRunner{model: visionModel(), chat: says("A woman at a window.")}
	painter := &fakePainter{fakeRunner: &fakeRunner{model: api.Model{ID: "some/painter", Edit: true}}, picture: read(t, made)}
	tools, err := photos.Open(config.Section{}, toolsapi.Host{Names: toolsapi.Names{Character: "Paula", User: "Caio"},
		Avatar: avatar, Image: true})
	if err != nil {
		t.Fatal(err)
	}
	return openReplyOffering(t, f, withPainter(f, eyes, painter), config.DefaultEngine(), tools...), painter
}

// picture is how big a picture is, and of what kind.
func picture(t *testing.T, b []byte) (int, int, string) {
	t.Helper()
	c, kind, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return c.Width, c.Height, kind
}

func takePhoto(id string) api.ToolCall {
	return api.ToolCall{ID: id, Name: "take_photo", Arguments: `{"prompt":"a selfie at the window","shape":"portrait"}`}
}

func sendPhoto1(id string) api.ToolCall {
	return api.ToolCall{ID: id, Name: "send_photo", Arguments: `{"number":1}`}
}

// A photo is made by the image model from her avatar and her prompt, in the
// shape she asked for, under the call that took it. It is described as a
// picture she is sent is, and once she sends it, it goes with her reply, after
// what she wrote, to the frontends as well.
func TestAPhotoSheTookGoesWithHerReply(t *testing.T) {
	r, painter := openTakingPhotos(t,
		round{calls: []api.ToolCall{takePhoto("call_1")}},
		round{calls: []api.ToolCall{sendPhoto1("call_2")}},
		round{text: "me right now"},
	)
	r.say(t, "send me a selfie")

	if len(painter.edits) != 1 {
		t.Fatalf("the image model was asked %d times, want once", len(painter.edits))
	}
	edit := painter.edits[0]
	if w, h, kind := picture(t, edit.Picture); edit.Model != "some/painter" || !strings.HasSuffix(edit.Prompt, "a selfie at the window") ||
		edit.Shape != api.ShapePortrait || w != 256 || h != 256 || kind != "jpeg" {
		t.Errorf("the image model was asked for %q with %q in %q from a %dx%d %s, want the 256x256 JPEG avatar",
			edit.Model, edit.Prompt, edit.Shape, w, h, kind)
	}

	reply, calls := stored(t, r)
	if reply.Text() != "me right now" || len(reply.Images()) != 1 || reply.Parts[1].Type != store.PartImage {
		t.Fatalf("the reply is %+v, want what she wrote and then the photo", reply.Parts)
	}
	if len(calls) != 2 || calls[0].Result != "#1 A woman at a window." || calls[1].Result != "photo #1 goes with your reply" {
		t.Errorf("the calls answered %+v", calls)
	}
	taken, err := r.store.Photo(context.Background(), 1)
	if err != nil || taken.SHA256 != reply.Images()[0].SHA256 || taken.Caption != "A woman at a window." {
		t.Errorf("photo #1 is %+v, %v, want the one in the reply, described", taken, err)
	}

	requests, err := r.store.Requests(context.Background(), reply.EntryID)
	if err != nil {
		t.Fatal(err)
	}
	var purposes []string
	for _, req := range requests {
		purposes = append(purposes, req.Purpose)
	}
	want := []string{store.PurposeReply, store.PurposeTool, store.PurposeCaption, store.PurposeReply, store.PurposeReply}
	if !slices.Equal(purposes, want) {
		t.Fatalf("the reply kept requests for %v, want %v", purposes, want)
	}
	if requests[1].ToolCall != calls[0].ID || requests[1].Model != "some/painter" {
		t.Errorf("the image model's request was kept as %+v, want it under the call that took the photo", requests[1])
	}

	var done *store.Message
	for _, ev := range published(r.Engine) {
		if ev.Kind == ReplyDone {
			done = ev.Message
		}
	}
	if done == nil || len(done.Images()) != 1 || done.Images()[0].SHA256 != taken.SHA256 {
		t.Errorf("the reply was published as %+v, want it with the photo", done)
	}
}

// An avatar put in the data directory is any file a phone makes: one that is
// stored sideways goes to the image model upright, as a JPEG, as every picture
// Paula keeps does, rather than as the image model would read its bytes.
func TestAnAvatarGoesUprightToTheImageModel(t *testing.T) {
	sideways := filepath.Join("..", "media", "testdata", "quadrants-32x16-orientation-6.jpg")
	r, painter := openTakingPhotosFrom(t, sideways,
		round{calls: []api.ToolCall{takePhoto("call_1")}},
		round{text: "one sec"},
	)
	r.say(t, "send me a selfie")

	if len(painter.edits) != 1 {
		t.Fatalf("the image model was asked %d times, want once", len(painter.edits))
	}
	if w, h, kind := picture(t, painter.edits[0].Picture); w != 16 || h != 32 || kind != "jpeg" {
		t.Errorf("the avatar went as a %dx%d %s, want the 16x32 JPEG it is upright", w, h, kind)
	}
}

// Her photo is told after her message in the prompts that follow, the way a
// time is, as what it showed: no host takes a picture in a message of hers,
// and what is written inside her message she would write herself.
func TestHerPhotoIsToldAfterHerMessage(t *testing.T) {
	r, _ := openTakingPhotos(t,
		round{calls: []api.ToolCall{takePhoto("call_1")}},
		round{calls: []api.ToolCall{sendPhoto1("call_2")}},
		round{text: "me right now"},
	)
	r.say(t, "send me a selfie")
	r.say(t, "you look great")

	msgs := r.runner.replied().Messages
	i := slices.IndexFunc(msgs, func(m api.Message) bool { return m.Role == api.RoleAssistant })
	if i < 0 || i+1 >= len(msgs) {
		t.Fatalf("the prompt %+v carries no reply of hers", msgs)
	}
	if text(msgs[i]) != "me right now" || len(images(msgs[i])) != 0 {
		t.Errorf("her message went as %+v, want what she wrote alone", msgs[i])
	}
	if note := msgs[i+1]; note.Role != api.RoleSystem || text(note) != "Your message above came with [photo: A woman at a window.]." {
		t.Errorf("after her message came %+v, want the note of her photo", note)
	}
}

// A photo sent with nothing written is a reply, and is told after the
// message she answered as what she sent: a message of hers with nothing in it
// is one a host refuses.
func TestAPhotoAloneIsAReply(t *testing.T) {
	r, _ := openTakingPhotos(t,
		round{calls: []api.ToolCall{takePhoto("call_1")}},
		round{calls: []api.ToolCall{sendPhoto1("call_2")}},
		round{},
	)
	r.say(t, "send me a selfie")

	reply, _ := stored(t, r)
	if reply.Text() != "" || len(reply.Images()) != 1 {
		t.Fatalf("the reply is %+v, want the photo alone", reply.Parts)
	}

	r.say(t, "you look great")
	msgs := r.runner.replied().Messages
	for _, m := range msgs {
		if m.Role == api.RoleAssistant {
			t.Errorf("the prompt carries a message of hers, %+v", m)
		}
	}
	if !slices.ContainsFunc(msgs, func(m api.Message) bool {
		return m.Role == api.RoleSystem && text(m) == "You sent [photo: A woman at a window.]."
	}) {
		t.Errorf("the prompt %+v does not say what she sent", msgs)
	}
}

// A number that is no photo she took is refused, and nothing goes with the
// reply.
func TestAPhotoThatIsNotThereIsNotSent(t *testing.T) {
	r, painter := openTakingPhotos(t,
		round{calls: []api.ToolCall{sendPhoto1("call_1")}},
		round{text: "hm, it did not go"},
	)
	r.say(t, "send me a selfie")

	reply, calls := stored(t, r)
	if len(reply.Images()) != 0 || len(painter.edits) != 0 {
		t.Errorf("the reply is %+v after %d photos, want no photo", reply.Parts, len(painter.edits))
	}
	if len(calls) != 1 || calls[0].Error != "no photo you took is numbered 1" {
		t.Errorf("the call was written down as %+v", calls)
	}
}
