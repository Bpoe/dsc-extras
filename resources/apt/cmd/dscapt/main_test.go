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
	code := run([]string{operation}, strings.NewReader(encodeInput(t, value)), &stdout, &stderr, dir)
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
		"Signed-By:\n -----BEGIN PGP PUBLIC KEY BLOCK-----\n .\n YWJjZA==\n -----END PGP PUBLIC KEY BLOCK-----\n",
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
	if len(entries) != 2 {
		t.Errorf("source directory contains %d files, want only the managed and unrelated files", len(entries))
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
	code, stdout, stderr := runInput(t, dir, "set", repository{Name: "example", Ensure: "Absent"})
	if code != 0 || stderr != "" {
		t.Fatalf("set Absent returned (%d, %q), want success", code, stderr)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("managed file still exists or cannot be checked: %v", err)
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
	actual, err := getRepository(dir, desired.Name)
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
			code := run(test.args, strings.NewReader(test.input), &stdout, &stderr, t.TempDir())
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
	if _, err := getRepository(dir, "example"); err == nil {
		t.Fatal("getRepository followed a symlink")
	}
}
