package read

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/gif"
	"os"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/exa-pub/grokbot-tunnel/internal/pkg/compattest"
)

// Images are scaled to the size the desktop's zct computes
// (testdata/compat, "imageFit").
func TestReferenceImageFit(t *testing.T) {
	b, err := os.ReadFile(compattest.Path("pure-functions.json"))
	require.NoError(t, err)
	var ref struct {
		ImageFit []struct {
			Width, Height, Side int
			Result              struct {
				Width, Height int
				NeedsResize   bool
			}
		}
	}
	require.NoError(t, json.Unmarshal(b, &ref))
	require.NotEmpty(t, ref.ImageFit)
	for _, tc := range ref.ImageFit {
		w, h, resize := fitImage(tc.Width, tc.Height, tc.Side)
		assert.Equal(t, tc.Result.Width, w, "%+v", tc)
		assert.Equal(t, tc.Result.Height, h, "%+v", tc)
		assert.Equal(t, tc.Result.NeedsResize, resize, "%+v", tc)
	}
}

// A PNG whose compressed pixel data ends early is reported as the desktop's
// check words it.
func TestPNGPixelDataCheck(t *testing.T) {
	ihdr := []byte{0, 0, 0, 2, 0, 0, 0, 2, 8, 2, 0, 0, 0} // 2×2 RGB
	assert.Equal(t, int64(2*(1+6)), pngPixelBytes(ihdr))
	ihdr[12] = 1 // interlaced: passes 1, 6 and 7 hold pixels
	assert.Equal(t, int64((1+3)+(1+3)+2*(1+6)/2), pngPixelBytes(ihdr))
	ihdr[9] = 5
	assert.Equal(t, int64(-1), pngPixelBytes(ihdr), "unknown color type")
}

// The first frame of a GIF is scaled on its canvas, transparent around it,
// as jimp decodes it.
func TestImageGIFFrameOnCanvas(t *testing.T) {
	frame := image.NewPaletted(image.Rect(100, 50, 600, 300), color.Palette{color.Transparent, color.RGBA{255, 0, 0, 255}})
	for i := range frame.Pix {
		frame.Pix[i] = 1
	}
	var b bytes.Buffer
	require.NoError(t, gif.EncodeAll(&b, &gif.GIF{
		Image: []*image.Paletted{frame}, Delay: []int{0},
		Config: image.Config{ColorModel: frame.Palette, Width: 2000, Height: 1000},
	}))
	out, err := prepareImage(context.Background(), b.Bytes())
	require.NoError(t, err)
	img, err := gif.Decode(bytes.NewReader(out))
	require.NoError(t, err)
	assert.Equal(t, image.Pt(1024, 512), img.Bounds().Size())
	r, _, _, _ := img.At(10, 10).RGBA()
	assert.Zero(t, r, "around the frame: transparent, black in the Plan 9 palette")
	r, _, _, _ = img.At(150, 80).RGBA()
	assert.Equal(t, uint32(0xffff), r, "in the frame")
}

// An extended WEBP whose frame is not the size of its canvas is not
// decoded: it gets the desktop's error.
func TestImageWebPFrameSize(t *testing.T) {
	frame := []byte("VP8L\x0d\x00\x00\x00\x2f\x00\x00\x00\x10\x07\x10\x11\x11\x88\x88\xfe\x07\x00") // 1×1
	vp8x := []byte("VP8X\x0a\x00\x00\x00\x00\x00\x00\x00\xcf\x07\x00\xdb\x05\x00")                  // 2000×1500
	data := slices.Concat([]byte("RIFF\x00\x00\x00\x00WEBP"), vp8x, frame)
	_, err := prepareImage(context.Background(), data)
	require.EqualError(t, err, msgWebPCodec)
}
