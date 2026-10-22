package services

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

func (m *Manager) configDirs(scope string) ([]string, error) {
	dirs := []string{filepath.Join("etc", "systemd", "system")}
	if scope == "user" {
		home := os.Getenv("XDG_CONFIG_HOME")
		if home == "" {
			userHome, err := os.UserHomeDir()
			if err != nil {
				return nil, fmt.Errorf("locate current user's configuration: %w", err)
			}
			home = filepath.Join(userHome, ".config")
		}
		if !filepath.IsAbs(home) {
			return nil, errors.New("XDG_CONFIG_HOME must be an absolute path")
		}
		dirs = []string{filepath.Join(home, "systemd", "user")}
		config := os.Getenv("XDG_CONFIG_DIRS")
		if config == "" {
			config = filepath.Join(string(filepath.Separator), "etc", "xdg")
		}
		for _, path := range filepath.SplitList(config) {
			if !filepath.IsAbs(path) {
				return nil, errors.New("XDG_CONFIG_DIRS entries must be absolute paths")
			}
			dirs = append(dirs, filepath.Join(path, "systemd", "user"))
		}
		dirs = append(dirs, filepath.Join("etc", "systemd", "user"))
	}
	for i, path := range dirs {
		if m.root != "" {
			path = strings.TrimPrefix(strings.TrimPrefix(path, filepath.VolumeName(path)), string(filepath.Separator))
			dirs[i] = filepath.Join(m.root, path)
		} else if !filepath.IsAbs(path) {
			dirs[i] = string(filepath.Separator) + path
		}
	}
	return dirs, nil
}

type configLink struct {
	path, name, target string
	dependency         bool
}

func (m *Manager) persistentEnablement(scope string, names []string) (bool, []configLink, error) {
	dirs, err := m.configDirs(scope)
	if err != nil {
		return false, nil, err
	}
	var links []configLink
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, nil, fmt.Errorf("inspect persistent systemd configuration %s: %w", dir, err)
		}
		for _, entry := range entries {
			path := filepath.Join(dir, entry.Name())
			if strings.HasSuffix(entry.Name(), ".wants") || strings.HasSuffix(entry.Name(), ".requires") ||
				strings.HasSuffix(entry.Name(), ".upholds") {
				children, err := os.ReadDir(path)
				if err != nil {
					return false, nil, fmt.Errorf("inspect persistent dependencies %s: %w", path, err)
				}
				for _, child := range children {
					if child.Type()&os.ModeSymlink != 0 {
						link, err := readConfigLink(filepath.Join(path, child.Name()), child.Name(), true)
						if err != nil {
							return false, nil, err
						}
						links = append(links, link)
					}
				}
			} else if entry.Type()&os.ModeSymlink != 0 {
				link, err := readConfigLink(path, entry.Name(), false)
				if err != nil {
					return false, nil, err
				}
				links = append(links, link)
			}
		}
	}
	// Do not resolve dependency links through the filesystem: an enabled unit's
	// links can resolve to /dev/null while its separate unit-file mask is present.
	for changed := true; changed; {
		changed = false
		for _, link := range links {
			if !link.dependency && slices.Contains(names, filepath.Base(link.target)) && !slices.Contains(names, link.name) {
				names = append(names, link.name)
				changed = true
			}
		}
	}
	enabled := false
	var enablement []configLink
	for _, link := range links {
		target := filepath.Base(link.target)
		if link.target == "/dev/null" {
			continue
		}
		if (link.dependency && slices.Contains(names, link.name)) ||
			(!link.dependency && link.name != target && slices.Contains(names, target)) {
			enabled = true
			enablement = append(enablement, link)
		}
	}
	return enabled, enablement, nil
}

func readConfigLink(path, name string, dependency bool) (configLink, error) {
	target, err := os.Readlink(path)
	if err != nil {
		return configLink{}, fmt.Errorf("read persistent symlink %s: %w", path, err)
	}
	return configLink{path: path, name: name, target: target, dependency: dependency}, nil
}

// systemctl unmask may prune links pointing to the removed mask. Restore only
// those missing links, never overwriting a concurrent configuration change.
func restoreEnablement(links []configLink) (bool, error) {
	restored := false
	for _, link := range links {
		if _, err := os.Lstat(link.path); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return restored, fmt.Errorf("inspect enablement after unmask: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(link.path), 0755); err != nil {
			return restored, fmt.Errorf("restore enablement directory: %w", err)
		}
		if err := os.Symlink(link.target, link.path); err != nil {
			return restored, fmt.Errorf("restore enablement link %s: %w", link.path, err)
		}
		restored = true
	}
	return restored, nil
}
