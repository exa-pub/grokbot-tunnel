//go:build e2e

package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildVersion builds grokbot-tunnel reporting version into dir.
func buildVersion(t *testing.T, dir, version string) string {
	t.Helper()
	out := filepath.Join(dir, "grokbot-tunnel-"+version)
	ldflags := "-X github.com/exa-pub/grokbot-tunnel/internal/api/cli.version=" + version
	build := exec.CommandContext(context.Background(), "go", "build", "-ldflags", ldflags, "-o", out, "../../cmd/grokbot-tunnel")
	b, err := build.CombinedOutput()
	require.NoError(t, err, string(b))
	return out
}

// fakeGitHub serves releases of the given binaries, latest last, the way
// GitHub does: /releases/latest redirects to the tag, assets live under
// /releases/download/<tag>/. corrupt breaks the checksum of a tag.
type fakeGitHub struct {
	assets map[string][]byte
	latest string

	mu      sync.Mutex
	corrupt string
}

func (gh *fakeGitHub) setCorrupt(tag string) {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	gh.corrupt = tag
}

func newFakeGitHub(t *testing.T, binaries map[string]string, latest string) (*fakeGitHub, string) {
	t.Helper()
	gh := &fakeGitHub{assets: map[string][]byte{}, latest: latest}
	name := "grokbot-tunnel-linux-" + runtime.GOARCH
	for tag, path := range binaries {
		bin, err := os.ReadFile(path)
		require.NoError(t, err)
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(bin)), Typeflag: tar.TypeReg}))
		_, err = tw.Write(bin)
		require.NoError(t, err)
		require.NoError(t, tw.Close())
		require.NoError(t, gz.Close())
		gh.assets[tag+"/"+name+".tar.gz"] = buf.Bytes()
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/releases/latest" {
			http.Redirect(w, r, "/releases/tag/"+gh.latest, http.StatusFound)
			return
		}
		asset, ok := strings.CutPrefix(r.URL.Path, "/releases/download/")
		if tag, ok2 := strings.CutSuffix(asset, "/checksums.txt"); ok && ok2 {
			archive := gh.assets[tag+"/"+name+".tar.gz"]
			sum := sha256.Sum256(archive)
			gh.mu.Lock()
			corrupt := tag == gh.corrupt
			gh.mu.Unlock()
			if corrupt {
				sum = sha256.Sum256(nil)
			}
			fmt.Fprintf(w, "%s  %s.tar.gz\n", hex.EncodeToString(sum[:]), name)
			return
		}
		if b, found := gh.assets[asset]; ok && found {
			_, _ = w.Write(b)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return gh, srv.URL
}

// update runs exe update args against repo and returns its output and exit
// code.
func update(t *testing.T, exe, repo string, args ...string) (string, int) {
	t.Helper()
	home := t.TempDir()
	c := &cli{home: home, env: []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "GROKBOT_TUNNEL_RELEASES_URL=" + repo}}
	cmd, out := c.command(context.Background(), append([]string{"update", "--no-restart"}, args...)...)
	cmd.Path, cmd.Args[0] = exe, exe
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return out.String(), exit.ExitCode()
	}
	require.NoError(t, err, out.String())
	return out.String(), 0
}

func installedVersion(t *testing.T, exe string) string {
	t.Helper()
	b, err := exec.CommandContext(context.Background(), exe, "--version").Output()
	require.NoError(t, err)
	return strings.TrimSpace(string(b))
}

// The built binary updates itself from a fake GitHub: check, update, up to
// date, refused downgrade, forced downgrade, and a bad checksum that leaves
// the binary alone.
func TestUpdate(t *testing.T) {
	build := t.TempDir()
	old, current := buildVersion(t, build, "v1.0.0"), buildVersion(t, build, "v1.1.0")
	gh, repo := newFakeGitHub(t, map[string]string{"v1.0.0": old, "v1.1.0": current}, "v1.1.0")

	dir := t.TempDir()
	exe := filepath.Join(dir, "grokbot-tunnel")
	b, err := os.ReadFile(old)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(exe, b, 0o755)) //nolint:gosec // an executable

	out, code := update(t, exe, repo, "--check")
	assert.Equal(t, 0, code, out)
	assert.Contains(t, out, "Installed: v1.0.0\nAvailable: v1.1.0 (an update is available)")

	out, code = update(t, exe, repo)
	assert.Equal(t, 0, code, out)
	assert.Contains(t, out, "Updated grokbot-tunnel v1.0.0 -> v1.1.0")
	assert.Contains(t, installedVersion(t, exe), "v1.1.0")

	out, code = update(t, exe, repo)
	assert.Equal(t, 0, code, out)
	assert.Contains(t, out, "is up to date")

	out, code = update(t, exe, repo, "v1.0.0")
	assert.NotEqual(t, 0, code, out)
	assert.Contains(t, out, "use --force to downgrade")

	gh.setCorrupt("v1.0.0")
	out, code = update(t, exe, repo, "--force", "v1.0.0")
	assert.NotEqual(t, 0, code, out)
	assert.Contains(t, installedVersion(t, exe), "v1.1.0", "binary replaced despite a bad checksum")

	gh.setCorrupt("")
	out, code = update(t, exe, repo, "--force", "1.0.0")
	assert.Equal(t, 0, code, out)
	assert.Contains(t, installedVersion(t, exe), "v1.0.0")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "temporary files left behind")
}

// A development build is only replaced with --force.
func TestUpdateDevelopmentBuild(t *testing.T) {
	build := t.TempDir()
	_, repo := newFakeGitHub(t, map[string]string{"v1.1.0": buildVersion(t, build, "v1.1.0")}, "v1.1.0")
	exe := filepath.Join(t.TempDir(), "grokbot-tunnel")
	b, err := os.ReadFile(binary)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(exe, b, 0o755)) //nolint:gosec // an executable

	out, code := update(t, exe, repo)
	assert.NotEqual(t, 0, code, out)
	assert.Contains(t, out, "this is a development build")
	out, code = update(t, exe, repo, "--force")
	assert.Equal(t, 0, code, out)
	assert.Contains(t, installedVersion(t, exe), "v1.1.0")
}
