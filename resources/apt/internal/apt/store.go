package apt

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	defaultSourcesDir  = "/etc/apt/sources.list.d"
	defaultKeyringsDir = "/etc/apt/keyrings"
)

type Store struct {
	SourcesDir  string
	KeyringsDir string
}

func NewStore(sourcesDir, keyringsDir string) Store {
	if sourcesDir == "" {
		sourcesDir = defaultSourcesDir
	}
	if keyringsDir == "" {
		keyringsDir = defaultKeyringsDir
	}
	return Store{SourcesDir: sourcesDir, KeyringsDir: keyringsDir}
}

func (s Store) Get(name string) (Repository, error) {
	if err := ValidateName(name); err != nil {
		return Repository{}, err
	}
	actual, _, err := s.observe(name)
	return actual, err
}

func (s Store) Test(desired Repository) (Repository, bool, error) {
	if err := ValidateDesired(&desired); err != nil {
		return Repository{}, false, err
	}
	actual, complete, sourceExists, err := s.observe(desired.Name)
	if err != nil {
		return Repository{}, false, err
	}
	if desired.Ensure == "Present" {
		return actual, sourceExists && complete && Equal(desired, actual), nil
	}
	if sourceExists {
		return actual, false, nil
	}
	keyExists, err := s.keyringExists(desired.Name)
	if err != nil {
		return Repository{}, false, err
	}
	if !keyExists {
		return actual, true, nil
	}
	referenced, err := keyringReferenced(s.SourcesDir, s.sourcePath(desired.Name), s.keyringPath(desired.Name))
	if err != nil {
		return Repository{}, false, err
	}
	return actual, referenced, nil
}

func (s Store) Set(desired Repository) (Repository, error) {
	if err := ValidateDesired(&desired); err != nil {
		return Repository{}, err
	}
	if desired.Ensure == "Absent" {
		keyExists, err := s.keyringExists(desired.Name)
		if err != nil {
			return Repository{}, err
		}
		referenced := false
		if keyExists {
			referenced, err = keyringReferenced(s.SourcesDir, s.sourcePath(desired.Name), s.keyringPath(desired.Name))
			if err != nil {
				return Repository{}, err
			}
		}
		if err := removeIfExists(s.sourcePath(desired.Name)); err != nil {
			return Repository{}, fmt.Errorf("remove repository source: %w", err)
		}
		if keyExists && !referenced {
			if err := removeIfExists(s.keyringPath(desired.Name)); err != nil {
				return Repository{}, fmt.Errorf("remove repository keyring: %w", err)
			}
		}
		return Repository{Name: desired.Name, Ensure: "Absent"}, nil
	}

	actual, complete, sourceExists, err := s.observe(desired.Name)
	if err != nil {
		return Repository{}, err
	}
	if sourceExists && complete && Equal(desired, actual) {
		return actual, nil
	}
	if err := os.MkdirAll(s.KeyringsDir, 0755); err != nil {
		return Repository{}, fmt.Errorf("create APT keyring directory: %w", err)
	}
	key, err := ValidateSigningKey(desired.SigningKey)
	if err != nil {
		return Repository{}, err
	}
	if err := atomicWrite(s.keyringPath(desired.Name), []byte(key), 0644); err != nil {
		return Repository{}, fmt.Errorf("write repository keyring: %w", err)
	}
	if err := os.MkdirAll(s.SourcesDir, 0755); err != nil {
		return Repository{}, fmt.Errorf("create APT source directory: %w", err)
	}
	if err := atomicWrite(s.sourcePath(desired.Name), serializeDeb822(desired, s.keyringPath(desired.Name)), 0644); err != nil {
		return Repository{}, fmt.Errorf("write repository source: %w", err)
	}
	actual, _, err = s.observe(desired.Name)
	if err != nil {
		return Repository{}, fmt.Errorf("read reconciled repository: %w", err)
	}
	return actual, nil
}

func (s Store) observe(name string) (Repository, bool, bool, error) {
	path := s.sourcePath(name)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return Repository{Name: name, Ensure: "Absent"}, true, false, nil
	}
	if err != nil {
		return Repository{}, false, false, fmt.Errorf("inspect repository source: %w", err)
	}
	actual := Repository{Name: name, Ensure: "Present"}
	if !info.Mode().IsRegular() {
		return actual, false, true, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Repository{}, false, true, fmt.Errorf("read repository source: %w", err)
	}
	parsed, supported := parseDeb822(name, s.keyringPath(name), data)
	actual = parsed
	if !supported {
		return actual, false, true, nil
	}
	keyPath := s.keyringPath(name)
	keyInfo, err := os.Lstat(keyPath)
	if errors.Is(err, os.ErrNotExist) {
		return actual, false, true, nil
	}
	if err != nil {
		return Repository{}, false, true, fmt.Errorf("inspect repository keyring: %w", err)
	}
	if !keyInfo.Mode().IsRegular() {
		return actual, false, true, nil
	}
	keyData, err := os.ReadFile(keyPath)
	if err != nil {
		return Repository{}, false, true, fmt.Errorf("read repository keyring: %w", err)
	}
	key, err := ValidateSigningKey(string(keyData))
	if err != nil {
		return actual, false, true, nil
	}
	actual.SigningKey = key
	if err := ValidateDesired(&actual); err != nil {
		return actual, false, true, nil
	}
	return actual, true, true, nil
}

func (s Store) keyringExists(name string) (bool, error) {
	_, err := os.Lstat(s.keyringPath(name))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect repository keyring: %w", err)
	}
	return true, nil
}

func (s Store) sourcePath(name string) string {
	return filepath.Join(s.SourcesDir, name+".sources")
}

func (s Store) keyringPath(name string) string {
	return filepath.Join(s.KeyringsDir, name+".asc")
}

func atomicWrite(path string, content []byte, mode os.FileMode) error {
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

func removeIfExists(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

