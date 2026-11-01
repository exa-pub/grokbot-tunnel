package textcodec

import (
	"bytes"
	"unicode/utf8"
)

// SniffLen is how much of the start of a file Detect looks at, like the
// desktop's getFormatForFile.
const SniffLen = 8192

// Encodings that Detect reports.
const (
	EncodingUTF8    = "UTF-8"
	EncodingUTF16LE = "UTF-16LE"
	EncodingUTF16BE = "UTF-16BE"
	EncodingUTF32LE = "UTF-32LE"
	EncodingUTF32BE = "UTF-32BE"
	EncodingBinary  = "binary"
)

// Format is what the start of a file says about its content.
type Format struct {
	// Encoding is the text encoding, or EncodingBinary.
	Encoding string
	// Binary data is not text; Image is binary data with the signature of
	// a PNG, JPEG, WEBP or GIF image.
	Binary, Image bool
}

// Detect classifies a file by its first SniffLen bytes as the desktop does
// (getFormatForFile, ZO): a UTF-16 or UTF-32 byte order mark or pattern of
// zero bytes, binary data with a NUL byte or at least 5% control bytes, and
// UTF-8 otherwise. The desktop also guesses legacy encodings with jschardet;
// such files are decoded as UTF-8, which falls back to lossy UTF-8 or
// Latin-1 (see Fallback).
func Detect(head []byte) Format {
	if len(head) >= SniffLen {
		head = trimIncomplete(head[:SniffLen])
	}
	if len(head) == 0 {
		return Format{Encoding: EncodingUTF8}
	}
	if e := utf16Pattern(head); e != "" {
		return Format{Encoding: e}
	}
	if e := utf32Pattern(head); e != "" {
		return Format{Encoding: e}
	}
	if !textual(head) {
		return Format{Encoding: EncodingBinary, Binary: true, Image: imageSignature(head)}
	}
	return Format{Encoding: EncodingUTF8}
}

// trimIncomplete drops a UTF-8 sequence that the cut at SniffLen leaves
// incomplete (XKt).
func trimIncomplete(b []byte) []byte {
	for i := len(b) - 1; i >= max(0, len(b)-3); i-- {
		c := b[i]
		if c < utf8.RuneSelf {
			return b
		}
		if c >= 0xc0 {
			n := 2
			if c >= 0xf0 {
				n = 4
			} else if c >= 0xe0 {
				n = 3
			}
			if len(b)-i < n {
				return b[:i]
			}
			return b
		}
	}
	return b
}

// printable is an ASCII byte that text in a wide encoding pairs with zeros.
func printable(c byte) bool { return c >= 32 && c <= 126 || c == '\t' || c == '\n' || c == '\r' }

// utf16Pattern recognizes UTF-16 by its byte order mark or by zero bytes
// beside ASCII in the first 50 code units (rYt).
func utf16Pattern(b []byte) string {
	switch {
	case len(b) < 4:
		return ""
	case b[0] == 0xff && b[1] == 0xfe && (b[2] != 0 || b[3] != 0):
		return EncodingUTF16LE
	case b[0] == 0xfe && b[1] == 0xff:
		return EncodingUTF16BE
	}
	return widePattern(b, 2, EncodingUTF16LE, EncodingUTF16BE)
}

// utf32Pattern recognizes UTF-32 by its byte order mark or by zero bytes
// beside ASCII in the first 50 code units (sYt).
func utf32Pattern(b []byte) string {
	switch {
	case len(b) < 8:
		return ""
	case bytes.HasPrefix(b, []byte{0, 0, 0xfe, 0xff}):
		return EncodingUTF32BE
	case bytes.HasPrefix(b, []byte{0xff, 0xfe, 0, 0}):
		return EncodingUTF32LE
	}
	return widePattern(b, 4, EncodingUTF32LE, EncodingUTF32BE)
}

// widePattern finds the encoding in which more than 80% of the first 50
// code units of size bytes are ASCII: the low byte first and zeros after it
// (little endian), or zeros and the low byte last (big endian).
func widePattern(b []byte, size int, little, big string) string {
	le, be, n := 0, 0, min(50, len(b)/size)
	for i := 0; i < n*size && i+size-1 < len(b); i += size {
		unit := b[i : i+size]
		if allZero(unit[1:]) && printable(unit[0]) {
			le++
		}
		if allZero(unit[:size-1]) && printable(unit[size-1]) {
			be++
		}
	}
	switch {
	case float64(le) > float64(n)*0.8:
		return little
	case float64(be) > float64(n)*0.8:
		return big
	}
	return ""
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// textual reports whether b has no NUL byte and less than 5% control bytes
// other than tab, newline and carriage return (YKt).
func textual(b []byte) bool {
	if bytes.IndexByte(b, 0) >= 0 {
		return false
	}
	control := 0
	for _, c := range b {
		if c < 32 && c != '\t' && c != '\n' && c != '\r' {
			control++
		}
	}
	return float64(control)/float64(len(b))*100 < 5
}

// imageSignature reports the signature of a PNG, JPEG, WEBP or GIF image
// (ZKt).
func imageSignature(b []byte) bool {
	switch {
	case len(b) < 4:
		return false
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")),
		bytes.HasPrefix(b, []byte{0xff, 0xd8, 0xff}),
		len(b) >= 12 && bytes.HasPrefix(b, []byte("RIFF")) && string(b[8:12]) == "WEBP",
		bytes.HasPrefix(b, []byte("GIF87a")), bytes.HasPrefix(b, []byte("GIF89a")):
		return true
	}
	return false
}
