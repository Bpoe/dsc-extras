package apt

import (
	"bytes"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp/armor"
	openpgp "github.com/ProtonMail/go-crypto/openpgp/v2"
)

func TestValidateSigningKeyRejectsSecretKeyMaterial(t *testing.T) {
	entity, err := openpgp.NewEntity("synthetic test", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var armored bytes.Buffer
	block, err := armor.Encode(&armored, openpgp.PrivateKeyType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := entity.SerializePrivate(block, nil); err != nil {
		t.Fatal(err)
	}
	if err := block.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateSigningKey(armored.String()); err == nil {
		t.Fatal("ValidateSigningKey accepted private-key armor")
	}
}

func TestSignedByReferenceDetection(t *testing.T) {
	keyPath := "/etc/apt/keyrings/example.asc"
	for _, test := range []struct {
		name      string
		value     string
		reference bool
		wantErr   bool
	}{
		{name: "multiple file references", value: "/usr/share/keyrings/other.gpg " + keyPath, reference: true},
		{name: "unrelated file", value: "/usr/share/keyrings/other.gpg"},
		{name: "fingerprint", value: "0123456789abcdef0123456789abcdef01234567"},
		{name: "inline public key", value: publicTestKey},
		{name: "path mixed with inline key", value: publicTestKey + " " + keyPath, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			referenced, err := signedByReferencesKey(test.value, keyPath)
			if (err != nil) != test.wantErr || referenced != test.reference {
				t.Fatalf("signedByReferencesKey()=(%t,%v), want (%t,error=%t)", referenced, err, test.reference, test.wantErr)
			}
		})
	}
}
