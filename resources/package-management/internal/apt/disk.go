package apt

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var sourceNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func (s Store) checkWrite(path string, original []byte, info os.FileInfo, create bool) error {
	relative, err := filepath.Rel(s.Root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("refusing to write outside APT filesystem root: %s", path)
	}
	for current := path; ; current = filepath.Dir(current) {
		stat, err := os.Lstat(current)
		if err == nil {
			if stat.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("refusing to change symlinked APT path %s; manage its target explicitly", current)
			}
			if current == path && !stat.Mode().IsRegular() || current != path && !stat.IsDir() {
				return fmt.Errorf("unsafe APT file or directory at %s", current)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if current == s.Root || current == filepath.Dir(current) {
			break
		}
	}
	current, err := os.Lstat(path)
	if create {
		if err == nil {
			return fmt.Errorf("APT path appeared since discovery: %s; retry with fresh state", path)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("APT path changed since discovery: %s: %w", path, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if info == nil || !os.SameFile(info, current) || !info.ModTime().Equal(current.ModTime()) ||
		info.Mode() != current.Mode() || !bytes.Equal(original, data) {
		return fmt.Errorf("APT path changed since discovery: %s; retry with fresh state", path)
	}
	return nil
}

func atomicWrite(path string, content []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".dscapt-*.tmp")
	if err != nil {
		return err
	}
	tempPath := file.Name()
	defer os.Remove(tempPath)
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(content); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}
