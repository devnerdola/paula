`a photo.jpg` is a copy of `internal/media/testdata/red-blue-8x4.jpg`, which
`go generate ./internal/media` writes. Its name carries a space, since a
terminal has to quote a path for `/image` to read it whole.
