// Package staterepo persists CLI state as JSON files under ~/.grok/bot-tunnel:
// the identity, the machine and the cached computer credential, with a lock
// shared by every process using the store.
package staterepo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/exa-pub/grokbot-tunnel/internal/entity"
	"github.com/exa-pub/grokbot-tunnel/internal/pkg/atomicfile"
	"github.com/exa-pub/grokbot-tunnel/internal/pkg/uuid"
)

// Store reads and writes named JSON documents in Dir.
type Store struct{ Dir string }

// Default returns the store in the user's home directory.
func Default() (*Store, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return &Store{Dir: filepath.Join(home, ".grok", "bot-tunnel")}, nil
}

// Load decodes the named document into v.
func (s *Store) Load(name string, v any) error {
	b, err := os.ReadFile(s.path(name))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// writeOptions write documents durably: the rename is synced too, so that a
// rotated refresh token is not lost after a power failure, when the server
// has already revoked the old one.
var writeOptions = atomicfile.Options{Prefix: ".tmp-", SyncDir: true}

// Save atomically replaces the named document with v. The directory and the
// file are readable only by the owner because they hold secrets. An
// entity.Machine is saved in the format of Machine.
func (s *Store) Save(name string, v any) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(s.Dir, 0o700); err != nil { //nolint:gosec // directories need the execute bit
		return err
	}
	switch m := v.(type) {
	case entity.Machine:
		v = toFile(m)
	case *entity.Machine:
		v = toFile(*m)
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(s.path(name), b, writeOptions)
}

// lockPoll is how often Lock retries while another process holds the lock.
const lockPoll = 50 * time.Millisecond

// Lock takes an exclusive advisory lock shared by every process using the
// store and returns a function that releases it. It gives up when ctx ends.
func (s *Store) Lock(ctx context.Context) (func(), error) {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(s.Dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		// Closing the file releases the lock, also if the process dies.
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) //nolint:gosec // fds fit in int
		if err == nil {
			return func() { f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			f.Close()
			return nil, err
		}
		t := time.NewTimer(lockPoll)
		select {
		case <-ctx.Done():
			t.Stop()
			f.Close()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

// machineFile is machine.json. Label holds only a label chosen with --label
// and is marked by LabelSet. Files written before LabelSet existed always
// saved a label, the host name unless one was chosen.
type machineFile struct {
	MachineID string `json:"machine_id"`
	Label     string `json:"label,omitempty"`
	LabelSet  bool   `json:"label_set,omitempty"`
	HostID    string `json:"host_id,omitempty"`
}

// hostname is os.Hostname, empty on error; a variable for tests.
var hostname = func() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}

// Machine loads the machine identity, creating it on first use. Its Label
// is the saved --label, else the current host name as the desktop
// registers it, so that it follows renames. Identities saved before HostID
// existed get the current host's, and older files are rewritten in the
// current format.
func (s *Store) Machine() (entity.Machine, error) {
	var f machineFile
	err := s.Load("machine", &f)
	switch {
	case errors.Is(err, os.ErrNotExist):
		f = machineFile{MachineID: uuid.New(), HostID: HostID()}
		err = s.Save("machine", f)
	case err != nil:
		return entity.Machine{}, err
	case f.MachineID == "":
		return entity.Machine{}, errors.New("invalid machine.json: empty machine_id")
	default:
		migrated := f
		if migrated.HostID == "" {
			migrated.HostID = HostID()
		}
		if migrated.Label != "" && !migrated.LabelSet {
			// The host name saved as the default label is not a choice.
			migrated.LabelSet = entity.NormalizeLabel(migrated.Label) != entity.NormalizeLabel(hostname())
			if !migrated.LabelSet {
				migrated.Label = ""
			}
		}
		if migrated != f {
			f = migrated
			err = s.Save("machine", f)
		}
	}
	m := entity.Machine{MachineID: f.MachineID, Label: entity.NormalizeLabel(f.Label), HostID: f.HostID}
	if m.Label == "" {
		m.Label = entity.DefaultLabel(hostname())
	}
	return m, err
}

// toFile returns machine.json for m: its Label is saved only if it is not
// the default one, which then follows the host name.
func toFile(m entity.Machine) machineFile {
	f := machineFile{MachineID: m.MachineID, HostID: m.HostID}
	if label := entity.NormalizeLabel(m.Label); label != "" && label != entity.DefaultLabel(hostname()) {
		f.Label, f.LabelSet = label, true
	}
	return f
}

// machineIDPath is the systemd machine ID, unique per installation.
var machineIDPath = "/etc/machine-id"

// HostID identifies this host by its machine ID, or is empty without one.
// The host name would not do: containers recreated on the same volume get a
// new one each time.
func HostID() string {
	b, err := os.ReadFile(machineIDPath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Identity returns the saved login, or entity.ErrNotLoggedIn without one.
func (s *Store) Identity() (entity.Identity, error) {
	var identity entity.Identity
	err := s.Load("auth", &identity)
	switch {
	case errors.Is(err, os.ErrNotExist), err == nil && identity.RefreshToken == "":
		return identity, entity.ErrNotLoggedIn
	case err != nil:
		return identity, fmt.Errorf("load authentication (run grokbot-tunnel login): %w", err)
	}
	return identity, nil
}

func (s *Store) path(name string) string {
	return filepath.Join(s.Dir, name+".json")
}
