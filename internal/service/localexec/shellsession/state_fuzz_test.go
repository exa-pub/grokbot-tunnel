package shellsession

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var optionLine = regexp.MustCompile(`^(set [-+]o|shopt -[su]) [A-Za-z0-9_]+$`)

// A snapshot is accepted only with an absolute cwd, and only well-formed
// option lines and environment entries survive: they are replayed by the
// next command.
func FuzzParseSnapshot(f *testing.F) {
	end := "\x00" + sectionEnd + "\x00"
	f.Add([]byte("/home\x00" + sectionEnvSaved + "\x00A=1\x00B=2\x00" + sectionOptions + "\x00set -o errexit\nshopt -s extglob\x00" +
		sectionFunctions + "\x00f () { :; }" + end))
	f.Add([]byte("/\x00X=1" + end))
	f.Add([]byte("relative" + end))
	f.Add([]byte("/\x00" + sectionOptions + "\x00set -o a;b\nset -o x y\n" + end))
	f.Fuzz(func(t *testing.T, b []byte) {
		st, ok := parseSnapshot(b)
		if !ok {
			return
		}
		if !filepath.IsAbs(st.cwd) {
			t.Fatalf("cwd %q", st.cwd)
		}
		for _, option := range st.options {
			if !optionLine.MatchString(option) {
				t.Fatalf("option %q", option)
			}
		}
		for _, kv := range st.env {
			if !strings.Contains(kv, "=") || isVolatileVar(kv) {
				t.Fatalf("env %q", kv)
			}
		}
	})
}
