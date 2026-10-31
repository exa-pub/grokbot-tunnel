package textcodec_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/exa-pub/grokbot-tunnel/internal/pkg/textcodec"
)

func TestDecode(t *testing.T) {
	for _, tc := range []struct {
		name, hint string
		in         []byte
		want       string
		base       int
	}{
		{"utf-8", "", []byte("héllo"), "héllo", 0},
		{"utf-8 bom", "", []byte("\xef\xbb\xbfhi"), "hi", 3},
		{"utf-16le bom", "", []byte{0xff, 0xfe, 'h', 0, 'i', 0}, "hi", -1},
		{"utf-16be bom", "", []byte{0xfe, 0xff, 0, 'h', 0, 'i'}, "hi", -1},
		{"latin1 hint", "latin1", []byte{'c', 0xe9}, "cé", -1},
		{"windows-1252 hint", "windows-1252", []byte{0x80}, "€", -1},
		{"iana hint", "koi8-r", []byte{0xc1}, "а", -1},
		{"latin1 fallback", "", []byte{'c', 0xe9, 'z'}, "céz", -1},
		{"lossy fallback", "", []byte("привет\xff"), "привет�", -1},
	} {
		got, base, err := textcodec.Decode(tc.in, tc.hint)
		require.NoError(t, err, tc.name)
		assert.Equal(t, tc.want, got, tc.name)
		assert.Equal(t, tc.base, base, tc.name)
	}
	_, _, err := textcodec.Decode([]byte("a\x00b"), "")
	require.ErrorIs(t, err, textcodec.ErrBinary)
}

// A long invalid text is classified by its first MiB, cut at a rune
// boundary.
func TestDecodeSniffsFirstMiB(t *testing.T) {
	head := strings.Repeat("ж", 1<<19) // exactly 1 MiB of valid two-byte runes
	got, _, err := textcodec.Decode([]byte(head+"\xff"+strings.Repeat("\xe9", 1<<20)), "")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(got, head+"�"))
}

func TestUnits(t *testing.T) {
	assert.Equal(t, 4, textcodec.UTF16Len("a😀ж"))
	assert.Equal(t, "a", textcodec.TruncateUTF16("a😀ж", 2))
	assert.Equal(t, "a😀", textcodec.TruncateUTF16("a😀ж", 3))
	assert.Equal(t, "a😀ж", textcodec.TruncateUTF16("a😀ж", 10))
	assert.Equal(t, "a", textcodec.TruncateUTF8("aж", 2))
	assert.Equal(t, "aж", textcodec.TruncateUTF8("aж", 3))
	assert.Equal(t, 1, textcodec.CompleteLen([]byte("a\xd0")))
	assert.Equal(t, 3, textcodec.CompleteLen([]byte("aж")))
	assert.Equal(t, 1, textcodec.ChunkLen([]byte("aжж"), 2))
	assert.Equal(t, 3, textcodec.ChunkLen([]byte("\x80\x80\x80\x80"), 3), "no rune start: cut at size")
	assert.Equal(t, 2, textcodec.ChunkLen([]byte("ab"), 5))
}
