package apt

import (
	"bytes"
	"errors"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

var (
	namePattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	componentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)
	suitePattern     = regexp.MustCompile(`^(?:\./|[A-Za-z0-9][A-Za-z0-9._+/-]*)$`)
)

type Repository struct {
	Name          string   `json:"name"`
	Ensure        string   `json:"ensure"`
	URI           string   `json:"uri,omitempty"`
	Suite         string   `json:"suite,omitempty"`
	Components    []string `json:"components,omitempty"`
	Architectures []string `json:"architectures,omitempty"`
	SigningKey    string   `json:"signingKey,omitempty"`
	Enabled       *bool    `json:"enabled,omitempty"`
}

// AptRepository is one URI/suite/type combination, independent of its document.
// Deb822 entries expand to the Cartesian product of Types, URIs and Suites.
type AptRepository struct {
	Type          string
	URI           string
	Suite         string
	Components    []string
	Architectures []string
	Enabled       bool
	Signing       SigningConfiguration
	Options       map[string]string
	removed       bool
}

type SigningConfiguration struct {
	Keyrings     []string
	Fingerprints []string
	InlineKey    string
}

func (r Repository) enabled() bool {
	return r.Enabled == nil || *r.Enabled
}

func ValidateSelector(repository Repository) error {
	if err := ValidateName(repository.Name); err != nil {
		return err
	}
	if err := validateURI(repository.URI); err != nil || !suitePattern.MatchString(repository.Suite) {
		return errors.New("uri and suite are required to identify a logical repository; name only selects filenames for new repositories")
	}
	return nil
}

func ValidateName(name string) error {
	if !namePattern.MatchString(name) {
		return errors.New("name must be a safe filename containing only letters, numbers, dots, underscores, or hyphens")
	}
	return nil
}

func ValidateDesired(repository *Repository) error {
	if err := ValidateSelector(*repository); err != nil {
		return err
	}
	if repository.Ensure == "" {
		repository.Ensure = "Present"
	}
	if repository.Ensure != "Present" && repository.Ensure != "Absent" {
		return errors.New("ensure must be Present or Absent")
	}
	if repository.Ensure == "Absent" {
		return nil
	}
	if err := validateProperties(*repository); err != nil {
		return err
	}
	key, err := ValidateSigningKey(repository.SigningKey)
	if err != nil {
		return errors.New("signingKey must be a valid inline ASCII-armored OpenPGP public key")
	}
	repository.SigningKey = key
	return nil
}

func validateProperties(repository Repository) error {
	if err := validateURI(repository.URI); err != nil {
		return errors.New("uri must be an absolute HTTP, HTTPS, or file URL without user information or fragment")
	}
	if !suitePattern.MatchString(repository.Suite) {
		return errors.New("suite must be a non-empty APT suite token")
	}
	exactPath := strings.HasSuffix(repository.Suite, "/")
	if exactPath && len(repository.Components) != 0 {
		return errors.New("components must be omitted for an exact-path suite")
	}
	if !exactPath && len(repository.Components) == 0 {
		return errors.New("components are required unless suite is an exact path")
	}
	if !uniqueStrings(repository.Components) {
		return errors.New("components must contain unique APT component tokens")
	}
	for _, component := range repository.Components {
		if !componentPattern.MatchString(component) {
			return errors.New("components must contain valid APT component tokens")
		}
	}
	if !uniqueStrings(repository.Architectures) {
		return errors.New("architectures must not contain duplicates")
	}
	for _, architecture := range repository.Architectures {
		if !componentPattern.MatchString(architecture) {
			return errors.New("architectures must contain valid architecture tokens")
		}
	}
	return nil
}

func validateURI(value string) error {
	if value == "" || strings.ContainsAny(value, "#\r\n\t ") {
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

func uniqueStrings(values []string) bool {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

func equalStrings(left, right []string) bool {
	left, right = slices.Clone(left), slices.Clone(right)
	slices.Sort(left)
	slices.Sort(right)
	return slices.Equal(slices.Compact(left), slices.Compact(right))
}

func Equal(desired, actual Repository) bool {
	return len(DifferingProperties(desired, actual)) == 0
}

// DifferingProperties compares logical state, not management labels or serialization.
func DifferingProperties(desired, actual Repository) []string {
	if desired.Ensure != actual.Ensure {
		return []string{"ensure"}
	}
	differences := []string{}
	if desired.Ensure == "Absent" {
		return differences
	}
	if repositoryURI(desired.URI) != repositoryURI(actual.URI) {
		differences = append(differences, "uri")
	}
	if desired.Suite != actual.Suite {
		differences = append(differences, "suite")
	}
	if !equalStrings(desired.Components, actual.Components) {
		differences = append(differences, "components")
	}
	if !equalStrings(desired.Architectures, actual.Architectures) {
		differences = append(differences, "architectures")
	}
	desiredKey, desiredErr := signingKeyBinary(desired.SigningKey)
	actualKey, actualErr := signingKeyBinary(actual.SigningKey)
	if desiredErr != nil || actualErr != nil || !bytes.Equal(desiredKey, actualKey) {
		differences = append(differences, "signingKey")
	}
	if desired.enabled() != actual.enabled() {
		differences = append(differences, "enabled")
	}
	return differences
}

func repositoryURI(uri string) string {
	parsed, err := url.Parse(uri)
	if err != nil || parsed.RawQuery != "" {
		return uri
	}
	if !strings.HasSuffix(uri, "/") {
		return uri + "/"
	}
	return uri
}
