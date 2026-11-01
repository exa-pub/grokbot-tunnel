package textcodec_test

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/exa-pub/grokbot-tunnel/internal/pkg/compattest"
	"github.com/exa-pub/grokbot-tunnel/internal/pkg/textcodec"
)

// The start of a file is classified as the desktop's getFormatForFile does
// (testdata/compat, "format"), apart from its jschardet guesses, which the
// reference leaves out.
func TestReferenceFormat(t *testing.T) {
	b, err := os.ReadFile(compattest.Path("pure-functions.json"))
	require.NoError(t, err)
	var ref struct {
		Format []struct {
			Hex                       string
			Encoding                  string
			IsBinaryFile, IsImageFile bool
		}
	}
	require.NoError(t, json.Unmarshal(b, &ref))
	require.NotEmpty(t, ref.Format)
	for _, tc := range ref.Format {
		raw, err := hex.DecodeString(tc.Hex)
		require.NoError(t, err)
		got := textcodec.Detect(raw)
		want := tc.Encoding
		if strings.EqualFold(want, "utf8") {
			want = textcodec.EncodingUTF8
		}
		assert.Equal(t, textcodec.Format{Encoding: want, Binary: tc.IsBinaryFile, Image: tc.IsImageFile}, got, "%.80s", tc.Hex)
	}
}

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
		got, err := textcodec.Decode(raw, nil)
		require.NoError(t, err)
		assert.Equal(t, want, got, tc.Hex)
	}
}
