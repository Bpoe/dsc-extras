package apt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestStore(t *testing.T) Store {
	t.Helper()
	root := t.TempDir()
	return NewStore(filepath.Join(root, "sources.list.d"), filepath.Join(root, "keyrings"))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestGetWithNameOnlyAndActualState(t *testing.T) {
	store := newTestStore(t)
	desired := validDesired()
	if err := os.MkdirAll(store.SourcesDir, 0755); err != nil {
		t.Fatal(err)
	}
	actual, err := store.Get(desired.Name)
	if err != nil || actual.Ensure != "Absent" {
		t.Fatalf("Get missing repository = (%#v, %v), want Absent", actual, err)
	}
	if _, err := store.Set(desired); err != nil {
		t.Fatal(err)
	}
	actual, err = store.Get(desired.Name)
	if err != nil || !Equal(desired, actual) {
		t.Fatalf("Get current repository = (%#v, %v), want desired state", actual, err)
	}
}

func TestSetIsIdempotentAndReplacesAtomically(t *testing.T) {
	store := newTestStore(t)
	desired := validDesired()
	if _, err := store.Set(desired); err != nil {
		t.Fatal(err)
	}
	sourcePath := store.sourcePath(desired.Name)
	before, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Set(desired); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("idempotent Set replaced the source file")
	}

	desired.Components = []string{"main"}
	if _, err := store.Set(desired); err != nil {
		t.Fatal(err)
	}
	updated, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(updated), "Components: main\n") || strings.Contains(string(updated), "Components: main contrib") {
		t.Fatalf("property change was not reconciled: %s", updated)
	}
	if strings.Contains(string(updated), ".dscapt-") {
		t.Fatalf("temporary file left beside source: %s", updated)
	}
}

func TestSetPreservesUnchangedFileWhenRepairingOtherFile(t *testing.T) {
	store := newTestStore(t)
	desired := validDesired()
	if _, err := store.Set(desired); err != nil {
		t.Fatal(err)
	}

	keyringPath := store.keyringPath(desired.Name)
	keyringBefore, err := os.Stat(keyringPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(store.sourcePath(desired.Name)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Set(desired); err != nil {
		t.Fatal(err)
	}
	keyringAfter, err := os.Stat(keyringPath)
	if err != nil || !os.SameFile(keyringBefore, keyringAfter) {
		t.Fatalf("repairing missing source replaced valid keyring: info=%v err=%v", keyringAfter, err)
	}

	sourcePath := store.sourcePath(desired.Name)
	sourceBefore, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(keyringPath); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Set(desired); err != nil {
		t.Fatal(err)
	}
	sourceAfter, err := os.Stat(sourcePath)
	if err != nil || !os.SameFile(sourceBefore, sourceAfter) {
		t.Fatalf("repairing missing keyring replaced canonical source: info=%v err=%v", sourceAfter, err)
	}
}

func TestTestReportsRepairableDriftAndSetRepairs(t *testing.T) {
	tests := []struct {
		name    string
		source  func(Store) string
		keyring func(Store) string
	}{
		{name: "missing source"},
		{
			name: "missing keyring",
			source: func(store Store) string {
				return string(serializeDeb822(validDesired(), store.keyringPath("example")))
			},
		},
		{
			name:   "malformed source",
			source: func(Store) string { return "not deb822\n" },
		},
		{
			name: "invalid key",
			source: func(store Store) string {
				return string(serializeDeb822(validDesired(), store.keyringPath("example")))
			},
			keyring: func(Store) string { return "not a key" },
		},
		{
			name: "disabled repository",
			source: func(store Store) string {
				return string(append(serializeDeb822(validDesired(), store.keyringPath("example")), []byte("Enabled: no\n")...))
			},
			keyring: func(Store) string { return publicTestKey },
		},
		{
			name: "trusted repository",
			source: func(store Store) string {
				return string(append(serializeDeb822(validDesired(), store.keyringPath("example")), []byte("Trusted: yes\n")...))
			},
			keyring: func(Store) string { return publicTestKey },
		},
		{
			name: "unexpected behavior field",
			source: func(store Store) string {
				return string(append(serializeDeb822(validDesired(), store.keyringPath("example")), []byte("Check-Valid-Until: no\n")...))
			},
			keyring: func(Store) string { return publicTestKey },
		},
		{
			name: "wrong signing key path",
			source: func(Store) string {
				return "Types: deb\nURIs: https://packages.example.org/debian\nSuites: stable\nComponents: main contrib\nSigned-By: /tmp/unmanaged.asc\n"
			},
			keyring: func(Store) string { return publicTestKey },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := newTestStore(t)
			desired := validDesired()
			if test.source != nil {
				writeFile(t, store.sourcePath(desired.Name), test.source(store))
			}
			if test.keyring != nil {
				writeFile(t, store.keyringPath(desired.Name), test.keyring(store))
			}
			_, compliant, err := store.Test(desired)
			if err != nil {
				t.Fatalf("Test returned operational error for repairable drift: %v", err)
			}
			if compliant {
				t.Fatal("Test returned compliant for drift")
			}
			if _, err := store.Set(desired); err != nil {
				t.Fatalf("Set did not repair drift: %v", err)
			}
			actual, compliant, err := store.Test(desired)
			if err != nil || !compliant {
				t.Fatalf("Test after repair = (%#v, %t, %v), want compliant", actual, compliant, err)
			}
		})
	}
}

func TestGetMalformedSourceReturnsPartialObservedState(t *testing.T) {
	store := newTestStore(t)
	writeFile(t, store.sourcePath("example"), "Types: deb\nURIs: https://packages.example.org/debian\nEnabled: no\n")
	actual, err := store.Get("example")
	if err != nil {
		t.Fatal(err)
	}
	if actual.Ensure != "Present" || actual.URI != "https://packages.example.org/debian" || actual.SigningKey != "" {
		t.Fatalf("Get fabricated or omitted incorrect observed state: %#v", actual)
	}
}

func TestDeb822WhitespaceCommentsAndMultilineValues(t *testing.T) {
	store := newTestStore(t)
	desired := validDesired()
	writeFile(t, store.keyringPath(desired.Name), desired.SigningKey)
	data := "# managed source\n\n  # comment\nSigned-By:  " + store.keyringPath(desired.Name) +
		"\nComponents: main\n contrib\nTypes: deb\nURIs: https://packages.example.org/debian\nSuites: stable\nArchitectures: amd64\n"
	writeFile(t, store.sourcePath(desired.Name), data)
	actual, compliant, err := store.Test(desired)
	if err != nil || !compliant {
		t.Fatalf("Test formatted/multiline source = (%#v, %t, %v), want compliant", actual, compliant, err)
	}
}

func TestDuplicateFieldsAreDriftNotFatal(t *testing.T) {
	store := newTestStore(t)
	desired := validDesired()
	writeFile(t, store.keyringPath(desired.Name), desired.SigningKey)
	source := string(serializeDeb822(desired, store.keyringPath(desired.Name))) + "Trusted: no\nTrusted: yes\n"
	writeFile(t, store.sourcePath(desired.Name), source)
	actual, compliant, err := store.Test(desired)
	if err != nil || compliant {
		t.Fatalf("Test duplicate fields = (%#v, %t, %v), want noncompliant without operational error", actual, compliant, err)
	}
}

func TestSetAbsentPreservesSharedKeyring(t *testing.T) {
	for _, test := range []struct {
		name             string
		source           func(string) string
		primary          bool
		preservesKeyring bool
	}{
		{
			name:             "Deb822 whitespace-separated references",
			preservesKeyring: true,
			source: func(keyPath string) string {
				return "Types: deb\nURIs: https://other.example/debian\nSuites: stable\nComponents: main\n" +
					"Signed-By:\n  /etc/apt/keyrings/other.asc\n  " + keyPath + "\n"
			},
		},
		{
			name: "Deb822 inline public key is not a file reference",
			source: func(string) string {
				inlineKey := strings.ReplaceAll(publicTestKey, "\n\n", "\n.\n")
				return "Types: deb\nURIs: https://other.example/debian\nSuites: stable\nComponents: main\n" +
					"Signed-By:\n  " + strings.ReplaceAll(inlineKey, "\n", "\n  ") + "\n"
			},
		},
		{
			name:             "legacy list references",
			preservesKeyring: true,
			source: func(keyPath string) string {
				return "deb [arch=amd64 signed-by=/etc/apt/keyrings/other.asc," + keyPath + "] https://other.example stable main\n"
			},
		},
		{
			name:             "primary sources references",
			primary:          true,
			preservesKeyring: true,
			source: func(keyPath string) string {
				return "deb [signed-by=" + keyPath + "] https://other.example stable main\n"
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newTestStore(t)
			desired := validDesired()
			if _, err := store.Set(desired); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(store.SourcesDir, "other.sources")
			if strings.Contains(test.name, "list") {
				target = filepath.Join(store.SourcesDir, "other.list")
			}
			if test.primary {
				target = filepath.Join(filepath.Dir(store.SourcesDir), "sources.list")
			}
			writeFile(t, target, test.source(store.keyringPath(desired.Name)))
			absent := Repository{Name: desired.Name, Ensure: "Absent"}
			if _, err := store.Set(absent); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(store.sourcePath(desired.Name)); !os.IsNotExist(err) {
				t.Fatalf("owned source not removed: %v", err)
			}
			data, err := os.ReadFile(store.keyringPath(desired.Name))
			if test.preservesKeyring && (err != nil || strings.TrimSpace(string(data)) != strings.TrimSpace(publicTestKey)) {
				t.Fatalf("shared keyring deleted or changed: err=%v", err)
			}
			if !test.preservesKeyring && !os.IsNotExist(err) {
				t.Fatalf("unreferenced keyring was not removed: err=%v", err)
			}
			if _, compliant, err := store.Test(absent); err != nil || !compliant {
				t.Fatalf("Test absent shared keyring=(%t,%v), want compliant", compliant, err)
			}
		})
	}
}

func TestSetAbsentDoesNotDeleteKeyWhenReferencesAreMalformed(t *testing.T) {
	store := newTestStore(t)
	desired := validDesired()
	if _, err := store.Set(desired); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(store.SourcesDir, "broken.sources"), "this is not a valid Deb822 field\n")
	absent := Repository{Name: desired.Name, Ensure: "Absent"}
	if _, err := store.Set(absent); err == nil {
		t.Fatal("Set Absent succeeded despite uncertain reference scan")
	}
	if _, err := os.Stat(store.sourcePath(desired.Name)); err != nil {
		t.Fatalf("managed source removed before safe key scan: %v", err)
	}
	if _, err := os.Stat(store.keyringPath(desired.Name)); err != nil {
		t.Fatalf("keyring removed despite uncertain reference scan: %v", err)
	}
}

func TestSetRepairsManagedSymlinksWithoutFollowingThem(t *testing.T) {
	store := newTestStore(t)
	desired := validDesired()
	outside := filepath.Join(t.TempDir(), "outside")
	writeFile(t, outside, "unmanaged")
	if err := os.MkdirAll(store.SourcesDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, store.sourcePath(desired.Name)); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := os.MkdirAll(store.KeyringsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, store.keyringPath(desired.Name)); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := store.Set(desired); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{store.sourcePath(desired.Name), store.keyringPath(desired.Name)} {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("managed symlink not replaced by regular file at %s: info=%v err=%v", path, info, err)
		}
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "unmanaged" {
		t.Fatalf("outside symlink target modified: data=%q err=%v", data, err)
	}
}

func TestGetPropagatesFilesystemErrors(t *testing.T) {
	root := t.TempDir()
	notDirectory := filepath.Join(root, "not-directory")
	writeFile(t, notDirectory, "file")
	store := NewStore(notDirectory, filepath.Join(root, "keys"))
	if _, err := store.Get("example"); err == nil {
		t.Fatal("Get swallowed source path I/O error")
	}
}
