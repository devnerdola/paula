Every file here is written by `go generate ./internal/media`, by hand, and is
read as it is: nothing writes here while the tests run. Generating again gives
back the same bytes, so a fixture that changed is a change somebody made.

| File | What it is | Written by |
|---|---|---|
| `red-blue-WxH.png` | red on the left half, blue on the right | Go's `image/png`, in `../gen.go` |
| `red-blue-8x4.jpg`, `red-blue-64x32.jpg` | the same image as a JPEG | `sips -s format jpeg -s formatOptions best` |
| `red-blue-8x4.gif` | the same image as a GIF | `sips -s format gif` |
| `red-blue-8x4.webp` | the same image as a WebP | `cwebp -lossless` |
| `quadrants-32x16.jpg` | 32x16, a colour of its own in each quadrant | Go's `image/png`, then `sips` |
| `quadrants-32x16-orientation-N.jpg` | the same, carrying each of the eight orientations | `exiftool -n -Orientation=N` |
| `red-blue-8x4-orientation-9.jpg` | an orientation outside the eight the standard names | `exiftool -n -m -Orientation=9` |
| `red-blue-8x4-orientation-6-padded.jpg` | orientation 6, behind a `0xff` fill byte | `exiftool -n -Orientation=6`, then `../gen.go` |
| `red-blue-8x4-orientation-long.jpg` | the orientation tag written as a long, which the standard does not name and no encoder writes | `exiftool -n -Orientation=6`, then `../gen.go` rewrites the type |
| `huge-40000x40000.png`, `huge-32000x32000.gif`, `huge-16383x16383.webp` | headers that declare a size nothing could hold, and carry nothing else | `../gen.go`, byte by byte |
| `not-an-image.txt` | one line of text | `../gen.go` |

The orientations read back through `exiftool -n -Orientation`, and the sizes
through `file(1)`, which is what says these are what they claim rather than
only what Paula makes of them.
