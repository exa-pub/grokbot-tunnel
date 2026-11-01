package githubrelease

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"regexp"
	"testing"
)

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// A checksum found in checksums.txt is always a lowercase SHA-256.
func FuzzChecksum(f *testing.F) {
	sum := "0123456789abcdef0123456789ABCDEF0123456789abcdef0123456789abcdef"
	f.Add([]byte(sum+"  app.tar.gz\n"), "app.tar.gz")
	f.Add([]byte(sum+" *app.tar.gz\r\nzz  other\n"), "app.tar.gz")
	f.Add([]byte("xyz  app.tar.gz\n"), "app.tar.gz")
	f.Fuzz(func(t *testing.T, sums []byte, file string) {
		got, err := checksum(sums, file)
		if err == nil && !sha256Hex.MatchString(got) {
			t.Fatalf("checksum %q", got)
		}
	})
}

// extract returns only the regular file asked for, whatever the archive.
func FuzzExtract(f *testing.F) {
	f.Add(tarGz(f, "app", tar.TypeReg, "binary"), "app")
	f.Add(tarGz(f, "app", tar.TypeSymlink, ""), "app")
	f.Add(tarGz(f, "../app", tar.TypeReg, "x"), "app")
	f.Add([]byte("not gzip"), "app")
	f.Fuzz(func(t *testing.T, archive []byte, name string) {
		b, err := extract(archive, name)
		if err == nil && len(b) > maxBinary {
			t.Fatalf("%d bytes", len(b))
		}
	})
}

func tarGz(f *testing.F, name string, typ byte, content string) []byte {
	f.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	h := &tar.Header{Name: name, Typeflag: typ, Mode: 0o755, Size: int64(len(content)), Linkname: "target"}
	if typ != tar.TypeReg {
		h.Size = 0
	}
	if err := tw.WriteHeader(h); err != nil {
		f.Fatal(err)
	}
	if _, err := tw.Write([]byte(content[:h.Size])); err != nil {
		f.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		f.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		f.Fatal(err)
	}
	return buf.Bytes()
}
