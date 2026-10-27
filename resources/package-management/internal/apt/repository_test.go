package apt

import (
	"slices"
	"strings"
	"testing"
)

const publicTestKey = `-----BEGIN PGP PUBLIC KEY BLOCK-----

mDMEask7FRYJKwYBBAHaRw8BAQdASUQbPPk5g1WOCGZPo9Q23h5uyJfSlZgeWA1s
vAr/RjW0GWRzY2FwdCBhdXRvbWF0ZWQgdGVzdCBrZXmIkwQTFgoAOxYhBLxsjLom
ELpZUi2U2KgBEKDNrRCcBQJqyTsVAhsDBQsJCAcCAiICBhUKCQgLAgQWAgMBAh4H
AheAAAoJEKgBEKDNrRCcZ08BAL/uurviv01+D+CAQZ9tJlrsA1+F5fZUO/INIILK
TxkEAP0SyQl4tdiUJgP6HlHWZy4fJfHLAr6K34wAOxhaYXf8CQ==
=Qv6R
-----END PGP PUBLIC KEY BLOCK-----`

func validDesired() Repository {
	return Repository{
		Name:          "example",
		Ensure:        "Present",
		URI:           "https://packages.example.org/debian",
		Suite:         "stable",
		Components:    []string{"main", "contrib"},
		Architectures: []string{"amd64"},
		SigningKey:    publicTestKey + "\n",
	}
}

func TestValidateSigningKey(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		wantErr bool
	}{
		{name: "valid public key", key: publicTestKey},
		{name: "invalid armor", key: "-----BEGIN PGP PUBLIC KEY BLOCK-----\nnot armor", wantErr: true},
		{name: "base64 but not OpenPGP", key: "-----BEGIN PGP PUBLIC KEY BLOCK-----\n\nYWJjZA==\n-----END PGP PUBLIC KEY BLOCK-----", wantErr: true},
		{name: "corrupted payload", key: strings.Replace(publicTestKey, "mDME", "nDME", 1), wantErr: true},
		{name: "corrupted armor checksum", key: strings.Replace(publicTestKey, "=Qv6R", "=Qv6S", 1), wantErr: true},
		{name: "private armor type", key: strings.Replace(publicTestKey, "PUBLIC KEY", "PRIVATE KEY", 2), wantErr: true},
		{name: "trailing data", key: publicTestKey + "not armor", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			normalized, err := ValidateSigningKey(test.key)
			if (err != nil) != test.wantErr {
				t.Fatalf("ValidateSigningKey() error=%v, wantErr=%t", err, test.wantErr)
			}
			if !test.wantErr && (!strings.HasSuffix(normalized, "\n") || !strings.Contains(normalized, "PUBLIC KEY BLOCK")) {
				t.Fatalf("normalized key is not complete armor: %q", normalized)
			}
		})
	}
}

func TestValidateDesiredAndName(t *testing.T) {
	for _, name := range []string{"", "../escape", "/absolute", `bad\name`, "with space"} {
		if err := ValidateName(name); err == nil {
			t.Errorf("ValidateName(%q) succeeded, want failure", name)
		}
	}
	desired := validDesired()
	desired.Ensure = ""
	if err := ValidateDesired(&desired); err != nil {
		t.Fatal(err)
	}
	if desired.Ensure != "Present" {
		t.Fatalf("ensure = %q, want Present default", desired.Ensure)
	}
	absent := Repository{Name: "example", Ensure: "Absent", URI: desired.URI, Suite: desired.Suite}
	if err := ValidateDesired(&absent); err != nil {
		t.Fatalf("Absent with logical selector failed validation: %v", err)
	}
	if err := ValidateDesired(&Repository{Name: "example", Ensure: "Absent"}); err == nil {
		t.Fatal("accepted filename-only removal")
	}
}

func TestExactPathSuiteRequiresNoComponents(t *testing.T) {
	desired := validDesired()
	desired.Suite = "./"
	desired.Components = nil
	if err := ValidateDesired(&desired); err != nil {
		t.Fatalf("exact-path suite without components rejected: %v", err)
	}
	withComponents := validDesired()
	withComponents.Suite = "./"
	if err := ValidateDesired(&withComponents); err == nil {
		t.Fatal("exact-path suite with components was accepted")
	}
	withoutComponents := validDesired()
	withoutComponents.Components = nil
	if err := ValidateDesired(&withoutComponents); err == nil {
		t.Fatal("distribution suite without components was accepted")
	}
}

func TestDifferingPropertiesAndEqualAgree(t *testing.T) {
	enabled, disabled := true, false
	otherKey := otherTestKey(t)
	for _, test := range []struct {
		name   string
		change func(desired, actual *Repository)
		want   []string
	}{
		{name: "identical"},
		{name: "management label", change: func(_, actual *Repository) { actual.Name = "external" }},
		{name: "trailing URI slash", change: func(_, actual *Repository) { actual.URI += "/" }},
		{name: "mirror-list trailing slash is significant", change: func(desired, actual *Repository) {
			desired.URI = "mirror+file:///etc/apt/mirrors/debian.list"
			actual.URI = desired.URI + "/"
		}, want: []string{"uri"}},
		{name: "different URI", change: func(_, actual *Repository) { actual.URI += "/other" }, want: []string{"uri"}},
		{name: "URI query slash is significant", change: func(desired, actual *Repository) {
			desired.URI += "?channel=stable"
			actual.URI = desired.URI + "/"
		}, want: []string{"uri"}},
		{name: "different suite", change: func(_, actual *Repository) { actual.Suite = "testing" }, want: []string{"suite"}},
		{name: "unordered sets", change: func(desired, actual *Repository) {
			desired.Architectures = []string{"amd64", "arm64"}
			actual.Architectures = []string{"arm64", "amd64", "amd64"}
			actual.Components = []string{"contrib", "main", "main"}
		}},
		{name: "different components", change: func(_, actual *Repository) { actual.Components = []string{"main"} }, want: []string{"components"}},
		{name: "different architectures", change: func(_, actual *Repository) { actual.Architectures = []string{"arm64"} }, want: []string{"architectures"}},
		{name: "host default architectures", change: func(desired, _ *Repository) { desired.Architectures = nil }, want: []string{"architectures"}},
		{name: "empty architecture sets", change: func(desired, actual *Repository) {
			desired.Architectures = nil
			actual.Architectures = []string{}
		}},
		{name: "omitted enabled", change: func(_, actual *Repository) { actual.Enabled = &enabled }},
		{name: "disabled", change: func(_, actual *Repository) { actual.Enabled = &disabled }, want: []string{"enabled"}},
		{name: "equivalent armor", change: func(_, actual *Repository) {
			actual.SigningKey = strings.Replace(publicTestKey, "\n\n", "\nComment: alternate armor\n\n", 1)
			actual.SigningKey = strings.ReplaceAll(actual.SigningKey, "\n", "\r\n")
		}},
		{name: "different key", change: func(_, actual *Repository) { actual.SigningKey = otherKey }, want: []string{"signingKey"}},
		{name: "missing key", change: func(_, actual *Repository) { actual.SigningKey = "" }, want: []string{"signingKey"}},
		{name: "invalid keys are not equal", change: func(desired, actual *Repository) {
			desired.SigningKey, actual.SigningKey = "invalid", "invalid"
		}, want: []string{"signingKey"}},
		{name: "missing repository", change: func(_, actual *Repository) {
			*actual = Repository{Ensure: "Absent"}
		}, want: []string{"ensure"}},
		{name: "removal required", change: func(desired, _ *Repository) {
			*desired = Repository{Ensure: "Absent"}
		}, want: []string{"ensure"}},
		{name: "already absent", change: func(desired, actual *Repository) {
			desired.Ensure = "Absent"
			*actual = Repository{Ensure: "Absent"}
		}},
		{name: "multiple differences", change: func(_, actual *Repository) {
			actual.URI += "/other"
			actual.Suite = "testing"
			actual.Components = []string{"main"}
			actual.Architectures = nil
			actual.SigningKey = otherKey
			actual.Enabled = &disabled
		}, want: []string{"uri", "suite", "components", "architectures", "signingKey", "enabled"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			desired, actual := validDesired(), validDesired()
			if test.change != nil {
				test.change(&desired, &actual)
			}
			differences := DifferingProperties(desired, actual)
			if differences == nil || !slices.Equal(differences, test.want) {
				t.Fatalf("DifferingProperties = %v, want %v (non-nil)", differences, test.want)
			}
			if Equal(desired, actual) != (len(differences) == 0) {
				t.Fatal("Equal disagrees with DifferingProperties")
			}
		})
	}
}

func TestAPTTransportURIs(t *testing.T) {
	for _, uri := range []string{
		"mirror+file:///etc/apt/mirrors/debian.list",
		"mirror+file:///etc/apt/mirrors/debian-security.list",
		"mirror+file:/etc/apt/mirrors/debian.list",
		"mirror+http://mirrors.example/debian.list",
		"mirror+https://mirrors.example/debian.list",
		"mirror+copy:/etc/apt/mirrors/debian.list",
		"mirror://mirrors.example/debian.list",
		"http://archive.ubuntu.com/ubuntu",
		"https://security.ubuntu.com/ubuntu/",
		"file:///srv/apt/repo", "file:/srv/apt/repo",
		"copy:/srv/apt/repo",
		"mirror+https://MIRRORS.example:443/a/../debian%2elist?channel=stable",
	} {
		t.Run(uri, func(t *testing.T) {
			desired := validDesired()
			desired.URI = uri
			if err := ValidateDesired(&desired); err != nil {
				t.Fatal(err)
			}
			if desired.URI != uri {
				t.Fatalf("URI changed to %q", desired.URI)
			}
			for _, format := range []AptFormat{ListFormat, Deb822Format} {
				text := "deb " + uri + " trixie main\n"
				if format == Deb822Format {
					text = "Types: deb\nURIs: " + uri + "\nSuites: trixie\nComponents: main\n"
				}
				document, err := parseDocument("transport"+string(format), []byte(text))
				if err != nil {
					t.Fatal(err)
				}
				// Force serialization of the physical entry as well as parsing it.
				document.Entries[0].Components = []string{"main", "contrib"}
				data, err := document.contents()
				if err != nil {
					t.Fatal(err)
				}
				roundTrip, err := parseDocument(document.Path, data)
				if err != nil || roundTrip.Entries[0].URI != uri {
					t.Fatalf("URI round trip: %s (%v)", data, err)
				}
			}
			if strings.HasPrefix(uri, "mirror") {
				if repositoryURI(uri) != uri || repositoryURI(uri) == repositoryURI(uri+"/") {
					t.Fatal("mirror-list filenames must be compared exactly")
				}
			}
		})
	}
}

func TestAPTTransportRejectsUnsafeURIs(t *testing.T) {
	for _, uri := range []string{
		"", "/etc/apt/mirrors/debian.list", "https:///repo", "http:repo",
		"mirror+http:///debian.list", "mirror+https:debian.list",
		"mirror+file:relative.list", "mirror+copy:relative.list",
		"file://remote/srv/apt", "mirror+file://remote/etc/apt/debian.list",
		"mirror+copy://remote/etc/apt/debian.list",
		"mirror+ftp://example/debian.list", "unknown://example/repo",
		"mirror+mirror+https://example/list", "mirror+mirror://example/list", "https://example/%zz",
		"https://user:password@example/repo", "http://user@example/repo",
		"file://user@/srv/apt", "mirror+file://user@/etc/apt/debian.list",
		"mirror+http://user:password@example/debian.list",
		"mirror+https://user%40name:password@example/debian.list",
		"mirror://user@example/debian.list", "mirror+copy://user@/list",
		"https://example/repo#fragment", "mirror+file:///list#",
		"mirror+https://example/list#fragment", "mirror+file:///list\n",
		"mirror+file:///list extra", "mirror+https://example/\tlist",
	} {
		t.Run(uri, func(t *testing.T) {
			if err := validateURI(uri); err == nil {
				t.Fatalf("accepted unsafe URI %q", uri)
			}
			desired := validDesired()
			desired.URI = uri
			if err := ValidateSelector(desired); err == nil {
				t.Fatal("selector accepted unsafe URI")
			}
			if err := ValidateDesired(&desired); err == nil {
				t.Fatal("desired state accepted unsafe URI")
			}
		})
	}
}
