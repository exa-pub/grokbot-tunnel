package proc

import (
	"errors"
	"io"
	"log"
	"os"
)

// TempFile returns an unlinked temporary file in dir holding content,
// positioned at its start; only open descriptors keep it alive.
func TempFile(dir, prefix, content string) (*os.File, error) {
	f, err := os.CreateTemp(dir, prefix)
	if err != nil {
		return nil, err
	}
	if err := os.Remove(f.Name()); err != nil {
		log.Printf("remove %s: %v", f.Name(), err)
	}
	if _, err := f.WriteString(content); err != nil {
		CloseLogged(f)
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		CloseLogged(f)
		return nil, err
	}
	return f, nil
}

// CloseLogged closes f and logs a failure.
func CloseLogged(f *os.File) {
	if err := f.Close(); err != nil {
		log.Printf("close %s: %v", f.Name(), err)
	}
}

// RemoveLogged removes path and logs a failure other than a missing file.
func RemoveLogged(path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("remove %s: %v", path, err)
	}
}

// ReadAtMost reads up to n bytes at off; a short read at the end of the file
// is not an error.
func ReadAtMost(f *os.File, n, off int64) ([]byte, error) {
	b := make([]byte, n)
	read, err := f.ReadAt(b, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return b[:read], nil
}
