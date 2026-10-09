package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testSigningKey = `-----BEGIN PGP PUBLIC KEY BLOCK-----

YWJjZA==
-----END PGP PUBLIC KEY BLOCK-----`

func validRepository() repository {
	return repository{
		Name:          "example",
		Ensure:        "Present",
		URI:           "https://packages.example.org/debian",
		Suite:         "trixie",
		Components:    []string{"main", "contrib"},
		Architectures: []string{"amd64"},
		SigningKey:    testSigningKey,
	}
}

func encodeInput(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func runInput(t *testing.T, dir, operation string, value any) (int, string, string) {
	t.Helper()
	var stdout, stderr strings.Builder
	code := run([]string{operation}, strings.NewReader(encodeInput(t, value)), &stdout, &stderr, dir, filepath.Join(dir, "keyrings"))
	return code, stdout.String(), stderr.String()
}

func TestSetWritesAndGetsOneDeb822File(t *testing.T) {
	dir := t.TempDir()
	otherPath := filepath.Join(dir, "other.sources")
	if err := os.WriteFile(otherPath, []byte("other repository"), 0600); err != nil {
		t.Fatal(err)
	}
	desired := validRepository()
	code, stdout, stderr := runInput(t, dir, "set", desired)
	if code != 0 || stderr != "" {
		t.Fatalf("set returned (%d, %q), want success", code, stderr)
	}
	var actual repository
	if err := json.Unmarshal([]byte(stdout), &actual); err != nil {
		t.Fatalf("set output is invalid JSON: %v", err)
	}
	if !inDesiredState(desired, actual) {
		t.Fatalf("set returned state %#v, want %#v", actual, desired)
	}

	sourcePath := filepath.Join(dir, "example.sources")
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	for _, want := range []string{
		"Types: deb\n",
		"URIs: https://packages.example.org/debian\n",
		"Suites: trixie\n",
		"Components: main contrib\n",
		"Architectures: amd64\n",
		"Signed-By: " + filepath.Join(dir, "keyrings", "example.asc") + "\n",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("source file does not contain %q:\n%s", want, content)
		}
	}
	info, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0644 {
		t.Errorf("source file permissions = %04o, want 0644", info.Mode().Perm())
	}
	keyPath := filepath.Join(dir, "keyrings", "example.asc")
	keyData, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(keyData) != testSigningKey {
		t.Errorf("keyring file = %q, want complete ASCII-armored key", keyData)
	}
	keyInfo, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if keyInfo.Mode().Perm() != 0644 {
		t.Errorf("keyring file permissions = %04o, want 0644", keyInfo.Mode().Perm())
	}
	otherContents, err := os.ReadFile(otherPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(otherContents) != "other repository" {
		t.Errorf("unowned file changed: %q", otherContents)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Errorf("source directory contains %d entries, want only the managed file, unrelated file, and keyring directory", len(entries))
	}
	keyEntries, err := os.ReadDir(filepath.Join(dir, "keyrings"))
	if err != nil {
		t.Fatal(err)
	}
	if len(keyEntries) != 1 || keyEntries[0].Name() != "example.asc" {
		t.Errorf("keyring directory entries = %v, want only example.asc", keyEntries)
	}

	code, stdout, stderr = runInput(t, dir, "get", desired)
	if code != 0 || stderr != "" {
		t.Fatalf("get returned (%d, %q), want success", code, stderr)
	}
	if err := json.Unmarshal([]byte(stdout), &actual); err != nil {
		t.Fatalf("get output is invalid JSON: %v", err)
	}
	if !inDesiredState(desired, actual) {
		t.Fatalf("get returned state %#v, want %#v", actual, desired)
	}
}

func TestSetAbsentNeedsOnlyNameAndRemovesOnlyItsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "example.sources")
	if err := os.WriteFile(path, []byte("malformed or unmanaged content"), 0600); err != nil {
		t.Fatal(err)
	}
	otherPath := filepath.Join(dir, "unrelated.sources")
	if err := os.WriteFile(otherPath, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "keyrings", "example.asc")
	if err := os.MkdirAll(filepath.Dir(keyPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte(testSigningKey), 0644); err != nil {
		t.Fatal(err)
	}
	otherKeyPath := filepath.Join(dir, "keyrings", "unrelated.asc")
	if err := os.WriteFile(otherKeyPath, []byte("unrelated key"), 0644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runInput(t, dir, "set", repository{Name: "example", Ensure: "Absent"})
	if code != 0 || stderr != "" {
		t.Fatalf("set Absent returned (%d, %q), want success", code, stderr)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("managed file still exists or cannot be checked: %v", err)
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("managed keyring still exists or cannot be checked: %v", err)
	}
	if data, err := os.ReadFile(otherKeyPath); err != nil || string(data) != "unrelated key" {
		t.Fatalf("unrelated keyring changed: data=%q err=%v", data, err)
	}
	if data, err := os.ReadFile(otherPath); err != nil || string(data) != "preserve" {
		t.Fatalf("unrelated file changed: data=%q err=%v", data, err)
	}
	var actual repository
	if err := json.Unmarshal([]byte(stdout), &actual); err != nil {
		t.Fatal(err)
	}
	if actual.Name != "example" || actual.Ensure != "Absent" {
		t.Fatalf("set output = %#v, want absent state", actual)
	}
}

func TestTestAbsentDetectsOrphanedKeyring(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "keyrings", "example.asc")
	if err := os.MkdirAll(filepath.Dir(keyPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte(testSigningKey), 0644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runInput(t, dir, "test", repository{Name: "example", Ensure: "Absent"})
	if code != 0 || stderr != "" {
		t.Fatalf("test Absent returned (%d, %q), want success", code, stderr)
	}
	var result testState
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatal(err)
	}
	if result.InDesiredState {
		t.Fatalf("test Absent returned compliant with leftover owned keyring: %#v", result)
	}
}

func TestSetAbsentPreservesKeyringReferencedByOtherRepositories(t *testing.T) {
	for _, test := range []struct {
		name      string
		extension string
		content   func(string) string
	}{
		{
			name:      "Deb822 source",
			extension: ".sources",
			content: func(path string) string {
				return "Types: deb\nURIs: https://other.example/debian\nSuites: stable\nComponents: main\nSigned-By: " + path + "\n"
			},
		},
		{
			name:      "one-line source",
			extension: ".list",
			content: func(path string) string {
				return "deb [signed-by=" + path + "] https://other.example/debian stable main\n"
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			desired := validRepository()
			if _, _, stderr := runInput(t, dir, "set", desired); stderr != "" {
				t.Fatalf("failed to create repository: %s", stderr)
			}
			keyPath := filepath.Join(dir, "keyrings", "example.asc")
			otherSource := filepath.Join(dir, "other"+test.extension)
			if err := os.WriteFile(otherSource, []byte(test.content(keyPath)), 0644); err != nil {
				t.Fatal(err)
			}

			code, _, stderr := runInput(t, dir, "set", repository{Name: "example", Ensure: "Absent"})
			if code != 0 || stderr != "" {
				t.Fatalf("set Absent returned (%d, %q), want success", code, stderr)
			}
			if _, err := os.Stat(filepath.Join(dir, "example.sources")); !os.IsNotExist(err) {
				t.Fatalf("managed source still exists or cannot be checked: %v", err)
			}
			if data, err := os.ReadFile(keyPath); err != nil || string(data) != testSigningKey {
				t.Fatalf("shared keyring was not preserved: data=%q err=%v", data, err)
			}
			code, stdout, stderr := runInput(t, dir, "test", repository{Name: "example", Ensure: "Absent"})
			if code != 0 || stderr != "" {
				t.Fatalf("test Absent returned (%d, %q), want success", code, stderr)
			}
			var result testState
			if err := json.Unmarshal([]byte(stdout), &result); err != nil {
				t.Fatal(err)
			}
			if !result.InDesiredState {
				t.Fatalf("test Absent returned noncompliant despite shared keyring: %#v", result)
			}
		})
	}
}

func TestPrimaryOneLineSourcesReferenceIsRecognized(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "repo.asc")
	data := []byte("deb [arch=amd64 signed-by=" + keyPath + "] https://other.example stable main\n")
	referenced, err := oneLineSourceReferencesKey(data, keyPath)
	if err != nil || !referenced {
		t.Fatalf("oneLineSourceReferencesKey() = (%t, %v), want true, nil", referenced, err)
	}

	root := t.TempDir()
	sourceDir := filepath.Join(root, "sources.list.d")
	if err := os.MkdirAll(sourceDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sources.list"), data, 0644); err != nil {
		t.Fatal(err)
	}
	referenced, err = keyringReferenced(sourceDir, filepath.Join(sourceDir, "managed.sources"), keyPath)
	if err != nil || !referenced {
		t.Fatalf("keyringReferenced() for sources.list = (%t, %v), want true, nil", referenced, err)
	}
}

func TestTestReturnsActualStateAndCompliance(t *testing.T) {
	dir := t.TempDir()
	desired := validRepository()
	if _, _, stderr := runInput(t, dir, "set", desired); stderr != "" {
		t.Fatalf("failed to create repository: %s", stderr)
	}

	code, stdout, stderr := runInput(t, dir, "test", desired)
	if code != 0 || stderr != "" {
		t.Fatalf("test returned (%d, %q), want success", code, stderr)
	}
	var result testState
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatal(err)
	}
	if !result.InDesiredState || result.Name != desired.Name || result.Ensure != "Present" {
		t.Fatalf("test output = %#v, want compliant actual state", result)
	}

	desired.Components = []string{"main"}
	code, stdout, stderr = runInput(t, dir, "test", desired)
	if code != 0 || stderr != "" {
		t.Fatalf("test returned (%d, %q), want success", code, stderr)
	}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatal(err)
	}
	if result.InDesiredState {
		t.Fatalf("test output = %#v, want noncompliant state", result)
	}
}

func TestValidateRepositoryRejectsUnsafeNamesAndKeys(t *testing.T) {
	tests := []struct {
		name       string
		repository repository
	}{
		{name: "empty name", repository: repository{Ensure: "Absent"}},
		{name: "traversal", repository: repository{Name: "../outside", Ensure: "Absent"}},
		{name: "absolute path", repository: repository{Name: "/etc/apt/sources.list", Ensure: "Absent"}},
		{name: "backslash", repository: repository{Name: `folder\source`, Ensure: "Absent"}},
		{name: "URL key", repository: repository{Name: "example", URI: "https://repo.example", Suite: "stable", Components: []string{"main"}, SigningKey: "https://example.org/key"}},
		{name: "path key", repository: repository{Name: "example", URI: "https://repo.example", Suite: "stable", Components: []string{"main"}, SigningKey: "/etc/apt/keyrings/repo.gpg"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateRepository(&test.repository); err == nil {
				t.Fatal("validateRepository() succeeded, want error")
			}
		})
	}
}

func TestValidateRepositoryDefaultsEnsureAndValidatesPresentProperties(t *testing.T) {
	desired := validRepository()
	desired.Ensure = ""
	if err := validateRepository(&desired); err != nil {
		t.Fatal(err)
	}
	if desired.Ensure != "Present" {
		t.Fatalf("ensure = %q, want Present", desired.Ensure)
	}
	localRepository := validRepository()
	localRepository.URI = "file:///srv/apt/repository"
	if err := validateRepository(&localRepository); err != nil {
		t.Fatalf("valid file URI rejected: %v", err)
	}

	for _, test := range []struct {
		name string
		edit func(*repository)
	}{
		{name: "unsupported URI scheme", edit: func(r *repository) { r.URI = "ftp://repo.example" }},
		{name: "URI fragment", edit: func(r *repository) { r.URI += "#fragment" }},
		{name: "field injection", edit: func(r *repository) { r.Suite = "stable\nSigned-By: /tmp/key" }},
		{name: "missing components", edit: func(r *repository) { r.Components = nil }},
		{name: "duplicate component", edit: func(r *repository) { r.Components = []string{"main", "main"} }},
		{name: "invalid architecture", edit: func(r *repository) { r.Architectures = []string{"amd64\nTypes: deb-src"} }},
		{name: "duplicate architecture", edit: func(r *repository) { r.Architectures = []string{"amd64", "amd64"} }},
		{name: "private key", edit: func(r *repository) { r.SigningKey = strings.Replace(testSigningKey, "PUBLIC", "PRIVATE", 1) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			invalid := validRepository()
			test.edit(&invalid)
			if err := validateRepository(&invalid); err == nil {
				t.Fatal("validateRepository() succeeded, want error")
			}
		})
	}
}

func TestSetIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	desired := validRepository()
	if _, _, stderr := runInput(t, dir, "set", desired); stderr != "" {
		t.Fatalf("initial set failed: %s", stderr)
	}
	path := filepath.Join(dir, "example.sources")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, stderr := runInput(t, dir, "set", desired); stderr != "" {
		t.Fatalf("second set failed: %s", stderr)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("idempotent Set replaced the source file")
	}
}

func TestSetRepairsMalformedManagedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "example.sources")
	if err := os.WriteFile(path, []byte("not a Deb822 source"), 0600); err != nil {
		t.Fatal(err)
	}
	desired := validRepository()
	if _, _, stderr := runInput(t, dir, "set", desired); stderr != "" {
		t.Fatalf("set failed to repair malformed source file: %s", stderr)
	}
	actual, err := getRepository(dir, filepath.Join(dir, "keyrings"), desired.Name)
	if err != nil {
		t.Fatalf("repaired source is invalid: %v", err)
	}
	if !inDesiredState(desired, actual) {
		t.Fatalf("repaired source state = %#v, want %#v", actual, desired)
	}
}

func TestRunRejectsMalformedInputAndUnknownOperation(t *testing.T) {
	for _, test := range []struct {
		name  string
		args  []string
		input string
	}{
		{name: "malformed JSON", args: []string{"get"}, input: "{"},
		{name: "unknown property", args: []string{"get"}, input: `{"name":"example","unknown":true}`},
		{name: "unknown operation", args: []string{"delete"}, input: `{"name":"example","ensure":"Absent"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr strings.Builder
			dir := t.TempDir()
			code := run(test.args, strings.NewReader(test.input), &stdout, &stderr, dir, filepath.Join(dir, "keyrings"))
			if code == 0 || stdout.Len() != 0 || stderr.Len() == 0 {
				t.Fatalf("run() = (%d, %q, %q), want error with no stdout", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestGetRejectsSymlinkInsteadOfReadingOutsideOwnedFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "other.sources")
	if err := os.WriteFile(target, []byte("not an APT source"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "example.sources")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := getRepository(dir, filepath.Join(dir, "keyrings"), "example"); err == nil {
		t.Fatal("getRepository followed a symlink")
	}
}
