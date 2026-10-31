package proc

import (
	"errors"
	"log"
	"os"
)

// TempFile returns an unlinked temporary file holding content; only open
// descriptors keep it alive.
func TempFile(prefix, content string) (*os.File, error) {
	f, err := os.CreateTemp("", prefix)
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
