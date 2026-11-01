package textcodec

import (
	"unicode/utf16"
	"unicode/utf8"
)

// UTF16Len returns the length of s in UTF-16 code units, as JavaScript
// counts characters.
func UTF16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

// TruncateUTF16 cuts s to at most n UTF-16 code units without splitting a
// character.
func TruncateUTF16(s string, n int) string {
	units := 0
	for i, r := range s {
		if units += utf16.RuneLen(r); units > n {
			return s[:i]
		}
	}
	return s
}

// TruncateUTF8 cuts s to at most n bytes at a rune boundary.
func TruncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// CompleteLen returns the length of b without a trailing incomplete UTF-8
// sequence.
func CompleteLen(b []byte) int {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			if utf8.FullRune(b[i:]) {
				return len(b)
			}
			return i
		}
	}
	return len(b)
}

// ChunkLen returns at most size bytes of b, cut at a rune boundary unless no
// rune starts within them.
func ChunkLen(b []byte, size int) int {
	if len(b) <= size {
		return len(b)
	}
	n := size
	for n > 0 && !utf8.RuneStart(b[n]) {
		n--
	}
	if n == 0 {
		return size
	}
	return n
}
