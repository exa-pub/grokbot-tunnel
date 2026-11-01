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

func (s *Store) path(name string) string {
	return filepath.Join(s.Dir, name+".json")
}

// Load decodes the named document into v.
func (s *Store) Load(name string, v any) error {
	b, err := os.ReadFile(s.path(name))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// Save atomically replaces the named document with v. The directory and the
// file are readable only by the owner because they hold secrets.
func (s *Store) Save(name string, v any) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(s.Dir, 0o700); err != nil { //nolint:gosec // directories need the execute bit
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(s.path(name), b, atomicfile.Options{Prefix: ".tmp-"})
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

// Machine loads the machine identity, creating it on first use. Identities
// saved before HostID existed get the current host's.
func (s *Store) Machine() (entity.Machine, error) {
	var m entity.Machine
	err := s.Load("machine", &m)
	switch {
	case err == nil && m.MachineID == "":
		return m, errors.New("invalid machine.json: empty machine_id")
	case err == nil && (m.HostID != "" || HostID() == ""):
		return m, nil
	case err == nil:
		m.HostID = HostID()
		return m, s.Save("machine", m)
	case !errors.Is(err, os.ErrNotExist):
		return m, err
	}
	m.MachineID = uuid.New()
	m.HostID = HostID()
	if host, err := os.Hostname(); err == nil {
		m.Label = host
	}
	return m, s.Save("machine", m)
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
