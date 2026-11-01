//go:build e2e

package e2e

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/exa-pub/grokbot-tunnel/tests/e2e/fakesand"
)

// connect tells systemd it is ready only once the server accepted the
// computer on the watch stream, so that install, which waits for it, does
// not report a connection that is not there.
func TestReadyAfterConnected(t *testing.T) {
	cloud := fakesand.New(t)
	hold := make(chan struct{})
	cloud.HoldConnected = hold
	c := newCLI(t, cloud)
	out, code := c.run(t, "login", "--no-browser")
	require.Equal(t, 0, code, out)

	dir, err := os.MkdirTemp("", "notify")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: sock, Net: "unixgram"})
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	states := make(chan string, 8)
	go func() {
		buf := make([]byte, 256)
		for {
			n, _, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			states <- string(buf[:n])
		}
	}()
	c.env = append(c.env, "NOTIFY_SOCKET="+sock)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd, log := c.command(ctx, "connect", "--permission", "always", "--root", t.TempDir())
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
	select {
	case state := <-states:
		t.Fatalf("%q before the watch connected:\n%s", state, log.String())
	case <-time.After(500 * time.Millisecond):
	}
	close(hold)
	select {
	case state := <-states:
		require.Equal(t, "READY=1", state)
	case <-time.After(10 * time.Second):
		t.Fatalf("no READY=1 after the watch connected:\n%s", log.String())
	}

	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	select {
	case err := <-exited:
		require.NoError(t, err, log.String())
	case <-time.After(10 * time.Second):
		t.Fatalf("no exit after SIGTERM:\n%s", log.String())
	}
}
