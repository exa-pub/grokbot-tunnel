package shellsession

import (
	"strings"
	"testing"
)

func TestParseSnapshotRejectsTruncated(t *testing.T) {
	full := "/w\x00A=1\x00SHLVL=3\x00_=/usr/bin/env\x00\x00" + sectionEnvSaved + "\x00\x00" + sectionOptions + "\x00set -o noclobber\nset -o errexit\nset -o monitor\nset +o nounset\nshopt -s login_shell\nshopt -s nullglob\n\x00" +
		sectionFunctions + "\x00f () { :; }\n\x00" + sectionEnd + "\x00"
	st, ok := parseSnapshot([]byte(full))
	if !ok || strings.Join(st.env, " ") != "A=1" || strings.Join(st.options, "|") != "set -o noclobber|shopt -s nullglob" || st.functions != "f () { :; }\n" {
		t.Fatal(ok, st)
	}
	if _, ok := parseSnapshot([]byte(full[:len(full)-len(sectionEnd)-2])); ok {
		t.Fatal("truncated snapshot accepted")
	}
	if _, ok := parseSnapshot([]byte("relative\x00A=1\x00" + sectionEnd + "\x00")); ok {
		t.Fatal("relative cwd accepted")
	}
}

func TestParseSnapshotWithoutSavedEnv(t *testing.T) {
	st, ok := parseSnapshot([]byte("/w\x00A=1\x00\x00" + sectionOptions + "\x00\x00" + sectionFunctions + "\x00\x00" + sectionEnd + "\x00"))
	if !ok || st.env != nil {
		t.Fatal(ok, st)
	}
}

func TestWithGitConfig(t *testing.T) {
	for _, tc := range []struct{ env, want string }{
		{"", "GIT_CONFIG_KEY_0=core.quotePath GIT_CONFIG_VALUE_0=false GIT_CONFIG_COUNT=1"},
		{"GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=user.name", "GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=user.name GIT_CONFIG_KEY_1=core.quotePath GIT_CONFIG_VALUE_1=false GIT_CONFIG_COUNT=2"},
		{"GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=Core.QuotePath", "GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=Core.QuotePath"},
		// A key past the count does not count; parseInt reads leading digits.
		{"GIT_CONFIG_COUNT=0x GIT_CONFIG_KEY_0=core.quotepath", "GIT_CONFIG_COUNT=0x GIT_CONFIG_KEY_0=core.quotepath GIT_CONFIG_KEY_0=core.quotePath GIT_CONFIG_VALUE_0=false GIT_CONFIG_COUNT=1"},
		{"GIT_CONFIG_COUNT=-1", "GIT_CONFIG_COUNT=-1 GIT_CONFIG_KEY_0=core.quotePath GIT_CONFIG_VALUE_0=false GIT_CONFIG_COUNT=1"},
		{"GIT_CONFIG_COUNT=2abc", "GIT_CONFIG_COUNT=2abc GIT_CONFIG_KEY_2=core.quotePath GIT_CONFIG_VALUE_2=false GIT_CONFIG_COUNT=3"},
	} {
		if got := strings.Join(withGitConfig(strings.Fields(tc.env)), " "); got != tc.want {
			t.Errorf("%q: %q, want %q", tc.env, got, tc.want)
		}
	}
	for s, want := range map[string]int{" \t+7": 7, "": 0, "x1": 0, "-0": 0, "99999999999999999999": 0} {
		if got := parseCount(s); got != want {
			t.Errorf("parseCount(%q) = %d, want %d", s, got, want)
		}
	}
}

func TestShellEnvironmentMarkersOnce(t *testing.T) {
	t.Setenv("SHLVL", "2")
	st := State{env: []string{"A=1", "CURSOR_AGENT=0", "SAND_AGENT=1", "CURSOR_AGENT_X=1"}}
	got := strings.Join(st.environ(), " ")
	if got != "A=1 CURSOR_AGENT_X=1 SHLVL=2 CURSOR_AGENT=1 SAND_AGENT=1" {
		t.Fatal(got)
	}
}
