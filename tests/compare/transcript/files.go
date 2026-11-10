package transcript

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Snapshot describes what is at each of the paths, relative to root, and
// below them: a file by its mode and content, a link by its target, a
// directory by its mode. A path that does not exist is recorded as missing.
func Snapshot(root string, paths []string) (map[string]any, error) {
	out := map[string]any{}
	for _, rel := range paths {
		start := filepath.Join(root, rel)
		if _, err := os.Lstat(start); errors.Is(err, fs.ErrNotExist) {
			out[rel] = map[string]any{"missing": true}
			continue
		}
		err := filepath.WalkDir(start, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			name, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			entry, err := describe(path, d)
			if err != nil {
				return err
			}
			out[filepath.ToSlash(name)] = entry
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func describe(path string, d fs.DirEntry) (map[string]any, error) {
	info, err := d.Info()
	if err != nil {
		return nil, err
	}
	mode := fmt.Sprintf("%04o", info.Mode().Perm())
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return nil, err
		}
		return map[string]any{"link": target}, nil
	case info.IsDir():
		return map[string]any{"dir": true, "mode": mode}, nil
	case !info.Mode().IsRegular():
		return map[string]any{"type": info.Mode().Type().String()}, nil
	}
	data, err := os.ReadFile(path) //nolint:gosec // a file of the test's own root
	if err != nil {
		return nil, err
	}
	entry := Blob(data)
	entry["mode"] = mode
	return entry, nil
}
