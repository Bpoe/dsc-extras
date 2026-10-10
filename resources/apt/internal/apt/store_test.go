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

func writePublicKeyring(t *testing.T, store Store, name, armored string) {
	t.Helper()
	keyring, err := signingKeyBinary(armored)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, store.keyringPath(name), string(keyring))
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
	if filepath.Ext(store.keyringPath(desired.Name)) != ".gpg" {
		t.Fatalf("keyring path = %q, want .gpg", store.keyringPath(desired.Name))
	}
	keyInfo, err := os.Stat(store.keyringPath(desired.Name))
	if err != nil || keyInfo.Mode().Perm() != 0644 {
		t.Fatalf("keyring permissions = %v, err=%v, want 0644", keyInfo, err)
	}
	keyData, err := os.ReadFile(store.keyringPath(desired.Name))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := canonicalPublicKeyring(keyData); err != nil {
		t.Fatalf("stored keyring is not binary OpenPGP: %v", err)
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
	keyringPath := store.keyringPath(desired.Name)
	keyringBefore, err := os.Stat(keyringPath)
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
	keyringAfter, err := os.Stat(keyringPath)
	if err != nil || !os.SameFile(keyringBefore, keyringAfter) {
		t.Fatalf("idempotent Set replaced the binary keyring: info=%v err=%v", keyringAfter, err)
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
	entries, err := os.ReadDir(store.SourcesDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".dscapt-") && strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("temporary file left in source directory: %s", entry.Name())
		}
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
				return "Types: deb\nURIs: https://packages.example.org/debian\nSuites: stable\nComponents: main contrib\nSigned-By: /tmp/unmanaged.gpg\n"
			},
			keyring: func(Store) string { return publicTestKey },
		},
		{
			name: "missing signing key path",
			source: func(Store) string {
				return "Types: deb\nURIs: https://packages.example.org/debian\nSuites: stable\nComponents: main contrib\n"
			},
			keyring: func(Store) string { return publicTestKey },
		},
		{
			name: "another repository key path",
			source: func(Store) string {
				return "Types: deb\nURIs: https://packages.example.org/debian\nSuites: stable\nComponents: main contrib\nSigned-By: /etc/apt/keyrings/other.gpg\n"
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
				keyring := test.keyring(store)
				if keyring == "not a key" {
					writeFile(t, store.keyringPath(desired.Name), keyring)
				} else {
					writePublicKeyring(t, store, desired.Name, keyring)
				}
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
			sourceBefore, err := os.Stat(store.sourcePath(desired.Name))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Set(desired); err != nil {
				t.Fatalf("repeated Set after repair failed: %v", err)
			}
			sourceAfter, err := os.Stat(store.sourcePath(desired.Name))
			if err != nil || !os.SameFile(sourceBefore, sourceAfter) {
				t.Fatalf("repeated Set replaced repaired source: info=%v err=%v", sourceAfter, err)
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
	writePublicKeyring(t, store, desired.Name, desired.SigningKey)
	data := "# managed source\n\n  # comment\nSigned-By:  " + store.keyringPath(desired.Name) +
		"\nComponents: main\n contrib\nTypes: deb\nURIs: https://packages.example.org/debian\nSuites: stable\nArchitectures: amd64\n"
	writeFile(t, store.sourcePath(desired.Name), data)
	actual, compliant, err := store.Test(desired)
	if err != nil || !compliant {
		t.Fatalf("Test formatted/multiline source = (%#v, %t, %v), want compliant", actual, compliant, err)
	}
}

func TestExactPathSuiteOmitsComponentsAndRoundTrips(t *testing.T) {
	store := newTestStore(t)
	desired := validDesired()
	desired.Suite = "./"
	desired.Components = nil
	if _, err := store.Set(desired); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(store.sourcePath(desired.Name))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "Components:") {
		t.Fatalf("exact-path source unexpectedly includes components: %s", data)
	}
	actual, compliant, err := store.Test(desired)
	if err != nil || !compliant {
		t.Fatalf("Test exact-path repository=(%#v,%t,%v), want compliant", actual, compliant, err)
	}
}

func TestDuplicateFieldsAreDriftNotFatal(t *testing.T) {
	store := newTestStore(t)
	desired := validDesired()
	writePublicKeyring(t, store, desired.Name, desired.SigningKey)
	source := string(serializeDeb822(desired, store.keyringPath(desired.Name))) + "Trusted: no\nTrusted: yes\n"
	writeFile(t, store.sourcePath(desired.Name), source)
	actual, compliant, err := store.Test(desired)
	if err != nil || compliant {
		t.Fatalf("Test duplicate fields = (%#v, %t, %v), want noncompliant without operational error", actual, compliant, err)
	}
}

func TestAbsentOwnsAndRemovesExactlyItsFiles(t *testing.T) {
	for _, test := range []struct {
		name         string
		sourceExists bool
		keyExists    bool
	}{
		{name: "both files present", sourceExists: true, keyExists: true},
		{name: "only source present", sourceExists: true},
		{name: "only keyring present", keyExists: true},
		{name: "both files absent"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newTestStore(t)
			desired := validDesired()
			if test.sourceExists {
				writeFile(t, store.sourcePath(desired.Name), string(serializeDeb822(desired, store.keyringPath(desired.Name))))
			}
			if test.keyExists {
				writePublicKeyring(t, store, desired.Name, desired.SigningKey)
			}

			otherSource := filepath.Join(store.SourcesDir, "other.sources")
			otherKeyring := filepath.Join(store.KeyringsDir, "other.gpg")
			writeFile(t, otherSource, "unrelated source data")
			writeFile(t, otherKeyring, "unrelated key data")
			// Removal is owned-file based and does not parse other APT sources.
			writeFile(t, filepath.Join(store.SourcesDir, "malformed.sources"), "not valid Deb822")

			absent := Repository{Name: desired.Name, Ensure: "Absent"}
			_, compliant, err := store.Test(absent)
			if err != nil {
				t.Fatal(err)
			}
			if compliant != (!test.sourceExists && !test.keyExists) {
				t.Fatalf("Test compliance=%t with source=%t keyring=%t", compliant, test.sourceExists, test.keyExists)
			}
			if _, err := store.Set(absent); err != nil {
				t.Fatal(err)
			}
			if _, compliant, err := store.Test(absent); err != nil || !compliant {
				t.Fatalf("Test after removal=(%t,%v), want compliant", compliant, err)
			}
			if _, err := store.Set(absent); err != nil {
				t.Fatalf("repeated removal failed: %v", err)
			}
			for _, path := range []string{store.sourcePath(desired.Name), store.keyringPath(desired.Name)} {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("managed path remains at %s: %v", path, err)
				}
			}
			for _, path := range []string{otherSource, otherKeyring} {
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("unrelated path changed at %s: %v", path, err)
				}
			}
		})
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
