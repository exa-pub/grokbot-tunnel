// Package install_test runs install.sh against a fake GitHub with a stub
// release, with the tools it uses linked into an isolated PATH and stubs for
// id and systemctl.
package install_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const version = "v1.2.3"

// tools are the commands install.sh and tar need, linked from the system.
var tools = []string{"bash", "cat", "mktemp", "rm", "sha256sum", "awk", "cut", "tar", "gzip", "mkdir", "install", "uname", "id", "sh"}

// release is the fake repository: what it serves and what it was asked for.
type release struct {
	archive   []byte
	checksums string
	latest    string // the tag /releases/latest redirects to; "" means no releases

	mu        sync.Mutex
	requested []string
}

// paths returns the paths requested so far.
func (r *release) paths() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.requested)
}

func newRelease(t *testing.T, binary string) *release {
	t.Helper()
	name := "grokbot-tunnel-linux-" + runtime.GOARCH
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(binary)), Typeflag: tar.TypeReg}))
	_, err := tw.Write([]byte(binary))
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	sum := sha256.Sum256(buf.Bytes())
	return &release{
		archive:   buf.Bytes(),
		checksums: hex.EncodeToString(sum[:]) + "  " + name + ".tar.gz\n",
		latest:    version,
	}
}

func (r *release) serve(t *testing.T) string {
	t.Helper()
	archive := "/releases/download/" + version + "/grokbot-tunnel-linux-" + runtime.GOARCH + ".tar.gz"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.requested = append(r.requested, req.URL.Path)
		r.mu.Unlock()
		switch req.URL.Path {
		case "/releases/latest":
			if r.latest == "" {
				http.Redirect(w, req, "/releases", http.StatusFound)
				return
			}
			http.Redirect(w, req, "/releases/tag/"+r.latest, http.StatusFound)
		case "/releases", "/releases/tag/" + version:
			fmt.Fprint(w, "page")
		case archive:
			_, _ = w.Write(r.archive)
		case "/releases/download/" + version + "/checksums.txt":
			fmt.Fprint(w, r.checksums)
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// env is one run of install.sh.
type env struct {
	home, bin string
	vars      []string
}

// newEnv returns an isolated PATH with the system tools, curl or wget as
// downloader, and stubs; uid is what id -u prints.
func newEnv(t *testing.T, downloader string, uid int) *env {
	t.Helper()
	e := &env{home: t.TempDir(), bin: t.TempDir()}
	for _, tool := range append(tools, downloader) {
		path, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("%s is not installed", tool)
		}
		require.NoError(t, os.Symlink(path, filepath.Join(e.bin, tool)))
	}
	require.NoError(t, os.Remove(filepath.Join(e.bin, "id")))
	e.stub(t, "id", fmt.Sprintf(`[ "$1" = -u ] && echo %d || exec /usr/bin/id "$@"`, uid))
	e.vars = []string{"HOME=" + e.home, "PATH=" + e.bin}
	return e
}

func (e *env) stub(t *testing.T, name, script string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(e.bin, name), []byte("#!/bin/sh\n"+script+"\n"), 0o755)) //nolint:gosec // a test command
}

// run runs install.sh with args against repo and returns its stderr.
func (e *env) run(t *testing.T, repo string, args ...string) (string, error) {
	t.Helper()
	c := exec.CommandContext(t.Context(), filepath.Join(e.bin, "bash"), append([]string{"../../install.sh"}, args...)...)
	c.Env = append(e.vars, "GROKBOT_TUNNEL_RELEASES_URL="+repo)
	var stderr bytes.Buffer
	c.Stderr = &stderr
	err := c.Run()
	return stderr.String(), err
}

func (e *env) installed() string { return filepath.Join(e.home, ".local", "bin", "grokbot-tunnel") }

const goodBinary = "#!/bin/sh\necho 'grokbot-tunnel " + version + "'\n"

func TestInstallLatest(t *testing.T) {
	for _, downloader := range []string{"curl", "wget"} {
		t.Run(downloader, func(t *testing.T) {
			rel := newRelease(t, goodBinary)
			e := newEnv(t, downloader, 1000)
			out, err := e.run(t, rel.serve(t))
			require.NoError(t, err, out)
			assert.Contains(t, out, "Checksum verified")
			assert.Contains(t, out, "Installed grokbot-tunnel "+version+" to "+e.installed())
			assert.Contains(t, out, ".local/bin is not in your PATH")
			b, err := os.ReadFile(e.installed())
			require.NoError(t, err)
			assert.Equal(t, goodBinary, string(b))
		})
	}
}

func TestInstallVersionAndDir(t *testing.T) {
	rel := newRelease(t, goodBinary)
	repo := rel.serve(t)
	e := newEnv(t, "curl", 1000)
	dir := filepath.Join(e.home, "custom bin")
	e.vars = append(e.vars, "GROKBOT_INSTALL_DIR="+dir, "PATH="+e.bin+":"+dir)
	out, err := e.run(t, repo, "1.2.3")
	require.NoError(t, err, out)
	assert.NotContains(t, rel.paths(), "/releases/latest")
	assert.FileExists(t, filepath.Join(dir, "grokbot-tunnel"))
	assert.NotContains(t, out, "not in your PATH")

	out, err = e.run(t, repo, "not-a-version")
	require.Error(t, err)
	assert.Contains(t, out, "Usage:")
}

func TestInstallChecksum(t *testing.T) {
	for _, tc := range []struct {
		name, checksums, skip, want string
		ok                          bool
	}{
		{"mismatch", strings.Repeat("0", 64) + "  grokbot-tunnel-linux-" + runtime.GOARCH + ".tar.gz\n", "", "checksum mismatch", false},
		{"missing", strings.Repeat("0", 64) + "  other.tar.gz\n", "", "is missing from checksums.txt", false},
		{"skipped", "", "1", "skipping checksum verification", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rel := newRelease(t, goodBinary)
			rel.checksums = tc.checksums
			e := newEnv(t, "curl", 1000)
			e.vars = append(e.vars, "GROKBOT_SKIP_CHECKSUM="+tc.skip)
			out, err := e.run(t, rel.serve(t))
			assert.Contains(t, out, tc.want)
			if tc.ok {
				require.NoError(t, err, out)
				assert.FileExists(t, e.installed())
			} else {
				require.Error(t, err)
				assert.NoFileExists(t, e.installed())
			}
		})
	}
}

func TestInstallNoRelease(t *testing.T) {
	rel := newRelease(t, goodBinary)
	rel.latest = ""
	repo := rel.serve(t)
	e := newEnv(t, "curl", 1000)
	out, err := e.run(t, repo)
	require.Error(t, err)
	assert.Contains(t, out, "could not determine the latest release")

	out, err = e.run(t, repo, "v9.9.9")
	require.Error(t, err)
	assert.Contains(t, out, "check that release v9.9.9 exists")
	assert.NoFileExists(t, e.installed())
}

func TestInstallBrokenBinary(t *testing.T) {
	for script, want := range map[string]string{
		"#!/bin/sh\nexit 1\n":        "was installed but does not run on this system",
		"#!/bin/sh\nkill -9 $$\n":    "was killed by signal 9 while starting",
		"#!/nonexistent/interpreter": "does not run on this system",
	} {
		rel := newRelease(t, script)
		e := newEnv(t, "curl", 1000)
		out, err := e.run(t, rel.serve(t))
		require.Error(t, err)
		assert.Contains(t, out, want, script)
	}
}

func TestInstallUnderSudo(t *testing.T) {
	rel := newRelease(t, goodBinary)
	repo := rel.serve(t)
	e := newEnv(t, "curl", 0)
	e.vars = append(e.vars, "SUDO_USER=alice")
	out, err := e.run(t, repo)
	require.Error(t, err)
	assert.Contains(t, out, "do not run this installer with sudo")
	assert.Empty(t, rel.paths(), "downloaded before refusing")

	e.vars = append(e.vars, "GROKBOT_INSTALL_ALLOW_SUDO=1")
	out, err = e.run(t, repo)
	require.NoError(t, err, out)
	assert.FileExists(t, e.installed())
}

// A running service of this binary is restarted, in the user manager for a
// user and the system manager for root; a service of another binary is not.
func TestInstallRestartsService(t *testing.T) {
	for _, tc := range []struct {
		uid     int
		ours    bool
		restart string
	}{
		{1000, true, "--user restart --no-block grokbot-tunnel"},
		{0, true, "restart --no-block grokbot-tunnel"},
		{1000, false, ""},
	} {
		rel := newRelease(t, goodBinary)
		e := newEnv(t, "curl", tc.uid)
		path := e.installed()
		if !tc.ours {
			path = "/opt/other/grokbot-tunnel"
		}
		log := filepath.Join(e.home, "systemctl.log")
		e.stub(t, "systemctl", fmt.Sprintf(`echo "$*" >> %q
case "$*" in
*is-active*) exit 0 ;;
*show*) echo "{ path=%s ; argv[]=%s connect ; ignore_errors=no }" ;;
esac`, log, path, path))
		out, err := e.run(t, rel.serve(t))
		require.NoError(t, err, out)
		calls, _ := os.ReadFile(log)
		if tc.restart == "" {
			assert.NotContains(t, string(calls), "restart", out)
			continue
		}
		assert.Contains(t, string(calls), tc.restart+"\n")
		assert.Contains(t, out, "Restarting the grokbot-tunnel service")
	}
}

func TestInstallWarnsAboutOldBinary(t *testing.T) {
	rel := newRelease(t, goodBinary)
	e := newEnv(t, "curl", 1000)
	e.stub(t, "grokbot-cli", "true")
	out, err := e.run(t, rel.serve(t))
	require.NoError(t, err, out)
	assert.Contains(t, out, "Found the old grokbot-cli binary at "+filepath.Join(e.bin, "grokbot-cli"))
}
