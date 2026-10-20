package apt

import (
	"bytes"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp/armor"
	openpgp "github.com/ProtonMail/go-crypto/openpgp/v2"
)

func TestMultipleKeyringsAndFingerprintTrustSelections(t *testing.T) {
	store := newTestStore(t)
	desired := validDesired()
	first, err := signingKeyBinary(desired.SigningKey)
	if err != nil {
		t.Fatal(err)
	}
	second, err := signingKeyBinary(otherTestKey(t))
	if err != nil {
		t.Fatal(err)
	}
	fixtureFile(t, store, "/etc/apt/keyrings/first.asc", desired.SigningKey)
	fixtureFile(t, store, "/etc/apt/keyrings/second.gpg", string(second))
	combined := append(append([]byte(nil), second...), first...)
	desired.SigningKey, err = armoredKeyring(combined)
	if err != nil {
		t.Fatal(err)
	}
	path := fixtureFile(t, store, "/etc/apt/sources.list", "deb [arch=amd64 signed-by=/etc/apt/keyrings/first.asc,/etc/apt/keyrings/second.gpg] "+desired.URI+" stable main contrib\n")
	assertCompliantNoOp(t, store, desired)

	desired.SigningKey = publicTestKey
	if _, differences, err := store.Test(desired); err != nil || !slices.Equal(differences, []string{"signingKey"}) {
		t.Fatalf("extra trusted key differences = %v, %v", differences, err)
	}
	entities, err := openpgp.ReadKeyRing(bytes.NewReader(first))
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := fmt.Sprintf("%X", entities[0].PrimaryKey.Fingerprint)
	writeFile(t, path, "deb [arch=amd64 signed-by=/etc/apt/keyrings/first.asc,/etc/apt/keyrings/second.gpg,"+fingerprint+"] "+desired.URI+" stable main contrib\n")
	assertCompliantNoOp(t, store, desired)
	writeFile(t, path, "deb [arch=amd64 signed-by=/etc/apt/keyrings/first.asc,"+fingerprint+"!] "+desired.URI+" stable main contrib\n")
	before := snapshot(t, store.Root)
	if _, err := store.Set(desired); err == nil {
		t.Fatal("restricted fingerprint trust was silently broadened")
	}
	assertUnchanged(t, before, snapshot(t, store.Root))
}

func otherTestKey(t *testing.T) string {
	t.Helper()
	entity, err := openpgp.NewEntity("second synthetic test", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var binary bytes.Buffer
	if err := entity.Serialize(&binary); err != nil {
		t.Fatal(err)
	}
	armored, err := armoredKeyring(binary.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return armored
}

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

func TestSigningKeySerializationIsStableWithMultipleIdentities(t *testing.T) {
	entity, err := openpgp.NewEntity("primary identity", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := entity.AddUserId("secondary identity", "", "", nil); err != nil {
		t.Fatal(err)
	}
	var armored bytes.Buffer
	block, err := armor.Encode(&armored, openpgp.PublicKeyType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := entity.Serialize(block); err != nil {
		t.Fatal(err)
	}
	if err := block.Close(); err != nil {
		t.Fatal(err)
	}
	first, err := signingKeyBinary(armored.String())
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 20; attempt++ {
		next, err := signingKeyBinary(armored.String())
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, next) {
			t.Fatalf("canonical keyring changed on attempt %d", attempt)
		}
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
	if _, differences, err := store.Test(desired); err != nil || differences == nil || len(differences) != 0 {
		t.Fatalf("Test equivalent armor=(%v,%v), want empty differences", differences, err)
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
	if _, differences, err := store.Test(desired); err != nil || !slices.Equal(differences, []string{"signingKey"}) {
		t.Fatalf("Test different key=(%v,%v), want signingKey drift", differences, err)
	}
}
