package staterepo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/exa-pub/grokbot-tunnel/internal/entity"
)

func TestStorageModesAndMachinePersistence(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "store")}
	m, e := s.Machine()
	if e != nil {
		t.Fatal(e)
	}
	again, e := s.Machine()
	if e != nil || m.MachineID != again.MachineID || len(m.MachineID) != 36 {
		t.Fatal("unstable machine ID", e)
	}
	if e = s.Save("auth", entity.Identity{AccessToken: "secret", RefreshToken: "r"}); e != nil {
		t.Fatal(e)
	}
	for name, want := range map[string]os.FileMode{"": 0700, "auth.json": 0600, "machine.json": 0600} {
		st, e := os.Stat(filepath.Join(s.Dir, name))
		if e != nil {
			t.Fatal(e)
		}
		if st.Mode().Perm() != want {
			t.Fatalf("%s mode %o", name, st.Mode().Perm())
		}
	}
	entries, _ := os.ReadDir(s.Dir)
	if len(entries) != 2 {
		t.Fatal("temporary files left behind")
	}
}

func TestLockExcludesOtherHolders(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "store")}
	unlock, err := s.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// flock locks belong to the open file, so a second Lock in the same
	// process contends like another process would.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err = s.Lock(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("lock not exclusive", err)
	}
	unlock()
	again, err := s.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	again()
}

func TestMachineRecordsHost(t *testing.T) {
	id := filepath.Join(t.TempDir(), "machine-id")
	os.WriteFile(id, []byte("abc123\n"), 0o644)
	old := machineIDPath
	machineIDPath = id
	t.Cleanup(func() { machineIDPath = old })

	s := &Store{Dir: filepath.Join(t.TempDir(), "store")}
	m, err := s.Machine()
	if err != nil || m.HostID != "abc123" {
		t.Fatal(m, err)
	}
	// Identities saved before host_id existed get the current host's.
	if err = s.Save("machine", entity.Machine{MachineID: m.MachineID, Label: "box"}); err != nil {
		t.Fatal(err)
	}
	m, err = s.Machine()
	if err != nil || m.HostID != "abc123" || m.Label != "box" {
		t.Fatal(m, err)
	}
	var saved entity.Machine
	if err = s.Load("machine", &saved); err != nil || saved != m {
		t.Fatal(saved, err)
	}
	// A copied store keeps the host it was created on.
	os.WriteFile(id, []byte("other\n"), 0o644)
	if m, err = s.Machine(); err != nil || m.HostID != "abc123" || HostID() != "other" {
		t.Fatal(m, err)
	}
	os.Remove(id)
	if HostID() != "" {
		t.Fatal(HostID())
	}
}

// fakeHostname makes hostname return *name.
func fakeHostname(t *testing.T, name *string) {
	t.Helper()
	old := hostname
	hostname = func() string { return *name }
	t.Cleanup(func() { hostname = old })
}

// The default label is the current host name, as the desktop registers it;
// only a chosen label is saved.
func TestMachineLabelFollowsHostname(t *testing.T) {
	host := " box-1\n"
	fakeHostname(t, &host)
	s := &Store{Dir: filepath.Join(t.TempDir(), "store")}
	m, err := s.Machine()
	if err != nil || m.Label != "box-1" {
		t.Fatal(m, err)
	}
	b, _ := os.ReadFile(filepath.Join(s.Dir, "machine.json"))
	if strings.Contains(string(b), "label") {
		t.Fatal(string(b))
	}
	host = ""
	if m, err = s.Machine(); err != nil || m.Label != runtime.GOOS {
		t.Fatal(m, err)
	}
	host = strings.Repeat("h", 120)
	if m, err = s.Machine(); err != nil || m.Label != strings.Repeat("h", 100) {
		t.Fatal(m, err)
	}

	// A chosen label stays across renames; saving the default saves none.
	host = "box-2"
	m.Label = "  my box "
	if err = s.Save("machine", m); err != nil {
		t.Fatal(err)
	}
	host = "box-3"
	if m, err = s.Machine(); err != nil || m.Label != "my box" {
		t.Fatal(m, err)
	}
	m.Label = "box-3"
	if err = s.Save("machine", m); err != nil {
		t.Fatal(err)
	}
	host = "box-4"
	if m, err = s.Machine(); err != nil || m.Label != "box-4" {
		t.Fatal(m, err)
	}
}

// Files written before label_set always hold a label: the host name, unless
// the user chose another. They are rewritten in the current format.
func TestMachineLabelMigration(t *testing.T) {
	host := "box"
	fakeHostname(t, &host)
	for _, tc := range []struct {
		saved, want string
		set         bool
	}{
		{"box", "box", false},
		{"box ", "box", false},
		{"custom", "custom", true},
		{"", "box", false},
	} {
		s := &Store{Dir: filepath.Join(t.TempDir(), "store")}
		os.MkdirAll(s.Dir, 0o700)
		old := `{"machine_id":"m","label":"` + tc.saved + `","host_id":"h"}`
		os.WriteFile(filepath.Join(s.Dir, "machine.json"), []byte(old), 0o600)
		m, err := s.Machine()
		if err != nil || m.Label != tc.want || m.MachineID != "m" || m.HostID != "h" {
			t.Fatal(tc, m, err)
		}
		var f machineFile
		if err = s.Load("machine", &f); err != nil || f.LabelSet != tc.set || f.LabelSet != (f.Label != "") {
			t.Fatal(tc, f, err)
		}
		// The rename after the migration shows whether the label was kept.
		host = "renamed"
		if m, _ = s.Machine(); tc.set != (m.Label == tc.want) {
			t.Fatal(tc, m)
		}
		host = "box"
	}
}

// Documents are written durably: a rotated refresh token must survive a
// power failure.
func TestSaveSyncsDirectory(t *testing.T) {
	if !writeOptions.SyncDir {
		t.Fatal("rename not synced")
	}
}
