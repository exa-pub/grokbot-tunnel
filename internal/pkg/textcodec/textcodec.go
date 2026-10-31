// Package textcodec decodes file and process output to text the way the
// desktop client does, and measures and cuts text in the units JavaScript
// uses.
package textcodec

import (
	"bytes"
	"errors"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/ianaindex"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

// sniffSize is how much of a text that is not UTF-8 decides between lossy
// UTF-8 and Latin-1.
const sniffSize = 1 << 20

// ErrBinary is returned for data with a NUL byte, which is not text.
var ErrBinary = errors.New("file is binary")

// Decode decodes b using the encoding hint. Without a hint it honors a UTF-16
// BOM and strips a UTF-8 BOM; invalid UTF-8 is decoded as lossy UTF-8 or, if
// it does not look like UTF-8, as Latin-1. It also returns the offset in b
// from which the text holds b's bytes as they are, or -1.
func Decode(b []byte, hint string) (string, int, error) {
	if decoder := decoder(b, hint); decoder != nil {
		out, _, err := transform.Bytes(decoder, b)
		return string(out), -1, err
	}
	base := 0
	if bytes.HasPrefix(b, []byte{0xef, 0xbb, 0xbf}) {
		b, base = b[3:], 3
	}
	if bytes.IndexByte(b, 0) >= 0 {
		return "", -1, ErrBinary
	}
	if utf8.Valid(b) {
		return string(b), base, nil
	}
	if MostlyUTF8(LossyUTF8(sniff(b))) {
		return LossyUTF8(b), -1, nil
	}
	out, _, err := transform.Bytes(charmap.ISO8859_1.NewDecoder(), b)
	return string(out), -1, err
}

// sniff returns the start of b that decides how invalid UTF-8 is decoded,
// without a character that the cut leaves incomplete.
func sniff(b []byte) []byte {
	if len(b) <= sniffSize {
		return b
	}
	return b[:CompleteLen(b[:sniffSize])]
}

// LossyUTF8 decodes b, replacing each maximal invalid subsequence with
// U+FFFD as a WHATWG decoder does.
func LossyUTF8(b []byte) string {
	out, _, err := transform.Bytes(unicode.UTF8.NewDecoder(), b)
	if err != nil {
		return strings.ToValidUTF8(string(b), "�") // not expected: the decoder replaces
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

// decoder returns the decoder for hint, or nil to treat b as UTF-8.
func decoder(b []byte, hint string) transform.Transformer {
	switch strings.ToLower(hint) {
	case "utf-16", "utf-16le", "utf16le":
		return unicode.UTF16(unicode.LittleEndian, unicode.UseBOM).NewDecoder()
	case "utf-16be", "utf16be":
		return unicode.UTF16(unicode.BigEndian, unicode.UseBOM).NewDecoder()
	case "latin1", "latin-1", "iso-8859-1":
		return charmap.ISO8859_1.NewDecoder()
	case "windows-1252":
		return charmap.Windows1252.NewDecoder()
	case "", "utf8", "utf-8", "utf-8-bom", "utf8bom", "utf-8 bom":
		switch {
		case bytes.HasPrefix(b, []byte{0xff, 0xfe}):
			return unicode.UTF16(unicode.LittleEndian, unicode.UseBOM).NewDecoder()
		case bytes.HasPrefix(b, []byte{0xfe, 0xff}):
			return unicode.UTF16(unicode.BigEndian, unicode.UseBOM).NewDecoder()
		}
		return nil
	}
	if encoding, err := ianaindex.MIME.Encoding(hint); err == nil && encoding != nil {
		return encoding.NewDecoder()
	}
	return nil
}
