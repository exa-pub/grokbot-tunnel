// Package install runs connect as a systemd service: it renders the unit
// files, installs and enables them, and removes them again.
package install

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/exa-pub/grokbot-tunnel/internal/adapter/systemd"
	"github.com/exa-pub/grokbot-tunnel/internal/pkg/atomicfile"
)

// installUnits writes the units for c in m's scope, enables them and restarts the tunnel. The
// restart waits until the tunnel reports that it is connected, so an error
// means the service does not work; it stays installed and keeps retrying.
func installUnits(ctx context.Context, m *systemd.Manager, c Config) error {
	units, err := Units(m.Scope, c)
	if err != nil {
		return err
	}
	if !c.AutoUpdate {
		if err = removeTimer(ctx, m); err != nil {
			return err
		}
	}
	for _, name := range []string{TunnelUnit, UpdateUnit, UpdateTimer} {
		if content, ok := units[name]; ok {
			if err = writeUnit(m, name, content); err != nil {
				return err
			}
		}
	}
	if _, err = m.Systemctl(ctx, "daemon-reload"); err != nil {
		return err
	}
	if _, err = m.Systemctl(ctx, "enable", TunnelUnit); err != nil {
		return err
	}
	if c.AutoUpdate {
		if _, err = m.Systemctl(ctx, "enable", "--now", UpdateTimer); err != nil {
			return err
		}
	}
	if m.Scope.User {
		m.EnableLinger(ctx)
	}
	fmt.Fprintln(m.Out, "Starting the service...")
	if _, err = m.Systemctl(ctx, "restart", TunnelUnit); err != nil {
		if journal := m.Journal(ctx, TunnelUnit); journal != "" {
			err = fmt.Errorf("%w\n\nService log:\n%s", err, journal)
		}
		return fmt.Errorf("the service did not start: %w", err)
	}
	return nil
}

// removeTimer removes update units left by an install with auto-update.
func removeTimer(ctx context.Context, m *systemd.Manager) error {
	timer := filepath.Join(m.Scope.Dir, UpdateTimer)
	if _, err := os.Stat(timer); err == nil {
		if _, err = m.Systemctl(ctx, "disable", "--now", UpdateTimer); err != nil {
			return err
		}
	}
	for _, name := range []string{UpdateTimer, UpdateUnit} {
		err := os.Remove(filepath.Join(m.Scope.Dir, name))
		if err == nil {
			fmt.Fprintf(m.Out, "Removed %s\n", filepath.Join(m.Scope.Dir, name))
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// writeUnit atomically replaces the unit file name with content unless it
// already has that content.
func writeUnit(m *systemd.Manager, name, content string) error {
	path := filepath.Join(m.Scope.Dir, name)
	old, err := os.ReadFile(path)
	switch {
	case err == nil && bytes.Equal(old, []byte(content)):
		fmt.Fprintf(m.Out, "%s is up to date\n", path)
		return nil
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return err
	}
	if err := os.MkdirAll(m.Scope.Dir, 0o755); err != nil { //nolint:gosec // unit directories are world-readable
		return err
	}
	if err := atomicfile.Write(path, []byte(content), atomicfile.Options{Mode: 0o644, Prefix: ".tmp-" + name + "-"}); err != nil {
		return err
	}
	if old == nil {
		fmt.Fprintf(m.Out, "Installed %s\n", path)
	} else {
		fmt.Fprintf(m.Out, "Updated %s\n", path)
	}
	return nil
}

// removeUnits disables, removes and stops the units that exist. It reports
// whether there were any. Stopping comes last: run from inside the service,
// e.g. by the agent, it ends this process too.
func removeUnits(ctx context.Context, m *systemd.Manager) (bool, error) {
	found, err := m.Existing(TunnelUnit, UpdateUnit, UpdateTimer)
	if err != nil || len(found) == 0 {
		return false, err
	}
	var units, running []string
	for _, name := range found {
		// The update service has no [Install] section; its timer starts it.
		if name != UpdateUnit {
			units = append(units, name)
		}
		if m.Active(ctx, name) {
			running = append(running, name)
		}
	}
	if len(units) > 0 {
		if _, err = m.Systemctl(ctx, append([]string{"disable"}, units...)...); err != nil {
			return true, err
		}
	}
	for _, name := range found {
		path := filepath.Join(m.Scope.Dir, name)
		if err = os.Remove(path); err != nil {
			return true, err
		}
		fmt.Fprintf(m.Out, "Removed %s\n", path)
	}
	if _, err = m.Systemctl(ctx, "daemon-reload"); err != nil {
		return true, err
	}
	// Units removed while running stay loaded until they stop; stopping
	// the others would fail, since they are gone.
	if len(running) > 0 {
		if _, err = m.Systemctl(ctx, append([]string{"stop"}, running...)...); err != nil {
			return true, err
		}
	}
	// Forget a failed state, so the unit does not linger in systemctl --failed.
	_, _ = m.Systemctl(ctx, "reset-failed", TunnelUnit) //nolint:errcheck // fails if the unit never failed
	return true, nil
}
