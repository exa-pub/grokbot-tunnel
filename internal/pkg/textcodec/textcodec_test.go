package textcodec_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/exa-pub/grokbot-tunnel/internal/pkg/textcodec"
)

func hint(s string) *string { return &s }

func TestDecode(t *testing.T) {
	for _, tc := range []struct {
		name string
		hint *string
		in   []byte
		want string
	}{
		{"utf-8", nil, []byte("héllo"), "héllo"},
		{"utf-8 bom", nil, []byte("\xef\xbb\xbfhi"), "hi"},
		{"utf-16le bom", nil, []byte{0xff, 0xfe, 'h', 0, 'i', 0}, "hi"},
		{"utf-16be bom", nil, []byte{0xfe, 0xff, 0, 'h', 0, 'i'}, "hi"},
		{"utf-16le without bom", nil, []byte("h\x00e\x00l\x00l\x00o\x00"), "hello"},
		{"utf-16be without bom", nil, []byte("\x00h\x00e\x00l\x00l\x00o"), "hello"},
		{"utf-32le bom", nil, []byte("\xff\xfe\x00\x00h\x00\x00\x00i\x00\x00\x00"), "hi"},
		{"utf-32be without bom", nil, []byte("\x00\x00\x00h\x00\x00\x00i"), "hi"},
		{"latin1 hint", hint("latin1"), []byte{'c', 0xe9}, "cé"},
		{"windows-1252 hint", hint("windows-1252"), []byte{0x80}, "€"},
		{"iana hint", hint("koi8-r"), []byte{0xc1}, "а"},
		{"utf-16le hint", hint("UTF-16LE"), []byte("h\x00i\x00"), "hi"},
		{"unknown hint", hint("no-such-encoding"), []byte("plain"), "plain"},
		{"utf-8 hint ignores a utf-16 bom", hint("utf-8"), []byte{0xff, 0xfe, 'h', 'i'}, "ÿþhi"},
		{"latin1 fallback", nil, []byte{'c', 0xe9, 'z'}, "céz"},
		{"latin1 fallback keeps a utf-8 bom", nil, []byte{0xef, 0xbb, 0xbf, 0xff, '\n', 'a'}, "ï»¿ÿ\na"},
		{"lossy fallback", nil, []byte("привет\xff"), "привет\xef\xbf\xbd"},
		{"nul after the sniffed start", nil, []byte(strings.Repeat("a", 9000) + "\x00"), strings.Repeat("a", 9000) + "\x00"},
	} {
		got, err := textcodec.Decode(tc.in, tc.hint)
		require.NoError(t, err, tc.name)
		assert.Equal(t, tc.want, got, tc.name)
	}
	for _, tc := range []struct {
		name string
		hint *string
		in   string
	}{
		{"nul", nil, "a\x00b"},
		{"nul with a hint", hint("latin1"), "ab\x00cd"},
		{"escape sequences", nil, "\x1b[31mred\x1b[0m\n"},
	} {
		_, err := textcodec.Decode([]byte(tc.in), tc.hint)
		require.ErrorIs(t, err, textcodec.ErrBinary, tc.name)
	}
}

// Detect finds images by their signature in binary data and keeps a
// character that the cut at 8 KiB leaves incomplete out of its count.
func TestDetect(t *testing.T) {
	assert.True(t, textcodec.Detect([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")).Image)
	assert.True(t, textcodec.Detect([]byte("GIF89a\x01\x00\x01\x00\x00")).Image)
	assert.False(t, textcodec.Detect([]byte("\x89PNG\r\n\x1a\nno zero bytes here")).Binary, "a signature in text")
	assert.False(t, textcodec.Detect([]byte(strings.Repeat("ж", textcodec.SniffLen/2-1)+"a\xd0")).Binary)
}

// A long invalid text is classified by its first MiB, cut at a rune
// boundary.
func TestDecodeSniffsFirstMiB(t *testing.T) {
	head := strings.Repeat("ж", 1<<19) // exactly 1 MiB of valid two-byte runes
	got, err := textcodec.Decode([]byte(head+"\xff"+strings.Repeat("\xe9", 1<<20)), nil)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(got, head+"\xef\xbf\xbd"))
}

// A Decoder completes a character cut by a chunk boundary with the next
// chunk; a strict one fails on invalid UTF-8 and on a cut character at the
// end.
func TestDecoderChunks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		d      textcodec.Decoding
		chunks []string
		want   []string
	}{
		{"strict", textcodec.Resolve(nil, textcodec.EncodingUTF8), []string{"a\xd0", "\xb6b", ""}, []string{"a", "жb", ""}},
		{"lossy", textcodec.Lossy, []string{"a\xd0", "\xb6\xff", ""}, []string{"a", "ж\xef\xbf\xbd", ""}},
		{"latin1", textcodec.Latin1, []string{"\xe9", "z"}, []string{"é", "z"}},
		{"utf-16le", textcodec.Resolve(nil, textcodec.EncodingUTF16LE), []string{"h\x00i", "\x00"}, []string{"h", "i"}},
	} {
		dec := tc.d.NewDecoder()
		for i, chunk := range tc.chunks {
			out, err := dec.Decode([]byte(chunk), i == len(tc.chunks)-1)
			require.NoError(t, err, tc.name)
			assert.Equal(t, tc.want[i], string(out), tc.name)
		}
	}
	strict := textcodec.Resolve(nil, textcodec.EncodingUTF8)
	_, err := strict.NewDecoder().Decode([]byte("a\xffb"), false)
	require.ErrorIs(t, err, textcodec.ErrInvalidUTF8)
	_, err = strict.NewDecoder().Decode([]byte("a\xd0"), true)
	require.ErrorIs(t, err, textcodec.ErrInvalidUTF8)
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
