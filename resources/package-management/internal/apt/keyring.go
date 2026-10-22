package apt

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp/armor"
	openpgp "github.com/ProtonMail/go-crypto/openpgp/v2"
)

type KeyringFormat string

const (
	ArmoredKeyring KeyringFormat = "ascii-armored"
	BinaryKeyring  KeyringFormat = "binary-openpgp"
)

type AptKeyring struct {
	Reference        string
	Path             string
	Format           KeyringFormat
	OriginalContents []byte
	Material         []byte
	originalMaterial []byte
	info             os.FileInfo
	store            Store
	create           bool
}

type keyringContentError struct{ error }

func (s Store) ReadKeyring(reference string) (*AptKeyring, error) {
	path, err := s.resolve(reference)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect signing key %q: %w", reference, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("signing key %q is not a regular file", reference)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read signing key %q: %w", reference, err)
	}
	key := &AptKeyring{Reference: reference, Path: path, OriginalContents: data, info: info, store: s}
	switch filepath.Ext(reference) {
	case ".asc":
		key.Format = ArmoredKeyring
		key.Material, err = signingKeyBinary(string(data))
	case ".gpg":
		key.Format = BinaryKeyring
		key.Material, err = canonicalPublicKeyring(data)
	default:
		err = errors.New("APT keyrings must use .asc for ASCII armor or .gpg for binary OpenPGP")
	}
	if err != nil {
		return key, &keyringContentError{fmt.Errorf("read signing key %q: %w", reference, err)}
	}
	key.originalMaterial = slices.Clone(key.Material)
	return key, nil
}

func (k *AptKeyring) changed() bool {
	return k.create || !bytes.Equal(k.Material, k.originalMaterial)
}

func (k *AptKeyring) preflight() error {
	if !k.changed() {
		return nil
	}
	return k.store.checkWrite(k.Path, k.OriginalContents, k.info, k.create)
}

func (k *AptKeyring) WriteToDisk() error {
	if !k.changed() {
		return nil
	}
	if err := k.preflight(); err != nil {
		return err
	}
	data, err := canonicalPublicKeyring(k.Material)
	if err != nil {
		return err
	}
	if k.Format == ArmoredKeyring {
		armored, err := armoredKeyring(data)
		if err != nil {
			return err
		}
		data = []byte(armored)
	}
	mode := os.FileMode(0644)
	if k.info != nil {
		mode = k.info.Mode().Perm()
	}
	if err := atomicWrite(k.Path, data, mode); err != nil {
		return fmt.Errorf("write signing key %q: %w", k.Reference, err)
	}
	reloaded, err := k.store.ReadKeyring(k.Reference)
	if err != nil {
		return err
	}
	*k = *reloaded
	return nil
}

func ValidateSigningKey(value string) (string, error) {
	keyring, err := signingKeyBinary(value)
	if err != nil {
		return "", err
	}
	return armoredKeyring(keyring)
}

func signingKeyBinary(value string) ([]byte, error) {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "-----BEGIN PGP PUBLIC KEY BLOCK-----") ||
		!strings.HasSuffix(value, "-----END PGP PUBLIC KEY BLOCK-----") ||
		strings.Count(value, "-----BEGIN PGP PUBLIC KEY BLOCK-----") != 1 ||
		strings.Count(value, "-----END PGP PUBLIC KEY BLOCK-----") != 1 {
		return nil, errors.New("expected one complete public-key armor block")
	}
	input := strings.NewReader(value)
	block, err := armor.Decode(input)
	if err != nil {
		return nil, fmt.Errorf("decode OpenPGP armor: %w", err)
	}
	if block.Type != openpgp.PublicKeyType {
		return nil, errors.New("expected public-key armor")
	}
	remaining, err := io.ReadAll(block.Body)
	if err != nil {
		return nil, fmt.Errorf("read armored OpenPGP key: %w", err)
	}
	if len(bytes.TrimSpace(remaining)) == 0 {
		return nil, errors.New("empty OpenPGP payload")
	}
	if err := validateArmorChecksum(value, remaining); err != nil {
		return nil, err
	}
	return canonicalPublicKeyring(remaining)
}

func armoredKeyring(keyring []byte) (string, error) {
	canonical, err := canonicalPublicKeyring(keyring)
	if err != nil {
		return "", err
	}
	var output bytes.Buffer
	armored, err := armor.Encode(&output, openpgp.PublicKeyType, nil)
	if err != nil {
		return "", fmt.Errorf("encode OpenPGP armor: %w", err)
	}
	if _, err := armored.Write(canonical); err != nil {
		return "", fmt.Errorf("write OpenPGP armor: %w", err)
	}
	if err := armored.Close(); err != nil {
		return "", fmt.Errorf("close OpenPGP armor: %w", err)
	}
	return output.String() + "\n", nil
}

func canonicalPublicKeyring(data []byte) ([]byte, error) {
	entities, err := openpgp.ReadKeyRing(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("parse binary OpenPGP keyring: %w", err)
	}
	if len(entities) == 0 {
		return nil, errors.New("empty binary OpenPGP keyring")
	}
	var keys [][]byte
	for _, entity := range entities {
		if entity.PrimaryKey == nil || entity.PrivateKey != nil {
			return nil, errors.New("not a public-only OpenPGP keyring")
		}
		if _, err := entity.VerifyPrimaryKey(time.Now(), nil); err != nil {
			return nil, fmt.Errorf("verify OpenPGP primary key: %w", err)
		}
		for _, subkey := range entity.Subkeys {
			if subkey.PrivateKey != nil {
				return nil, errors.New("private OpenPGP subkey")
			}
		}
		var key bytes.Buffer
		if err := serializeEntity(&key, entity); err != nil {
			return nil, fmt.Errorf("serialize OpenPGP public key: %w", err)
		}
		keys = append(keys, key.Bytes())
	}
	slices.SortFunc(keys, bytes.Compare)
	keys = slices.CompactFunc(keys, bytes.Equal)
	return bytes.Join(keys, nil), nil
}

func (s Store) signingMaterial(signing SigningConfiguration) ([]byte, error) {
	if signing.InlineKey != "" {
		material, err := signingKeyBinary(signing.InlineKey)
		if err != nil {
			return nil, &keyringContentError{err}
		}
		return material, nil
	}
	if len(signing.Keyrings) == 0 {
		if len(signing.Fingerprints) != 0 {
			return nil, errors.New("Signed-By selects keys from global trust; configure an explicit keyring before managing this repository")
		}
		// Global APT trust is deliberately not equivalent to an explicit key.
		return nil, nil
	}
	var material []byte
	for _, reference := range signing.Keyrings {
		key, err := s.ReadKeyring(reference)
		if err != nil {
			return nil, err
		}
		material = append(material, key.Material...)
	}
	if len(signing.Fingerprints) != 0 {
		entities, err := openpgp.ReadKeyRing(bytes.NewReader(material))
		if err != nil {
			return nil, err
		}
		var selected bytes.Buffer
		for _, fingerprint := range signing.Fingerprints {
			found := false
			for _, entity := range entities {
				if strings.EqualFold(fingerprint, fmt.Sprintf("%X", entity.PrimaryKey.Fingerprint)) {
					if err := serializeEntity(&selected, entity); err != nil {
						return nil, err
					}
					found = true
				}
			}
			if !found {
				return nil, fmt.Errorf("Signed-By selector %q is missing or restricts subkeys; provision an explicit keyring with the intended trust before managing this repository", fingerprint)
			}
		}
		material = selected.Bytes()
	}
	return canonicalPublicKeyring(material)
}

func serializeEntity(destination io.Writer, entity *openpgp.Entity) error {
	if err := entity.PrimaryKey.Serialize(destination); err != nil {
		return err
	}
	for _, revocation := range entity.Revocations {
		if err := revocation.Packet.Serialize(destination); err != nil {
			return err
		}
	}
	for _, signature := range entity.DirectSignatures {
		if err := signature.Packet.Serialize(destination); err != nil {
			return err
		}
	}
	identityNames := make([]string, 0, len(entity.Identities))
	for name := range entity.Identities {
		identityNames = append(identityNames, name)
	}
	sort.Strings(identityNames)
	for _, name := range identityNames {
		if err := entity.Identities[name].Serialize(destination); err != nil {
			return err
		}
	}
	for _, subkey := range entity.Subkeys {
		if err := subkey.Serialize(destination, false); err != nil {
			return err
		}
	}
	return nil
}

func validateArmorChecksum(value string, payload []byte) error {
	lines := strings.Split(value, "\n")
	separator := -1
	for i := 1; i < len(lines)-1; i++ {
		if lines[i] == "" {
			separator = i
			break
		}
	}
	if separator < 0 {
		return errors.New("OpenPGP armor has no body separator")
	}
	checksum := ""
	for _, line := range lines[separator+1 : len(lines)-1] {
		if strings.HasPrefix(line, "=") {
			if checksum != "" {
				return errors.New("OpenPGP armor has multiple checksums")
			}
			checksum = line
		} else if checksum != "" {
			return errors.New("data follows OpenPGP armor checksum")
		}
	}
	if checksum == "" {
		return nil
	}
	if len(checksum) != 5 {
		return errors.New("invalid OpenPGP armor checksum")
	}
	decoded, err := base64.StdEncoding.DecodeString(checksum[1:])
	if err != nil || len(decoded) != 3 {
		return errors.New("invalid OpenPGP armor checksum")
	}
	actual := crc24(payload)
	expected := uint32(decoded[0])<<16 | uint32(decoded[1])<<8 | uint32(decoded[2])
	if actual != expected {
		return errors.New("OpenPGP armor checksum mismatch")
	}
	return nil
}

func crc24(data []byte) uint32 {
	checksum := uint32(0xB704CE)
	for _, value := range data {
		checksum ^= uint32(value) << 16
		for bit := 0; bit < 8; bit++ {
			checksum <<= 1
			if checksum&0x1000000 != 0 {
				checksum ^= 0x1864CFB
			}
		}
	}
	return checksum & 0xFFFFFF
}
