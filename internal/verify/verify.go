// Package verify implements the Konect release verifier contract (design #1702 §3.7), mirroring
// verifyKonectRelease(manifestBytes, sigBytes, trustedKeys, expect) of the train module
// sparkhub-workday-gateway/src/konect/releases.ts. Both must give the same answer for every vector in
// fixtures/konect-releases/vectors.json (fixtures_test.go). The order of the rules is part of the contract:
//
//  1. Trusted keys are compiled in (keys.go): a keyId and a P-256 public key; the algorithm is fixed (ECDSA P-256 /
//     SHA-256, DER signatures). Nothing in the files selects it. No keys: keys_not_pinned.
//  2. The request: an exact version, a known environment (dev, preprod, prod). Else request_invalid.
//  3. manifest.sig.json is parsed strictly (≤ 16 KiB, a JSON object, schema "konect-release-signature/1", 1 to 4
//     entries): sig_file_invalid. The manifest is at most 64 KiB: manifest_invalid.
//  4. Entries whose keyId is not trusted, or that are junk, are ignored.
//  5. At least one valid signature from a trusted key over the exact manifest.json bytes: sig_none_valid.
//  6. Only then is the manifest parsed (from the verified bytes, never re-serialised): its shape (manifest_invalid),
//     version = the requested version (version_mismatch), tag = "v" + version (tag_mismatch), a prerelease never on
//     prod (channel_refused), the entry for the platform (platform_missing).
//  7. Only then are objectSha256, objectSize, sha256 and size of that entry used. A failure names the rule.
package verify

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Schema identifiers.
const (
	SignatureSchema = "konect-release-signature/1"
	ManifestSchema  = "konect-release-manifest/1"
)

// Size caps (T/releases.ts).
const (
	MaxManifestSize      = 65536
	MaxSignatureFileSize = 16384
	MaxSignatures        = 4
	MaxArtifactSize      = int64(1) << 30
	// MaxFileSize caps either file when it is read from disk or the network (the verifier applies the exact caps).
	MaxFileSize = MaxManifestSize
)

// Rule names (contract rule 7). They are T/releases.ts's KONECT_RELEASE_VERIFY_ERROR_CODES.
const (
	RuleKeysNotPinned   = "keys_not_pinned"
	RuleKeysInvalid     = "keys_invalid"
	RuleRequestInvalid  = "request_invalid"
	RuleSigFileInvalid  = "sig_file_invalid"
	RuleSigNoneValid    = "sig_none_valid"
	RuleManifestInvalid = "manifest_invalid"
	RuleVersionMismatch = "version_mismatch"
	RuleTagMismatch     = "tag_mismatch"
	RuleChannelRefused  = "channel_refused"
	RulePlatformMissing = "platform_missing"
)

// Environments a request may name.
var Environments = map[string]bool{"dev": true, "preprod": true, "prod": true}

// Error is a refusal by the verifier. Rule is one of the Rule* names.
type Error struct {
	Rule   string
	Detail string
}

func (e *Error) Error() string {
	if e.Detail == "" {
		return "release verification failed: " + e.Rule
	}
	return "release verification failed: " + e.Rule + " (" + e.Detail + ")"
}

func fail(rule, detail string) error { return &Error{Rule: rule, Detail: detail} }

// RuleOf returns the rule name of a verifier error, or "" for any other error.
func RuleOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Rule
	}
	return ""
}

// Expect is what the caller asked for.
type Expect struct {
	Version     string // the requested version (KONECT_VERSION)
	Environment string // dev | preprod | prod; prod refuses a prerelease manifest
	Platform    string // linux-x64, linux-arm64, macos-arm64, macos-x64, windows-x64, node; "" = the whole manifest
}

// Artifact is one entry of the manifest.
type Artifact struct {
	Name         string
	Platform     string
	SHA256       string // the binary
	Size         int64
	Object       string // the gzip object's name in the bucket: <name>.gz
	ObjectSHA256 string
	ObjectSize   int64
	Encoding     string
}

// Manifest is the parsed konect-release-manifest/1.
type Manifest struct {
	Version   string
	Tag       string
	Commit    string
	Channel   string
	BuiltAt   string
	Artifacts []Artifact
}

// Result is a verified release.
type Result struct {
	Manifest       *Manifest
	Artifact       Artifact // the entry for Expect.Platform (zero when Platform is "")
	KeyID          string   // the first trusted key whose signature verified
	KeyIDs         []string // every trusted key id with a valid signature, in file order
	ManifestSHA256 string
}

// VersionPattern is T/releases.ts's KONECT_VERSION_PATTERN: SemVer X.Y.Z with an optional pre-release, no build
// metadata, no "v"; at most 64 characters (checked separately).
var VersionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-((?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*))?$`)

// ValidVersion reports whether v is an exact release version.
func ValidVersion(v string) bool { return len(v) <= 64 && VersionPattern.MatchString(v) }

var (
	hex64Re   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	commitRe  = regexp.MustCompile(`^[0-9a-f]{40}$`)
	base64Re  = regexp.MustCompile(`^[A-Za-z0-9+/]*={0,2}$`)
	dateRe    = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(:\d{2}(\.\d+)?)?Z$`)
	artifacts = map[string]string{ // platform -> artifact name
		"linux-x64":   "konect-linux-x64",
		"linux-arm64": "konect-linux-arm64",
		"macos-arm64": "konect-macos-arm64",
		"macos-x64":   "konect-macos-x64",
		"windows-x64": "konect-windows-x64.exe",
		"node":        "konect.mjs",
	}
)

// ArtifactName returns the manifest's artifact name for a platform ("" when the platform is unknown).
func ArtifactName(platform string) string { return artifacts[platform] }

// Release runs the verifier contract over the exact bytes of manifest.json and manifest.sig.json.
func Release(manifestBytes, sigBytes []byte, keys []Key, exp Expect) (*Result, error) {
	// Rule 1.
	if len(keys) == 0 {
		return nil, fail(RuleKeysNotPinned, "")
	}
	seen := map[string]bool{}
	for _, k := range keys {
		if k.ID == "" || len(k.ID) > 128 || k.Pub == nil || seen[k.ID] {
			return nil, fail(RuleKeysInvalid, "")
		}
		seen[k.ID] = true
	}
	// Rule 2.
	if !ValidVersion(exp.Version) || !Environments[exp.Environment] {
		return nil, fail(RuleRequestInvalid, "")
	}
	// Rule 3.
	entries, err := parseSignatureFile(sigBytes)
	if err != nil {
		return nil, err
	}
	if len(manifestBytes) > MaxManifestSize {
		return nil, fail(RuleManifestInvalid, "too large")
	}
	// Rules 4-5.
	digest := sha256.Sum256(manifestBytes)
	var valid []string
	for _, e := range entries {
		fields, err := objectFields(e)
		if err != nil {
			continue
		}
		id, ok1 := stringField(fields, "keyId")
		sig64, ok2 := stringField(fields, "sig")
		if !ok1 || !ok2 || contains(valid, id) {
			continue
		}
		var key *Key
		for i := range keys {
			if keys[i].ID == id {
				key = &keys[i]
				break
			}
		}
		if key == nil {
			continue
		}
		der, ok := decodeBase64(sig64)
		if !ok {
			continue
		}
		if ecdsa.VerifyASN1(key.Pub, digest[:], der) {
			valid = append(valid, id)
		}
	}
	if len(valid) == 0 {
		return nil, fail(RuleSigNoneValid, "")
	}
	// Rule 6.
	m, err := parseManifest(manifestBytes)
	if err != nil {
		return nil, err
	}
	if m.Version != exp.Version {
		return nil, fail(RuleVersionMismatch, "")
	}
	if m.Tag != "v"+m.Version {
		return nil, fail(RuleTagMismatch, "")
	}
	if m.Channel != "release" && exp.Environment == "prod" {
		return nil, fail(RuleChannelRefused, "a prerelease is never run against prod")
	}
	res := &Result{Manifest: m, KeyID: valid[0], KeyIDs: valid, ManifestSHA256: hex.EncodeToString(digest[:])}
	if exp.Platform != "" {
		var found []Artifact
		for _, a := range m.Artifacts {
			if a.Platform == exp.Platform {
				found = append(found, a)
			}
		}
		if len(found) != 1 {
			return nil, fail(RulePlatformMissing, exp.Platform)
		}
		res.Artifact = found[0]
	}
	return res, nil
}

func parseSignatureFile(b []byte) ([]json.RawMessage, error) {
	bad := fail(RuleSigFileInvalid, "")
	if len(b) > MaxSignatureFileSize || !utf8.Valid(b) {
		return nil, bad
	}
	obj, err := objectFields(b)
	if err != nil {
		return nil, bad
	}
	if s, ok := stringField(obj, "schema"); !ok || s != SignatureSchema {
		return nil, bad
	}
	raw, ok := obj["signatures"]
	if !ok || len(raw) == 0 || raw[0] != '[' {
		return nil, bad
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil || len(entries) < 1 || len(entries) > MaxSignatures {
		return nil, bad
	}
	return entries, nil
}

// decodeBase64 is strict standard base64 (RFC 4648 §4): padded, no whitespace, canonical trailing bits, ≤ 256 chars.
func decodeBase64(s string) ([]byte, bool) {
	if len(s) == 0 || len(s) > 256 || len(s)%4 != 0 || !base64Re.MatchString(s) {
		return nil, false
	}
	b, err := base64.StdEncoding.Strict().DecodeString(s)
	return b, err == nil
}

// parseManifest checks the manifest shape (T/releases.ts KonectReleaseManifestSchema) on the verified bytes.
func parseManifest(b []byte) (*Manifest, error) {
	bad := func(field string) error { return fail(RuleManifestInvalid, field) }
	if !utf8.Valid(b) { // Go's decoder would replace invalid UTF-8; the train's fatal TextDecoder refuses it
		return nil, bad("not UTF-8")
	}
	obj, err := objectFields(b)
	if err != nil {
		return nil, bad("not a JSON object")
	}
	if s, ok := stringField(obj, "schema"); !ok || s != ManifestSchema {
		return nil, bad("schema")
	}
	m := &Manifest{}
	var ok bool
	if m.Version, ok = stringField(obj, "version"); !ok || !ValidVersion(m.Version) {
		return nil, bad("version")
	}
	if m.Tag, ok = stringField(obj, "tag"); !ok || utf16Len(m.Tag) > 65 {
		return nil, bad("tag")
	}
	if m.Commit, ok = stringField(obj, "commit"); !ok || !commitRe.MatchString(m.Commit) {
		return nil, bad("commit")
	}
	if m.Channel, ok = stringField(obj, "channel"); !ok || (m.Channel != "release" && m.Channel != "prerelease") {
		return nil, bad("channel")
	}
	if m.BuiltAt, ok = stringField(obj, "builtAt"); !ok || !validDate(m.BuiltAt) {
		return nil, bad("builtAt")
	}
	var list []json.RawMessage
	if raw, has := obj["artifacts"]; !has || len(raw) == 0 || raw[0] != '[' || json.Unmarshal(raw, &list) != nil || len(list) != len(artifacts) {
		return nil, bad("artifacts")
	}
	platforms := map[string]bool{}
	for i, raw := range list {
		a, err := parseArtifact(raw)
		if err != nil {
			return nil, bad(fmt.Sprintf("artifacts[%d]", i))
		}
		if platforms[a.Platform] {
			return nil, bad("artifacts: one entry per platform")
		}
		platforms[a.Platform] = true
		m.Artifacts = append(m.Artifacts, a)
	}
	if err := checkEmbedded(obj["embedded"]); err != nil {
		return nil, bad("embedded")
	}
	if (m.Channel == "prerelease") != strings.Contains(m.Version, "-") {
		return nil, bad("channel: prerelease exactly when the version has a pre-release part")
	}
	return m, nil
}

func validDate(s string) bool {
	if !dateRe.MatchString(s) {
		return false
	}
	layout := "2006-01-02T15:04Z07:00"
	if strings.Count(s, ":") == 2 {
		layout = time.RFC3339Nano
	}
	_, err := time.Parse(layout, s)
	return err == nil
}

func parseArtifact(raw json.RawMessage) (Artifact, error) {
	var a Artifact
	f, err := objectFields(raw)
	if err != nil {
		return a, err
	}
	var ok [8]bool
	a.Name, ok[0] = stringField(f, "name")
	a.Platform, ok[1] = stringField(f, "platform")
	a.SHA256, ok[2] = stringField(f, "sha256")
	a.Size, ok[3] = sizeField(f, "size")
	a.Object, ok[4] = stringField(f, "object")
	a.ObjectSHA256, ok[5] = stringField(f, "objectSha256")
	a.ObjectSize, ok[6] = sizeField(f, "objectSize")
	a.Encoding, ok[7] = stringField(f, "encoding")
	for _, v := range ok {
		if !v {
			return a, errors.New("missing or invalid field")
		}
	}
	want, known := artifacts[a.Platform]
	if !known || a.Name != want {
		return a, errors.New("name/platform")
	}
	if !hex64Re.MatchString(a.SHA256) || !hex64Re.MatchString(a.ObjectSHA256) {
		return a, errors.New("hash")
	}
	if a.Encoding != "gzip" || a.Object != a.Name+".gz" {
		return a, errors.New("object")
	}
	return a, nil
}

func checkEmbedded(raw json.RawMessage) error {
	if raw == nil {
		return errors.New("missing")
	}
	f, err := objectFields(raw)
	if err != nil {
		return err
	}
	var templates []json.RawMessage
	if t, ok := f["templates"]; !ok || len(t) == 0 || t[0] != '[' || json.Unmarshal(t, &templates) != nil || len(templates) > 32 {
		return errors.New("templates")
	}
	for _, t := range templates {
		var s string
		if len(t) == 0 || t[0] != '"' || json.Unmarshal(t, &s) != nil || s == "" || utf16Len(s) > 64 {
			return errors.New("templates")
		}
	}
	ca, ok := f["ca"]
	if !ok {
		return errors.New("ca")
	}
	caf, err := objectFields(ca)
	if err != nil {
		return err
	}
	for k := range caf {
		if v, ok := stringField(caf, k); k == "" || utf16Len(k) > 64 || !ok || !hex64Re.MatchString(v) {
			return errors.New("ca")
		}
	}
	return nil
}

// utf16Len is JavaScript's string length.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// objectFields parses one JSON object into its raw members. Keys are matched exactly (Go's struct decoding is
// case-insensitive, JavaScript's is not); duplicate keys and trailing data are refused (JSON.parse would keep the last
// duplicate; refusing is the safe side of that difference: a release job never writes one).
func objectFields(raw []byte) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	t, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := t.(json.Delim); !ok || d != '{' {
		return nil, errors.New("not an object")
	}
	out := map[string]json.RawMessage{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		k, ok := t.(string)
		if !ok {
			return nil, errors.New("bad key")
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("duplicate key %q", k)
		}
		out[k] = v
	}
	if _, err := dec.Token(); err != nil { // the closing '}'
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data")
	}
	return out, nil
}

func stringField(f map[string]json.RawMessage, k string) (string, bool) {
	raw, ok := f[k]
	if !ok || len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// sizeField reads a JSON number that is a positive integer no larger than MaxArtifactSize (as zod's
// z.number().int().positive().max() does: 1e3 and 100.0 are integers).
func sizeField(f map[string]json.RawMessage, k string) (int64, bool) {
	raw, ok := f[k]
	if !ok || len(raw) == 0 || (raw[0] != '-' && (raw[0] < '0' || raw[0] > '9')) {
		return 0, false
	}
	var x float64
	if json.Unmarshal(raw, &x) != nil || x != math.Trunc(x) || x <= 0 || x > float64(MaxArtifactSize) {
		return 0, false
	}
	return int64(x), true
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
