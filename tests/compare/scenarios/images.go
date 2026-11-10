package scenarios

import (
	"embed"
)

// The image fixtures are files, not pictures drawn when the scenarios are
// built: what the standard library's PNG and JPEG encoders write changes
// between Go releases, and a scenario whose files change is another
// scenario than the one that was recorded.
//
// All but the WebP show the same gradient, red growing to the right and
// green downward: small.png, small.jpg and small.gif at 64x48, tiny.png at
// 8x8, large.png at 3000x2000 and big.jpg, of quality 90, at 4000x3000.
// pixel.webp is a lossless WebP of one pixel.
//
//go:embed testdata
var images embed.FS

// image returns the fixture testdata/name.
func image(name string) []byte {
	b, err := images.ReadFile("testdata/" + name)
	if err != nil {
		panic(err)
	}
	return b
}

// noise is n bytes that are neither text nor an image, the same on every
// run.
func noise(n int) []byte {
	out := make([]byte, n)
	x := uint32(2463534242)
	for i := range out {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		out[i] = byte(x) //nolint:gosec // the low byte
	}
	return out
}
