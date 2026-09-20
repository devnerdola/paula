package media

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// read is one of the images beside this file. They are written once, by hand;
// see testdata/SOURCES.md.
func read(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The colours the fixtures are painted in: red on the left half of a red-blue
// image and blue on the right, and a colour of its own in each quadrant of the
// quadrants one.
var (
	red         = color.RGBA{255, 0, 0, 255}
	blue        = color.RGBA{0, 0, 255, 255}
	topLeft     = color.RGBA{255, 0, 0, 255}
	bottomRight = color.RGBA{255, 255, 0, 255}
)

func decode(t *testing.T, b []byte) image.Image {
	t.Helper()
	m, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// nearly reports whether two colors are the same after a JPEG round trip.
func nearly(a, b color.Color) bool {
	ar, ag, ab, _ := a.RGBA()
	br, bg, bb, _ := b.RGBA()
	const slack = 40 << 8
	return abs(int(ar)-int(br)) < slack && abs(int(ag)-int(bg)) < slack && abs(int(ab)-int(bb)) < slack
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func TestDetect(t *testing.T) {
	for _, tc := range []struct {
		name string
		b    []byte
		want string
	}{
		{"jpeg", read(t, "red-blue-8x4.jpg"), "image/jpeg"},
		{"png", read(t, "red-blue-8x4.png"), "image/png"},
		{"gif", read(t, "red-blue-8x4.gif"), "image/gif"},
		{"webp", read(t, "red-blue-8x4.webp"), "image/webp"},
		{"text", read(t, "not-an-image.txt"), ""},
		{"empty", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Detect(tc.b); got != tc.want {
				t.Errorf("Detect = %q, want %q", got, tc.want)
			}
		})
	}
	if Supported("application/pdf") {
		t.Error("pdf reads as supported")
	}
	if len(Types()) != 4 {
		t.Errorf("Types = %v", Types())
	}
}

func TestProcessEveryType(t *testing.T) {
	for _, name := range []string{
		"red-blue-8x4.jpg",
		"red-blue-8x4.png",
		"red-blue-8x4.gif",
		"red-blue-8x4.webp",
	} {
		t.Run(name, func(t *testing.T) {
			out, err := Process(read(t, name), 0)
			if err != nil {
				t.Fatal(err)
			}
			if Detect(out) != MIMEJPEG {
				t.Fatalf("stored type = %q, want %q", Detect(out), MIMEJPEG)
			}
			got := decode(t, out)
			if got.Bounds().Dx() != 8 || got.Bounds().Dy() != 4 {
				t.Fatalf("size = %v, want 8x4", got.Bounds())
			}
			if !nearly(got.At(1, 1), red) || !nearly(got.At(6, 1), blue) {
				t.Errorf("colors = %v, %v", got.At(1, 1), got.At(6, 1))
			}
		})
	}
}

func TestProcessRefusesWhatItCannotDecode(t *testing.T) {
	if _, err := Process(read(t, "not-an-image.txt"), 0); err == nil {
		t.Fatal("Process succeeded")
	}
}

func TestScaling(t *testing.T) {
	for _, tc := range []struct {
		name         string
		file         string
		maxPx        int
		wantW, wantH int
	}{
		{"wider than tall", "red-blue-800x400.png", 100, 100, 50},
		{"taller than wide", "red-blue-400x800.png", 100, 50, 100},
		{"square", "red-blue-500x500.png", 100, 100, 100},
		{"already small", "red-blue-80x40.png", 100, 80, 40},
		{"no limit", "red-blue-800x400.png", 0, 800, 400},
		{"very thin", "red-blue-1000x3.png", 100, 100, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := Process(read(t, tc.file), tc.maxPx)
			if err != nil {
				t.Fatal(err)
			}
			b := decode(t, out).Bounds()
			if b.Dx() != tc.wantW || b.Dy() != tc.wantH {
				t.Errorf("size = %dx%d, want %dx%d", b.Dx(), b.Dy(), tc.wantW, tc.wantH)
			}
		})
	}
}

func TestExifOrientations(t *testing.T) {
	// Where the colour of each quadrant of the upright image is found in the
	// stored one, and the size the stored one has.
	for _, tc := range []struct {
		o            int
		wantW, wantH int
		at           map[image.Point]color.RGBA
	}{
		{1, 32, 16, map[image.Point]color.RGBA{{4, 4}: topLeft, {27, 11}: bottomRight}},
		{2, 32, 16, map[image.Point]color.RGBA{{27, 4}: topLeft, {4, 11}: bottomRight}},
		{3, 32, 16, map[image.Point]color.RGBA{{27, 11}: topLeft, {4, 4}: bottomRight}},
		{4, 32, 16, map[image.Point]color.RGBA{{4, 11}: topLeft, {27, 4}: bottomRight}},
		{5, 16, 32, map[image.Point]color.RGBA{{3, 8}: topLeft, {12, 24}: bottomRight}},
		{6, 16, 32, map[image.Point]color.RGBA{{12, 8}: topLeft, {3, 24}: bottomRight}},
		{7, 16, 32, map[image.Point]color.RGBA{{12, 24}: topLeft, {3, 8}: bottomRight}},
		{8, 16, 32, map[image.Point]color.RGBA{{3, 24}: topLeft, {12, 8}: bottomRight}},
	} {
		name := fmt.Sprintf("quadrants-32x16-orientation-%d.jpg", tc.o)
		out, err := Process(read(t, name), 0)
		if err != nil {
			t.Fatalf("orientation %d: %v", tc.o, err)
		}
		got := decode(t, out)
		if got.Bounds().Dx() != tc.wantW || got.Bounds().Dy() != tc.wantH {
			t.Errorf("orientation %d: size = %v, want %dx%d", tc.o, got.Bounds(), tc.wantW, tc.wantH)
		}
		for at, want := range tc.at {
			if !nearly(got.At(at.X, at.Y), want) {
				t.Errorf("orientation %d: %v = %v, want %v", tc.o, at, got.At(at.X, at.Y), want)
			}
		}
	}
}

func TestOrientationIsIgnoredWhenThereIsNone(t *testing.T) {
	if got := orientation(read(t, "red-blue-8x4.jpg")); got != 0 {
		t.Errorf("orientation = %d, want 0", got)
	}
	if got := orientation(read(t, "red-blue-8x4.png")); got != 0 {
		t.Errorf("orientation of a png = %d, want 0", got)
	}
	if got := orientation(nil); got != 0 {
		t.Errorf("orientation of nothing = %d, want 0", got)
	}

	// One outside the eight the standard names is read as it is written, and
	// leaves the image alone.
	outside := read(t, "red-blue-8x4-orientation-9.jpg")
	if got := orientation(outside); got != 9 {
		t.Errorf("orientation = %d, want it read as written", got)
	}
	out, err := Process(outside, 0)
	if err != nil {
		t.Fatal(err)
	}
	if b := decode(t, out).Bounds(); b.Dx() != 8 || b.Dy() != 4 {
		t.Errorf("an orientation outside 1-8 changed the image: %v", b)
	}
}

func TestFiles(t *testing.T) {
	dir := t.TempDir()
	f := New(dir, 4)

	sha, err := f.Store(read(t, "red-blue-8x4.png"))
	if err != nil {
		t.Fatal(err)
	}
	if len(sha) != 64 {
		t.Fatalf("Store = %q, want the digest of what it kept", sha)
	}
	// An image lives under media/, in a directory named after the first byte of
	// its digest.
	if want := filepath.Join(dir, "media", sha[:2], sha+".jpg"); f.Path(sha) != want {
		t.Errorf("Path = %q, want %q", f.Path(sha), want)
	}

	b, err := f.Load(sha)
	if err != nil {
		t.Fatal(err)
	}
	// Everything kept is a JPEG, which is what Detect says of what came back.
	if got := Detect(b); got != MIMEJPEG {
		t.Errorf("what was kept is %q, want %q", got, MIMEJPEG)
	}
	if got := decode(t, b).Bounds(); got.Dx() != 4 || got.Dy() != 2 {
		t.Errorf("stored size = %v, want it scaled to 4x2", got)
	}

	again, err := f.Store(read(t, "red-blue-8x4.png"))
	if err != nil || again != sha {
		t.Errorf("storing the same image = %q, %v, want %q", again, err, sha)
	}
	if _, err := f.Load("0000000000"); err == nil {
		t.Error("Load of a missing image succeeded")
	}

	fi, err := os.Stat(f.Path(sha))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
	// Nothing but the image is left in the directory it was written to.
	entries, err := os.ReadDir(filepath.Dir(f.Path(sha)))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(f.Path(sha)) {
		t.Errorf("the directory holds %v, want the image alone", entries)
	}
}

func TestTheSameImageKeptTwiceAtOnce(t *testing.T) {
	f := New(t.TempDir(), 1024)
	b := read(t, "red-blue-64x32.jpg")

	var wg sync.WaitGroup
	shas := make([]string, 8)
	errs := make([]error, 8)
	for i := range shas {
		wg.Add(1)
		go func() {
			defer wg.Done()
			shas[i], errs[i] = f.Store(b)
		}()
	}
	wg.Wait()

	for i, sha := range shas {
		if errs[i] != nil {
			t.Fatalf("store %d: %v", i, errs[i])
		}
		if sha != shas[0] {
			t.Fatalf("store %d kept the image as %s, want %s", i, sha, shas[0])
		}
		got, err := f.Load(sha)
		if err != nil {
			t.Fatalf("load %d: %v", i, err)
		}
		// What was kept is a whole image of the size that was asked for, read
		// back by the decoder rather than held against what wrote it.
		m := decode(t, got)
		if m.Bounds().Dx() != 64 || m.Bounds().Dy() != 32 {
			t.Fatalf("what was kept is %v", m.Bounds())
		}
	}

	// Nothing half-written is left in the directory.
	entries, err := os.ReadDir(filepath.Join(f.dir, shas[0][:2]))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("the directory holds %d files, want the image alone", len(entries))
	}
}

func TestProcessRefusesAnImageTooBigToDecode(t *testing.T) {
	_, err := Process(read(t, "huge-40000x40000.png"), 1024)
	if err == nil {
		t.Fatal("Process succeeded")
	}
	if !strings.Contains(err.Error(), "40000 by 40000") {
		t.Errorf("error = %v, want the size it declared", err)
	}

	// One that is merely large is decoded as it always was.
	if _, err := Process(read(t, "red-blue-800x400.png"), 100); err != nil {
		t.Errorf("Process = %v", err)
	}
}

// The standard does not name a long there, and the two bytes where a short's
// value would be mean something else: reading them turns an image nobody asked
// to be turned.
func TestAnOrientationWrittenAsSomethingElse(t *testing.T) {
	b := read(t, "red-blue-8x4-orientation-long.jpg")
	if got := orientation(b); got != 0 {
		t.Errorf("orientation = %d, want the tag left alone", got)
	}
	out, err := Process(b, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := decode(t, out).Bounds(); got.Dx() != 8 || got.Dy() != 4 {
		t.Errorf("size = %v, want the image as it was", got)
	}
}

// The standard allows the padding, so the orientation is still read.
func TestAFillByteBeforeTheExifSegment(t *testing.T) {
	padded := read(t, "red-blue-8x4-orientation-6-padded.jpg")
	if got := orientation(padded); got != 6 {
		t.Errorf("orientation = %d, want the 6 it carries", got)
	}
	out, err := Process(padded, 0)
	if err != nil {
		t.Fatal(err)
	}
	if b := decode(t, out).Bounds(); b.Dx() != 4 || b.Dy() != 8 {
		t.Errorf("size = %dx%d, want it turned upright", b.Dx(), b.Dy())
	}
}

// A header is a few bytes whatever size it declares, so the bound holds for
// every type she reads.
func TestProcessRefusesEveryTypeTooBigToDecode(t *testing.T) {
	for _, tc := range []struct {
		name string
		file string
		want string
	}{
		{"png", "huge-40000x40000.png", "40000 by 40000"},
		{"gif", "huge-32000x32000.gif", "32000 by 32000"},
		{"webp", "huge-16383x16383.webp", "16383 by 16383"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Process(read(t, tc.file), 1024)
			if err == nil {
				t.Fatal("Process succeeded")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want the size it declared", err)
			}
		})
	}
}
