package apt

import (
	"bytes"
	"errors"
	"net/url"
	"regexp"
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
}

func ValidateName(name string) error {
	if !namePattern.MatchString(name) {
		return errors.New("name must be a safe filename containing only letters, numbers, dots, underscores, or hyphens")
	}
	return nil
}

func ValidateDesired(repository *Repository) error {
	if err := ValidateName(repository.Name); err != nil {
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
	if len(repository.Components) == 0 || !uniqueStrings(repository.Components) {
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

func Equal(desired, actual Repository) bool {
	if desired.Name != actual.Name || desired.Ensure != actual.Ensure {
		return false
	}
	if desired.Ensure == "Absent" {
		return true
	}
	desiredKey, desiredErr := signingKeyBinary(desired.SigningKey)
	actualKey, actualErr := signingKeyBinary(actual.SigningKey)
	return desiredErr == nil && actualErr == nil && bytes.Equal(desiredKey, actualKey) &&
		desired.Ensure == actual.Ensure &&
		desired.URI == actual.URI &&
		desired.Suite == actual.Suite &&
		equalStrings(desired.Components, actual.Components) &&
		equalStrings(desired.Architectures, actual.Architectures)
}
