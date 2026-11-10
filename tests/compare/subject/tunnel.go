package subject

import (
	"context"
	"os/exec"
	"testing"
)

// Tunnel is grokbot-tunnel: the built binary, logged in against the fake
// backend and connected.
type Tunnel struct {
	// Binary is the path of the built grokbot-tunnel.
	Binary string
}

// Name implements Subject.
func (Tunnel) Name() string { return "go" }

// Start implements Subject.
func (s Tunnel) Start(t testing.TB, env Env) *Running {
	t.Helper()
	requireImage(t, "base", "desktop")
	environ := env.environ("SAND_BACKEND_URL="+env.BackendURL, "SAND_CURSOR_WEBSITE_URL=https://example.invalid")
	run(t, environ, s.Binary, "login", "--no-browser")
	//nolint:gosec // the client under test
	cmd := exec.CommandContext(context.Background(), s.Binary, "connect", "--permission", env.Permission, "--root", env.Root)
	cmd.Env = environ
	return start(t, env, cmd)
}
