// Package textcodec decodes file and process output to text the way the
// desktop client does, and measures and cuts text in the units JavaScript
// uses.
package textcodec

import (
	"errors"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

// ErrBinary is returned for data that Detect finds binary.
var ErrBinary = errors.New("file is binary")

// Decode decodes a whole file as the desktop's streaming reader does: binary
// data is refused before the encoding hint applies; without a hint the
// encoding is detected; invalid UTF-8 is decoded again as lossy UTF-8 or
// Latin-1 (Fallback). A leading U+FEFF is removed.
func Decode(b []byte, hint *string) (string, error) {
	format := Detect(b)
	if format.Binary {
		return "", ErrBinary
	}
	text, err := decodeAll(Resolve(hint, format.Encoding), b)
	if errors.Is(err, ErrInvalidUTF8) {
		text, err = decodeAll(Fallback(b[:min(len(b), FallbackSniff)], int64(len(b))), b)
	}
	return strings.TrimPrefix(text, "\uFEFF"), err
}

func decodeAll(d Decoding, b []byte) (string, error) {
	out, err := d.NewDecoder().Decode(b, true)
	return string(out), err
}

// LossyUTF8 decodes b, replacing each maximal invalid subsequence with
// U+FFFD as a WHATWG decoder does.
func LossyUTF8(b []byte) string {
	out, _, err := transform.Bytes(unicode.UTF8.NewDecoder(), b)
	if err != nil {
		return strings.ToValidUTF8(string(b), "\uFFFD") // not expected: the decoder replaces
	}
	return string(out)
}

// MostlyUTF8 reports whether text decoded from invalid UTF-8 has more
// characters beyond ASCII than replacement characters, as the desktop
// decides: a run of them loses its last two to a replacement character that
// follows, and the two after one do not count.
func MostlyUTF8(text string) bool {
	const skip = 2
	replaced, valid, run, skipped := 0, 0, 0, 0
	for _, r := range text {
		for range utf16.RuneLen(r) {
			switch {
			case r < utf8.RuneSelf:
				valid, run, skipped = valid+run, 0, 0
			case r == utf8.RuneError:
				replaced++
				valid, run, skipped = valid+max(0, run-skip), 0, skip
			case skipped > 0:
				skipped--
			default:
				run++
			}
		}
	}
	return valid+run > replaced
}
