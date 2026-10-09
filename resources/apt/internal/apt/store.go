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

type observedState struct {
	repository      Repository
	sourceExists    bool
	sourceCanonical bool
	keyringValid    bool
	keyringContents string
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
	state, err := s.observe(name)
	return state.repository, err
}

func (s Store) Test(desired Repository) (Repository, bool, error) {
	if err := ValidateDesired(&desired); err != nil {
		return Repository{}, false, err
	}
	state, err := s.observe(desired.Name)
	if err != nil {
		return Repository{}, false, err
	}
	if desired.Ensure == "Present" {
		return state.repository, state.sourceCanonical && state.keyringValid && Equal(desired, state.repository), nil
	}
	if state.sourceExists {
		return state.repository, false, nil
	}
	keyExists, err := s.keyringExists(desired.Name)
	if err != nil {
		return Repository{}, false, err
	}
	if !keyExists {
		return state.repository, true, nil
	}
	referenced, err := keyringReferenced(s.SourcesDir, s.sourcePath(desired.Name), s.keyringPath(desired.Name))
	if err != nil {
		return Repository{}, false, err
	}
	return state.repository, referenced, nil
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
		if err := removeManagedFile(s.sourcePath(desired.Name)); err != nil {
			return Repository{}, fmt.Errorf("remove repository source: %w", err)
		}
		if keyExists && !referenced {
			if err := removeManagedFile(s.keyringPath(desired.Name)); err != nil {
				return Repository{}, fmt.Errorf("remove repository keyring: %w", err)
			}
		}
		return Repository{Name: desired.Name, Ensure: "Absent"}, nil
	}

	state, err := s.observe(desired.Name)
	if err != nil {
		return Repository{}, err
	}
	if !state.keyringValid {
		state.keyringContents, state.keyringValid, err = s.readKeyring(desired.Name)
		if err != nil {
			return Repository{}, err
		}
	}
	if state.sourceCanonical && state.keyringValid && Equal(desired, state.repository) {
		return state.repository, nil
	}
	if !state.keyringValid || state.keyringContents != desired.SigningKey {
		if err := os.MkdirAll(s.KeyringsDir, 0755); err != nil {
			return Repository{}, fmt.Errorf("create APT keyring directory: %w", err)
		}
		if err := atomicWrite(s.keyringPath(desired.Name), []byte(desired.SigningKey), 0644); err != nil {
			return Repository{}, fmt.Errorf("write repository keyring: %w", err)
		}
	}
	if !state.sourceCanonical || !sourcePropertiesEqual(desired, state.repository) {
		if err := os.MkdirAll(s.SourcesDir, 0755); err != nil {
			return Repository{}, fmt.Errorf("create APT source directory: %w", err)
		}
		if err := atomicWrite(s.sourcePath(desired.Name), serializeDeb822(desired, s.keyringPath(desired.Name)), 0644); err != nil {
			return Repository{}, fmt.Errorf("write repository source: %w", err)
		}
	}
	state, err = s.observe(desired.Name)
	if err != nil {
		return Repository{}, fmt.Errorf("read reconciled repository: %w", err)
	}
	if !state.sourceCanonical || !state.keyringValid || !Equal(desired, state.repository) {
		return Repository{}, errors.New("repository did not converge to desired state")
	}
	return state.repository, nil
}

func (s Store) observe(name string) (observedState, error) {
	path := s.sourcePath(name)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return observedState{
			repository:   Repository{Name: name, Ensure: "Absent"},
			sourceExists: false,
		}, nil
	}
	if err != nil {
		return observedState{}, fmt.Errorf("inspect repository source: %w", err)
	}
	state := observedState{
		repository:   Repository{Name: name, Ensure: "Present"},
		sourceExists: true,
	}
	if !info.Mode().IsRegular() {
		return state, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return observedState{}, fmt.Errorf("read repository source: %w", err)
	}
	parsed, supported, keyringMatches := parseDeb822(name, s.keyringPath(name), data)
	state.repository = parsed
	state.sourceCanonical = supported && info.Mode().Perm() == 0644
	if !keyringMatches {
		return state, nil
	}
	key, valid, err := s.readKeyring(name)
	if err != nil {
		return observedState{}, err
	}
	if valid {
		state.keyringValid = true
		state.keyringContents = key
		state.repository.SigningKey = key
	}
	return state, nil
}

func (s Store) readKeyring(name string) (string, bool, error) {
	keyPath := s.keyringPath(name)
	keyInfo, err := os.Lstat(keyPath)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("inspect repository keyring: %w", err)
	}
	if !keyInfo.Mode().IsRegular() || keyInfo.Mode().Perm() != 0644 {
		return "", false, nil
	}
	keyData, err := os.ReadFile(keyPath)
	if err != nil {
		return "", false, fmt.Errorf("read repository keyring: %w", err)
	}
	key, err := ValidateSigningKey(string(keyData))
	if err != nil {
		return "", false, nil
	}
	return key, true, nil
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

func sourcePropertiesEqual(desired, actual Repository) bool {
	return desired.Name == actual.Name &&
		desired.URI == actual.URI &&
		desired.Suite == actual.Suite &&
		equalStrings(desired.Components, actual.Components) &&
		equalStrings(desired.Architectures, actual.Architectures)
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

func removeManagedFile(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("managed path %q is not a regular file", path)
	}
	return os.Remove(path)
}
