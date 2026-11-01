//go:build systemd

// Package systemd runs install, update and uninstall of the service against a
// real systemd, as root in the system scope and as an unprivileged user in the
// user scope. It runs inside the container of run.sh; elsewhere it skips.
package systemd

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	"github.com/exa-pub/grokbot-tunnel/tests/e2e/fakesand"
)

const (
	unit       = "grokbot-tunnel.service"
	timer      = "grokbot-tunnel-update.timer"
	testUser   = "tester"
	newVersion = "v9.9.9"
)

// scope runs commands as root or as testUser.
type scope struct {
	user *user.User // nil: root, system scope
	bin  string     // where grokbot-tunnel is installed
	home string
	env  []string
}

func newScope(t *testing.T, name string) *scope {
	t.Helper()
	if name == "" {
		return &scope{bin: "/usr/local/bin/grokbot-tunnel", home: "/root"}
	}
	u, err := user.Lookup(name)
	require.NoError(t, err)
	s := &scope{user: u, bin: filepath.Join(u.HomeDir, ".local", "bin", "grokbot-tunnel"), home: u.HomeDir}
	s.env = []string{"XDG_RUNTIME_DIR=/run/user/" + u.Uid}
	return s
}

// command returns name with args, run as the scope's user with env.
func (s *scope) command(env []string, name string, args ...string) *exec.Cmd {
	env = append(append([]string{
		"HOME=" + s.home, "PATH=" + filepath.Dir(s.bin) + ":/usr/local/bin:/usr/bin:/bin", "SHELL=/bin/bash",
	}, s.env...), env...)
	if s.user == nil {
		c := exec.CommandContext(context.Background(), name, args...)
		c.Env = env
		return c
	}
	all := append(append([]string{"-u", s.user.Username, "--", "env", "-i"}, env...), name)
	return exec.CommandContext(context.Background(), "runuser", append(all, args...)...)
}

func (s *scope) run(t *testing.T, env []string, name string, args ...string) string {
	t.Helper()
	out, err := s.command(env, name, args...).CombinedOutput()
	require.NoError(t, err, "%s %s: %s", name, strings.Join(args, " "), out)
	return string(out)
}

func (s *scope) systemctl(args ...string) (string, error) {
	if s.user != nil {
		args = append([]string{"--user"}, args...)
	}
	out, err := s.command(nil, "systemctl", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (s *scope) property(t *testing.T, name string) string {
	t.Helper()
	out, err := s.systemctl("show", "--property="+name, "--value", unit)
	require.NoError(t, err, out)
	return out
}

// installBinary copies the binary of env var name to the scope's bin.
func (s *scope) installBinary(t *testing.T, name string) {
	t.Helper()
	b, err := os.ReadFile(os.Getenv(name))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(s.bin), 0o755))
	require.NoError(t, os.WriteFile(s.bin, b, 0o755)) //nolint:gosec // an executable
	if s.user != nil {
		uid, _ := strconv.Atoi(s.user.Uid)
		gid, _ := strconv.Atoi(s.user.Gid)
		for _, p := range []string{filepath.Join(s.home, ".local"), filepath.Dir(s.bin), s.bin} {
			require.NoError(t, os.Chown(p, uid, gid))
		}
	}
}

// releases serves a fake GitHub repository whose latest release holds the
// binary of GROK_RELEASE_BIN.
func releases(t *testing.T) string {
	t.Helper()
	bin, err := os.ReadFile(os.Getenv("GROK_RELEASE_BIN"))
	require.NoError(t, err)
	name := "grokbot-tunnel-linux-" + runtime.GOARCH
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(bin))}))
	_, err = tw.Write(bin)
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	archive := buf.Bytes()
	sum := sha256.Sum256(archive)
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/releases/tag/"+newVersion, http.StatusFound)
	})
	mux.HandleFunc("/releases/download/"+newVersion+"/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "%s  %s.tar.gz\n", hex.EncodeToString(sum[:]), name)
	})
	mux.HandleFunc("/releases/download/"+newVersion+"/"+name+".tar.gz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write(archive) //nolint:errcheck // test server
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// shell runs command through the service and waits for its stdout.
func shell(t *testing.T, cloud *fakesand.Server, id, command, want string) {
	t.Helper()
	cloud.Enqueue(fakesand.Exec(id, fmt.Sprintf(`{"id":1,"shellStreamArgs":{"command":%q}}`, command)))
	cloud.Wait(30*time.Second, func(frames []*aiserverv1.GrokBotUserComputerResponseFrame) bool {
		var out strings.Builder
		closed := false
		for _, f := range frames {
			if f.RequestId != id {
				continue
			}
			out.WriteString(fakesand.Client(f).GetShellStream().GetStdout().GetData())
			closed = closed || fakesand.Control(f).GetStreamClose() != nil
		}
		return closed && out.String() == want
	})
}

func TestMain(m *testing.M) {
	if os.Getuid() != 0 || os.Getenv("GROK_BIN") == "" {
		fmt.Println("skipped: run tests/systemd/run.sh")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestSystemScope(t *testing.T) { testLifecycle(t, newScope(t, ""), true) }

func TestUserScope(t *testing.T) {
	// The user manager runs without a session only with lingering, which
	// install would enable through polkit; enable it as root instead.
	out, err := exec.CommandContext(context.Background(), "loginctl", "enable-linger", testUser).CombinedOutput()
	require.NoError(t, err, string(out))
	s := newScope(t, testUser)
	require.Eventually(t, func() bool {
		_, err := os.Stat("/run/user/" + s.user.Uid + "/systemd/private")
		return err == nil
	}, 30*time.Second, 100*time.Millisecond, "user manager did not start")
	testLifecycle(t, s, false)
}

// testLifecycle logs in, installs the service, checks that it serves,
// updates it to a new release, which restarts it, and uninstalls it.
func testLifecycle(t *testing.T, s *scope, autoUpdate bool) {
	cloud := fakesand.New(t)
	env := append(cloud.Env(), "GROKBOT_TUNNEL_RELEASES_URL="+releases(t))
	s.installBinary(t, "GROK_BIN")

	assert.Contains(t, s.run(t, env, s.bin, "login", "--no-browser"), "Logged in as "+fakesand.Email)
	args := []string{"install", "--permission", "always"}
	if !autoUpdate {
		args = append(args, "--no-auto-update")
	}
	assert.Contains(t, s.run(t, env, s.bin, args...), "The service is connected")

	state, _ := s.systemctl("is-active", unit)
	assert.Equal(t, "active", state)
	assert.Equal(t, "notify", s.property(t, "Type"))
	if autoUpdate {
		enabled, _ := s.systemctl("is-enabled", timer)
		assert.Equal(t, "enabled", enabled)
	}
	shell(t, cloud, "before", "printf before", "before")
	pid := s.property(t, "MainPID")

	out := s.run(t, env, s.bin, "update")
	assert.Contains(t, out, "Updated grokbot-tunnel v1.0.0 -> "+newVersion)
	assert.Contains(t, out, "Restarting the service")
	require.Eventually(t, func() bool {
		state, _ := s.systemctl("is-active", unit)
		return state == "active" && s.property(t, "MainPID") != pid
	}, 30*time.Second, 200*time.Millisecond, "service not restarted")
	assert.Contains(t, s.run(t, nil, s.bin, "--version"), newVersion)
	shell(t, cloud, "after", "printf after", "after")

	assert.Contains(t, s.run(t, env, s.bin, "uninstall"), "Uninstalled the service")
	state, _ = s.systemctl("is-active", unit)
	assert.NotEqual(t, "active", state)
	dir := "/etc/systemd/system"
	if s.user != nil {
		dir = filepath.Join(s.home, ".config", "systemd", "user")
	}
	_, err := os.Stat(filepath.Join(dir, unit))
	assert.ErrorIs(t, err, os.ErrNotExist)
}
