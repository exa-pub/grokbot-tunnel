//go:build e2e

// Package e2e runs the built grokbot-tunnel binary against the fake backend
// of package fakesand. Run with "make e2e" or
// "go test -tags e2e ./tests/e2e/".
package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	"github.com/exa-pub/grokbot-tunnel/internal/service/install"
	"github.com/exa-pub/grokbot-tunnel/tests/e2e/fakesand"
)

// binary is the grokbot-tunnel built once for all tests.
var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "grokbot-e2e-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	binary = filepath.Join(dir, "grokbot-tunnel")
	build := exec.CommandContext(context.Background(), "go", "build", "-o", binary, "../../cmd/grokbot-tunnel")
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build:", err)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// cli runs the binary in its own home, pointed at the fake backend.
type cli struct {
	home string
	env  []string
}

func newCLI(t *testing.T, cloud *fakesand.Server) *cli {
	t.Helper()
	home := t.TempDir()
	env := append([]string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "SHELL=/bin/bash"}, cloud.Env()...)
	return &cli{home: home, env: env}
}

func (c *cli) command(ctx context.Context, args ...string) (*exec.Cmd, *bytes.Buffer) {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = c.env
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	return cmd, &out
}

func (c *cli) run(t *testing.T, args ...string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd, out := c.command(ctx, args...)
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return out.String(), exit.ExitCode()
	}
	require.NoError(t, err, out.String())
	return out.String(), 0
}

func TestVersion(t *testing.T) {
	out, code := newCLI(t, fakesand.New(t)).run(t, "--version")
	assert.Equal(t, 0, code)
	assert.Contains(t, out, "grokbot-tunnel")
}

// connect without a login exits with the code that tells systemd not to
// restart.
func TestConnectWithoutLogin(t *testing.T) {
	out, code := newCLI(t, fakesand.New(t)).run(t, "connect", "--permission", "always")
	assert.Equal(t, install.ExitLogin, code, out)
}

// login --no-browser, connect, one shell command, SIGTERM: the process
// answers the request and exits cleanly.
func TestLoginConnectShellSigterm(t *testing.T) {
	cloud := fakesand.New(t)
	c := newCLI(t, cloud)
	out, code := c.run(t, "login", "--no-browser")
	require.Equal(t, 0, code, out)
	require.Contains(t, out, "Logged in as "+fakesand.Email)

	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd, log := c.command(ctx, "connect", "--permission", "always", "--root", root)
	require.NoError(t, cmd.Start())
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			<-exited
		}
	})

	cloud.WaitWatching(10 * time.Second)
	cloud.Enqueue(fakesand.Exec("shell", `{"id":1,"shellStreamArgs":{"command":"printf shell-ok; pwd >&2"}}`))
	frames := cloud.Wait(10*time.Second, func(frames []*aiserverv1.GrokBotUserComputerResponseFrame) bool {
		for _, f := range frames {
			if fakesand.Control(f).GetStreamClose() != nil {
				return true
			}
		}
		return false
	})
	var stdout, stderr strings.Builder
	for _, f := range frames {
		s := fakesand.Client(f).GetShellStream()
		stdout.WriteString(s.GetStdout().GetData())
		stderr.WriteString(s.GetStderr().GetData())
	}
	assert.Equal(t, "shell-ok", stdout.String())
	assert.Equal(t, root+"\n", stderr.String())

	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	select {
	case err := <-exited:
		require.NoError(t, err, log.String())
	case <-time.After(10 * time.Second):
		t.Fatalf("no exit after SIGTERM:\n%s", log.String())
	}
}
