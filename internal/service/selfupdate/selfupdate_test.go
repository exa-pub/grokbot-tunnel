package selfupdate

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func script(version string) []byte {
	return []byte("#!/bin/sh\necho 'grokbot-tunnel " + version + " (Sand protocol 1.0.1)'\n")
}

func TestReplace(t *testing.T) {
	bin := script("v1.2.3")
	dir := t.TempDir()
	exe := filepath.Join(dir, "grokbot-tunnel")
	os.WriteFile(exe, script("v1.0.0"), 0o755)
	link := filepath.Join(dir, "link")
	os.Symlink(exe, link)
	target, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatal(err)
	}
	if err = Replace(context.Background(), target, bin, "v1.2.3"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(exe)
	st, _ := os.Stat(exe)
	if !bytes.Equal(got, bin) || st.Mode().Perm() != 0o755 {
		t.Fatal(string(got), st.Mode())
	}
	if l, err := os.Lstat(link); err != nil || l.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink replaced", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Fatal("temporary files left behind", entries)
	}
}

func TestReplaceKeepsBinaryIfNewOneFails(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "grokbot-tunnel")
	old := script("v1.0.0")
	os.WriteFile(exe, old, 0o755)
	for name, bin := range map[string][]byte{
		"wrong version": script("v1.2.4"),
		"exits 1":       []byte("#!/bin/sh\nexit 1\n"),
		"not runnable":  []byte("\x7fELF garbage"),
	} {
		if err := Replace(context.Background(), exe, bin, "v1.2.3"); err == nil {
			t.Error(name, "accepted")
		}
		if got, _ := os.ReadFile(exe); !bytes.Equal(got, old) {
			t.Fatal(name, "replaced the binary")
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatal("temporary files left behind", entries)
	}
}

func TestVersions(t *testing.T) {
	for v, want := range map[string]bool{
		"v1.2.3": true, "v0.2.0-rc1": true,
		"dev": false, "dev-abc1234": false, "v0.77.0-3-gabc1234": false, "v0.77.0-3-gabc1234-dirty": false,
		"v0.77.0-dirty": false, "1.2.3": false, "abc1234": false,
	} {
		if IsRelease(v) != want {
			t.Errorf("IsRelease(%q) = %v", v, !want)
		}
	}
	for in, want := range map[string]string{"latest": "latest", "1.2.3": "v1.2.3", "v1.2.3-rc.1": "v1.2.3-rc.1"} {
		if got, err := Normalize(in); err != nil || got != want {
			t.Errorf("Normalize(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "v1", "1.2", "newest", "v1.2.3 x"} {
		if _, err := Normalize(bad); err == nil {
			t.Errorf("Normalize(%q) accepted", bad)
		}
	}
	if Compare("v1.2.3", "v1.10.0") >= 0 || Compare("v1.2.3-rc1", "v1.2.3") >= 0 {
		t.Error("Compare")
	}
}
