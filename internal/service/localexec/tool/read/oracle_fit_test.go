//go:build compare

package read

import (
	"math/rand/v2"
	"testing"

	"github.com/exa-pub/grokbot-tunnel/internal/pkg/compattest"
)

// Images are scaled to the size the desktop computes, on random sizes.
func TestOracleImageFit(t *testing.T) {
	oracle := compattest.NewOracle(t)
	r := rand.New(rand.NewPCG(5, 6))
	// Sizes near the limits and far from them.
	size := func() int {
		switch r.IntN(4) {
		case 0:
			return r.IntN(20)
		case 1:
			return 1000 + r.IntN(300)
		default:
			return r.IntN(12000)
		}
	}
	for range *compattest.OracleRuns {
		width, height, side := size(), size(), []int{1024, 1280}[r.IntN(2)]
		var want struct {
			Width, Height int
			NeedsResize   bool
		}
		oracle.Call(&want, "imageFit", width, height, side)
		w, h, resize := fitImage(width, height, side)
		if w != want.Width || h != want.Height || resize != want.NeedsResize {
			t.Errorf("fitImage(%d, %d, %d) = %d, %d, %v; the desktop says %+v", width, height, side, w, h, resize, want)
		}
	}
}
