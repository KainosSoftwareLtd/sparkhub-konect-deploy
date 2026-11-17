package verify

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
)

// Key is a trusted release key: an id and an ECDSA P-256 public key. The algorithm is fixed (ECDSA P-256 with
// SHA-256 over the manifest bytes, DER signatures); nothing in a signature file can select another.
type Key struct {
	ID  string
	Pub *ecdsa.PublicKey
}

// releaseKeys are the Konect release public keys (design #1702 §3.3): AWS KMS asymmetric keys in sparkhub-prod
// (967880486164), ECC_NIST_P256, SIGN_VERIFY. Each value is the base64 DER SubjectPublicKeyInfo that
// `aws kms get-public-key --query PublicKey --output text` prints, the same string as the train's
// KONECT_RELEASE_SIGNING_KEYS (T/releases.ts) and Konect's K/src/upgrade/release-keys.ts.
//
//   - konect-release-a: alias/konect-release-a (enabled, signs releases)
//   - konect-release-b: alias/konect-release-b (created disabled; enabled only for a planned rotation or a compromise)
//
// PENDING Phase 0c (2026-11-17): neither slot exists yet. A release build refuses to publish a bootstrap that trusts no
// key (the release workflow checks `konect-bootstrap version --json`), and every verification fails closed
// (keys_not_pinned) meanwhile.
var releaseKeys = []struct{ ID, SPKI string }{
	// {"konect-release-a", "<base64 SPKI>"},
	// {"konect-release-b", "<base64 SPKI>"},
}

// extraKeys is empty in a release build. The e2e build tag (keys_e2e.go) adds a test key that only the e2e
// binary trusts; the release workflow refuses a binary that lists it.
var extraKeys []struct{ ID, PEM string }

// TrustedKeys returns the compiled-in keys. It panics on a malformed compiled key: that is a build defect.
func TrustedKeys() []Key {
	var out []Key
	for _, k := range releaseKeys {
		key, err := ParseKeySPKI(k.ID, k.SPKI)
		if err != nil {
			panic(fmt.Sprintf("compiled release key %s: %v", k.ID, err))
		}
		out = append(out, key)
	}
	for _, k := range extraKeys {
		key, err := ParseKeyPEM(k.ID, k.PEM)
		if err != nil {
			panic(fmt.Sprintf("e2e key %s: %v", k.ID, err))
		}
		out = append(out, key)
	}
	return out
}

// ParseKeySPKI reads a base64 DER SubjectPublicKeyInfo (what KMS returns) that must hold a P-256 ECDSA key.
func ParseKeySPKI(id, b64 string) (Key, error) {
	der, err := base64.StdEncoding.Strict().DecodeString(b64)
	if err != nil {
		return Key{}, err
	}
	return ParseKeyDER(id, der)
}

// ParseKeyPEM reads a PEM "PUBLIC KEY" (SubjectPublicKeyInfo) that must hold a P-256 ECDSA key.
func ParseKeyPEM(id, s string) (Key, error) {
	block, _ := pem.Decode([]byte(s))
	if block == nil || block.Type != "PUBLIC KEY" {
		return Key{}, errors.New("not a PEM PUBLIC KEY")
	}
	return ParseKeyDER(id, block.Bytes)
}

// ParseKeyDER reads a DER SubjectPublicKeyInfo that must hold a P-256 ECDSA key.
func ParseKeyDER(id string, der []byte) (Key, error) {
	pub, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return Key{}, err
	}
	ec, ok := pub.(*ecdsa.PublicKey)
	if !ok || ec.Curve != elliptic.P256() {
		return Key{}, errors.New("not an ECDSA P-256 key")
	}
	if id == "" {
		return Key{}, errors.New("empty key id")
	}
	return Key{ID: id, Pub: ec}, nil
}

// Fingerprint is the SHA-256 of the key's DER SubjectPublicKeyInfo, hex (what `konect-bootstrap version` prints).
func Fingerprint(k Key) string {
	der, err := x509.MarshalPKIXPublicKey(k.Pub)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}
