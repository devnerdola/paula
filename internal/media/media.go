// Package media decodes the images Paula is sent, turns them upright, scales
// them down and keeps them as files next to the database.
package media

//go:generate go run gen.go

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/image/draw"
	"golang.org/x/image/math/f64"
	_ "golang.org/x/image/webp"
)

// MIMEJPEG is what every stored image is kept as, whatever it arrived as, so a
// prompt never carries a second type.
const MIMEJPEG = "image/jpeg"

// jpegQuality is high enough that what a reader sees is the scaling, not the
// encoder.
const jpegQuality = 85

// Dir holds the images inside the data directory.
const Dir = "media"

// maxPixels is the largest image decoded, which the header is held against
// before anything is: decoding costs what the header declares, not what arrived.
const maxPixels = 64 << 20

var types = []string{"image/jpeg", "image/png", "image/gif", "image/webp"}

// Types are the image types Paula decodes.
func Types() []string { return slices.Clone(types) }

// Supported reports whether Paula decodes that type.
func Supported(mime string) bool { return slices.Contains(types, mime) }

// Detect reports the type of the bytes, and is empty when Paula does not
// decode it.
func Detect(b []byte) string {
	mime, _, _ := strings.Cut(http.DetectContentType(b), ";")
	if !Supported(mime) {
		return ""
	}
	return mime
}

// Files keeps the stored images.
type Files struct {
	dir   string
	maxPx int
}

// New opens the images of a data directory. maxPx is the longest side a stored
// image may have, and 0 keeps the size.
func New(dataDir string, maxPx int) *Files {
	return &Files{dir: filepath.Join(dataDir, Dir), maxPx: maxPx}
}

// Store turns an image upright, scales it down, keeps it as JPEG and returns
// the sha256 of what it kept. Everything kept is MIMEJPEG, so nothing says so
// a second time.
func (f *Files) Store(b []byte) (string, error) {
	out, err := Process(b, f.maxPx)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(out)
	sha := hex.EncodeToString(sum[:])
	path := f.Path(sha)
	if _, err := os.Stat(path); err == nil {
		return sha, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	// The file is written under a name of its own, so two images kept at once
	// cannot write over each other.
	tmp, err := os.CreateTemp(filepath.Dir(path), "tmp-")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}
	return sha, nil
}

// Load reads a stored image, which is a JPEG like every other.
func (f *Files) Load(sha string) ([]byte, error) {
	return os.ReadFile(f.Path(sha))
}

// Path is where a stored image lives.
func (f *Files) Path(sha string) string {
	if len(sha) < 2 {
		return filepath.Join(f.dir, sha)
	}
	return filepath.Join(f.dir, sha[:2], sha+".jpg")
}

// Process decodes an image, turns it upright, scales its longest side down to
// maxPx and encodes it as JPEG. maxPx 0 keeps the size.
func Process(b []byte, maxPx int) ([]byte, error) {
	mime := Detect(b)
	if mime == "" {
		return nil, fmt.Errorf("the image is not one of %s", strings.Join(types, ", "))
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	if px := int64(cfg.Width) * int64(cfg.Height); px > maxPixels {
		return nil, fmt.Errorf("the image is %d by %d, which is more than the %d pixels one may have",
			cfg.Width, cfg.Height, int64(maxPixels))
	}
	img, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	// Scaling first, since turning upright copies the image pixel by pixel and
	// the longest side is the same either way.
	img = scale(img, maxPx)
	img = orient(img, orientation(b))

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func scale(img image.Image, maxPx int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if maxPx <= 0 || (w <= maxPx && h <= maxPx) {
		return img
	}
	if w >= h {
		h = max(1, h*maxPx/w)
		w = maxPx
	} else {
		w = max(1, w*maxPx/h)
		h = maxPx
	}
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(out, out.Bounds(), img, b, draw.Src, nil)
	return out
}

// orient turns an image upright, which is one flip, one quarter turn, or both.
// It is a single transform: a pixel of the answer comes from one pixel of the
// image, so nothing is resampled and nothing is copied pixel by pixel here.
func orient(img image.Image, o int) image.Image {
	if o <= 1 || o > 8 {
		return img
	}
	b := img.Bounds()
	w, h := float64(b.Dx()), float64(b.Dy())

	// Where the image goes, as x' = a*x + b*y + c and y' = d*x + e*y + f. The
	// quarter turns, orientations 5 to 8, swap the sides.
	var m f64.Aff3
	size := image.Rect(0, 0, b.Dx(), b.Dy())
	switch o {
	case 2: // flipped left to right
		m = f64.Aff3{-1, 0, w, 0, 1, 0}
	case 3: // turned halfway round
		m = f64.Aff3{-1, 0, w, 0, -1, h}
	case 4: // flipped top to bottom
		m = f64.Aff3{1, 0, 0, 0, -1, h}
	case 5: // flipped over the diagonal
		m = f64.Aff3{0, 1, 0, 1, 0, 0}
	case 6: // a quarter turn clockwise
		m = f64.Aff3{0, -1, h, 1, 0, 0}
	case 7: // flipped over the other diagonal
		m = f64.Aff3{0, -1, h, -1, 0, w}
	case 8: // a quarter turn the other way
		m = f64.Aff3{0, 1, 0, -1, 0, w}
	}
	if o >= 5 {
		size = image.Rect(0, 0, b.Dy(), b.Dx())
	}
	// The matrix is written for an image whose corner is the origin, so one that
	// begins elsewhere is moved there first.
	m[2] -= m[0]*float64(b.Min.X) + m[1]*float64(b.Min.Y)
	m[5] -= m[3]*float64(b.Min.X) + m[4]*float64(b.Min.Y)

	out := image.NewRGBA(size)
	draw.NearestNeighbor.Transform(out, m, img, b, draw.Src, nil)
	return out
}
