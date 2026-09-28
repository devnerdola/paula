package conversation

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"nerdola.dev/x/paula/internal/avatar"
	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/media"
	"nerdola.dev/x/paula/internal/runners/api"
	"nerdola.dev/x/paula/internal/store"
)

// TestLivePhotos holds the photos tools to the models of the file PAULA_LIVE
// names: her avatar is painted from the card by the avatar model, and asked
// for a selfie, she takes one with the image model from it, looks at it, and
// sends it. Everything goes to a data directory of its own, with a report.md
// of what every check found and the pictures as files.
//
//	PAULA_LIVE=live.yaml go test ./internal/conversation -run TestLivePhotos -v -timeout 20m
func TestLivePhotos(t *testing.T) {
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
	if !slices.Contains(offered, "take_photo") {
		t.Fatalf("the file offers %v, and no photos tool", offered)
	}
	data := filepath.Join(lv.dir, "data")
	painter := lv.set.Defaults[config.RoleAvatar]
	if painter == nil {
		t.Fatal("the file names no avatar model")
	}
	if err := avatar.Paint(context.Background(), data, lv.cfg.Engine.ImageMaxPx, lv.card, lv.set, painter); err != nil {
		t.Fatal(err)
	}
	face, _ := os.ReadFile(avatar.Find(data))
	lv.check("her avatar is painted from the card", media.Detect(face) == media.MIMEJPEG,
		"a JPEG at "+avatar.Path(data), fmt.Sprintf("%d bytes of %q", len(face), media.Detect(face)))

	// She is asked in the evening of today, as a person asks for a selfie: at
	// an hour she would be asleep, her instructions have her put the message
	// off rather than take one.
	now := time.Now()
	clock := &pastClock{}
	clock.set(time.Date(now.Year(), now.Month(), now.Day(), 19, 30, 0, 0, now.Location()))

	r := lv.run("photos", clock)
	r.send([]string{"send me a selfie of you right now, I miss your face"}, nil)
	lv.checkRan(r.turns[0], "take_photo", "asked for a selfie, she takes a photo")
	lv.checkPhotoShown(r, r.turns[0])
	lv.checkPhotoSent(r.turns[0])
	r.end()
	lv.requestsTable(r)
	lv.printf("\n%d checks passed, %d failed.", lv.passed, lv.failed)
}

// checkPhotoShown holds the round after the photo to carrying it in the answer
// of the call to a model shown a picture there, and to the answer's text alone
// for any other, and the host to having read it as a picture. A picture read
// as the text of its bytes came to a hundred thousand tokens more than the
// round that asked for it; one read as a picture comes to a few thousand.
func (lv *live) checkPhotoShown(r *liveRun, turn liveTurn) {
	ctx := context.Background()
	sees := r.model().catalogue.AnswerVision
	calls, _ := lv.st.ToolCalls(ctx, turn.entry)
	i := slices.IndexFunc(calls, func(c store.ToolCall) bool { return c.Name == "take_photo" && c.Error == "" })
	if i < 0 {
		lv.check("the photo is shown to a model shown a picture in an answer", false, "a take_photo call that ran", "none")
		return
	}
	requests := lv.requests(turn.entry)
	asked := slices.IndexFunc(requests, func(q store.Request) bool { return q.ID == calls[i].RequestID })
	next := slices.IndexFunc(requests, func(q store.Request) bool {
		return q.ID > calls[i].RequestID && q.Purpose == store.PurposeReply
	})
	if asked < 0 || next < 0 || requests[asked].Usage == nil || requests[next].Usage == nil {
		lv.check("the photo is shown to a model shown a picture in an answer", false,
			"the round that asked for the photo and the one after it, both counted", "not both")
		return
	}
	var inAnswers int
	for _, msg := range sentMessages(requests[next].RequestBody) {
		if msg.role == api.RoleTool {
			inAnswers += msg.images
		}
	}
	want := 0
	if sees {
		want = 1
	}
	grew := requests[next].Usage.PromptTokens - requests[asked].Usage.PromptTokens
	lv.check("the photo is in the answer of its call to a model shown a picture there, and read as one",
		inAnswers == want && grew < 10_000,
		fmt.Sprintf("%d pictures in the answers (the model is shown one there: %v), and fewer than 10,000 tokens more than the round that asked", want, sees),
		fmt.Sprintf("%d pictures in the answers, %d tokens more", inAnswers, grew),
		requests[asked].ID, requests[next].ID)
}

// checkPhotoSent holds a turn to a reply that carries a photo she took: one
// found by its number as hers, described, whose file is a JPEG.
func (lv *live) checkPhotoSent(turn liveTurn) {
	ctx := context.Background()
	files := media.New(filepath.Join(lv.dir, "data"), 0)
	reply, _ := lv.st.ReplyOfEntry(ctx, turn.entry)
	images, err := lv.st.Images(ctx)
	if err != nil {
		lv.t.Fatal(err)
	}
	found, ok := "no reply", false
	if reply != nil {
		found = fmt.Sprintf("a reply of %d photos", len(reply.Images()))
		for _, p := range reply.Images() {
			i := slices.IndexFunc(images, func(img store.Image) bool { return img.SHA256 == p.SHA256 })
			if i < 0 {
				continue
			}
			taken, err := lv.st.Photo(ctx, images[i].ID)
			data, _ := files.Load(p.SHA256)
			found = fmt.Sprintf("photo #%d at %s: %q", images[i].ID, files.Path(p.SHA256), clip(images[i].Caption, 200))
			ok = err == nil && taken.Caption != "" && media.Detect(data) == media.MIMEJPEG
		}
	}
	lv.check("she sends the photo she took", ok,
		"a reply carrying a photo she took, described, whose file is a JPEG", found, lv.ids(turn.entry)...)
}
