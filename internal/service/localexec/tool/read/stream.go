package read

import (
	"bytes"
	"math"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
)

const (
	// readChunk is the size of the chunks the desktop reads text files in.
	readChunk = 64 << 10
	// mostlyCRLF is the share of CRLF line endings, in percent, from which
	// they are normalized to LF.
	mostlyCRLF = 5
	unlimited  = math.MaxInt
)

// textResult is what the streaming reader returns.
type textResult struct {
	content      text
	totalLines   int
	rangeApplied bool
}

// collection is lines kept for the result with their total length, the CRs
// of their CRLF endings, and whether a cap or max_chars stopped it (the
// desktop's h/g/_/S/w and b/T/O/x/L).
type collection struct {
	lineStore
	units   int
	crs     int
	capped  bool
	over    bool
	limit   int // max_chars in UTF-16 code units, or unlimited
	limitAt *int
}

func (c *collection) done() bool { return c.over || c.capped && c.units >= maxUnits }

// push keeps a line (the desktop's ne and me); line is the line the reader
// assembled, which a cap may have cut already.
func (c *collection) push(line *text, hasNewline bool, lineNo int) {
	if c.done() {
		return
	}
	sep, room := 0, maxUnits-c.units
	if c.len() > 0 {
		if room <= 0 {
			c.capped = true
			return
		}
		sep, room = 1, room-1
	}
	b, units, cut := line.b, line.units, line.truncated
	if units > room {
		b = truncateUnits(b, room)
		units, cut = utf16Len(b), true
	}
	c.lineStore.push(b, units, cut, hasNewline)
	if cut {
		c.units += sep + room // the desktop counts a cut line as filling the room
	} else {
		c.units += sep + units
	}
	if line.truncated || cut {
		c.capped = true
	}
	if hasNewline && endsWithCR(b) {
		c.crs++
	}
	if c.units-c.crs > c.limit {
		c.over = true
		c.limitAt = &lineNo
	}
}

// textReader follows the desktop's streaming read (W$e): it takes decoded
// chunks and keeps only the lines the read returns, so memory does not grow
// with the file.
type textReader struct {
	ranged, fromEnd bool
	start, end      int // end < 0: no limit
	tail            int // lines from the end
	limit           *uint32
	maxChars        *uint32

	collectWhole bool // keep the text for an offset past the end
	whole, rng   collection
	ring         lineStore
	ringUnits    int
	ringCut      bool

	cur        text // the line being read
	cut        bool // max_chars cut the line being read
	continued  bool // a cut line went on
	firstEmpty bool
	lines      int
	crlf, lf   int
	seenText   bool
	prevCR     bool
}

func newTextReader(x *agentv1.ReadArgs) *textReader {
	r := &textReader{ranged: x.Offset != nil || x.Limit != nil, end: -1, limit: x.Limit, maxChars: x.MaxChars}
	offset := 1
	if x.Offset != nil {
		offset = int(*x.Offset)
	}
	r.fromEnd = offset < 0
	r.start = max(0, offset-1)
	if x.Limit != nil {
		r.end = r.start + int(*x.Limit)
	}
	if r.fromEnd {
		r.tail = -offset
	}
	r.whole.limit, r.rng.limit = unlimited, unlimited
	if x.MaxChars != nil {
		if !r.ranged {
			r.whole.limit = int(*x.MaxChars)
		} else if !r.fromEnd {
			r.rng.limit = int(*x.MaxChars)
		}
	}
	r.collectWhole = !r.fromEnd && (!r.ranged || r.start > 0)
	return r
}

func (r *textReader) inRange(line int) bool {
	return line >= r.start && (r.end < 0 || line < r.end)
}

// wants reports whether the line being read is kept (Te).
func (r *textReader) wants() bool {
	switch {
	case r.cut:
		return false
	case r.collectWhole && !r.whole.done(), r.ranged && r.lines == 0:
		return true
	case r.fromEnd:
		return r.tail > 0
	case !r.ranged || r.rng.done():
		return false
	}
	return r.inRange(r.lines)
}

// count counts a line ending (se).
func (r *textReader) count(hasNewline, crlf bool) {
	if hasNewline {
		if crlf {
			r.crlf++
		} else {
			r.lf++
		}
	}
	r.lines++
}

// lineEnd keeps the line just read where the read needs it (we).
func (r *textReader) lineEnd(hasNewline, crlf bool) {
	n := r.lines
	if n == 0 {
		r.firstEmpty = len(r.cur.b) == 0
	}
	if !r.fromEnd && r.ranged && r.collectWhole && n >= r.start {
		r.collectWhole = false
		r.whole.reset()
	}
	if r.collectWhole {
		r.whole.push(&r.cur, hasNewline, n)
	}
	switch {
	case !r.fromEnd:
		if r.ranged && r.inRange(n) {
			r.rng.push(&r.cur, hasNewline, n)
		}
	case r.tail > 0:
		if r.ring.len() == r.tail {
			r.ringUnits -= r.ring.popFront().units + 1
		}
		r.ring.push(r.cur.b, r.cur.units, r.cur.truncated, hasNewline)
		r.ringUnits += r.cur.units + 1
		for r.ring.len() > 1 && r.ringUnits > maxUnits {
			r.ringUnits -= r.ring.popFront().units + 1
			r.ringCut = true
		}
	}
	r.count(hasNewline, crlf)
}

// partial checks the start of a line that a chunk ends in: one that takes
// the text past max_chars is cut at the chunk boundary (Pe).
func (r *textReader) partial() {
	units := r.cur.units - 1
	c := &r.whole
	if r.ranged {
		if r.fromEnd || !r.inRange(r.lines) {
			return
		}
		c = &r.rng
	}
	sep := min(c.len(), 1)
	if c.units-c.crs+sep+units <= c.limit {
		return
	}
	c.push(&r.cur, false, r.lines)
	r.cur.reset()
	r.cut = true
}

// chunk reads the next decoded chunk.
func (r *textReader) chunk(q []byte) {
	if !r.seenText && len(q) > 0 {
		r.seenText = true
		q = bytes.TrimPrefix(q, bom)
	}
	z := 0
	for {
		i := bytes.IndexByte(q[z:], '\n')
		if i < 0 {
			break
		}
		y := z + i
		crlf := r.prevCR
		if y > z {
			crlf = q[y-1] == '\r'
		}
		if r.wants() {
			r.cur.extend(q[z:y])
			r.lineEnd(true, crlf)
		} else {
			if r.cut && y > z {
				r.continued = true
			}
			r.count(true, crlf)
		}
		r.cur.reset()
		r.cut, r.prevCR, z = false, false, y+1
	}
	if z < len(q) {
		if r.wants() {
			r.cur.extend(q[z:])
			r.partial()
		} else if r.cut {
			r.continued = true
		}
		r.prevCR = q[len(q)-1] == '\r'
	}
}

// cutOff reports whether max_chars left out text that the read covers: the
// rest of the line it stopped in, or further lines.
func (r *textReader) cutOff() bool {
	limitAt := r.whole.limitAt
	if limitAt == nil {
		limitAt = r.rng.limitAt
	}
	last := r.lines
	if r.ranged && r.end >= 0 {
		last = min(last, r.end)
	}
	return limitAt != nil && (r.continued || last > *limitAt+1)
}

// finish reads the last line and returns the result.
func (r *textReader) finish() textResult {
	if r.wants() {
		r.lineEnd(false, false)
	} else {
		r.count(false, false)
	}
	endings := r.crlf + r.lf
	crlf := endings > 0 && float64(r.crlf)/float64(endings)*100 >= mostlyCRLF
	cutOff := r.cutOff()
	headroom := unlimited
	if r.maxChars != nil {
		headroom = int(*r.maxChars) + readChunk
	}
	whole := func() text { return r.whole.join(r.whole.capped || cutOff, crlf, unlimited) }
	switch {
	case !r.ranged:
		return textResult{content: limitUnits(whole(), headroom), totalLines: r.lines}
	case r.lines <= 1 && r.firstEmpty:
		return textResult{totalLines: r.lines}
	case r.fromEnd:
		n := r.tail
		if r.limit != nil {
			n = int(*r.limit)
		}
		return textResult{content: r.ring.join(r.ringCut, crlf, n), totalLines: r.lines, rangeApplied: true}
	case r.start >= r.lines:
		return textResult{content: whole(), totalLines: r.lines}
	}
	content := r.rng.join(r.rng.capped || cutOff, crlf, unlimited)
	return textResult{content: limitUnits(content, headroom), totalLines: r.lines, rangeApplied: true}
}
