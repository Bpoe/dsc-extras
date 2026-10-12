package apt

import (
	"bytes"
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
		return nil, err
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
		return nil, err
	}
	d.info, d.store = info, s
	return d, nil
}

func (s Store) Discover() ([]*AptDocument, error) {
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
	for _, path := range paths {
		d, err := s.readDocument(path)
		if err != nil {
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

func matches(documents []*AptDocument, selector Repository) []repositoryMatch {
	var found []repositoryMatch
	for _, document := range documents {
		for index, entry := range document.Entries {
			if entry.Type == "deb" && repositoryURI(entry.URI) == repositoryURI(selector.URI) && entry.Suite == selector.Suite {
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

func (s Store) observe(selector Repository, documents []*AptDocument) (Repository, []repositoryMatch, error) {
	actual := Repository{Name: selector.Name, URI: selector.URI, Suite: selector.Suite, Ensure: "Absent"}
	found := matches(documents, selector)
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
		if err != nil && !errors.Is(err, os.ErrNotExist) {
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
	actual, _, err := s.observe(selector, documents)
	return actual, err
}

func (s Store) Test(desired Repository) (Repository, bool, error) {
	if err := ValidateDesired(&desired); err != nil {
		return Repository{}, false, err
	}
	actual, err := s.Get(desired)
	return actual, err == nil && Equal(desired, actual), err
}

func (s Store) Set(desired Repository) (Repository, error) {
	if err := ValidateDesired(&desired); err != nil {
		return Repository{}, err
	}
	documents, err := s.Discover()
	if err != nil {
		return Repository{}, err
	}
	actual, found, err := s.observe(desired, documents)
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
		material, err := signingKeyBinary(desired.SigningKey)
		if err != nil {
			return Repository{}, err
		}
		key, err = s.prepareKey(desired.Name, material, false, documents, nil)
		if err != nil {
			return Repository{}, err
		}
		data := append([]byte(managedMarker(desired.Name)), serializeDeb822(desired, key.Reference)...)
		document, err = parseDocument(s.sourcePath(desired.Name), data)
		if err != nil {
			return Repository{}, err
		}
		document.store, document.create = s, true
		documents = append(documents, document)
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
			if !equalStrings(actual.Architectures, desired.Architectures) {
				if entry.Options["architectures-add"] != "" || entry.Options["architectures-remove"] != "" {
					return Repository{}, errors.New("cannot change architecture restrictions with modifiers; simplify Architectures explicitly first")
				}
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
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return Repository{}, err
			}
			if !bytes.Equal(current, material) {
				if entry.Signing.InlineKey != "" {
					entry.Signing.InlineKey = desired.SigningKey
				} else {
					unrestricted := len(entry.Signing.Keyrings) == 0 && len(entry.Signing.Fingerprints) == 0
					owned := bytes.HasPrefix(document.OriginalContents, []byte(managedMarker(desired.Name))) &&
						equalStrings(entry.Signing.Keyrings, []string{s.keyringReference(desired.Name)}) &&
						len(entry.Signing.Fingerprints) == 0
					if !unrestricted && !owned {
						return Repository{}, fmt.Errorf("signing key for %s is externally managed; provision the intended key explicitly rather than overwriting its trust material", document.Path)
					}
					key, err = s.prepareKey(desired.Name, material, owned, documents, &match)
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
	actual, compliant, err := s.Test(desired)
	if err != nil {
		return Repository{}, fmt.Errorf("read reconciled repository: %w", err)
	}
	if !compliant {
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
			if first != nil && !sameSigningReferences(first.Signing, entry.Signing) {
				return fmt.Errorf("change would conflict with Signed-By in %s; align related deb/deb-src entries explicitly", document.Path)
			}
			copy := entry
			first = &copy
		}
	}
	return nil
}

func (s Store) prepareKey(name string, material []byte, owned bool, documents []*AptDocument, target *repositoryMatch) (*AptKeyring, error) {
	reference := s.keyringReference(name)
	key, err := s.ReadKeyring(reference)
	if errors.Is(err, os.ErrNotExist) {
		key = &AptKeyring{Reference: reference, Path: s.keyringPath(name), Format: BinaryKeyring, store: s, create: true}
	} else if err != nil {
		return nil, err
	} else if bytes.Equal(key.Material, material) {
		return key, nil
	} else if !owned {
		return nil, fmt.Errorf("keyring %s already exists with different material; choose a different name or reconcile its owner", reference)
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
	key.Material, key.allowChange = material, owned
	return key, nil
}
