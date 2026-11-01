package read

import (
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// exifJPEG encodes img as a JPEG with an APP1 Exif segment whose IFD0 holds
// Orientation, in big- or little-endian TIFF.
func exifJPEG(t *testing.T, img image.Image, orientation uint16, order binary.ByteOrder) []byte {
	t.Helper()
	var j bytes.Buffer
	require.NoError(t, jpeg.Encode(&j, img, &jpeg.Options{Quality: 90}))
	tiff := make([]byte, 26)
	copy(tiff, "MM")
	if order == binary.LittleEndian {
		copy(tiff, "II")
	}
	order.PutUint16(tiff[2:], 42)
	order.PutUint32(tiff[4:], 8) // IFD0
	order.PutUint16(tiff[8:], 1) // one entry
	order.PutUint16(tiff[10:], 0x0112)
	order.PutUint16(tiff[12:], 3) // SHORT
	order.PutUint32(tiff[14:], 1)
	order.PutUint16(tiff[18:], orientation)
	app1 := append([]byte("Exif\x00\x00"), tiff...)
	seg := []byte{0xff, 0xe1, byte((len(app1) + 2) >> 8), byte(len(app1) + 2)}
	b := j.Bytes()
	return append(append(append(append([]byte{}, b[:2]...), seg...), app1...), b[2:]...)
}

// quadrants is an image whose quarters are red, green, blue and white.
func quadrants(w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	quarter := [2][2]color.NRGBA{{{255, 0, 0, 255}, {0, 255, 0, 255}}, {{0, 0, 255, 255}, {255, 255, 255, 255}}}
	for y := range h {
		for x := range w {
			img.SetNRGBA(x, y, quarter[2*y/h][2*x/w])
		}
	}
	return img
}

// orientedQuarter is the stored quarter (x, y) that lands at quarter (x, y)
// of the image jimp rotates by Orientation (Zdr).
func orientedQuarter(o, x, y int) (int, int) {
	switch o {
	case 2:
		return 1 - x, y
	case 3:
		return 1 - x, 1 - y
	case 4:
		return x, 1 - y
	case 5:
		return y, x
	case 6:
		return y, 1 - x
	case 7:
		return 1 - y, 1 - x
	case 8:
		return 1 - y, x
	}
	return x, y
}

func nearColor(a, b color.Color) bool {
	r1, g1, b1, _ := a.RGBA()
	r2, g2, b2, _ := b.RGBA()
	d := func(p, q uint32) bool { return max(p, q)-min(p, q) < 40<<8 }
	return d(r1, r2) && d(g1, g2) && d(b1, b2)
}

// A JPEG that is scaled down is first turned by its EXIF Orientation, as
// jimp does on the desktop (Qdr), in both TIFF byte orders (F2-1).
func TestImageExifOrientation(t *testing.T) {
	stored := quadrants(1100, 550)
	for o := range 9 {
		orders := []binary.ByteOrder{binary.BigEndian}
		if o == 6 || o == 8 {
			orders = append(orders, binary.LittleEndian)
		}
		for _, order := range orders {
			out, err := prepareImage(context.Background(), exifJPEG(t, stored, uint16(o), order)) //nolint:gosec // 0–8
			require.NoError(t, err)
			img, format, err := image.Decode(bytes.NewReader(out))
			require.NoError(t, err)
			assert.Equal(t, "jpeg", format)
			want := image.Pt(1024, 512)
			if o > 4 {
				want = image.Pt(512, 1024)
			}
			require.Equal(t, want, img.Bounds().Size(), "orientation %d", o)
			for y := range 2 {
				for x := range 2 {
					sx, sy := orientedQuarter(o, x, y)
					got := img.At(want.X/4+x*want.X/2, want.Y/4+y*want.Y/2)
					want := stored.At(275+sx*550, 137+sy*275)
					assert.True(t, nearColor(want, got), "orientation %d %v: quarter %d,%d is %v, want %v", o, order, x, y, got, want)
				}
			}
		}
	}

	out, err := prepareImage(context.Background(), exifJPEG(t, quadrants(2000, 1000), 6, binary.BigEndian))
	require.NoError(t, err)
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(out))
	require.NoError(t, err)
	assert.Equal(t, image.Pt(512, 1024), image.Pt(cfg.Width, cfg.Height))

	small := exifJPEG(t, quadrants(200, 100), 6, binary.BigEndian)
	out, err = prepareImage(context.Background(), small)
	require.NoError(t, err)
	assert.Equal(t, small, out, "a JPEG passed on as it is keeps its EXIF")
}
