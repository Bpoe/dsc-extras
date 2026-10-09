package apt

import (
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
	absent := Repository{Name: "example", Ensure: "Absent"}
	if err := ValidateDesired(&absent); err != nil {
		t.Fatalf("Absent with only name failed validation: %v", err)
	}
}
