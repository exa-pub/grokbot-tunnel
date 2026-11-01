package systemd

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSystem records commands and answers them from replies, keyed by the
// command line.
type fakeSystem struct {
	calls   []string
	replies map[string]string
}

func (f *fakeSystem) run(_ context.Context, name string, args ...string) ([]byte, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, line)
	return []byte(f.replies[line]), nil
}

func TestExecPath(t *testing.T) {
	f := &fakeSystem{replies: map[string]string{}}
	m := &Manager{Scope: Scope{User: true}, Run: f.run}
	f.replies["systemctl --user show --property=ExecStart --value grokbot-tunnel.service"] =
		"{ path=/home/u/.local/bin/grokbot-tunnel ; argv[]=/home/u/.local/bin/grokbot-tunnel connect ; ignore_errors=no }\n"
	if p, err := m.ExecPath(context.Background(), "grokbot-tunnel.service"); err != nil || p != "/home/u/.local/bin/grokbot-tunnel" {
		t.Fatal(p, err)
	}
}

func TestNotify(t *testing.T) {
	if err := (&Notifier{}).Notify("READY=1"); err != nil {
		t.Fatal("notify outside systemd:", err)
	}
	for _, addr := range []string{filepath.Join(t.TempDir(), "notify"), fmt.Sprintf("@grokbot-test-%d", os.Getpid())} {
		conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: addr, Net: "unixgram"})
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("NOTIFY_SOCKET", addr)
		n := NewNotifier()
		if _, set := os.LookupEnv("NOTIFY_SOCKET"); set {
			t.Fatal("NOTIFY_SOCKET left in the environment")
		}
		if err = n.Notify("READY=1"); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 64)
		k, err := conn.Read(buf)
		conn.Close()
		if err != nil || string(buf[:k]) != "READY=1" {
			t.Fatal(addr, string(buf[:k]), err)
		}
	}
}

func TestOwner(t *testing.T) {
	old := procDir
	procDir = t.TempDir()
	t.Cleanup(func() { procDir = old })
	for pid, cgroup := range map[int]string{
		1: "0::/user.slice/user-1000.slice/user@1000.service/app.slice/grokbot-tunnel.service\n",
		2: "0::/system.slice/grokbot-tunnel.service\n",
		3: "0::/user.slice/user-1000.slice/session-4.scope\n",
	} {
		os.MkdirAll(filepath.Join(procDir, fmt.Sprint(pid)), 0o755)
		os.WriteFile(filepath.Join(procDir, fmt.Sprint(pid), "cgroup"), []byte(cgroup), 0o644)
	}
	if s, ok := Owner(1, "grokbot-tunnel.service"); !ok || !s.User {
		t.Error("user service", s, ok)
	}
	if s, ok := Owner(2, "grokbot-tunnel.service"); !ok || s.User {
		t.Error("system service", s, ok)
	}
	for _, pid := range []int{3, 4} {
		if _, ok := Owner(pid, "grokbot-tunnel.service"); ok {
			t.Error("not the service", pid)
		}
	}
}

func TestDetectScope(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("runs as root")
	}
	t.Setenv("XDG_CONFIG_HOME", "/cfg")
	if s, err := DetectScope(); err != nil || s != (Scope{User: true, Dir: "/cfg/systemd/user"}) {
		t.Fatal(s, err)
	}
	t.Setenv("XDG_CONFIG_HOME", "relative")
	t.Setenv("HOME", "/home/u")
	if s, err := DetectScope(); err != nil || s.Dir != "/home/u/.config/systemd/user" {
		t.Fatal(s, err)
	}
}
