package apt

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp/armor"
	openpgp "github.com/ProtonMail/go-crypto/openpgp/v2"
)

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
	entities, err := openpgp.ReadKeyRing(bytes.NewReader(remaining))
	if err != nil {
		return nil, fmt.Errorf("parse OpenPGP keyring: %w", err)
	}
	if len(entities) == 0 {
		return nil, errors.New("empty OpenPGP keyring")
	}
	var canonical bytes.Buffer
	for _, entity := range entities {
		if entity.PrimaryKey == nil || entity.PrivateKey != nil {
			return nil, errors.New("not a public-only OpenPGP key")
		}
		if _, err := entity.VerifyPrimaryKey(time.Now(), nil); err != nil {
			return nil, fmt.Errorf("verify OpenPGP primary key: %w", err)
		}
		for _, subkey := range entity.Subkeys {
			if subkey.PrivateKey != nil {
				return nil, errors.New("private OpenPGP subkey")
			}
		}
		if err := entity.Serialize(&canonical); err != nil {
			return nil, fmt.Errorf("serialize OpenPGP public key: %w", err)
		}
	}
	return canonical.Bytes(), nil
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
	var canonical bytes.Buffer
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
		if err := entity.Serialize(&canonical); err != nil {
			return nil, fmt.Errorf("serialize OpenPGP public key: %w", err)
		}
	}
	return canonical.Bytes(), nil
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
