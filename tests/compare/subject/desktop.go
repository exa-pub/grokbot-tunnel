package subject

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/exa-pub/grokbot-tunnel/tests/e2e/fakesand"
)

// Desktop is the desktop client's local-exec daemon, started by
// js/launcher.mjs the way the desktop's main process starts it.
type Desktop struct {
	// Version is the desktop release.
	Version string
	// Dist is the app/dist directory of the unpacked bundle.
	Dist string
	// Launcher is the path of launcher.mjs.
	Launcher string
}

// machineID is the ID the desktop daemon is given; grokbot-tunnel makes its
// own.
const machineID = "00000000-0000-4000-8000-00000000c0de"

// Name implements Subject.
func (s Desktop) Name() string { return "desktop-" + s.Version }

// Start implements Subject.
func (s Desktop) Start(t testing.TB, env Env) *Running {
	t.Helper()
	requireImage(t, "desktop")
	environ := map[string]string{}
	for _, kv := range env.environ() {
		name, value, _ := strings.Cut(kv, "=")
		environ[name] = value
	}
	config, err := json.Marshal(map[string]any{
		"dist":                s.Dist,
		"dataRoot":            filepath.Join(env.Home, ".grokbot"),
		"root":                env.Root,
		"backendUrl":          env.BackendURL,
		"machineId":           machineID,
		"credential":          fakesand.Credential,
		"accountScope":        fakesand.AccountID,
		"serverAuthoritative": env.ServerAuthoritative,
		"permission":          env.Permission,
		"patches":             env.Patches,
		"env":                 environ,
	})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(filepath.Dir(env.Home), "launcher.json")
	if err := os.WriteFile(file, config, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(context.Background(), "node", s.Launcher, file) //nolint:gosec // the client under test
	// The launcher itself needs the image marker and a PATH; the daemon gets
	// the environment of the config.
	cmd.Env = append(os.Environ(), marker+"="+env.Run)
	return start(t, env, cmd)
}
