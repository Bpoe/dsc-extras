package apt

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Root is "/" on the target machine and a temporary filesystem root in tests.
type Store struct {
	Root string
}

func NewStore(root string) Store {
	if root == "" {
		root = string(filepath.Separator)
	}
	return Store{Root: filepath.Clean(root)}
}

func (s Store) resolve(reference string) (string, error) {
	if !strings.HasPrefix(reference, "/") || strings.Contains(reference, "\\") ||
		strings.Contains(reference, ":") || slices.Contains(strings.Split(reference, "/"), "..") {
		return "", fmt.Errorf("unsafe absolute APT path %q", reference)
	}
	return filepath.Join(s.Root, filepath.FromSlash(strings.TrimPrefix(reference, "/"))), nil
}

func (s Store) sourcePath(name string) string {
	return filepath.Join(s.Root, "etc", "apt", "sources.list.d", name+".sources")
}

func (s Store) keyringReference(name string) string {
	return "/etc/apt/keyrings/" + name + ".gpg"
}

func (s Store) keyringPath(name string) string {
	return filepath.Join(s.Root, "etc", "apt", "keyrings", name+".gpg")
}

func (s Store) readDocument(path string) (*AptDocument, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect APT source %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("APT source %q is not a regular file", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read APT source %q: %w", path, err)
	}
	d, err := parseDocument(path, data)
	if err != nil {
		return &AptDocument{Path: path, OriginalContents: data, Format: AptFormat(filepath.Ext(path)),
			info: info, store: s, parseErr: err}, err
	}
	d.info, d.store = info, s
	return d, nil
}

func (s Store) Discover() ([]*AptDocument, error) {
	return s.discover("")
}

func (s Store) discover(repairPath string) ([]*AptDocument, error) {
	paths := []string{}
	main := filepath.Join(s.Root, "etc", "apt", "sources.list")
	if _, err := os.Lstat(main); err == nil {
		paths = append(paths, main)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect sources.list: %w", err)
	}
	dir := filepath.Join(s.Root, "etc", "apt", "sources.list.d")
	info, err := os.Stat(dir)
	if err == nil && !info.IsDir() {
		return nil, fmt.Errorf("APT source directory %s is not a directory", dir)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("discover APT sources: %w", err)
	}
	var files []os.DirEntry
	if err == nil {
		files, err = os.ReadDir(dir)
		if err != nil {
			return nil, fmt.Errorf("read APT source directory: %w", err)
		}
	}
	for _, file := range files {
		ext := filepath.Ext(file.Name())
		if (ext == ".list" || ext == ".sources") && sourceNamePattern.MatchString(file.Name()) {
			paths = append(paths, filepath.Join(dir, file.Name()))
		}
	}
	var documents []*AptDocument
	if repairPath != "" && !slices.Contains(paths, repairPath) {
		repairPath = strings.TrimSuffix(repairPath, ".sources") + ".list"
	}
	for _, path := range paths {
		d, err := s.readDocument(path)
		if err != nil && (d == nil || path != repairPath) {
			return nil, err
		}
		documents = append(documents, d)
	}
	return documents, nil
}

type repositoryMatch struct {
	document *AptDocument
	index    int
}

func (s Store) namedDocument(documents []*AptDocument, name string) *AptDocument {
	for _, path := range []string{s.sourcePath(name), strings.TrimSuffix(s.sourcePath(name), ".sources") + ".list"} {
		for _, document := range documents {
			if document.Path == path {
				return document
			}
		}
	}
	return nil
}

func (s Store) matches(documents []*AptDocument, selector Repository) []repositoryMatch {
	named := s.namedDocument(documents, selector.Name)
	var namedEntries []int
	if named != nil {
		for index, entry := range named.Entries {
			if entry.Type == "deb" {
				namedEntries = append(namedEntries, index)
			}
		}
		if len(namedEntries) != 1 {
			namedEntries = slices.DeleteFunc(namedEntries, func(index int) bool {
				entry := named.Entries[index]
				return repositoryURI(entry.URI) != repositoryURI(selector.URI) || entry.Suite != selector.Suite
			})
		}
	}
	var found []repositoryMatch
	for _, document := range documents {
		for index, entry := range document.Entries {
			if entry.Type == "deb" && (document == named && slices.Contains(namedEntries, index) ||
				document != named && repositoryURI(entry.URI) == repositoryURI(selector.URI) && entry.Suite == selector.Suite) {
				found = append(found, repositoryMatch{document, index})
			}
		}
	}
	return found
}

func effectiveArchitectures(entry AptRepository) ([]string, error) {
	result := slices.Clone(entry.Architectures)
	for _, name := range []string{"architectures-add", "architectures-remove"} {
		if entry.Options[name] != "" && len(entry.Architectures) == 0 {
			return nil, errors.New("architecture modifiers depend on host APT defaults; configure explicit Architectures before managing this entry")
		}
	}
	for _, arch := range strings.Fields(entry.Options["architectures-add"]) {
		if !slices.Contains(result, arch) {
			result = append(result, arch)
		}
	}
	for _, arch := range strings.Fields(entry.Options["architectures-remove"]) {
		result = slices.DeleteFunc(result, func(value string) bool { return value == arch })
	}
	if len(entry.Architectures) != 0 && len(result) == 0 {
		return nil, errors.New("architecture modifiers remove every architecture; cannot represent this as host-default architectures")
	}
	return result, nil
}

func checkOptions(entry AptRepository) error {
	for name, value := range entry.Options {
		switch name {
		case "architectures-add", "architectures-remove":
			continue
		case "allow-insecure", "allow-weak", "allow-downgrade-to-insecure":
			if strings.EqualFold(value, "no") {
				continue
			}
		case "check-valid-until", "check-date":
			if strings.EqualFold(value, "yes") {
				continue
			}
		default:
			if strings.HasPrefix(name, "x-") {
				continue
			}
		}
		return fmt.Errorf("APT option %q=%q changes trust or unsupported behavior; resolve it explicitly before managing this entry", name, value)
	}
	return nil
}

func (s Store) observe(selector Repository, documents []*AptDocument, repair bool) (Repository, []repositoryMatch, error) {
	actual := Repository{Name: selector.Name, URI: selector.URI, Suite: selector.Suite, Ensure: "Absent"}
	found := s.matches(documents, selector)
	for _, document := range documents {
		if document.parseErr != nil {
			if len(found) != 0 {
				return actual, found, fmt.Errorf("cannot reconcile malformed named source %s while a matching repository exists in %s; repair or remove the malformed source explicitly",
					document.Path, found[0].document.Path)
			}
			actual = Repository{Name: selector.Name, Ensure: "Present"}
		}
	}
	var firstMaterial []byte
	for i, match := range found {
		entry := match.document.Entries[match.index]
		if err := checkOptions(entry); err != nil {
			return actual, found, fmt.Errorf("%s: %w", match.document.Path, err)
		}
		architectures, err := effectiveArchitectures(entry)
		if err != nil {
			return actual, found, fmt.Errorf("%s: %w", match.document.Path, err)
		}
		material, err := s.signingMaterial(entry.Signing)
		var invalidKey *keyringContentError
		if err != nil && !errors.Is(err, os.ErrNotExist) && !(repair && errors.As(err, &invalidKey)) {
			return actual, found, err
		}
		if i == 0 {
			actual.Ensure, actual.URI, actual.Suite = "Present", entry.URI, entry.Suite
			enabled := entry.Enabled
			actual.Enabled = &enabled
			actual.Architectures = architectures
			firstMaterial = material
			if len(material) > 0 {
				actual.SigningKey, err = armoredKeyring(material)
				if err != nil {
					return actual, found, err
				}
			}
		} else {
			if !equalStrings(actual.Architectures, architectures) || actual.enabled() != entry.Enabled ||
				!bytes.Equal(firstMaterial, material) || !sameSigningReferences(found[0].document.Entries[found[0].index].Signing, entry.Signing) ||
				len(entry.Components) == 0 || intersects(actual.Components, entry.Components) {
				return actual, found, fmt.Errorf("conflicting or duplicate definitions for %s %s in %s and %s; consolidate them explicitly",
					selector.URI, selector.Suite, found[0].document.Path, match.document.Path)
			}
		}
		actual.Components = append(actual.Components, entry.Components...)
	}
	// APT shares Release-file trust options between deb and deb-src entries.
	for _, doc := range documents {
		for _, entry := range doc.Entries {
			if entry.Type == "deb-src" && len(found) > 0 && entry.Enabled &&
				repositoryURI(entry.URI) == repositoryURI(selector.URI) && entry.Suite == selector.Suite {
				if err := checkOptions(entry); err != nil {
					return actual, found, fmt.Errorf("%s: %w", doc.Path, err)
				}
				first := found[0].document.Entries[found[0].index]
				if !sameSigningReferences(first.Signing, entry.Signing) {
					return actual, found, fmt.Errorf("conflicting deb-src signing configuration in %s; align Signed-By before managing this repository", doc.Path)
				}
			}
		}
	}
	return actual, found, nil
}

func sameSigningReferences(a, b SigningConfiguration) bool {
	return slices.Equal(a.Keyrings, b.Keyrings) && slices.Equal(a.Fingerprints, b.Fingerprints) &&
		strings.TrimSpace(a.InlineKey) == strings.TrimSpace(b.InlineKey)
}

func intersects(a, b []string) bool {
	for _, value := range a {
		if slices.Contains(b, value) {
			return true
		}
	}
	return false
}

func (s Store) Get(selector Repository) (Repository, error) {
	if err := ValidateSelector(selector); err != nil {
		return Repository{}, err
	}
	documents, err := s.Discover()
	if err != nil {
		return Repository{}, err
	}
	actual, _, err := s.observe(selector, documents, false)
	return actual, err
}

func (s Store) Export(filter Repository) ([]Repository, error) {
	filtered := filter.Name != "" || filter.URI != "" || filter.Suite != ""
	if filtered {
		if err := ValidateSelector(filter); err != nil {
			return nil, err
		}
	}
	documents, err := s.Discover()
	if err != nil {
		return nil, err
	}
	type repositoryIdentity struct{ uri, suite string }
	identities := map[repositoryIdentity]bool{}
	for _, document := range documents {
		if document.parseErr != nil {
			return nil, fmt.Errorf("cannot export malformed APT source %s: %w", document.Path, document.parseErr)
		}
		for _, entry := range document.Entries {
			if entry.Type == "deb" {
				identities[repositoryIdentity{repositoryURI(entry.URI), entry.Suite}] = true
			}
		}
	}
	ordered := make([]repositoryIdentity, 0, len(identities))
	for identity := range identities {
		ordered = append(ordered, identity)
	}
	slices.SortFunc(ordered, func(a, b repositoryIdentity) int {
		if order := strings.Compare(a.uri, b.uri); order != 0 {
			return order
		}
		return strings.Compare(a.suite, b.suite)
	})
	result := make([]Repository, 0, len(ordered))
	for _, identity := range ordered {
		sum := sha256.Sum256([]byte(identity.uri + "\x00" + identity.suite))
		name := "repo-" + hex.EncodeToString(sum[:])
		if filtered && (repositoryURI(filter.URI) != identity.uri || filter.Suite != identity.suite || filter.Name != name) {
			continue
		}
		selector := Repository{Name: name, URI: identity.uri, Suite: identity.suite}
		actual, _, err := s.observe(selector, documents, false)
		if err != nil {
			return nil, fmt.Errorf("export APT repository %s %s: %w", identity.uri, identity.suite, err)
		}
		if actual.Ensure == "Present" {
			if err := ValidateName(actual.Name); err != nil {
				return nil, err
			}
			if err := validateProperties(actual); err != nil {
				return nil, fmt.Errorf("export APT repository %s %s: %w", identity.uri, identity.suite, err)
			}
			if actual.SigningKey == "" {
				return nil, fmt.Errorf("export APT repository %s %s: no readable explicit signing key; this resource requires signingKey to reconcile Present state",
					identity.uri, identity.suite)
			}
			if actual.SigningKey, err = ValidateSigningKey(actual.SigningKey); err != nil {
				return nil, fmt.Errorf("export APT repository %s %s has unrepresentable signing material: %w",
					identity.uri, identity.suite, err)
			}
			result = append(result, actual)
		}
	}
	return result, nil
}

func (s Store) Test(desired Repository) (Repository, []string, error) {
	if err := ValidateDesired(&desired); err != nil {
		return Repository{}, nil, err
	}
	documents, err := s.discover(s.sourcePath(desired.Name))
	if err != nil {
		return Repository{}, nil, err
	}
	actual, _, err := s.observe(desired, documents, true)
	if err != nil {
		return actual, nil, err
	}
	for _, document := range documents {
		if document.parseErr != nil {
			if err := s.checkWrite(document.Path, document.OriginalContents, document.info, false); err != nil {
				return actual, nil, err
			}
		}
	}
	return actual, DifferingProperties(desired, actual), nil
}

func (s Store) Set(desired Repository) (Repository, error) {
	if err := ValidateDesired(&desired); err != nil {
		return Repository{}, err
	}
	documents, err := s.discover(s.sourcePath(desired.Name))
	if err != nil {
		return Repository{}, err
	}
	actual, found, err := s.observe(desired, documents, true)
	if err != nil {
		return Repository{}, err
	}
	if Equal(desired, actual) {
		return actual, nil
	}
	if len(found) > 1 {
		return Repository{}, errors.New("repository is spread across multiple entries; consolidate them explicitly before changing it")
	}
	var document *AptDocument
	var key *AptKeyring
	if len(found) == 0 {
		document = s.namedDocument(documents, desired.Name)
		path := s.sourcePath(desired.Name)
		if document != nil {
			if len(document.Entries) != 0 {
				return Repository{}, fmt.Errorf("cannot select a unique deb entry in %s; specify the URI and suite of the entry to manage", document.Path)
			}
			path = document.Path
		}
		data := []byte{}
		if desired.Ensure != "Absent" {
			material, err := signingKeyBinary(desired.SigningKey)
			if err != nil {
				return Repository{}, err
			}
			key, err = s.prepareKey(desired.Name, material, documents, nil)
			if err != nil {
				return Repository{}, err
			}
			source := serializeDeb822(desired, key.Reference)
			if filepath.Ext(path) == ".list" {
				text, err := serializeList(AptRepository{Type: "deb", URI: desired.URI, Suite: desired.Suite,
					Components: desired.Components, Architectures: desired.Architectures, Enabled: desired.enabled(),
					Signing: SigningConfiguration{Keyrings: []string{key.Reference}}}, "", "\n")
				if err != nil {
					return Repository{}, err
				}
				source = []byte(text)
			}
			data = append([]byte(managedMarker(desired.Name)), source...)
		}
		planned, err := parseDocument(path, data)
		if err != nil {
			return Repository{}, err
		}
		planned.store = s
		if document == nil {
			document = planned
			document.create = true
			documents = append(documents, document)
		} else {
			planned.info, planned.OriginalContents, planned.replacement = document.info, document.OriginalContents, data
			*document = *planned
		}
	} else {
		match := found[0]
		document = match.document
		// A key-only change also alters this source's trust, even when its bytes
		// remain unchanged. Require the source to be editable in that case too.
		if err := s.checkWrite(document.Path, document.OriginalContents, document.info, false); err != nil {
			return Repository{}, err
		}
		entry := &document.Entries[match.index]
		if desired.Ensure == "Absent" {
			entry.removed = true
		} else {
			entry.URI, entry.Suite = desired.URI, desired.Suite
			if !equalStrings(actual.Architectures, desired.Architectures) {
				delete(entry.Options, "architectures-add")
				delete(entry.Options, "architectures-remove")
				entry.Architectures = slices.Clone(desired.Architectures)
			}
			if !equalStrings(actual.Components, desired.Components) {
				entry.Components = slices.Clone(desired.Components)
			}
			entry.Enabled = desired.enabled()
			material, err := signingKeyBinary(desired.SigningKey)
			if err != nil {
				return Repository{}, err
			}
			current, err := s.signingMaterial(entry.Signing)
			var invalidKey *keyringContentError
			if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.As(err, &invalidKey) {
				return Repository{}, err
			}
			if !bytes.Equal(current, material) {
				if entry.Signing.InlineKey != "" {
					entry.Signing.InlineKey = desired.SigningKey
				} else {
					key, err = s.prepareKey(desired.Name, material, documents, &match)
					if err != nil {
						return Repository{}, err
					}
					entry.Signing = SigningConfiguration{Keyrings: []string{key.Reference}}
				}
			}
		}
	}
	// Validate the entire plan, including shared Release-file options, before any writes.
	if desired.Ensure != "Absent" {
		if err := checkPlannedTrust(documents, desired); err != nil {
			return Repository{}, err
		}
	}
	if err := document.preflight(); err != nil {
		return Repository{}, err
	}
	if key != nil {
		if err := key.preflight(); err != nil {
			return Repository{}, err
		}
		if err := key.WriteToDisk(); err != nil {
			return Repository{}, err
		}
	}
	if err := document.WriteToDisk(); err != nil {
		return Repository{}, err
	}
	actual, differences, err := s.Test(desired)
	if err != nil {
		return Repository{}, fmt.Errorf("read reconciled repository: %w", err)
	}
	if len(differences) != 0 {
		return Repository{}, errors.New("repository did not converge; inspect the current source definitions")
	}
	return actual, nil
}

func managedMarker(name string) string { return "# Managed by dscapt: " + name + "\n" }

func checkPlannedTrust(documents []*AptDocument, desired Repository) error {
	var first *AptRepository
	for _, document := range documents {
		for _, entry := range document.Entries {
			if entry.removed || !entry.Enabled || repositoryURI(entry.URI) != repositoryURI(desired.URI) || entry.Suite != desired.Suite {
				continue
			}
			if err := checkOptions(entry); err != nil {
				return fmt.Errorf("%s: %w", document.Path, err)
			}
			if first != nil && !sameSigningReferences(first.Signing, entry.Signing) {
				return fmt.Errorf("change would conflict with Signed-By in %s; align related deb/deb-src entries explicitly", document.Path)
			}
			copy := entry
			first = &copy
		}
	}
	return nil
}

func (s Store) prepareKey(name string, material []byte, documents []*AptDocument, target *repositoryMatch) (*AptKeyring, error) {
	reference := s.keyringReference(name)
	key, err := s.ReadKeyring(reference)
	if errors.Is(err, os.ErrNotExist) {
		key = &AptKeyring{Reference: reference, Path: s.keyringPath(name), Format: BinaryKeyring, store: s, create: true}
	} else if err != nil {
		var invalidKey *keyringContentError
		if !errors.As(err, &invalidKey) {
			return nil, err
		}
	} else if bytes.Equal(key.Material, material) {
		return key, nil
	}
	for _, document := range documents {
		for index, entry := range document.Entries {
			if target != nil && document == target.document && index == target.index {
				continue
			}
			for _, other := range entry.Signing.Keyrings {
				path, err := s.resolve(other)
				if err != nil {
					return nil, err
				}
				info, err := os.Stat(path)
				if err != nil && !errors.Is(err, os.ErrNotExist) {
					return nil, err
				}
				if errors.Is(err, os.ErrNotExist) {
					link, linkErr := os.Lstat(path)
					if linkErr == nil && link.Mode()&os.ModeSymlink != 0 {
						return nil, fmt.Errorf("cannot establish keyring sharing through unresolved symlink %s; repair it before changing trust", path)
					}
					if linkErr != nil && !errors.Is(linkErr, os.ErrNotExist) {
						return nil, linkErr
					}
				}
				if filepath.Clean(path) == filepath.Clean(key.Path) || key.info != nil && info != nil && os.SameFile(key.info, info) {
					return nil, fmt.Errorf("keyring %s is shared with %s; provision separate trust material before changing it", reference, document.Path)
				}
			}
		}
	}
	key.Material = material
	return key, nil
}
