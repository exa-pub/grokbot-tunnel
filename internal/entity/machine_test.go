package entity

import (
	"runtime"
	"strings"
	"testing"
)

// Like the desktop's hostname().trim()||process.platform, cut to 100 code
// points.
func TestDefaultLabel(t *testing.T) {
	long := strings.Repeat("é", 150)
	for host, want := range map[string]string{
		"box":                    "box",
		" box\n":                 "box",
		"\ufeffbox\u00a0":        "box",
		"\u0085box":              "\u0085box",
		"":                       runtime.GOOS,
		" \t ":                   runtime.GOOS,
		long:                     strings.Repeat("é", 100),
		"bad\xffname":            "bad\ufffdname",
		strings.Repeat("😀", 101): strings.Repeat("😀", 100),
	} {
		if got := DefaultLabel(host); got != want {
			t.Errorf("%q: got %q, want %q", host, got, want)
		}
	}
	if got := NormalizeLabel("  "); got != "" {
		t.Fatal(got)
	}
}
