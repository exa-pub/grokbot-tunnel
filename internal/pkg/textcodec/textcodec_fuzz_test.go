package textcodec_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/exa-pub/grokbot-tunnel/internal/pkg/textcodec"
)

// Decoded text is valid UTF-8; valid UTF-8 without a hint decodes as it is,
// and a stream decoder gives the same text whatever the chunk size.
func FuzzDecode(f *testing.F) {
	for _, seed := range []string{"", "plain\n", "\xef\xbb\xbfbom", "\xff\xfeh\x00i\x00", "caf\xe9", "a\x00b", "\xe2\x82"} {
		f.Add([]byte(seed), "", 3)
	}
	f.Add([]byte("h\x00i\x00"), "utf-16le", 1)
	f.Add([]byte("caf\xe9"), "latin1", 2)
	f.Fuzz(func(t *testing.T, b []byte, hint string, chunk int) {
		var h *string
		if hint != "" {
			h = &hint
		}
		text, err := textcodec.Decode(b, h)
		if err != nil {
			return
		}
		if !utf8.ValidString(text) {
			t.Fatalf("invalid UTF-8 from %q", b)
		}
		if format := textcodec.Detect(b); h == nil && format.Encoding == textcodec.EncodingUTF8 && utf8.Valid(b) &&
			text != strings.TrimPrefix(string(b), "\xef\xbb\xbf") {
			t.Fatalf("%q decoded as %q", b, text)
		}
		chunk = max(1, chunk%16+1)
		d := textcodec.Resolve(h, textcodec.Detect(b).Encoding).NewDecoder()
		var streamed []byte
		for i := 0; ; i += chunk {
			out, err := d.Decode(b[min(i, len(b)):min(i+chunk, len(b))], i+chunk >= len(b))
			if err != nil {
				return // strict UTF-8 that the reader decodes again
			}
			streamed = append(streamed, out...)
			if i+chunk >= len(b) {
				break
			}
		}
		if got := strings.TrimPrefix(string(streamed), "\xef\xbb\xbf"); got != text {
			t.Fatalf("%q in chunks of %d: %q, whole %q", b, chunk, got, text)
		}
	})
}

// The cutting helpers never split a character and never exceed their limit.
func FuzzCuts(f *testing.F) {
	f.Add([]byte("héllo 😀 wörld"), 3)
	f.Add([]byte("\xf0\x9f\x98"), 1)
	f.Fuzz(func(t *testing.T, b []byte, n int) {
		n = max(n%64, 0)
		if c := textcodec.CompleteLen(b); c > len(b) || c < len(b)-3 || utf8.Valid(b) && c != len(b) {
			t.Fatalf("CompleteLen(%q) = %d", b, c)
		}
		if n > 0 && len(b) > 0 {
			if c := textcodec.ChunkLen(b, n); c <= 0 || c > len(b) {
				t.Fatalf("ChunkLen(%q, %d) = %d", b, n, c)
			}
		}
		s := textcodec.LossyUTF8(b)
		if !utf8.ValidString(s) {
			t.Fatalf("LossyUTF8(%q) = %q", b, s)
		}
		if u := textcodec.TruncateUTF16(s, n); textcodec.UTF16Len(u) > n || !utf8.ValidString(u) || s[:len(u)] != u {
			t.Fatalf("TruncateUTF16(%q, %d) = %q", s, n, u)
		}
		if u := textcodec.TruncateUTF8(s, n); len(u) > n || !utf8.ValidString(u) || s[:len(u)] != u {
			t.Fatalf("TruncateUTF8(%q, %d) = %q", s, n, u)
		}
	})
}
