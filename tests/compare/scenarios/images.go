package scenarios

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
)

// gradient is the picture every image fixture shows.
func gradient(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x * 255 / w), G: uint8(y * 255 / h), B: 128, A: 255}) //nolint:gosec // below 256
		}
	}
	return img
}

func pngImage(w, h int) []byte {
	var b bytes.Buffer
	if err := png.Encode(&b, gradient(w, h)); err != nil {
		panic(err)
	}
	return b.Bytes()
}

func jpegImage(w, h int) []byte {
	var b bytes.Buffer
	if err := jpeg.Encode(&b, gradient(w, h), &jpeg.Options{Quality: 90}); err != nil {
		panic(err)
	}
	return b.Bytes()
}

func gifImage(w, h int) []byte {
	var b bytes.Buffer
	if err := gif.Encode(&b, gradient(w, h), nil); err != nil {
		panic(err)
	}
	return b.Bytes()
}

// webpImage is a lossless WebP of one pixel: the standard library has no
// WebP encoder.
func webpImage() []byte {
	b, err := base64.StdEncoding.DecodeString("UklGRhoAAABXRUJQVlA4TA0AAAAvAAAAEAcQERGIiP4HAA==")
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
