package read

import (
	"encoding/json"
	"os"
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
