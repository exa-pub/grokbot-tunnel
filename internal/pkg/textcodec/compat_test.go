package textcodec_test

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/exa-pub/grokbot-tunnel/internal/pkg/compattest"
	"github.com/exa-pub/grokbot-tunnel/internal/pkg/textcodec"
)

// Invalid UTF-8 is decoded as lossy UTF-8 or Latin-1 exactly as the desktop
// decides (testdata/compat, "fallback").
func TestReferenceFallback(t *testing.T) {
	ref := compattest.Load(t)
	require.NotEmpty(t, ref.Fallback)
	for _, tc := range ref.Fallback {
		raw, err := hex.DecodeString(tc.Hex)
		require.NoError(t, err)
		lossy := textcodec.LossyUTF8(raw)
		assert.Equal(t, tc.Lossy, lossy, tc.Hex)
		assert.Equal(t, tc.MostlyUtf8, textcodec.MostlyUTF8(lossy), tc.Hex)
		want := tc.Lossy
		if !tc.MostlyUtf8 {
			out := make([]rune, len(raw))
			for i, c := range raw {
				out[i] = rune(c) // Latin-1
			}
			want = string(out)
		}
		got, _, err := textcodec.Decode(raw, "")
		require.NoError(t, err)
		assert.Equal(t, want, got, tc.Hex)
	}
}
