package apt

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp/armor"
	openpgp "github.com/ProtonMail/go-crypto/openpgp/v2"
)

var keyFingerprint = regexp.MustCompile(`(?i)^[a-f0-9]{8,64}!?$`)

func ValidateSigningKey(value string) (string, error) {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "-----BEGIN PGP PUBLIC KEY BLOCK-----") ||
		!strings.HasSuffix(value, "-----END PGP PUBLIC KEY BLOCK-----") ||
		strings.Count(value, "-----BEGIN PGP PUBLIC KEY BLOCK-----") != 1 ||
		strings.Count(value, "-----END PGP PUBLIC KEY BLOCK-----") != 1 {
		return "", errors.New("expected one complete public-key armor block")
	}
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
	if len(bytes.TrimSpace(remaining)) == 0 {
		return "", errors.New("empty OpenPGP payload")
	}
	if err := validateArmorChecksum(value, remaining); err != nil {
		return "", err
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
		if _, err := entity.VerifyPrimaryKey(time.Now(), nil); err != nil {
			return "", fmt.Errorf("verify OpenPGP primary key: %w", err)
		}
		for _, subkey := range entity.Subkeys {
			if subkey.PrivateKey != nil {
				return "", errors.New("private OpenPGP subkey")
			}
		}
	}
	return value + "\n", nil
}

func validateArmorChecksum(value string, payload []byte) error {
	lines := strings.Split(value, "\n")
	separator := -1
	for i := 1; i < len(lines)-1; i++ {
		if lines[i] == "" {
			separator = i
			break
		}
	}
	if separator < 0 {
		return errors.New("OpenPGP armor has no body separator")
	}
	checksum := ""
	for _, line := range lines[separator+1 : len(lines)-1] {
		if strings.HasPrefix(line, "=") {
			if checksum != "" {
				return errors.New("OpenPGP armor has multiple checksums")
			}
			checksum = line
		} else if checksum != "" {
			return errors.New("data follows OpenPGP armor checksum")
		}
	}
	if checksum == "" {
		return nil
	}
	if len(checksum) != 5 {
		return errors.New("invalid OpenPGP armor checksum")
	}
	decoded, err := base64.StdEncoding.DecodeString(checksum[1:])
	if err != nil || len(decoded) != 3 {
		return errors.New("invalid OpenPGP armor checksum")
	}
	actual := crc24(payload)
	expected := uint32(decoded[0])<<16 | uint32(decoded[1])<<8 | uint32(decoded[2])
	if actual != expected {
		return errors.New("OpenPGP armor checksum mismatch")
	}
	return nil
}

func crc24(data []byte) uint32 {
	checksum := uint32(0xB704CE)
	for _, value := range data {
		checksum ^= uint32(value) << 16
		for bit := 0; bit < 8; bit++ {
			checksum <<= 1
			if checksum&0x1000000 != 0 {
				checksum ^= 0x1864CFB
			}
		}
	}
	return checksum & 0xFFFFFF
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
	currentValue := ""
	fields := make(map[string]struct{})
	finishField := func() (bool, error) {
		if currentField == "signed-by" {
			return signedByReferencesKey(currentValue, keyringPath)
		}
		return false, nil
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			referenced, err := finishField()
			if err != nil || referenced {
				return referenced, err
			}
			currentField = ""
			currentValue = ""
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
			currentValue += " " + trimmed
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok || !deb822FieldName.MatchString(name) {
			return false, errors.New("malformed Deb822 field while checking keyring references")
		}
		referenced, err := finishField()
		if err != nil || referenced {
			return referenced, err
		}
		currentField = strings.ToLower(name)
		if _, exists := fields[currentField]; exists {
			return false, errors.New("duplicate Deb822 field while checking keyring references")
		}
		fields[currentField] = struct{}{}
		currentValue = strings.TrimSpace(value)
	}
	referenced, err := finishField()
	if err != nil || referenced {
		return referenced, err
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
			optionPrefix := strings.TrimSpace(line[:open])
			if close < 0 || strings.ContainsAny(line[close+1:], "[]") ||
				(optionPrefix != "deb" && optionPrefix != "deb-src") {
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
			if !ok || name == "" {
				return false, errors.New("malformed APT source option while checking keyring references")
			}
			if !strings.EqualFold(name, "signed-by") {
				continue
			}
			if value == "" {
				return false, errors.New("empty signed-by option while checking keyring references")
			}
			referenced, err := signedByReferencesKey(value, keyringPath)
			if err != nil || referenced {
				return referenced, err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return false, fmt.Errorf("scan one-line APT source while checking keyring references: %w", err)
	}
	return false, nil
}

func signedByReferencesKey(value, keyringPath string) (bool, error) {
	if value == "" {
		return false, errors.New("empty Signed-By field while checking keyring references")
	}
	if strings.Contains(value, "-----BEGIN PGP PUBLIC KEY BLOCK-----") {
		if !strings.Contains(value, "-----END PGP PUBLIC KEY BLOCK-----") {
			return false, errors.New("unterminated inline key in APT source")
		}
		return false, nil
	}
	for _, token := range strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	}) {
		if !filepath.IsAbs(token) {
			if keyFingerprint.MatchString(token) {
				continue
			}
			return false, errors.New("unrecognized Signed-By value while checking keyring references")
		}
		if filepath.Clean(token) == keyringPath {
			return true, nil
		}
	}
	return false, nil
}
