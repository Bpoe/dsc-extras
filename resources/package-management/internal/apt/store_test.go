package apt

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) Store {
	t.Helper()
	store := NewStore(t.TempDir())
	if err := os.MkdirAll(filepath.Join(store.Root, "var", "lib", "dpkg", "info"), 0755); err != nil {
		t.Fatal(err)
	}
	return store
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	epoch := time.Unix(946684800, 0)
	if err := os.Chtimes(path, epoch, epoch); err != nil {
		t.Fatal(err)
	}
}

func fixtureFile(t *testing.T, store Store, reference, content string) string {
	t.Helper()
	path, err := store.resolve(reference)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, content)
	return path
}

func writePublicKeyring(t *testing.T, store Store, name, armored string) {
	t.Helper()
	keyring, err := signingKeyBinary(armored)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, store.keyringPath(name), string(keyring))
}

type fileSnapshot struct {
	info os.FileInfo
	data []byte
}

func snapshot(t *testing.T, root string) map[string]fileSnapshot {
	t.Helper()
	result := map[string]fileSnapshot{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		var data []byte
		if info.Mode().IsRegular() {
			data, err = os.ReadFile(path)
			if err != nil {
				return err
			}
		}
		result[path] = fileSnapshot{info, data}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func assertUnchanged(t *testing.T, before, after map[string]fileSnapshot) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("filesystem entry count changed: %d -> %d", len(before), len(after))
	}
	for path, a := range before {
		b, ok := after[path]
		if !ok {
			t.Errorf("filesystem entry disappeared: %s", path)
			continue
		}
		if !bytes.Equal(a.data, b.data) || a.info.Mode() != b.info.Mode() ||
			!a.info.ModTime().Equal(b.info.ModTime()) || !os.SameFile(a.info, b.info) {
			t.Errorf("filesystem entry changed (bytes, mode, timestamp or identity): %s", path)
		}
	}
}

func assertCompliantNoOp(t *testing.T, store Store, desired Repository) {
	t.Helper()
	before := snapshot(t, store.Root)
	actual, err := store.Get(desired)
	if err != nil || actual.Ensure != "Present" || !Equal(desired, actual) {
		t.Fatalf("Get logical repository = (%+v, %v)", actual, err)
	}
	for i := 0; i < 2; i++ {
		if _, differences, err := store.Test(desired); err != nil || differences == nil || len(differences) != 0 {
			t.Fatalf("Test = (%v, %v), want empty differences", differences, err)
		}
		if _, err := store.Set(desired); err != nil {
			t.Fatal(err)
		}
	}
	assertUnchanged(t, before, snapshot(t, store.Root))
}

func TestHomelabAnsibleDockerAndMicrosoftPackageAreAlreadyCompliant(t *testing.T) {
	for _, vendor := range []string{"docker", "microsoft"} {
		t.Run(vendor, func(t *testing.T) {
			store := newTestStore(t)
			source, err := os.ReadFile(filepath.Join("testdata", vendor+".list"))
			if err != nil {
				t.Fatal(err)
			}
			var desired Repository
			if vendor == "docker" {
				key, err := os.ReadFile(filepath.Join("testdata", "docker.asc"))
				if err != nil {
					t.Fatal(err)
				}
				fixtureFile(t, store, "/etc/apt/keyrings/docker.asc", string(key))
				desired = Repository{Name: "my-docker", Ensure: "Present", URI: "https://download.docker.com/linux/debian",
					Suite: "trixie", Components: []string{"stable"}, Architectures: []string{"amd64"}, SigningKey: string(key)}
			} else {
				key, err := os.ReadFile(filepath.Join("testdata", "microsoft-prod.gpg"))
				if err != nil {
					t.Fatal(err)
				}
				fixtureFile(t, store, "/usr/share/keyrings/microsoft-prod.gpg", string(key))
				fixtureFile(t, store, "/var/lib/dpkg/info/packages-microsoft-prod.list",
					"/etc/apt/sources.list.d/microsoft-prod.list\n/usr/share/keyrings/microsoft-prod.gpg\n")
				armored, err := armoredKeyring(key)
				if err != nil {
					t.Fatal(err)
				}
				desired = Repository{Name: "my-microsoft", Ensure: "Present", URI: "https://packages.microsoft.com/debian/12/prod",
					Suite: "bookworm", Components: []string{"main"}, Architectures: []string{"armhf", "amd64", "arm64"}, SigningKey: armored}
			}
			name := vendor
			if vendor == "microsoft" {
				name = "microsoft-prod"
			}
			fixtureFile(t, store, "/etc/apt/sources.list.d/"+name+".list", string(source))
			assertCompliantNoOp(t, store, desired)
		})
	}
}

func TestMissingRepositoryCreatesDeb822AndManagedBinaryKey(t *testing.T) {
	store := newTestStore(t)
	desired := validDesired()
	if actual, err := store.Get(desired); err != nil || actual.Ensure != "Absent" {
		t.Fatalf("Get = (%+v, %v)", actual, err)
	}
	before := snapshot(t, store.Root)
	if _, differences, err := store.Test(desired); err != nil || !slices.Equal(differences, []string{"ensure"}) {
		t.Fatalf("Test missing = (%v, %v)", differences, err)
	}
	assertUnchanged(t, before, snapshot(t, store.Root))
	if _, err := store.Set(desired); err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(store.sourcePath(desired.Name))
	if err != nil || !bytes.Contains(source, []byte("Signed-By: /etc/apt/keyrings/example.gpg\n")) {
		t.Fatalf("invalid source %q, %v", source, err)
	}
	key, err := store.ReadKeyring(store.keyringReference(desired.Name))
	if err != nil || key.Format != BinaryKeyring {
		t.Fatalf("invalid managed binary keyring: %+v, %v", key, err)
	}
	assertCompliantNoOp(t, store, desired)
}

func TestSemanticDifferencesDoNotWrite(t *testing.T) {
	disabled := false
	otherKey := otherTestKey(t)
	for _, extension := range []string{".asc", ".gpg"} {
		t.Run(extension, func(t *testing.T) {
			store := newTestStore(t)
			desired := validDesired()
			desired.Architectures = []string{"amd64", "arm64"}
			key := desired.SigningKey
			if extension == ".gpg" {
				binary, err := signingKeyBinary(key)
				if err != nil {
					t.Fatal(err)
				}
				key = string(binary)
			}
			reference := "/usr/share/keyrings/external" + extension
			fixtureFile(t, store, reference, key)
			actual := desired
			actual.Components = []string{"contrib", "main"}
			actual.Architectures = []string{"arm64", "amd64"}
			fixtureFile(t, store, "/etc/apt/sources.list.d/external.sources", string(serializeDeb822(actual, reference)))
			desired.SigningKey = strings.ReplaceAll(publicTestKey, "\n", "\r\n")
			assertCompliantNoOp(t, store, desired)

			before := snapshot(t, store.Root)
			desired.Components = []string{"main"}
			desired.Architectures = nil
			desired.Enabled = &disabled
			desired.SigningKey = otherKey
			want := []string{"components", "architectures", "signingKey", "enabled"}
			if _, differences, err := store.Test(desired); err != nil || !slices.Equal(differences, want) {
				t.Fatalf("Test drift = (%v, %v), want %v", differences, err, want)
			}
			desired.URI += "/missing"
			if _, differences, err := store.Test(desired); err != nil || !slices.Equal(differences, []string{"ensure"}) {
				t.Fatalf("Test missing = (%v, %v), want ensure only", differences, err)
			}
			desired.Ensure = "Absent"
			if _, differences, err := store.Test(desired); err != nil || differences == nil || len(differences) != 0 {
				t.Fatalf("Test absent = (%v, %v), want empty differences", differences, err)
			}
			if _, err := store.Set(desired); err != nil {
				t.Fatal(err)
			}
			assertUnchanged(t, before, snapshot(t, store.Root))
		})
	}
}

func TestAllDocumentAndKeyringFormatsRoundTripWithoutWrites(t *testing.T) {
	store := newTestStore(t)
	binary, err := signingKeyBinary(publicTestKey)
	if err != nil {
		t.Fatal(err)
	}
	fixtureFile(t, store, "/etc/apt/keyrings/vendor.asc", strings.ReplaceAll(publicTestKey, "\n", "\r\n"))
	fixtureFile(t, store, "/usr/share/keyrings/vendor.gpg", string(binary))
	fixtureFile(t, store, "/etc/apt/sources.list", "# original\r\ndeb [arch=amd64 signed-by=/etc/apt/keyrings/vendor.asc] https://a.example stable main # keep\r\n")
	fixtureFile(t, store, "/etc/apt/sources.list.d/custom.list", "# header\n# deb [signed-by=/usr/share/keyrings/vendor.gpg] https://b.example stable main\n")
	fixtureFile(t, store, "/etc/apt/sources.list.d/custom.sources", "# no normalization\r\nTypes: deb deb-src\r\nURIs: https://a.example\r\n https://b.example\r\nSuites: stable testing\r\nComponents: main\r\n contrib\r\nX-Owner: ansible\r\nSigned-By: /usr/share/keyrings/vendor.gpg\r\n\r\n# footer")
	before := snapshot(t, store.Root)
	documents, err := store.Discover()
	if err != nil || len(documents) != 3 {
		t.Fatalf("Discover = (%d, %v)", len(documents), err)
	}
	for _, d := range documents {
		if d.Format == Deb822Format && len(d.Entries) != 8 {
			t.Fatalf("multi-value Deb822 expanded to %d entries, want 8", len(d.Entries))
		}
		for attempt := 0; attempt < 2; attempt++ {
			if err := d.WriteToDisk(); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, ref := range []string{"/etc/apt/keyrings/vendor.asc", "/usr/share/keyrings/vendor.gpg"} {
		key, err := store.ReadKeyring(ref)
		if err != nil {
			t.Fatal(err)
		}
		if err := key.WriteToDisk(); err != nil {
			t.Fatal(err)
		}
	}
	assertUnchanged(t, before, snapshot(t, store.Root))
}

func TestEditingOneEntryPreservesOtherEntriesAndOriginalLocation(t *testing.T) {
	for _, format := range []string{".list", ".sources"} {
		t.Run(format, func(t *testing.T) {
			store := newTestStore(t)
			desired := validDesired()
			writePublicKeyring(t, store, "shared", desired.SigningKey)
			key := store.keyringReference("shared")
			var source, unrelated string
			if format == ".list" {
				unrelated = "# unrelated comment\r\ndeb [signed-by=" + key + "] https://other.example stable main # preserve exactly\r\n"
				source = unrelated + "deb [arch=amd64 signed-by=" + key + "] " + desired.URI + " stable main contrib # owned by ansible\r\n"
			} else {
				unrelated = "# other stanza\r\nTypes: deb\r\nURIs: https://other.example\r\nSuites: stable\r\nComponents: main\r\nSigned-By: " + key + "\r\n\r\n"
				source = unrelated + strings.ReplaceAll(string(serializeDeb822(desired, key)), "\n", "\r\n") + "X-Owner: ansible\r\n"
			}
			path := fixtureFile(t, store, "/etc/apt/sources.list.d/package-independent"+format, source)
			keyBefore := snapshot(t, filepath.Dir(store.keyringPath("shared")))
			desired.Components = []string{"main"}
			if _, err := store.Set(desired); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil || !strings.HasPrefix(string(data), unrelated) {
				t.Fatalf("unrelated entry changed: %q, %v", data, err)
			}
			if format == ".sources" && !bytes.Contains(data, []byte("X-Owner: ansible\r\n")) {
				t.Fatal("unrelated metadata removed")
			}
			if _, err := os.Stat(store.sourcePath(desired.Name)); !os.IsNotExist(err) {
				t.Fatal("Set created a duplicate managed source")
			}
			assertUnchanged(t, keyBefore, snapshot(t, filepath.Dir(store.keyringPath("shared"))))
			assertCompliantNoOp(t, store, desired)
		})
	}
}

func TestMultiValueStanzaCanBeSplitWithoutChangingOtherRepositories(t *testing.T) {
	store := newTestStore(t)
	desired := validDesired()
	writePublicKeyring(t, store, "shared", desired.SigningKey)
	source := "Types: deb deb-src\nURIs: " + desired.URI + " https://other.example\nSuites: stable testing\nComponents: main contrib\nArchitectures: amd64\nSigned-By: " + store.keyringReference("shared") + "\nX-Keep: custom\n"
	path := fixtureFile(t, store, "/etc/apt/sources.list.d/multi.sources", source)
	original, err := store.readDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	desired.Components = []string{"main"}
	if _, err := store.Set(desired); err != nil {
		t.Fatal(err)
	}
	updated, err := store.readDocument(path)
	if err != nil || len(updated.Entries) != 8 {
		t.Fatalf("split stanza = %+v, %v", updated, err)
	}
	for i, entry := range updated.Entries {
		want := original.Entries[i]
		if entry.URI == desired.URI && entry.Suite == desired.Suite && entry.Type == "deb" {
			want.Components = desired.Components
		}
		if !equalStrings(entry.Components, want.Components) || entry.URI != want.URI ||
			entry.Suite != want.Suite || entry.Type != want.Type || entry.Options["x-keep"] != "custom" {
			t.Errorf("entry %d changed unexpectedly: %+v", i, entry)
		}
	}
	assertCompliantNoOp(t, store, desired)
}

func TestSplitComponentsAreLogicallyEquivalent(t *testing.T) {
	store := newTestStore(t)
	desired := validDesired()
	writePublicKeyring(t, store, "shared", desired.SigningKey)
	var lines string
	for _, component := range desired.Components {
		lines += fmt.Sprintf("deb [arch=amd64 signed-by=%s] %s stable %s\n", store.keyringReference("shared"), desired.URI, component)
	}
	fixtureFile(t, store, "/etc/apt/sources.list", lines)
	assertCompliantNoOp(t, store, desired)
	before := snapshot(t, store.Root)
	desired.Components = []string{"main"}
	if _, err := store.Set(desired); err == nil || !strings.Contains(err.Error(), "multiple entries") {
		t.Fatalf("ambiguous multi-entry edit not rejected: %v", err)
	}
	assertUnchanged(t, before, snapshot(t, store.Root))
}

func TestUnsafeChangesAndConflictsDoNotWrite(t *testing.T) {
	cases := []struct {
		name      string
		setup     func(*testing.T, Store, *Repository)
		errorText string
	}{
		{"shared managed key", func(t *testing.T, s Store, r *Repository) {
			fixtureFile(t, s, "/etc/apt/sources.list", "deb [signed-by="+s.keyringReference(r.Name)+"] https://other.example stable main\n")
			r.SigningKey = otherTestKey(t)
		}, "shared"},
		{"disabled reference shares key", func(t *testing.T, s Store, r *Repository) {
			fixtureFile(t, s, "/etc/apt/sources.list", "# deb [signed-by="+s.keyringReference(r.Name)+"] https://other.example stable main\n")
			r.SigningKey = otherTestKey(t)
		}, "shared"},
		{"duplicate definition", func(t *testing.T, s Store, r *Repository) {
			fixtureFile(t, s, "/etc/apt/sources.list", "deb [arch=amd64 signed-by="+s.keyringReference(r.Name)+"] "+r.URI+" stable main\n")
		}, "conflicting"},
		{"architecture conflict", func(t *testing.T, s Store, r *Repository) {
			fixtureFile(t, s, "/etc/apt/sources.list", "deb [arch=arm64 signed-by="+s.keyringReference(r.Name)+"] "+r.URI+" stable non-free\n")
		}, "conflicting"},
		{"different Signed-By paths conflict", func(t *testing.T, s Store, r *Repository) {
			writePublicKeyring(t, s, "same-key-other-path", r.SigningKey)
			fixtureFile(t, s, "/etc/apt/sources.list", "deb [arch=amd64 signed-by="+s.keyringReference("same-key-other-path")+"] "+r.URI+" stable non-free\n")
		}, "conflicting"},
		{"malformed document", func(t *testing.T, s Store, r *Repository) {
			fixtureFile(t, s, "/etc/apt/sources.list.d/broken.sources", "Types: deb\nURIs: broken\n")
		}, "requires Types"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			desired := validDesired()
			if _, err := store.Set(desired); err != nil {
				t.Fatal(err)
			}
			tc.setup(t, store, &desired)
			before := snapshot(t, store.Root)
			if _, err := store.Set(desired); err == nil || !strings.Contains(err.Error(), tc.errorText) {
				t.Fatalf("Set error = %v, want %q", err, tc.errorText)
			}
			assertUnchanged(t, before, snapshot(t, store.Root))
		})
	}
}

func TestSignedTrustAndArchitectureRestrictionsAreNotIgnored(t *testing.T) {
	for _, option := range []string{"", "trusted=yes", "trusted=no", "allow-insecure=yes", "check-valid-until=no", "arch=arm64", "arch=amd64,arm64", "arch+=amd64", "signed-by=/etc/apt/keyrings/example.gpg,0123456789ABCDEF0123456789ABCDEF01234567!"} {
		t.Run(option, func(t *testing.T) {
			store := newTestStore(t)
			desired := validDesired()
			writePublicKeyring(t, store, desired.Name, desired.SigningKey)
			source := "deb"
			options := option
			if option == "" {
				options = "arch=amd64"
			} else {
				if !strings.Contains(option, "signed-by=") {
					options += " signed-by=" + store.keyringReference(desired.Name)
				}
				if !strings.Contains(option, "arch") {
					options += " arch=amd64"
				}
			}
			source += " [" + options + "]"
			source += " " + desired.URI + " stable main contrib\n"
			fixtureFile(t, store, "/etc/apt/sources.list", source)
			before := snapshot(t, store.Root)
			if _, differences, err := store.Test(desired); err == nil && len(differences) == 0 {
				t.Fatal("incompatible trust/architecture was treated as compliant")
			}
			assertUnchanged(t, before, snapshot(t, store.Root))
		})
	}
}

func TestFormattedDeb822AndArchitectureModifiersRemainUntouched(t *testing.T) {
	store := newTestStore(t)
	desired := validDesired()
	fixtureFile(t, store, "/etc/apt/keyrings/ansible.asc", publicTestKey)
	source := "# hand-formatted\r\nSigned-By: /etc/apt/keyrings/ansible.asc\r\ncomponents: contrib\r\n main\r\nSuites: stable\r\nURIs: " + desired.URI + "\r\nTypes: deb\r\nArchitectures: amd64 arm64\r\nArchitectures-Remove: arm64\r\nEnabled: yes\r\nX-Owner: ansible\r\n"
	fixtureFile(t, store, "/etc/apt/sources.list.d/not-dsc.sources", source)
	assertCompliantNoOp(t, store, desired)
	desired.Components = []string{"main"}
	if _, err := store.Set(desired); err != nil {
		t.Fatal(err)
	}
	assertCompliantNoOp(t, store, desired)
	desired.Architectures = []string{"arm64"}
	if _, err := store.Set(desired); err != nil {
		t.Fatal(err)
	}
	assertCompliantNoOp(t, store, desired)
}

func TestSymlinkedPathsAreReadOnly(t *testing.T) {
	for _, kind := range []string{"source", "keyring"} {
		t.Run(kind, func(t *testing.T) {
			store := newTestStore(t)
			desired := validDesired()
			if _, err := store.Set(desired); err != nil {
				t.Fatal(err)
			}
			path := store.sourcePath(desired.Name)
			target := filepath.Join(store.Root, "actual.sources")
			if kind == "keyring" {
				path = store.keyringPath(desired.Name)
				target = filepath.Join(store.Root, "actual.gpg")
			}
			if err := os.Rename(path, target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			assertCompliantNoOp(t, store, desired)
			before := snapshot(t, store.Root)
			if kind == "source" {
				desired.Components = []string{"main"}
			} else {
				desired.SigningKey = otherTestKey(t)
			}
			if _, err := store.Set(desired); err == nil || !strings.Contains(err.Error(), "symlink") {
				t.Fatalf("symlink mutation not rejected: %v", err)
			}
			assertUnchanged(t, before, snapshot(t, store.Root))
		})
	}
}

func TestMissingManagedFileRepairPreservesOtherFile(t *testing.T) {
	for _, missing := range []string{"source", "keyring"} {
		t.Run(missing, func(t *testing.T) {
			store := newTestStore(t)
			desired := validDesired()
			if _, err := store.Set(desired); err != nil {
				t.Fatal(err)
			}
			remove, keep := store.sourcePath(desired.Name), store.keyringPath(desired.Name)
			if missing == "keyring" {
				remove, keep = keep, remove
			}
			if err := os.Remove(remove); err != nil {
				t.Fatal(err)
			}
			before := snapshot(t, keep)
			if _, err := store.Set(desired); err != nil {
				t.Fatal(err)
			}
			assertUnchanged(t, before, snapshot(t, keep))
			assertCompliantNoOp(t, store, desired)
		})
	}
}

func TestCreatingSignedSourceDoesNotConflictWithExistingDebSrc(t *testing.T) {
	store := newTestStore(t)
	desired := validDesired()
	fixtureFile(t, store, "/etc/apt/sources.list", "deb-src "+desired.URI+" stable main\n")
	before := snapshot(t, store.Root)
	if _, err := store.Set(desired); err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("conflicting source creation not rejected: %v", err)
	}
	assertUnchanged(t, before, snapshot(t, store.Root))
}

func TestRelatedDebSrcSecurityOptions(t *testing.T) {
	for _, format := range []string{".list", ".sources"} {
		for _, option := range []struct {
			name, field, value string
			reject             bool
		}{
			{name: "defaults"},
			{name: "trusted", field: "Trusted", value: "yes", reject: true},
			{name: "untrusted override", field: "Trusted", value: "no", reject: true},
			{name: "insecure", field: "Allow-Insecure", value: "yes", reject: true},
			{name: "weak", field: "Allow-Weak", value: "yes", reject: true},
			{name: "downgrade", field: "Allow-Downgrade-To-Insecure", value: "yes", reject: true},
			{name: "expiry unchecked", field: "Check-Valid-Until", value: "no", reject: true},
			{name: "date unchecked", field: "Check-Date", value: "no", reject: true},
			{name: "insecure denied", field: "Allow-Insecure", value: "no"},
			{name: "weak denied", field: "Allow-Weak", value: "no"},
			{name: "downgrade denied", field: "Allow-Downgrade-To-Insecure", value: "no"},
			{name: "expiry checked", field: "Check-Valid-Until", value: "yes"},
			{name: "date checked", field: "Check-Date", value: "yes"},
		} {
			for _, state := range []string{"compliant", "drift", "missing"} {
				t.Run(format+"/"+option.name+"/"+state, func(t *testing.T) {
					store := newTestStore(t)
					desired := validDesired()
					keyReference := store.keyringReference(desired.Name)
					writePublicKeyring(t, store, desired.Name, desired.SigningKey)
					if state != "missing" {
						writeFile(t, store.sourcePath(desired.Name), string(serializeDeb822(desired, keyReference)))
					}
					source := strings.Replace(string(serializeDeb822(desired, keyReference)), "Types: deb\n", "Types: deb-src\n", 1)
					if option.field != "" {
						source += option.field + ": " + option.value + "\n"
					}
					if format == ".list" {
						options := "signed-by=" + keyReference
						if option.field != "" {
							options += " " + strings.ToLower(option.field) + "=" + option.value
						}
						source = "deb-src [" + options + "] " + desired.URI + " " + desired.Suite + " main contrib\n"
					}
					path := fixtureFile(t, store, "/etc/apt/sources.list.d/related"+format, "# preserve source packages\r\n"+source)
					if state == "drift" {
						desired.Components = []string{"main"}
					}
					before := snapshot(t, store.Root)
					if option.reject {
						diagnostic := fmt.Sprintf("APT option %q=%q", strings.ToLower(option.field), option.value)
						if state != "missing" {
							if _, err := store.Get(desired); err == nil || !strings.Contains(err.Error(), path) ||
								!strings.Contains(err.Error(), diagnostic) {
								t.Errorf("Get ignored unsafe related source: %v", err)
							}
							if _, _, err := store.Test(desired); err == nil {
								t.Error("Test ignored unsafe related source")
							}
						}
						if _, err := store.Set(desired); err == nil ||
							!strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), diagnostic) {
							t.Errorf("Set must identify the unsafe related source and option: %v", err)
						}
						assertUnchanged(t, before, snapshot(t, store.Root))
						return
					}
					if state == "compliant" {
						assertCompliantNoOp(t, store, desired)
						return
					}
					if _, err := store.Set(desired); err != nil {
						t.Fatal(err)
					}
					assertUnchanged(t, map[string]fileSnapshot{path: before[path]}, snapshot(t, path))
					assertCompliantNoOp(t, store, desired)
				})
			}
		}
	}
}

func TestDebSrcSecurityValidationOnlyAppliesToEnabledRelatedEntries(t *testing.T) {
	for _, scope := range []string{"disabled", "different URI", "different suite"} {
		t.Run(scope, func(t *testing.T) {
			store := newTestStore(t)
			desired := validDesired()
			writePublicKeyring(t, store, desired.Name, desired.SigningKey)
			writeFile(t, store.sourcePath(desired.Name), string(serializeDeb822(desired, store.keyringReference(desired.Name))))
			related := desired
			switch scope {
			case "disabled":
				enabled := false
				related.Enabled = &enabled
			case "different URI":
				related.URI = "https://other.example"
			case "different suite":
				related.Suite = "other"
			}
			source := strings.Replace(string(serializeDeb822(related, store.keyringReference(desired.Name))), "Types: deb\n", "Types: deb-src\n", 1)
			path := fixtureFile(t, store, "/etc/apt/sources.list.d/unrelated.sources", source+"Trusted: yes\n")
			assertCompliantNoOp(t, store, desired)
			before := snapshot(t, path)
			desired.Components = []string{"main"}
			if _, err := store.Set(desired); err != nil {
				t.Fatal(err)
			}
			assertUnchanged(t, before, snapshot(t, path))
			assertCompliantNoOp(t, store, desired)
		})
	}
}

func TestDiscoveryPropagatesFilesystemErrorsAndRejectsMalformedSources(t *testing.T) {
	store := newTestStore(t)
	fixtureFile(t, store, "/etc/apt/sources.list.d", "not a directory")
	if _, err := store.Get(validDesired()); err == nil {
		t.Fatal("Get swallowed directory I/O error")
	}
	for _, source := range []string{
		"Types: deb\nURIs: https://example.org\nSuites: stable\nComponents: main\nTrusted: no\nTrusted: yes\n",
		"Types: deb\nURIs: https://example.org\nSuites: stable\nComponents: main\nEnabled: maybe\n",
	} {
		store := newTestStore(t)
		fixtureFile(t, store, "/etc/apt/sources.list.d/bad.sources", source)
		before := snapshot(t, store.Root)
		if _, err := store.Set(validDesired()); err == nil {
			t.Fatal("Set ignored malformed APT configuration")
		}
		assertUnchanged(t, before, snapshot(t, store.Root))
	}
}

func TestUnsignedSourceCanBeRestrictedWithoutAddingDuplicate(t *testing.T) {
	store := newTestStore(t)
	desired := validDesired()
	path := fixtureFile(t, store, "/etc/apt/sources.list", "deb [arch=amd64] "+desired.URI+" stable main contrib\n")
	if _, err := store.Set(desired); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || !bytes.Contains(data, []byte("signed-by=/etc/apt/keyrings/example.gpg")) {
		t.Fatalf("source was not restricted in place: %s %v", data, err)
	}
	if _, err := os.Stat(store.sourcePath(desired.Name)); !os.IsNotExist(err) {
		t.Fatal("created duplicate instead of editing source")
	}
	assertCompliantNoOp(t, store, desired)
}

func TestExternalAndManagedKeysCanRotateWithoutOwnershipMarkers(t *testing.T) {
	for _, managed := range []bool{false, true} {
		t.Run(fmt.Sprint(managed), func(t *testing.T) {
			store := newTestStore(t)
			desired := validDesired()
			if _, err := store.Set(desired); err != nil {
				t.Fatal(err)
			}
			if !managed {
				writeFile(t, store.sourcePath(desired.Name), string(serializeDeb822(desired, store.keyringReference(desired.Name))))
			}
			before := snapshot(t, store.Root)
			desired.SigningKey = otherTestKey(t)
			if _, err := store.Set(desired); err != nil {
				t.Fatal(err)
			}
			after := snapshot(t, store.Root)
			assertUnchanged(t, map[string]fileSnapshot{store.sourcePath(desired.Name): before[store.sourcePath(desired.Name)]},
				map[string]fileSnapshot{store.sourcePath(desired.Name): after[store.sourcePath(desired.Name)]})
			assertCompliantNoOp(t, store, desired)
		})
	}
}

func TestEnabledStateAndLogicalRemovalPreserveUnrelatedEntriesAndKeys(t *testing.T) {
	for _, ext := range []string{".list", ".sources"} {
		t.Run(ext, func(t *testing.T) {
			store := newTestStore(t)
			desired := validDesired()
			writePublicKeyring(t, store, "shared", desired.SigningKey)
			var content string
			if ext == ".list" {
				content = "deb [arch=amd64 signed-by=" + store.keyringReference("shared") + "] " + desired.URI + " stable main contrib\n"
			} else {
				content = string(serializeDeb822(desired, store.keyringReference("shared")))
			}
			path := fixtureFile(t, store, "/etc/apt/sources.list.d/custom"+ext, "# retained\n"+content)
			enabled := false
			desired.Enabled = &enabled
			if _, err := store.Set(desired); err != nil {
				t.Fatal(err)
			}
			assertCompliantNoOp(t, store, desired)
			enabled = true
			if _, err := store.Set(desired); err != nil {
				t.Fatal(err)
			}
			assertCompliantNoOp(t, store, desired)
			desired.Ensure = "Absent"
			if _, err := store.Set(desired); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(store.keyringPath("shared")); err != nil {
				t.Fatal("removal changed shared signing material")
			}
			d, err := store.readDocument(path)
			if err != nil || len(d.Entries) != 0 {
				t.Fatalf("logical removal failed: %+v %v", d, err)
			}
			before := snapshot(t, store.Root)
			if _, err := store.Set(desired); err != nil {
				t.Fatal(err)
			}
			assertUnchanged(t, before, snapshot(t, store.Root))
		})
	}
}

func TestInlineSigningKeyAndExactPathSuite(t *testing.T) {
	store := newTestStore(t)
	desired := validDesired()
	desired.Suite, desired.Components = "./", nil
	desired.Architectures = nil
	inline := strings.ReplaceAll(strings.TrimSpace(publicTestKey), "\n\n", "\n.\n")
	inline = strings.ReplaceAll(inline, "\n", "\n ")
	fixtureFile(t, store, "/etc/apt/sources.list.d/inline.sources", "Types: deb\nURIs: "+desired.URI+"/\nSuites: ./\nSigned-By:\n "+inline+"\n")
	assertCompliantNoOp(t, store, desired)
}

func TestExistingDeclaredPathsAreReconciled(t *testing.T) {
	for _, collision := range []string{"source", "key", "ownership unavailable"} {
		t.Run(collision, func(t *testing.T) {
			store := newTestStore(t)
			desired := validDesired()
			switch collision {
			case "source":
				other := desired
				other.URI = "https://other.example"
				writeFile(t, store.sourcePath(desired.Name), string(serializeDeb822(other, "/usr/share/keyrings/other.gpg")))
			case "key":
				writePublicKeyring(t, store, desired.Name, otherTestKey(t))
			case "ownership unavailable":
				if err := os.Remove(filepath.Join(store.Root, "var", "lib", "dpkg", "info")); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.Set(desired); err != nil {
				t.Fatal(err)
			}
			assertCompliantNoOp(t, store, desired)
		})
	}
}

func TestConcurrentEditsAreRejectedBeforeWrites(t *testing.T) {
	store := newTestStore(t)
	desired := validDesired()
	if _, err := store.Set(desired); err != nil {
		t.Fatal(err)
	}
	d, err := store.readDocument(store.sourcePath(desired.Name))
	if err != nil {
		t.Fatal(err)
	}
	d.Entries[0].Components = []string{"main"}
	writeFile(t, d.Path, string(d.OriginalContents)+"# concurrent edit\n")
	before := snapshot(t, store.Root)
	if err := d.WriteToDisk(); err == nil || !strings.Contains(err.Error(), "changed since discovery") {
		t.Fatalf("concurrent edit not detected: %v", err)
	}
	assertUnchanged(t, before, snapshot(t, store.Root))
}

func TestHardlinkedKeyAliasesAreNotOverwritten(t *testing.T) {
	store := newTestStore(t)
	desired := validDesired()
	if _, err := store.Set(desired); err != nil {
		t.Fatal(err)
	}
	keyAlias := filepath.Join(store.Root, "alias.gpg")
	if err := os.Link(store.keyringPath(desired.Name), keyAlias); err != nil {
		t.Skipf("hardlinks unavailable: %v", err)
	}
	fixtureFile(t, store, "/etc/apt/sources.list", "deb [signed-by=/alias.gpg] https://other.example stable main\n")
	before := snapshot(t, store.Root)
	desired.SigningKey = otherTestKey(t)
	if _, err := store.Set(desired); err == nil || !strings.Contains(err.Error(), "shared") {
		t.Fatalf("shared alias not detected: %v", err)
	}
	assertUnchanged(t, before, snapshot(t, store.Root))
}

func TestInputOrderDoesNotAffectCompliance(t *testing.T) {
	store := newTestStore(t)
	desired := validDesired()
	desired.Architectures = []string{"amd64", "arm64"}
	if _, err := store.Set(desired); err != nil {
		t.Fatal(err)
	}
	slices.Reverse(desired.Components)
	slices.Reverse(desired.Architectures)
	desired.URI += "/"
	desired.Name = "different-management-label"
	assertCompliantNoOp(t, store, desired)
}

func TestNamedRepositoryPropertyDriftIsReconciled(t *testing.T) {
	for _, managed := range []bool{false, true} {
		for _, change := range []struct {
			name string
			edit func(*Repository)
		}{
			{"uri", func(r *Repository) { r.URI = "https://incorrect.example/debian" }},
			{"suite", func(r *Repository) { r.Suite = "incorrect" }},
			{"components", func(r *Repository) { r.Components = []string{"wrong"} }},
			{"architectures", func(r *Repository) { r.Architectures = []string{"arm64"} }},
			{"signingKey", func(r *Repository) { r.SigningKey = otherTestKey(t) }},
			{"enabled", func(r *Repository) { enabled := false; r.Enabled = &enabled }},
		} {
			t.Run(fmt.Sprintf("managed=%t/%s", managed, change.name), func(t *testing.T) {
				store := newTestStore(t)
				desired := validDesired()
				existing := desired
				change.edit(&existing)
				writePublicKeyring(t, store, desired.Name, existing.SigningKey)
				source := serializeDeb822(existing, store.keyringReference(desired.Name))
				if managed {
					source = append([]byte(managedMarker(desired.Name)), source...)
				}
				writeFile(t, store.sourcePath(desired.Name), string(source))
				fixtureFile(t, store, "/etc/apt/sources.list.d/unrelated.sources",
					string(serializeDeb822(Repository{URI: "https://unrelated.example", Suite: "stable", Components: []string{"main"}}, "/usr/share/keyrings/unrelated.gpg")))
				fixtureFile(t, store, "/usr/share/keyrings/unrelated.gpg", "unrelated key bytes")
				before := snapshot(t, store.Root)
				actual, err := store.Get(desired)
				if err != nil || actual.URI != existing.URI || actual.Suite != existing.Suite {
					t.Fatalf("Get must report actual identity properties: %+v, %v", actual, err)
				}
				if _, differences, err := store.Test(desired); err != nil || !slices.Equal(differences, []string{change.name}) {
					t.Fatalf("Test = (%v, %v), want %s", differences, err, change.name)
				}
				assertUnchanged(t, before, snapshot(t, store.Root))
				if _, err := store.Set(desired); err != nil {
					t.Fatal(err)
				}
				for _, path := range []string{
					filepath.Join(store.Root, "etc", "apt", "sources.list.d", "unrelated.sources"),
					filepath.Join(store.Root, "usr", "share", "keyrings", "unrelated.gpg"),
				} {
					assertUnchanged(t, map[string]fileSnapshot{path: before[path]}, snapshot(t, path))
				}
				assertCompliantNoOp(t, store, desired)
			})
		}
	}
}

func TestNamedMalformedRepositoryCanBeReplaced(t *testing.T) {
	for _, source := range []string{
		"Types: deb\nURIs: broken\n",
		"not a Deb822 definition\n",
		"Types: deb\nURIs: https://wrong.example\nSuites: wrong\nComponents: main\nEnabled: maybe\n",
		"Types: deb\nURIs: https://wrong.example\nURIs: https://duplicate.example\nSuites: wrong\n",
		"Types: deb\nURIs: https://wrong.example\nSuites: wrong\nComponents: main\nSigned-By: not-a-reference\n",
	} {
		t.Run(source, func(t *testing.T) {
			store := newTestStore(t)
			desired := validDesired()
			writeFile(t, store.sourcePath(desired.Name), source)
			before := snapshot(t, store.Root)
			if _, differences, err := store.Test(desired); err != nil || len(differences) == 0 {
				t.Fatalf("Test malformed = (%v, %v), want noncompliant", differences, err)
			}
			if _, err := store.Get(desired); err == nil {
				t.Fatal("Get must report unparseable actual state")
			}
			assertUnchanged(t, before, snapshot(t, store.Root))
			if _, err := store.Set(desired); err != nil {
				t.Fatal(err)
			}
			assertCompliantNoOp(t, store, desired)
		})
	}
}

func TestNamedListRepositoryCanBeCorrectedInPlace(t *testing.T) {
	for _, content := range []string{
		"deb [arch=arm64] https://incorrect.example wrong main\n",
		"deb [unterminated\n",
	} {
		t.Run(content, func(t *testing.T) {
			store := newTestStore(t)
			desired := validDesired()
			path := fixtureFile(t, store, "/etc/apt/sources.list.d/example.list", content)
			if _, differences, err := store.Test(desired); err != nil || len(differences) == 0 {
				t.Fatalf("Test = (%v, %v)", differences, err)
			}
			if _, err := store.Set(desired); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(store.sourcePath(desired.Name)); !os.IsNotExist(err) {
				t.Fatal("created a duplicate .sources file instead of correcting .list")
			}
			assertCompliantNoOp(t, store, desired)
		})
	}
}

func TestIncorrectSigningReferenceUsesDeclaredKeyWithoutChangingExternalKey(t *testing.T) {
	for _, content := range []string{"missing", "corrupt", "different", "inline corrupt"} {
		t.Run(content, func(t *testing.T) {
			store := newTestStore(t)
			desired := validDesired()
			reference := "/usr/share/keyrings/external.gpg"
			if content == "corrupt" {
				fixtureFile(t, store, reference, "corrupt key")
			} else if content == "different" {
				key, err := signingKeyBinary(otherTestKey(t))
				if err != nil {
					t.Fatal(err)
				}
				fixtureFile(t, store, reference, string(key))
			}
			source := string(serializeDeb822(desired, reference))
			if content == "inline corrupt" {
				source = strings.Replace(source, reference, "\n -----BEGIN PGP PUBLIC KEY BLOCK-----\n invalid\n -----END PGP PUBLIC KEY BLOCK-----", 1)
			}
			writeFile(t, store.sourcePath(desired.Name), source)
			before := snapshot(t, store.Root)
			if _, differences, err := store.Test(desired); err != nil || !slices.Equal(differences, []string{"signingKey"}) {
				t.Fatalf("Test signing reference = (%v, %v)", differences, err)
			}
			assertUnchanged(t, before, snapshot(t, store.Root))
			if _, err := store.Set(desired); err != nil {
				t.Fatal(err)
			}
			path, err := store.resolve(reference)
			if err != nil {
				t.Fatal(err)
			}
			if old, exists := before[path]; exists {
				assertUnchanged(t, map[string]fileSnapshot{path: old}, snapshot(t, path))
			} else if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatal("Set wrote an unrelated key path")
			}
			assertCompliantNoOp(t, store, desired)
		})
	}
}

func TestCorruptDeclaredKeyIsReplaced(t *testing.T) {
	store := newTestStore(t)
	desired := validDesired()
	writeFile(t, store.sourcePath(desired.Name), string(serializeDeb822(desired, store.keyringReference(desired.Name))))
	writeFile(t, store.keyringPath(desired.Name), "corrupt key")
	before := snapshot(t, store.Root)
	if _, differences, err := store.Test(desired); err != nil || !slices.Equal(differences, []string{"signingKey"}) {
		t.Fatalf("Test corrupt key = (%v, %v)", differences, err)
	}
	assertUnchanged(t, before, snapshot(t, store.Root))
	if _, err := store.Set(desired); err != nil {
		t.Fatal(err)
	}
	path := store.sourcePath(desired.Name)
	assertUnchanged(t, map[string]fileSnapshot{path: before[path]}, snapshot(t, path))
	assertCompliantNoOp(t, store, desired)
}

func TestReconciliationPreservesPermissionsAndDoesNotRequireDpkgOwnership(t *testing.T) {
	store := newTestStore(t)
	desired := validDesired()
	if _, err := store.Set(desired); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{store.sourcePath(desired.Name), store.keyringPath(desired.Name)} {
		if err := os.Chmod(path, 0640); err != nil {
			t.Fatal(err)
		}
	}
	fixtureFile(t, store, "/var/lib/dpkg/info/vendor.list",
		"/etc/apt/sources.list.d/example.sources\n"+store.keyringReference(desired.Name)+"\n")
	desired.URI = "https://corrected.example"
	desired.SigningKey = otherTestKey(t)
	if _, err := store.Set(desired); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		for _, path := range []string{store.sourcePath(desired.Name), store.keyringPath(desired.Name)} {
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0640 {
				t.Fatalf("permissions not preserved at %s: %v, %v", path, info, err)
			}
		}
	}
	assertCompliantNoOp(t, store, desired)
}

func TestNewIdentityDoesNotDeletePreviousFiles(t *testing.T) {
	store := newTestStore(t)
	desired := validDesired()
	if _, err := store.Set(desired); err != nil {
		t.Fatal(err)
	}
	paths := []string{store.sourcePath(desired.Name), store.keyringPath(desired.Name)}
	before := snapshot(t, store.Root)
	desired.Name = "new-identity"
	desired.URI = "https://new.example"
	if _, err := store.Set(desired); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		assertUnchanged(t, map[string]fileSnapshot{path: before[path]}, snapshot(t, path))
	}
	assertCompliantNoOp(t, store, desired)
}

func TestUnsafeTargetsFailBeforeAnyWrites(t *testing.T) {
	for _, kind := range []string{"source directory", "key directory", "source symlink", "key symlink", "parent symlink"} {
		t.Run(kind, func(t *testing.T) {
			store := newTestStore(t)
			desired := validDesired()
			path := store.sourcePath(desired.Name)
			if strings.HasPrefix(kind, "key") {
				path = store.keyringPath(desired.Name)
			} else if kind == "parent symlink" {
				path = filepath.Dir(path)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				t.Fatal(err)
			}
			if strings.HasSuffix(kind, "directory") {
				if err := os.Mkdir(path, 0755); err != nil {
					t.Fatal(err)
				}
			} else {
				target := filepath.Join(store.Root, "external")
				if kind == "parent symlink" {
					if err := os.Mkdir(target, 0755); err != nil {
						t.Fatal(err)
					}
				} else {
					writeFile(t, target, "malformed external content")
				}
				if err := os.Symlink(target, path); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			}
			before := snapshot(t, store.Root)
			if _, err := store.Set(desired); err == nil || !strings.Contains(err.Error(), filepath.Base(path)) {
				t.Fatalf("Set must identify unsafe path %s: %v", path, err)
			}
			assertUnchanged(t, before, snapshot(t, store.Root))
		})
	}
	for _, name := range []string{"../outside", `..\outside`, "/absolute", `C:\absolute`} {
		store := newTestStore(t)
		desired := validDesired()
		desired.Name = name
		before := snapshot(t, store.Root)
		if _, err := store.Set(desired); err == nil || !strings.Contains(err.Error(), "safe filename") {
			t.Fatalf("unsafe name %q accepted: %v", name, err)
		}
		assertUnchanged(t, before, snapshot(t, store.Root))
	}
}
