package apt

import (
	"bytes"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
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
