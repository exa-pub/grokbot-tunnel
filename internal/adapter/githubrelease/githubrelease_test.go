package githubrelease

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
	"strings"
	"testing"
)

func script(version string) []byte {
	return []byte("#!/bin/sh\necho 'grokbot-tunnel " + version + " (Sand protocol 0.78.4)'\n")
}

type entry struct {
	name string
	typ  byte
	body []byte
}

func tarball(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: 0o755, Size: int64(len(e.body))}
		if e.typ == tar.TypeSymlink {
			h.Linkname, h.Size = "/bin/sh", 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			tw.Write(e.body)
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// release serves a fake GitHub repository whose latest release is v1.2.3.
type release struct {
	archive []byte
	sums    string
	latest  string
}

func (r *release) serve(t *testing.T) *Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, r.latest, http.StatusFound)
	})
	mux.HandleFunc("/releases/download/v1.2.3/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, r.sums)
	})
	mux.HandleFunc("/releases/download/v1.2.3/grokbot-tunnel-linux-amd64.tar.gz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write(r.archive)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &Client{Base: srv.URL, Arch: "amd64", UserAgent: "test"}
}

func goodRelease(t *testing.T, body []byte) *release {
	t.Helper()
	archive := tarball(t, entry{"README", tar.TypeReg, []byte("x")}, entry{"grokbot-tunnel-linux-amd64", tar.TypeReg, body})
	sum := sha256.Sum256(archive)
	return &release{
		archive: archive,
		sums:    hex.EncodeToString(sum[:]) + "  grokbot-tunnel-linux-amd64.tar.gz\n",
		latest:  "/exa-pub/grokbot-tunnel/releases/tag/v1.2.3",
	}
}

func TestLatest(t *testing.T) {
	r := goodRelease(t, script("v1.2.3"))
	c := r.serve(t)
	if v, err := c.Latest(context.Background()); err != nil || v != "v1.2.3" {
		t.Fatal(v, err)
	}
	// Without releases GitHub redirects elsewhere.
	r.latest = "/exa-pub/grokbot-tunnel/releases"
	if v, err := c.Latest(context.Background()); err == nil {
		t.Fatal("no error for", v)
	}
}

func TestFetchRejectsBadReleases(t *testing.T) {
	good := goodRelease(t, script("v1.2.3"))
	for name, r := range map[string]*release{
		"checksum mismatch": {archive: good.archive, sums: strings.Repeat("0", 64) + "  grokbot-tunnel-linux-amd64.tar.gz\n"},
		"missing checksum":  {archive: good.archive, sums: strings.Repeat("0", 64) + "  other.tar.gz\n"},
		"missing binary":    signed(tarball(t, entry{"grokbot-tunnel-linux-arm64", tar.TypeReg, []byte("x")})),
		"symlink":           signed(tarball(t, entry{"grokbot-tunnel-linux-amd64", tar.TypeSymlink, nil})),
		"not gzip":          signed([]byte("not an archive")),
	} {
		if _, err := r.serve(t).Fetch(context.Background(), "v1.2.3"); err == nil {
			t.Error(name, "accepted")
		}
	}
	if _, err := good.serve(t).Fetch(context.Background(), "v9.9.9"); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Error("missing release:", err)
	}
}

func signed(archive []byte) *release {
	sum := sha256.Sum256(archive)
	return &release{archive: archive, sums: hex.EncodeToString(sum[:]) + " *grokbot-tunnel-linux-amd64.tar.gz\n"}
}

func TestFetch(t *testing.T) {
	c := goodRelease(t, script("v1.2.3")).serve(t)
	bin, err := c.Fetch(context.Background(), "v1.2.3")
	if err != nil || !bytes.Equal(bin, script("v1.2.3")) {
		t.Fatal(string(bin), err)
	}
}
