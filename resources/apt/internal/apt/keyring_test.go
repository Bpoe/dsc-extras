package apt

import (
	"bytes"
	"os"
	"strings"
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

func TestSigningKeyBinaryRoundTrip(t *testing.T) {
	binary, err := signingKeyBinary(publicTestKey)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(binary, []byte("BEGIN PGP")) {
		t.Fatal("binary keyring contains ASCII armor")
	}
	armored, err := armoredKeyring(binary)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := signingKeyBinary(armored)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(binary, roundTrip) {
		t.Fatal("public-key material changed during armor round trip")
	}
}

func TestSigningKeyArmorFormattingDoesNotChangeMaterial(t *testing.T) {
	binary, err := signingKeyBinary(publicTestKey)
	if err != nil {
		t.Fatal(err)
	}
	reformatted := strings.ReplaceAll(publicTestKey, "\n", "\r\n")
	alternate, err := signingKeyBinary(reformatted)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(binary, alternate) {
		t.Fatal("equivalent armor formatting changed public-key material")
	}
}

func TestSigningKeyBinaryPreservesMultipleKeys(t *testing.T) {
	entity, err := openpgp.NewEntity("second synthetic test", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var second bytes.Buffer
	if err := entity.Serialize(&second); err != nil {
		t.Fatal(err)
	}
	first, err := signingKeyBinary(publicTestKey)
	if err != nil {
		t.Fatal(err)
	}
	combined := append(append([]byte(nil), first...), second.Bytes()...)
	armored, err := armoredKeyring(combined)
	if err != nil {
		t.Fatal(err)
	}
	result, err := signingKeyBinary(armored)
	if err != nil {
		t.Fatal(err)
	}
	entities, err := openpgp.ReadKeyRing(bytes.NewReader(result))
	if err != nil || len(entities) != 2 {
		t.Fatalf("round trip retained %d keys, err=%v; want 2", len(entities), err)
	}
}

func TestCanonicalBinaryKeyringRejectsInvalidAndPrivateMaterial(t *testing.T) {
	for _, data := range [][]byte{[]byte("corrupt"), []byte(publicTestKey)} {
		if _, err := canonicalPublicKeyring(data); err == nil {
			t.Fatalf("accepted invalid binary keyring %q", data)
		}
	}

	entity, err := openpgp.NewEntity("synthetic private test", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var private bytes.Buffer
	if err := entity.SerializePrivate(&private, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := canonicalPublicKeyring(private.Bytes()); err == nil {
		t.Fatal("accepted binary private-key material")
	}
}

func TestTestUsesBinaryKeyMaterial(t *testing.T) {
	store := newTestStore(t)
	desired := validDesired()
	if _, err := store.Set(desired); err != nil {
		t.Fatal(err)
	}
	keyPath := store.keyringPath(desired.Name)
	data, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	entities, err := openpgp.ReadKeyRing(bytes.NewReader(data))
	if err != nil || len(entities) != 1 {
		t.Fatalf("stored .gpg keyring invalid: keys=%d err=%v", len(entities), err)
	}
	desired.SigningKey = strings.ReplaceAll(publicTestKey, "\n", "\r\n")
	if _, compliant, err := store.Test(desired); err != nil || !compliant {
		t.Fatalf("Test equivalent armor=(%t,%v), want compliant", compliant, err)
	}
	other, err := openpgp.NewEntity("different synthetic test", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var otherArmor bytes.Buffer
	block, err := armor.Encode(&otherArmor, openpgp.PublicKeyType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Serialize(block); err != nil {
		t.Fatal(err)
	}
	if err := block.Close(); err != nil {
		t.Fatal(err)
	}
	desired.SigningKey = otherArmor.String()
	if _, compliant, err := store.Test(desired); err != nil || compliant {
		t.Fatalf("Test different key=(%t,%v), want drift", compliant, err)
	}
}
