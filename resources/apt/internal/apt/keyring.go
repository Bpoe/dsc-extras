package apt

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
)

func ValidateSigningKey(value string) (string, error) {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.TrimSpace(value)
	input := strings.NewReader(value)
	block, err := armor.Decode(input)
	if err != nil {
		return "", fmt.Errorf("decode OpenPGP armor: %w", err)
	}
	if block.Type != openpgp.PublicKeyType {
		return "", errors.New("expected public-key armor")
	}
	remaining, err := io.ReadAll(block.Body)
	if err != nil {
		return "", fmt.Errorf("read armored OpenPGP key: %w", err)
	}
	_ = block.Body.Close()
	if len(bytes.TrimSpace(remaining)) == 0 {
		return "", errors.New("empty OpenPGP payload")
	}
	trailing, err := io.ReadAll(input)
	if err != nil {
		return "", fmt.Errorf("read trailing armor data: %w", err)
	}
	if len(bytes.TrimSpace(trailing)) != 0 {
		return "", errors.New("unexpected data after OpenPGP armor")
	}
	entities, err := openpgp.ReadArmoredKeyRing(strings.NewReader(value))
	if err != nil {
		return "", fmt.Errorf("parse OpenPGP keyring: %w", err)
	}
	if len(entities) == 0 {
		return "", errors.New("empty OpenPGP keyring")
	}
	for _, entity := range entities {
		if entity.PrimaryKey == nil || entity.PrivateKey != nil {
			return "", errors.New("not a public-only OpenPGP key")
		}
		for _, subkey := range entity.Subkeys {
			if subkey.PrivateKey != nil {
				return "", errors.New("private OpenPGP subkey")
			}
		}
	}
	return value + "\n", nil
}

func keyringReferenced(sourcesDir, ownSourcePath, keyringPath string) (bool, error) {
	ownSourcePath, err := filepath.Abs(ownSourcePath)
	if err != nil {
		return false, err
	}
	keyringPath, err = filepath.Abs(keyringPath)
	if err != nil {
		return false, err
	}
	entries, err := os.ReadDir(sourcesDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("list APT sources: %w", err)
	}
	for _, entry := range entries {
		extension := filepath.Ext(entry.Name())
		if extension != ".sources" && extension != ".list" {
			continue
		}
		path := filepath.Join(sourcesDir, entry.Name())
		absolutePath, err := filepath.Abs(path)
		if err != nil {
			return false, err
		}
		if absolutePath == ownSourcePath {
			continue
		}
		referenced, err := sourceFileReferencesKey(path, extension, keyringPath)
		if err != nil {
			return false, err
		}
		if referenced {
			return true, nil
		}
	}
	primarySources, err := filepath.Abs(filepath.Join(filepath.Dir(sourcesDir), "sources.list"))
	if err != nil {
		return false, err
	}
	if primarySources == ownSourcePath {
		return false, nil
	}
	return sourceFileReferencesKey(primarySources, ".list", keyringPath)
}

func sourceFileReferencesKey(path, extension, keyringPath string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect APT source file %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("APT source entry %q is not a regular file", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("read APT source file %q: %w", path, err)
	}
	if extension == ".sources" {
		return deb822ReferencesKey(data, keyringPath)
	}
	return oneLineSourceReferencesKey(data, keyringPath)
}

func deb822ReferencesKey(data []byte, keyringPath string) (bool, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), 4<<20)
	currentField := ""
	fields := make(map[string]struct{})
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			currentField = ""
			fields = make(map[string]struct{})
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			if currentField == "" {
				return false, errors.New("orphan Deb822 continuation while checking keyring references")
			}
			if currentField == "signed-by" && containsKeyringPath(trimmed, keyringPath) {
				return true, nil
			}
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok || !deb822FieldName.MatchString(name) {
			return false, errors.New("malformed Deb822 field while checking keyring references")
		}
		currentField = strings.ToLower(name)
		if _, exists := fields[currentField]; exists {
			return false, errors.New("duplicate Deb822 field while checking keyring references")
		}
		fields[currentField] = struct{}{}
		if currentField == "signed-by" && containsKeyringPath(strings.TrimSpace(value), keyringPath) {
			return true, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return false, fmt.Errorf("scan Deb822 source while checking keyring references: %w", err)
	}
	return false, nil
}

func oneLineSourceReferencesKey(data []byte, keyringPath string) (bool, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), 4<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if index := strings.IndexByte(line, '#'); index >= 0 {
			line = strings.TrimSpace(line[:index])
		}
		if line == "" {
			continue
		}
		open := strings.IndexByte(line, '[')
		close := strings.IndexByte(line, ']')
		if close >= 0 && (open < 0 || close < open) {
			return false, errors.New("malformed APT source options while checking keyring references")
		}
		repositoryLine := line
		options := ""
		if open >= 0 {
			if close < 0 {
				return false, errors.New("malformed APT source options while checking keyring references")
			}
			options = line[open+1 : close]
			repositoryLine = strings.TrimSpace(line[:open] + " " + line[close+1:])
		}
		fields := strings.Fields(repositoryLine)
		if len(fields) < 3 || (fields[0] != "deb" && fields[0] != "deb-src") {
			return false, errors.New("malformed APT one-line source while checking keyring references")
		}
		for _, option := range strings.Fields(options) {
			name, value, ok := strings.Cut(option, "=")
			if !ok || !strings.EqualFold(name, "signed-by") {
				continue
			}
			if value == "" {
				return false, errors.New("empty signed-by option while checking keyring references")
			}
			if containsKeyringPath(value, keyringPath) {
				return true, nil
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return false, fmt.Errorf("scan one-line APT source while checking keyring references: %w", err)
	}
	return false, nil
}

func containsKeyringPath(value, keyringPath string) bool {
	for _, token := range strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	}) {
		if filepath.IsAbs(token) && filepath.Clean(token) == keyringPath {
			return true
		}
	}
	return false
}
