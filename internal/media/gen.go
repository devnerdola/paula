//go:build ignore

// Command gen writes the images the media tests read, under testdata. It runs
// through `go generate ./internal/media`, by hand, when a fixture is added or
// changed; the tests only read what it left. See testdata/SOURCES.md.
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
)

var (
	red  = color.RGBA{255, 0, 0, 255}
	blue = color.RGBA{0, 0, 255, 255}

	topLeft     = color.RGBA{255, 0, 0, 255}
	topRight    = color.RGBA{0, 255, 0, 255}
	bottomLeft  = color.RGBA{0, 0, 255, 255}
	bottomRight = color.RGBA{255, 255, 0, 255}
)

// redBlue is red on its left half and blue on its right half.
func redBlue(w, h int) *image.RGBA {
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			c := red
			if x >= w/2 {
				c = blue
			}
			m.Set(x, y, c)
		}
	}
	return m
}

// quadrants has a colour of its own in each quadrant, so a flip and a rotation
// cannot be mistaken for each other.
func quadrants() *image.RGBA {
	m := image.NewRGBA(image.Rect(0, 0, 32, 16))
	for y := range 16 {
		for x := range 32 {
			c := topLeft
			switch {
			case x >= 16 && y >= 8:
				c = bottomRight
			case x >= 16:
				c = topRight
			case y >= 8:
				c = bottomLeft
			}
			m.Set(x, y, c)
		}
	}
	return m
}

func writePNG(path string, m image.Image) {
	f, err := os.Create(path)
	check(err)
	check(png.Encode(f, m))
	check(f.Close())
}

func run(name string, args ...string) {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		panic(fmt.Sprintf("%s %v: %v\n%s", name, args, err, out))
	}
}

// sips converts a PNG to another format, at the best quality it offers.
func sips(src, dst, format string) {
	run("sips", "-s", "format", format, "-s", "formatOptions", "best", src, "--out", dst)
}

func orientation(path string, o int) {
	run("exiftool", "-q", "-overwrite_original", "-n", "-m",
		fmt.Sprintf("-Orientation=%d", o), path)
}

func check(err error) {
	if err != nil {
		panic(err)
	}
}

// pngHeader is a PNG that says how big it is and carries nothing else, which is
// how an image that would take the memory of the whole machine arrives.
func pngHeader(w, h uint32) []byte {
	ihdr := []byte("IHDR")
	ihdr = binary.BigEndian.AppendUint32(ihdr, w)
	ihdr = binary.BigEndian.AppendUint32(ihdr, h)
	ihdr = append(ihdr, 8, 2, 0, 0, 0) // eight bits per sample, truecolour

	out := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	out = binary.BigEndian.AppendUint32(out, uint32(len(ihdr)-4))
	out = append(out, ihdr...)
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(ihdr))
}

func gifHeader(w, h uint16) []byte {
	b := []byte("GIF89a")
	b = binary.LittleEndian.AppendUint16(b, w)
	b = binary.LittleEndian.AppendUint16(b, h)
	return append(b, 0x00, 0x00, 0x00)
}

func webpHeader(w, h uint16) []byte {
	frame := []byte{0x50, 0x00, 0x00, 0x9d, 0x01, 0x2a}
	frame = binary.LittleEndian.AppendUint16(frame, w)
	frame = binary.LittleEndian.AppendUint16(frame, h)

	b := []byte("RIFF")
	b = binary.LittleEndian.AppendUint32(b, uint32(4+8+len(frame)))
	b = append(b, "WEBP"...)
	b = append(b, "VP8 "...)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(frame)))
	return append(b, frame...)
}

// asLong rewrites the type of the orientation tag from the short the standard
// names to a long, which no encoder writes: a reader that took the value on
// trust would read the two bytes it finds there as an orientation.
func asLong(path string) {
	b, err := os.ReadFile(path)
	check(err)
	i := bytes.Index(b, []byte("Exif\x00\x00"))
	if i < 0 {
		panic(path + " carries no Exif segment")
	}
	tiff := b[i+6:]
	var order binary.ByteOrder = binary.BigEndian
	if string(tiff[:2]) == "II" {
		order = binary.LittleEndian
	}
	off := int(order.Uint32(tiff[4:]))
	for n := range int(order.Uint16(tiff[off:])) {
		e := off + 2 + n*12
		if order.Uint16(tiff[e:]) != 0x0112 {
			continue
		}
		order.PutUint16(tiff[e+2:], 4) // long
		check(os.WriteFile(path, b, 0o644))
		return
	}
	panic(path + " carries no orientation")
}

// padMarker puts a fill byte before the segment after the start of the image,
// which the JPEG standard allows.
func padMarker(path string) {
	b, err := os.ReadFile(path)
	check(err)
	out := append([]byte{}, b[:2]...)
	out = append(out, 0xff)
	out = append(out, b[2:]...)
	check(os.WriteFile(path, out, 0o644))
}

func main() {
	check(os.Chdir("testdata"))

	for _, size := range [][2]int{{8, 4}, {64, 32}, {80, 40}, {400, 800}, {500, 500}, {800, 400}, {1000, 3}} {
		writePNG(fmt.Sprintf("red-blue-%dx%d.png", size[0], size[1]), redBlue(size[0], size[1]))
	}
	sips("red-blue-8x4.png", "red-blue-8x4.jpg", "jpeg")
	sips("red-blue-8x4.png", "red-blue-8x4.gif", "gif")
	run("cwebp", "-lossless", "-quiet", "red-blue-8x4.png", "-o", "red-blue-8x4.webp")
	sips("red-blue-64x32.png", "red-blue-64x32.jpg", "jpeg")
	check(os.Remove("red-blue-64x32.png"))

	// An orientation outside the eight the standard names.
	sips("red-blue-8x4.png", "red-blue-8x4-orientation-9.jpg", "jpeg")
	orientation("red-blue-8x4-orientation-9.jpg", 9)

	// An Exif segment preceded by a fill byte.
	sips("red-blue-8x4.png", "red-blue-8x4-orientation-6-padded.jpg", "jpeg")
	orientation("red-blue-8x4-orientation-6-padded.jpg", 6)
	padMarker("red-blue-8x4-orientation-6-padded.jpg")

	// An orientation tag written as something the standard does not name.
	sips("red-blue-8x4.png", "red-blue-8x4-orientation-long.jpg", "jpeg")
	orientation("red-blue-8x4-orientation-long.jpg", 6)
	asLong("red-blue-8x4-orientation-long.jpg")

	writePNG("quadrants-32x16.png", quadrants())
	sips("quadrants-32x16.png", "quadrants-32x16.jpg", "jpeg")
	for o := 1; o <= 8; o++ {
		name := fmt.Sprintf("quadrants-32x16-orientation-%d.jpg", o)
		sips("quadrants-32x16.png", name, "jpeg")
		orientation(name, o)
	}
	check(os.Remove("quadrants-32x16.png"))

	check(os.WriteFile("huge-40000x40000.png", pngHeader(40000, 40000), 0o644))
	check(os.WriteFile("huge-32000x32000.gif", gifHeader(32000, 32000), 0o644))
	check(os.WriteFile("huge-16383x16383.webp", webpHeader(16383, 16383), 0o644))
	check(os.WriteFile("not-an-image.txt", []byte("hello there, this is not an image\n"), 0o644))
}
