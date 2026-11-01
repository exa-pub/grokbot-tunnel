package textcodec

import (
	"errors"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/ianaindex"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/encoding/unicode/utf32"
	"golang.org/x/text/transform"
)

// ErrInvalidUTF8 is returned by a strict UTF-8 Decoder for data that is not
// UTF-8. The desktop then reads the file again as decided by Fallback.
var ErrInvalidUTF8 = errors.New("invalid UTF-8 while streaming read")

// Decoding is how a file's bytes become text.
type Decoding struct {
	// Strict is UTF-8 that fails with ErrInvalidUTF8 on invalid data.
	Strict bool
	// encoding decodes other text; nil with Strict false is lossy UTF-8.
	encoding encoding.Encoding
}

// Lossy is UTF-8 with invalid sequences replaced by U+FFFD.
var Lossy = Decoding{}

// Latin1 maps each byte to the character with its code.
var Latin1 = Decoding{encoding: charmap.ISO8859_1}

var strict = Decoding{Strict: true}

// Resolve chooses the decoding of a file from the read's encoding hint or,
// without one, the encoding Detect found, like the desktop's Rut: names of
// UTF-8 decode strictly, other known encodings with their decoder, unknown
// names as strict UTF-8.
func Resolve(hint *string, detected string) Decoding {
	name := detected
	if hint != nil {
		name = *hint
	}
	switch name = strings.ToLower(name); name {
	case "utf-8", "utf8", "utf-8-bom", "utf-8 bom", "utf8bom":
		return strict
	case "latin-1":
		name = "latin1"
	}
	if e := named(name); e != nil {
		return Decoding{encoding: e}
	}
	return strict
}

// wellKnown are encodings by the names iconv-lite gives them. The UTF-16
// and UTF-32 decoders of one byte order do not interpret a byte order mark;
// the decoded U+FEFF is removed with the text's first character.
var wellKnown = map[string]encoding.Encoding{
	"utf8":          unicode.UTF8,
	"unicode11utf8": unicode.UTF8,
	"utf16le":       unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM),
	"ucs2":          unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM),
	"utf16be":       unicode.UTF16(unicode.BigEndian, unicode.IgnoreBOM),
	"utf16":         unicode.UTF16(unicode.LittleEndian, unicode.UseBOM),
	"utf32le":       utf32.UTF32(utf32.LittleEndian, utf32.IgnoreBOM),
	"ucs4le":        utf32.UTF32(utf32.LittleEndian, utf32.IgnoreBOM),
	"utf32be":       utf32.UTF32(utf32.BigEndian, utf32.IgnoreBOM),
	"ucs4be":        utf32.UTF32(utf32.BigEndian, utf32.IgnoreBOM),
	"utf32":         utf32.UTF32(utf32.LittleEndian, utf32.UseBOM),
	"ucs4":          utf32.UTF32(utf32.LittleEndian, utf32.UseBOM),
	"latin1":        charmap.ISO8859_1,
	"binary":        charmap.ISO8859_1,
	"iso88591":      charmap.ISO8859_1,
	"l1":            charmap.ISO8859_1,
	"windows1252":   charmap.Windows1252,
	"cp1252":        charmap.Windows1252,
}

// named returns the decoder of an encoding name the way iconv-lite looks it
// up: case, punctuation and spaces do not matter.
func named(name string) encoding.Encoding {
	canonical := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, name)
	if canonical == "" {
		return nil
	}
	if e, ok := wellKnown[canonical]; ok {
		return e
	}
	if e, err := ianaindex.MIME.Encoding(name); err == nil && e != nil {
		return e
	}
	return nil
}

// Fallback decides how to read a file that is not valid UTF-8 from its first
// bytes, as the desktop's Cut does: lossy UTF-8 when they are mostly UTF-8 or
// are the whole file and valid, Latin-1 otherwise. head is the first
// min(size, FallbackSniff) bytes, or FallbackSniff bytes when the size is
// unknown (0).
func Fallback(head []byte, size int64) Decoding {
	if int64(len(head)) == size && utf8.Valid(head) {
		return Lossy
	}
	if len(head) == FallbackSniff && int64(len(head)) != size {
		head = head[:CompleteLen(head)] // a stream decoder keeps a cut character
	}
	if MostlyUTF8(LossyUTF8(head)) {
		return Lossy
	}
	return Latin1
}

// FallbackSniff is how much of a file that is not UTF-8 Fallback looks at.
const FallbackSniff = 1 << 20

// Decoder decodes a stream chunk by chunk. A character cut by a chunk
// boundary is completed with the next chunk, as Node's stream decoders do.
type Decoder struct {
	strict  bool
	t       transform.Transformer
	pending []byte
	buf     []byte // pending bytes followed by the chunk
	out     []byte // decoded text
}

// NewDecoder returns a Decoder for d.
func (d Decoding) NewDecoder() *Decoder {
	switch {
	case d.Strict:
		return &Decoder{strict: true}
	case d.encoding == nil:
		return &Decoder{t: unicode.UTF8.NewDecoder()}
	}
	return &Decoder{t: d.encoding.NewDecoder()}
}

// Decode decodes the next chunk of the stream, or flushes it when atEOF is
// set. The text is UTF-8 and valid until the next call.
func (d *Decoder) Decode(chunk []byte, atEOF bool) ([]byte, error) {
	src := chunk
	if len(d.pending) > 0 {
		d.buf = append(append(d.buf[:0], d.pending...), chunk...)
		src, d.pending = d.buf, d.pending[:0]
	}
	if d.strict {
		n := len(src)
		if !atEOF {
			n = CompleteLen(src)
		}
		if !utf8.Valid(src[:n]) {
			return nil, ErrInvalidUTF8
		}
		d.pending = append(d.pending, src[n:]...)
		return src[:n], nil
	}
	return d.transform(src, atEOF)
}

func (d *Decoder) transform(src []byte, atEOF bool) ([]byte, error) {
	out := d.out[:0]
	if cap(out) < len(src)+utf8.UTFMax {
		out = make([]byte, 0, len(src)*2+utf8.UTFMax)
	}
	for {
		nDst, nSrc, err := d.t.Transform(out[len(out):cap(out)], src, atEOF)
		out, src = out[:len(out)+nDst], src[nSrc:]
		switch {
		case errors.Is(err, transform.ErrShortDst):
			out = append(out[:cap(out)], make([]byte, cap(out))...)[:len(out)]
			continue
		case errors.Is(err, transform.ErrShortSrc) && !atEOF:
			d.pending = append(d.pending, src...)
			err = nil
		}
		d.out = out
		return out, err
	}
}
