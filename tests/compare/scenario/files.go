package scenario

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// File is a file, directory or link a scenario creates. Paths are relative
// to the directory the list is for and may not leave it.
type File struct {
	Path string
	// Data is the content of a file.
	Data []byte `json:",omitempty"`
	// Repeat writes Data that many times instead of once.
	Repeat int `json:",omitempty"`
	// Seq writes the numbers from 1 to Seq, a line each, instead.
	Seq int `json:",omitempty"`
	// Size makes a sparse file of that size instead.
	Size int64 `json:",omitempty"`
	// Link makes a symbolic link to that target instead.
	Link string `json:",omitempty"`
	// Dir makes a directory instead.
	Dir bool `json:",omitempty"`
	// Mode is the permission bits; 0644 for files and 0755 for directories
	// by default.
	Mode fs.FileMode `json:",omitempty"`
	// Remove removes the path instead.
	Remove bool `json:",omitempty"`
}

// Text is a file with the given content.
func Text(path, content string) File { return File{Path: path, Data: []byte(content)} }

// Bytes is a file with the given content.
func Bytes(path string, content []byte) File { return File{Path: path, Data: content} }

// Seq is a file with the numbers from 1 to n, a line each.
func Seq(path string, n int) File { return File{Path: path, Seq: n} }

// Sparse is a file of the given size that takes no space.
func Sparse(path string, size int64) File { return File{Path: path, Size: size} }

// Dir is a directory.
func Dir(path string) File { return File{Path: path, Dir: true} }

// Link is a symbolic link.
func Link(path, target string) File { return File{Path: path, Link: target} }

// WithMode returns f with the permission bits mode.
func (f File) WithMode(mode fs.FileMode) File {
	f.Mode = mode
	return f
}

// Created is the modification time every fixture gets: clients report
// times of files, and the answers must not depend on when the test ran.
var Created = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// Create makes the files below dir, in order.
func Create(dir string, files []File) error {
	for _, f := range files {
		if err := f.create(dir); err != nil {
			return fmt.Errorf("fixture %s: %w", f.Path, err)
		}
	}
	// Directories last: creating a file changes the time of its directory.
	for _, f := range files {
		for path := filepath.Join(dir, f.Path); len(path) > len(dir) && !f.Remove; path = filepath.Dir(path) {
			if info, err := os.Lstat(path); err == nil && info.Mode()&fs.ModeSymlink == 0 {
				if err := os.Chtimes(path, Created, Created); err != nil && !errors.Is(err, fs.ErrPermission) {
					return fmt.Errorf("fixture %s: %w", f.Path, err)
				}
			}
		}
	}
	return nil
}

func (f File) create(dir string) error {
	if !filepath.IsLocal(f.Path) {
		return fmt.Errorf("path leaves the directory")
	}
	path := filepath.Join(dir, f.Path)
	if f.Remove {
		return os.RemoveAll(path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // a fixture directory
		return err
	}
	switch {
	case f.Link != "":
		return os.Symlink(strings.ReplaceAll(f.Link, "$DIR", dir), path)
	case f.Dir:
		mode := f.Mode
		if mode == 0 {
			mode = 0o755
		}
		if err := os.MkdirAll(path, 0o755); err != nil { //nolint:gosec // a fixture directory
			return err
		}
		return os.Chmod(path, mode)
	}
	mode := f.Mode
	if mode == 0 {
		mode = 0o644
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644) //nolint:gosec // a fixture file
	if err != nil {
		return err
	}
	defer file.Close() //nolint:errcheck // the write below reports errors
	if err := f.write(file); err != nil {
		return err
	}
	return file.Chmod(mode)
}

// write writes the content of a regular file.
func (f File) write(file *os.File) error {
	switch {
	case f.Seq > 0:
		w := bufio.NewWriter(file)
		for i := 1; i <= f.Seq; i++ {
			fmt.Fprintln(w, i) //nolint:errcheck // Flush reports the error
		}
		return w.Flush()
	case f.Size > 0:
		return file.Truncate(f.Size)
	case f.Repeat > 0:
		_, err := file.Write(bytes.Repeat(f.Data, f.Repeat))
		return err
	}
	_, err := file.Write(f.Data)
	return err
}
