package read

import (
	"unicode/utf8"

	"github.com/exa-pub/grokbot-tunnel/internal/pkg/textcodec"
)

// maxUnits caps the text the desktop's streaming reader keeps (8 Mi UTF-16
// code units): a line, a collection of lines and the joined result.
const maxUnits = 8 << 20

// utf16Len is the length of valid UTF-8 text in UTF-16 code units.
func utf16Len(b []byte) int {
	n := 0
	for _, c := range b {
		switch {
		case c&0xc0 == 0x80: // continuation
		case c >= 0xf0:
			n += 2
		default:
			n++
		}
	}
	return n
}

// truncateUnits cuts valid UTF-8 text to at most n UTF-16 code units without
// splitting a character, like the desktop's zc.
func truncateUnits(b []byte, n int) []byte {
	units := 0
	for i := 0; i < len(b); {
		_, size := utf8.DecodeRune(b[i:])
		u := 1
		if size == 4 {
			u = 2
		}
		if units+u > n {
			return b[:i]
		}
		units += u
		i += size
	}
	return b
}

// text is a piece of text with its length in UTF-16 code units, and whether
// a cap cut it (the desktop's {text, truncated}).
type text struct {
	b         []byte
	units     int
	truncated bool
}

// add appends piece up to the 8 Mi cap (bye). Once cut, nothing is added.
func (t *text) add(piece []byte, units int) {
	if len(piece) == 0 {
		return
	}
	room := maxUnits - t.units
	switch {
	case room <= 0:
		t.truncated = true
	case units > room:
		piece = truncateUnits(piece, room)
		t.b = append(t.b, piece...)
		t.units += utf16Len(piece)
		t.truncated = true
	default:
		t.b = append(t.b, piece...)
		t.units += units
	}
}

// extend adds to a line that is not cut yet (hut).
func (t *text) extend(piece []byte) {
	if !t.truncated && len(piece) > 0 {
		t.add(piece, utf16Len(piece))
	}
}

func (t *text) reset() { *t = text{b: t.b[:0]} }

// span is a line kept in a lineStore.
type span struct {
	off, n     int
	units      int
	truncated  bool
	hasNewline bool
}

// lineStore keeps lines in one buffer. Lines can be dropped from the front,
// for a ring of the last lines.
type lineStore struct {
	buf   []byte
	lines []span
	head  int // first live line
	dead  int // bytes of dropped lines
}

func (s *lineStore) len() int { return len(s.lines) - s.head }

func (s *lineStore) push(b []byte, units int, truncated, hasNewline bool) {
	s.lines = append(s.lines, span{off: len(s.buf), n: len(b), units: units, truncated: truncated, hasNewline: hasNewline})
	s.buf = append(s.buf, b...)
}

func (s *lineStore) at(i int) (span, []byte) {
	l := s.lines[s.head+i]
	return l, s.buf[l.off : l.off+l.n]
}

// popFront drops the first line, compacting the buffer once dropped lines
// take more space than the live ones.
func (s *lineStore) popFront() span {
	l := s.lines[s.head]
	s.head++
	s.dead += l.n
	if s.dead > 1<<20 && s.dead > len(s.buf)-s.dead || s.head > 1024 && s.head*2 > len(s.lines) {
		live := s.lines[s.head:]
		buf := make([]byte, 0, len(s.buf)-s.dead)
		lines := make([]span, len(live))
		for i, l := range live {
			lines[i] = l
			lines[i].off = len(buf)
			buf = append(buf, s.buf[l.off:l.off+l.n]...)
		}
		s.buf, s.lines, s.head, s.dead = buf, lines, 0, 0
	}
	return l
}

func (s *lineStore) reset() { *s = lineStore{} }

// join joins lines first to last with newlines up to the 8 Mi cap (yye),
// removing the CR of CRLF endings when crlf is set (c_r).
func (s *lineStore) join(truncated, crlf bool, last int) text {
	out := text{truncated: truncated}
	for i := range min(last, s.len()) {
		l, b := s.at(i)
		units := l.units
		if crlf && l.hasNewline && len(b) > 0 && b[len(b)-1] == '\r' {
			b, units = b[:len(b)-1], units-1
		}
		if i > 0 {
			out.add([]byte{'\n'}, 1)
		}
		out.add(b, units)
		if l.truncated {
			out.truncated = true
		}
	}
	return out
}

// limitUnits cuts t to n UTF-16 code units, marking it truncated.
func limitUnits(t text, n int) text {
	if t.units > n {
		t.b = truncateUnits(t.b, n)
		t.truncated = true
	}
	return t
}

// endsWithCR reports a line that ends with a carriage return.
func endsWithCR(b []byte) bool { return len(b) > 0 && b[len(b)-1] == '\r' }

var bom = []byte("\xef\xbb\xbf")

// contentString converts the reader's result, cut at the JSON frame cap.
func contentString(t text) (string, bool) {
	s := string(t.b)
	if len(s) > maxTextRead {
		return textcodec.TruncateUTF8(s, maxTextRead), true
	}
	return s, t.truncated
}
