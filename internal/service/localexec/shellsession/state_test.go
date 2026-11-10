package shellsession

import (
	"os"
	"strings"
	"testing"
)

func TestParseSnapshotRejectsTruncated(t *testing.T) {
	full := "/w\x00A=1\x00SHLVL=3\x00_=/usr/bin/env\x00\x00" + sectionEnvSaved + "\x00\x00" + sectionOptions + "\x00set -o noclobber\nset -o errexit\nset -o monitor\nset +o nounset\nshopt -s login_shell\nshopt -s nullglob\n\x00" +
		sectionFunctions + "\x00f () { :; }\n\x00" + sectionAliases + "\x00alias a='b'\n\x00" + sectionEnd + "\x00"
	st, ok := parseSnapshot([]byte(full))
	if !ok || strings.Join(st.env, " ") != "A=1" || strings.Join(st.options, "|") != "set -o noclobber|shopt -s nullglob" || st.functions != "f () { :; }\n" {
		t.Fatal(ok, st)
	}
	if st.aliases != "alias a='b'\n" || !strings.Contains(st.replayScript(shell{}), "alias a='b'\n\nf () { :; }\n") {
		t.Fatal(st.aliases, st.replayScript(shell{}))
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

// The shell is picked like the desktop's kpe: $SHELL mentioning zsh, then
// bash, as it is; else zsh, then bash from PATH.
func TestUserShell(t *testing.T) {
	both, onlyBash := t.TempDir(), t.TempDir()
	for _, f := range []string{both + "/zsh", both + "/bash", onlyBash + "/bash"} {
		if err := os.WriteFile(f, []byte("#!/bin/sh\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		shell, path string
		want        shell
	}{
		{"/opt/zsh-5.9/bin/zsh-5.9", onlyBash, shell{path: "/opt/zsh-5.9/bin/zsh-5.9", zsh: true}},
		{"/usr/local/bin/bash5", both, shell{path: "/usr/local/bin/bash5"}},
		{"/usr/bin/fish", both, shell{path: both + "/zsh", zsh: true}},
		{"/bin/sh", onlyBash, shell{path: onlyBash + "/bash"}},
		{"", t.TempDir(), shell{path: "/bin/bash"}},
	} {
		t.Setenv("SHELL", tc.shell)
		t.Setenv("PATH", tc.path)
		if got := userShell(); got != tc.want {
			t.Errorf("SHELL=%q PATH=%q: %+v, want %+v", tc.shell, tc.path, got, tc.want)
		}
	}
}

func TestShellEnvironmentMarkersOnce(t *testing.T) {
	t.Setenv("SHLVL", "2")
	st := State{env: []string{"A=1", "CURSOR_AGENT=0", "SAND_AGENT=1", "CURSOR_AGENT_X=1"}}
	got := strings.Join(st.environ(shell{}, nil), " ")
	if got != "A=1 CURSOR_AGENT_X=1 SHLVL=2 CURSOR_AGENT=1 SAND_AGENT=1" {
		t.Fatal(got)
	}
	st.env = append(st.env, "CURSOR_REQUEST_ID=old", "AGENT_TRANSCRIPTS=/old")
	got = strings.Join(st.environ(shell{}, RequestEnv("/r", "", "req")), " ")
	if got != "A=1 CURSOR_AGENT_X=1 SHLVL=2 CURSOR_AGENT=1 SAND_AGENT=1 CURSOR_REQUEST_ID=req AGENT_TRANSCRIPTS=/r/agent-transcripts" {
		t.Fatal(got)
	}
}

// S3-2, like the desktop, whose shells start with the app's environment and
// replay the saved one inside: a shell starts with the CLI's values of the
// variables it reads at startup, and the replay exports the saved ones,
// unsets those the state lacks and restores an exported IFS. SHELLOPTS and
// BASHOPTS are not saved.
func TestShellStartupEnvReplayed(t *testing.T) {
	t.Setenv("SHLVL", "2")
	t.Setenv("BASH_ENV", "/cli/rc")
	t.Setenv("HOME", "/cli/home")
	t.Setenv("ZDOTDIR", "/same")
	if err := os.Unsetenv("ENV"); err != nil {
		t.Fatal(err)
	}
	st := State{env: []string{"A=1", "BASH_ENV=/saved/it's rc", "ENV=/saved/env", "HOME=/saved/home", "ZDOTDIR=/same", "IFS=:"}}
	bash, zsh := shell{path: "/bin/bash"}, shell{path: "/bin/zsh", zsh: true}
	for _, tc := range []struct {
		sh           shell
		env, replay  string
		withoutSaved string
	}{
		{bash, "A=1 HOME=/saved/home ZDOTDIR=/same IFS=: SHLVL=2 BASH_ENV=/cli/rc CURSOR_AGENT=1 SAND_AGENT=1",
			"builtin export BASH_ENV='/saved/it'\\''s rc'\nbuiltin export ENV='/saved/env'\nbuiltin export IFS=':'\n",
			"builtin unset BASH_ENV 2>/dev/null\n"},
		{zsh, "A=1 BASH_ENV=/saved/it's rc IFS=: SHLVL=2 ZDOTDIR=/same HOME=/cli/home CURSOR_AGENT=1 SAND_AGENT=1",
			"builtin export HOME='/saved/home'\nbuiltin export ENV='/saved/env'\nbuiltin export IFS=':'\n",
			"builtin unset ZDOTDIR 2>/dev/null\nbuiltin unset HOME 2>/dev/null\n"},
	} {
		if got := strings.Join(st.environ(tc.sh, nil), " "); got != tc.env {
			t.Errorf("zsh %v: environ %q, want %q", tc.sh.zsh, got, tc.env)
		}
		if got := st.replayEnv(tc.sh); got != tc.replay {
			t.Errorf("zsh %v: replay %q, want %q", tc.sh.zsh, got, tc.replay)
		}
		if got := (State{env: []string{"A=1"}}).replayEnv(tc.sh); got != tc.withoutSaved {
			t.Errorf("zsh %v: replay without saved values %q, want %q", tc.sh.zsh, got, tc.withoutSaved)
		}
	}
	if got := (State{}).replayEnv(bash); got != "" {
		t.Errorf("replay of the CLI's environment %q", got)
	}
	snap := "/w\x00A=1\x00SHELLOPTS=braceexpand:nounset\x00BASHOPTS=extglob\x00\x00" + sectionEnvSaved + "\x00\x00" + sectionEnd + "\x00"
	if st, ok := parseSnapshot([]byte(snap)); !ok || strings.Join(st.env, " ") != "A=1" {
		t.Fatal(ok, st.env)
	}
}

// The desktop's dS: AGENT_TRANSCRIPTS below the root, the conversation ID
// encoded like aoe and the request ID of an evaluation run hashed like DVe.
func TestRequestEnv(t *testing.T) {
	long := strings.Repeat("é", 40) // 240 characters encoded
	got := RequestEnv("/r", "a b/c~é'"+long, "run-cursorbench-1")
	want := []string{
		"CURSOR_CONVERSATION_ID=" + ("a_20b_2Fc~_C3_A9'" + strings.Repeat("_C3_A9", 40))[:200],
		"CURSOR_REQUEST_ID=410579276bbc84cbe3a50fbab0d5a94742bef453da66c64e3688850a6d5135a4",
		"AGENT_TRANSCRIPTS=/r/agent-transcripts",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("%q", got)
	}
	if got := RequestEnv("/r", "", "plain"); strings.Join(got, " ") != "CURSOR_REQUEST_ID=plain AGENT_TRANSCRIPTS=/r/agent-transcripts" {
		t.Fatalf("%q", got)
	}
}

// Like the desktop's dump, proxy settings and request-scoped variables are
// not carried over, matched anywhere without case; nor are the variables of
// the command scripts, which "set -a" exports.
func TestParseSnapshotDropsScopedEnv(t *testing.T) {
	env := []string{"KEEP=1", "http_proxy=x", "HTTPS_PROXY=y", "NOTE=a_proxy=b", "CURSOR_SANDBOX_X=1", "X=cursor_askpass",
		"SUDO_ASKPASS=/a", "CURSOR_CONVERSATION_ID=c", "CURSOR_REQUEST_ID=r", "CURSOR_AGENT_STORE_DIR=s",
		"__grok_dumped=1", "__grok_exit=0", "PROXY=kept"}
	snap := "/w\x00" + strings.Join(env, "\x00") + "\x00\x00" + sectionEnvSaved + "\x00\x00" + sectionEnd + "\x00"
	st, ok := parseSnapshot([]byte(snap))
	if !ok || strings.Join(st.env, " ") != "KEEP=1 PROXY=kept" {
		t.Fatal(ok, st.env)
	}
}
