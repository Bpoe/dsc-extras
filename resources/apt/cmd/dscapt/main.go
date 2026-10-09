package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	version       = "0.1.0"
	sourceDir     = "/etc/apt/sources.list.d"
	keyringDir    = "/etc/apt/keyrings"
	maxInputBytes = 1 << 20
)

var (
	namePattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	componentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)
	suitePattern     = regexp.MustCompile(`^(?:\./|[A-Za-z0-9][A-Za-z0-9._+/-]*)$`)
)

type repository struct {
	Name          string   `json:"name"`
	Ensure        string   `json:"ensure,omitempty"`
	URI           string   `json:"uri,omitempty"`
	Suite         string   `json:"suite,omitempty"`
	Components    []string `json:"components,omitempty"`
	Architectures []string `json:"architectures,omitempty"`
	SigningKey    string   `json:"signingKey,omitempty"`
}

type testState struct {
	repository
	InDesiredState bool `json:"_inDesiredState"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, sourceDir, keyringDir))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer, dir, keysDir string) int {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		_, _ = fmt.Fprintln(stdout, "Usage: dscapt <get|set|test>")
		return 0
	}
	if len(args) == 1 && args[0] == "--version" {
		_, _ = fmt.Fprintln(stdout, version)
		return 0
	}
	if len(args) != 1 {
		return reportError(stderr, "expected one operation: get, set, or test")
	}

	desired, err := readInput(stdin)
	if err != nil {
		return reportError(stderr, "invalid JSON input")
	}
	if err := validateRepository(&desired); err != nil {
		return reportError(stderr, err.Error())
	}

	switch args[0] {
	case "get":
		current, err := getRepository(dir, keysDir, desired.Name)
		if err != nil {
			return reportError(stderr, "failed to read the repository source file")
		}
		return writeJSON(stdout, current, stderr)
	case "test":
		current, err := getRepository(dir, keysDir, desired.Name)
		if err != nil {
			return reportError(stderr, "failed to read the repository source file")
		}
		inDesired := inDesiredState(desired, current)
		if desired.Ensure == "Absent" {
			exists, err := pathExists(keyringPath(keysDir, desired.Name))
			if err != nil {
				return reportError(stderr, "failed to inspect the repository keyring file")
			}
			inDesired = inDesired && !exists
		}
		return writeJSON(stdout, testState{
			repository:     current,
			InDesiredState: inDesired,
		}, stderr)
	case "set":
		current, err := setRepository(dir, keysDir, desired)
		if err != nil {
			return reportError(stderr, "failed to set the repository source or keyring file")
		}
		return writeJSON(stdout, current, stderr)
	default:
		return reportError(stderr, "unknown operation")
	}
}

func readInput(input io.Reader) (repository, error) {
	data, err := io.ReadAll(io.LimitReader(input, maxInputBytes+1))
	if err != nil {
		return repository{}, err
	}
	if len(data) > maxInputBytes {
		return repository{}, errors.New("input is too large")
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var desired repository
	if err := decoder.Decode(&desired); err != nil {
		return repository{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return repository{}, errors.New("multiple JSON values")
		}
		return repository{}, err
	}
	return desired, nil
}

func validateRepository(desired *repository) error {
	if !namePattern.MatchString(desired.Name) {
		return errors.New("name must be a filename containing only letters, numbers, dots, underscores, or hyphens")
	}
	if desired.Ensure == "" {
		desired.Ensure = "Present"
	}
	if desired.Ensure != "Present" && desired.Ensure != "Absent" {
		return errors.New("ensure must be Present or Absent")
	}
	if desired.Ensure == "Absent" {
		return nil
	}
	if err := validateURI(desired.URI); err != nil {
		return errors.New("uri must be an absolute HTTP or HTTPS URL without user information, query, or fragment")
	}
	if !suitePattern.MatchString(desired.Suite) {
		return errors.New("suite must be a non-empty APT suite token")
	}
	if len(desired.Components) == 0 {
		return errors.New("components must contain at least one APT component")
	}
	if !uniqueStrings(desired.Components) {
		return errors.New("components must not contain duplicates")
	}
	for _, component := range desired.Components {
		if !componentPattern.MatchString(component) {
			return errors.New("components must contain valid APT component tokens")
		}
	}
	if !uniqueStrings(desired.Architectures) {
		return errors.New("architectures must not contain duplicates")
	}
	for _, architecture := range desired.Architectures {
		if !componentPattern.MatchString(architecture) {
			return errors.New("architectures must contain valid architecture tokens")
		}
	}
	key, err := normalizeSigningKey(desired.SigningKey)
	if err != nil {
		return errors.New("signingKey must be an inline ASCII-armored OpenPGP public key")
	}
	desired.SigningKey = key
	return nil
}

func validateURI(value string) error {
	if value == "" || strings.ContainsAny(value, "#") || strings.ContainsAny(value, "\r\n\t ") {
		return errors.New("invalid URI")
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return err
	}
	if !parsed.IsAbs() || parsed.Opaque != "" || parsed.User != nil {
		return errors.New("invalid URI")
	}
	switch parsed.Scheme {
	case "http", "https":
		if parsed.Host == "" || parsed.Hostname() == "" {
			return errors.New("invalid URI")
		}
	case "file":
		if parsed.Host != "" || !strings.HasPrefix(parsed.Path, "/") {
			return errors.New("invalid URI")
		}
	default:
		return errors.New("invalid URI")
	}
	return nil
}

func normalizeSigningKey(value string) (string, error) {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.TrimSpace(value)
	lines := strings.Split(value, "\n")
	if len(lines) < 4 || lines[0] != "-----BEGIN PGP PUBLIC KEY BLOCK-----" ||
		lines[len(lines)-1] != "-----END PGP PUBLIC KEY BLOCK-----" {
		return "", errors.New("invalid public key armor")
	}

	inBody := false
	var encoded strings.Builder
	for _, line := range lines[1 : len(lines)-1] {
		if !inBody {
			if line == "" {
				inBody = true
				continue
			}
			if !strings.Contains(line, ": ") {
				return "", errors.New("invalid armor header")
			}
			continue
		}
		if line == "" || strings.HasPrefix(line, "=") {
			continue
		}
		encoded.WriteString(line)
	}
	if !inBody || encoded.Len() == 0 {
		return "", errors.New("missing armored key data")
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded.String())
	if err != nil || len(decoded) == 0 {
		return "", errors.New("invalid armored key data")
	}
	return value, nil
}

func getRepository(dir, keysDir, name string) (repository, error) {
	path := sourcePath(dir, name)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return repository{Name: name, Ensure: "Absent"}, nil
	}
	if err != nil {
		return repository{}, err
	}
	if !info.Mode().IsRegular() {
		return repository{}, errors.New("repository source is not a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return repository{}, err
	}
	return parseSource(dir, keysDir, name, data)
}

func parseSource(dir, keysDir, name string, data []byte) (repository, error) {
	values := make(map[string]string)
	currentKey := ""
	paragraphOpen := false
	paragraphs := 0
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if strings.TrimSpace(line) == "" {
			currentKey = ""
			paragraphOpen = false
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			if currentKey == "" {
				return repository{}, errors.New("orphan continuation line")
			}
			value := strings.TrimLeft(line, " \t")
			values[currentKey] += " " + strings.TrimSpace(value)
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return repository{}, errors.New("invalid Deb822 field")
		}
		key = strings.ToLower(strings.TrimSpace(key))
		if key == "" || key != strings.TrimSpace(key) {
			return repository{}, errors.New("invalid Deb822 field name")
		}
		if !paragraphOpen {
			paragraphs++
			paragraphOpen = true
		}
		if _, exists := values[key]; exists {
			return repository{}, errors.New("duplicate Deb822 field")
		}
		currentKey = key
		values[key] = strings.TrimSpace(value)
	}
	if err := scanner.Err(); err != nil {
		return repository{}, err
	}
	if paragraphs != 1 {
		return repository{}, errors.New("expected one Deb822 stanza")
	}
	if values["types"] != "deb" {
		return repository{}, errors.New("unsupported repository type")
	}
	uris := strings.Fields(values["uris"])
	suites := strings.Fields(values["suites"])
	components := strings.Fields(values["components"])
	architectures := strings.Fields(values["architectures"])
	if len(uris) != 1 || len(suites) != 1 || len(components) == 0 {
		return repository{}, errors.New("incomplete Deb822 stanza")
	}
	expectedKeyPath := keyringPath(keysDir, name)
	if values["signed-by"] != expectedKeyPath {
		return repository{}, errors.New("unexpected Signed-By keyring path")
	}
	keyInfo, err := os.Lstat(expectedKeyPath)
	if err != nil {
		return repository{}, err
	}
	if !keyInfo.Mode().IsRegular() {
		return repository{}, errors.New("repository keyring is not a regular file")
	}
	keyBytes, err := os.ReadFile(expectedKeyPath)
	if err != nil {
		return repository{}, err
	}
	key, err := normalizeSigningKey(string(keyBytes))
	if err != nil {
		return repository{}, errors.New("invalid external signing key")
	}
	actual := repository{
		Name:          name,
		Ensure:        "Present",
		URI:           uris[0],
		Suite:         suites[0],
		Components:    components,
		Architectures: architectures,
		SigningKey:    key,
	}
	if err := validateRepository(&actual); err != nil {
		return repository{}, errors.New("invalid Deb822 repository state")
	}
	return actual, nil
}

func setRepository(dir, keysDir string, desired repository) (repository, error) {
	path := sourcePath(dir, desired.Name)
	keyPath := keyringPath(keysDir, desired.Name)
	if desired.Ensure == "Absent" {
		sourceErr := removeIfExists(path)
		keyErr := removeIfExists(keyPath)
		if sourceErr != nil || keyErr != nil {
			return repository{}, errors.Join(sourceErr, keyErr)
		}
		return repository{Name: desired.Name, Ensure: "Absent"}, nil
	}
	current, err := getRepository(dir, keysDir, desired.Name)
	if err == nil && inDesiredState(desired, current) {
		return current, nil
	}
	if err := os.MkdirAll(keysDir, 0755); err != nil {
		return repository{}, err
	}
	if err := writeAtomicFile(keyPath, []byte(desired.SigningKey), 0644); err != nil {
		return repository{}, err
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return repository{}, err
	}
	if err := writeSourceFile(path, keyPath, desired); err != nil {
		return repository{}, err
	}
	return desired, nil
}

func writeSourceFile(path, keyPath string, desired repository) error {
	var content strings.Builder
	fmt.Fprintf(&content, "Types: deb\nURIs: %s\nSuites: %s\nComponents: %s\n",
		desired.URI, desired.Suite, strings.Join(desired.Components, " "))
	if len(desired.Architectures) > 0 {
		fmt.Fprintf(&content, "Architectures: %s\n", strings.Join(desired.Architectures, " "))
	}
	fmt.Fprintf(&content, "Signed-By: %s\n", keyPath)
	return writeAtomicFile(path, []byte(content.String()), 0644)
}

func writeAtomicFile(path string, content []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	file, err := os.CreateTemp(dir, ".dscapt-*.tmp")
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

func inDesiredState(desired, current repository) bool {
	if desired.Name != current.Name || desired.Ensure != current.Ensure {
		return false
	}
	if desired.Ensure == "Absent" {
		return true
	}
	return desired.URI == current.URI &&
		desired.Suite == current.Suite &&
		equalStrings(desired.Components, current.Components) &&
		equalStrings(desired.Architectures, current.Architectures) &&
		desired.SigningKey == current.SigningKey
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func uniqueStrings(values []string) bool {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

func sourcePath(dir, name string) string {
	return filepath.Join(dir, name+".sources")
}

func keyringPath(dir, name string) string {
	return filepath.Join(dir, name+".asc")
}

func pathExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func removeIfExists(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func writeJSON(stdout io.Writer, value any, stderr io.Writer) int {
	if err := json.NewEncoder(stdout).Encode(value); err != nil {
		return reportError(stderr, "failed to write JSON output")
	}
	return 0
}

func reportError(stderr io.Writer, message string) int {
	diagnostic, _ := json.Marshal(struct {
		Error string `json:"error"`
	}{Error: message})
	_, _ = fmt.Fprintln(stderr, string(diagnostic))
	return 1
}
