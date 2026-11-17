// Package testrelease builds signed test releases (manifest.json, manifest.sig.json, the gzip object) in the format
// of design #1702 §3.3 and §3.5. Tests only: no release binary imports it.
package testrelease

import (
	"bytes"
	"compress/gzip"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"strings"

	"github.com/KainosSoftwareLtd/sparkhub-konect-deploy/internal/verify"
)

// Platforms in manifest order.
var Platforms = []string{"linux-x64", "linux-arm64", "macos-arm64", "macos-x64", "windows-x64", "node"}

// Signer is a test signing key.
type Signer struct {
	ID   string
	Priv *ecdsa.PrivateKey
}

// NewSigner makes a fresh P-256 key.
func NewSigner(id string) *Signer {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	return &Signer{ID: id, Priv: k}
}

// Key is the verifier's view of the signer.
func (s *Signer) Key() verify.Key { return verify.Key{ID: s.ID, Pub: &s.Priv.PublicKey} }

// PublicPEM is the SubjectPublicKeyInfo PEM.
func (s *Signer) PublicPEM() string {
	der, err := x509.MarshalPKIXPublicKey(&s.Priv.PublicKey)
	if err != nil {
		panic(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

// Sign returns base64 DER over SHA-256(msg).
func (s *Signer) Sign(msg []byte) string {
	d := sha256.Sum256(msg)
	sig, err := ecdsa.SignASN1(rand.Reader, s.Priv, d[:])
	if err != nil {
		panic(err)
	}
	return base64.StdEncoding.EncodeToString(sig)
}

// Release is a built test release for one real platform (the others carry placeholder hashes).
type Release struct {
	Version   string
	Platform  string
	Binary    []byte
	Object    []byte // gzip of Binary
	Manifest  []byte
	Signature []byte
	SHA256    string
}

// Build makes a release of binary for platform, signed by signers (in order).
func Build(version, platform string, binary []byte, signers ...*Signer) *Release {
	var gz bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&gz, gzip.BestCompression)
	_, _ = zw.Write(binary)
	_ = zw.Close()
	r := &Release{Version: version, Platform: platform, Binary: binary, Object: gz.Bytes(), SHA256: hexSum(binary)}
	r.Manifest = ManifestJSON(version, platform, binary, r.Object)
	r.Signature = SignatureJSON(r.Manifest, signers...)
	return r
}

// ManifestJSON builds manifest.json bytes.
func ManifestJSON(version, platform string, binary, object []byte) []byte {
	channel := "release"
	if strings.Contains(version, "-") {
		channel = "prerelease"
	}
	var arts []map[string]any
	for i, p := range Platforms {
		a := map[string]any{
			"name": verify.ArtifactName(p), "platform": p,
			"sha256": strings.Repeat(string("0123456789abcdef"[i]), 64), "size": 1000 + i,
			"object": verify.ArtifactName(p) + ".gz", "objectSha256": strings.Repeat(string("fedcba9876543210"[i]), 64), "objectSize": 500 + i,
			"encoding": "gzip",
		}
		if p == platform {
			a["sha256"], a["size"] = hexSum(binary), len(binary)
			a["objectSha256"], a["objectSize"] = hexSum(object), len(object)
		}
		arts = append(arts, a)
	}
	b, _ := json.Marshal(map[string]any{
		"schema": verify.ManifestSchema, "version": version, "tag": "v" + version,
		"commit": strings.Repeat("c", 40), "channel": channel, "builtAt": "2026-11-17T09:00:00.000Z",
		"artifacts": arts,
		"embedded":  map[string]any{"templates": []string{"sqlite"}, "ca": map[string]string{}},
	})
	return b
}

// SignatureJSON builds manifest.sig.json bytes.
func SignatureJSON(manifest []byte, signers ...*Signer) []byte {
	sigs := []map[string]string{}
	for _, s := range signers {
		sigs = append(sigs, map[string]string{"keyId": s.ID, "sig": s.Sign(manifest)})
	}
	b, _ := json.Marshal(map[string]any{"schema": verify.SignatureSchema, "signatures": sigs})
	return b
}

func hexSum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
