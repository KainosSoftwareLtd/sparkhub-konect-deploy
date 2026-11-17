// Package verify implements the Konect release verifier contract (design #1702 §3.7).
//
// It mirrors verifyRelease(manifestBytes, sigBytes, trustedKeys, expect) of the train module T/releases.ts.
// The order of the rules is part of the contract:
//
//  1. Trusted keys are compiled in (keys.go). Each has a keyId, a public key and a fixed algorithm
//     (ECDSA P-256 / SHA-256). Nothing in the files selects the algorithm.
//  2. manifest.sig.json is parsed strictly: schema "konect-release-signature/1", 1 to 4 entries.
//  3. Entries whose keyId is not trusted, or that are junk, are ignored.
//  4. At least one valid signature from a trusted key over the exact manifest.json bytes is required.
//  5. Only then is the manifest parsed (from the verified bytes, never re-serialised) and checked, in order:
//     schema, shape, version = the requested version, tag = "v" + version, channel, this platform's entry.
//  6. Only then are objectSha256, objectSize, sha256 and size of that entry used.
//  7. A failure names the rule, never the bytes.
package verify

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Schema identifiers.
const (
	SignatureSchema = "konect-release-signature/1"
	ManifestSchema  = "konect-release-manifest/1"
)

// MaxArtifactSize caps every size a manifest may declare (1 GiB); a release binary is about 100 MB.
const MaxArtifactSize int64 = 1 << 30

// MaxFileSize caps manifest.json and manifest.sig.json when they are read from disk or the network.
const MaxFileSize = 1 << 20

// Rule names (contract rule 7). They match T/releases.ts.
const (
	RuleSigInvalid        = "sig_invalid"        // manifest.sig.json is not a JSON object
	RuleSigSchema         = "sig_schema"         // wrong or missing schema
	RuleSigCount          = "sig_count"          // signatures is not a list of 1 to 4 entries
	RuleSigNoneValid      = "sig_none_valid"     // no valid signature from a trusted key
	RuleManifestInvalid   = "manifest_invalid"   // not JSON, or not the manifest shape
	RuleManifestSchema    = "manifest_schema"    // wrong or missing schema
	RuleVersionMismatch   = "version_mismatch"   // version is not the requested one
	RuleTagMismatch       = "tag_mismatch"       // tag is not "v" + version
	RuleChannelInvalid    = "channel_invalid"    // channel unknown, or not consistent with the version
	RuleChannelRefused    = "channel_refused"    // a prerelease on prod
	RulePlatformMissing   = "platform_missing"   // no entry for this platform
	RulePlatformDuplicate = "platform_duplicate" // more than one entry for this platform
)

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
	Version  string // the requested version (KONECT_VERSION)
	Prod     bool   // KONECT_SPARKHUB=prod: a prerelease manifest is refused
	Platform string // linux-x64, linux-arm64, macos-arm64, macos-x64, windows-x64, node
}

// Artifact is one entry of the manifest.
type Artifact struct {
	Name         string
	Platform     string
	SHA256       string // the binary
	Size         int64
	Object       string // the gzip object's name in the bucket
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

// Result is a verified release for one platform.
type Result struct {
	Manifest *Manifest
	Artifact Artifact // the entry for Expect.Platform
	KeyID    string   // the trusted key whose signature verified
}

var (
	hex64Re   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	commitRe  = regexp.MustCompile(`^[0-9a-f]{40}$`)
	semverRe  = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)
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
	keyID, err := checkSignatures(manifestBytes, sigBytes, keys)
	if err != nil {
		return nil, err
	}
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
	// "prerelease" exactly when the version has a pre-release suffix; never on prod.
	pre := strings.Contains(m.Version, "-")
	switch {
	case m.Channel == "release" && !pre:
	case m.Channel == "prerelease" && pre:
		if exp.Prod {
			return nil, fail(RuleChannelRefused, "a prerelease is never run against prod")
		}
	case m.Channel == "release" || m.Channel == "prerelease":
		return nil, fail(RuleChannelInvalid, "channel does not match the version")
	default:
		return nil, fail(RuleChannelInvalid, "unknown channel")
	}
	var found []Artifact
	for _, a := range m.Artifacts {
		if a.Platform == exp.Platform {
			found = append(found, a)
		}
	}
	switch len(found) {
	case 0:
		return nil, fail(RulePlatformMissing, exp.Platform)
	case 1:
	default:
		return nil, fail(RulePlatformDuplicate, exp.Platform)
	}
	return &Result{Manifest: m, Artifact: found[0], KeyID: keyID}, nil
}

// checkSignatures applies rules 2 to 4 and returns the id of the key that verified.
func checkSignatures(manifestBytes, sigBytes []byte, keys []Key) (string, error) {
	obj, err := objectFields(sigBytes)
	if err != nil {
		return "", fail(RuleSigInvalid, "")
	}
	if s, ok := stringField(obj, "schema"); !ok || s != SignatureSchema {
		return "", fail(RuleSigSchema, "")
	}
	raw, ok := obj["signatures"]
	if !ok {
		return "", fail(RuleSigCount, "")
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil || entries == nil {
		return "", fail(RuleSigCount, "")
	}
	if len(entries) < 1 || len(entries) > 4 {
		return "", fail(RuleSigCount, "")
	}
	digest := sha256.Sum256(manifestBytes)
	for _, e := range entries {
		fields, err := objectFields(e)
		if err != nil {
			continue // junk entry: ignored (rule 3)
		}
		id, ok1 := stringField(fields, "keyId")
		sig64, ok2 := stringField(fields, "sig")
		if !ok1 || !ok2 {
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
			continue // unknown key id: ignored (rule 3)
		}
		der, err := base64.StdEncoding.Strict().DecodeString(sig64)
		if err != nil || len(der) == 0 || len(der) > 80 {
			continue
		}
		if ecdsa.VerifyASN1(key.Pub, digest[:], der) {
			return key.ID, nil
		}
	}
	return "", fail(RuleSigNoneValid, "")
}

// parseManifest applies rule 5's schema and shape checks to the verified bytes.
func parseManifest(b []byte) (*Manifest, error) {
	obj, err := objectFields(b)
	if err != nil {
		return nil, fail(RuleManifestInvalid, "not a JSON object")
	}
	if s, ok := stringField(obj, "schema"); !ok || s != ManifestSchema {
		return nil, fail(RuleManifestSchema, "")
	}
	m := &Manifest{}
	var ok bool
	bad := func(field string) error { return fail(RuleManifestInvalid, field) }
	if m.Version, ok = stringField(obj, "version"); !ok || !semverRe.MatchString(m.Version) {
		return nil, bad("version")
	}
	if m.Tag, ok = stringField(obj, "tag"); !ok {
		return nil, bad("tag")
	}
	if m.Commit, ok = stringField(obj, "commit"); !ok || !commitRe.MatchString(m.Commit) {
		return nil, bad("commit")
	}
	if m.Channel, ok = stringField(obj, "channel"); !ok {
		return nil, bad("channel")
	}
	if m.BuiltAt, ok = stringField(obj, "builtAt"); !ok {
		return nil, bad("builtAt")
	}
	if _, err := time.Parse(time.RFC3339Nano, m.BuiltAt); err != nil || !strings.HasSuffix(m.BuiltAt, "Z") {
		return nil, bad("builtAt")
	}
	var list []json.RawMessage
	if raw, has := obj["artifacts"]; !has || json.Unmarshal(raw, &list) != nil || len(list) != len(artifacts) {
		return nil, bad("artifacts")
	}
	for i, raw := range list {
		a, err := parseArtifact(raw)
		if err != nil {
			return nil, bad(fmt.Sprintf("artifacts[%d]", i))
		}
		m.Artifacts = append(m.Artifacts, a)
	}
	if err := checkEmbedded(obj["embedded"]); err != nil {
		return nil, bad("embedded")
	}
	return m, nil
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
			return a, errors.New("missing field")
		}
	}
	want, known := artifacts[a.Platform]
	if !known || a.Name != want {
		return a, errors.New("name/platform")
	}
	if !hex64Re.MatchString(a.SHA256) || !hex64Re.MatchString(a.ObjectSHA256) {
		return a, errors.New("hash")
	}
	if a.Encoding != "gzip" || a.Object == "" {
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
	var templates []string
	if t, ok := f["templates"]; !ok || json.Unmarshal(t, &templates) != nil || templates == nil {
		return errors.New("templates")
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
		if v, ok := stringField(caf, k); !ok || !hex64Re.MatchString(v) {
			return errors.New("ca")
		}
	}
	return nil
}

// objectFields parses one JSON object into its raw members. Keys are matched exactly (Go's struct decoding
// is case-insensitive, JavaScript's is not), duplicate keys and trailing data are refused, so this parser
// and JSON.parse never read two different values out of the same bytes.
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

// sizeField reads a positive integer no larger than MaxArtifactSize, written as plain digits.
func sizeField(f map[string]json.RawMessage, k string) (int64, bool) {
	raw, ok := f[k]
	if !ok || len(raw) == 0 || len(raw) > 12 {
		return 0, false
	}
	for _, c := range raw {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil || n <= 0 || n > MaxArtifactSize {
		return 0, false
	}
	return n, true
}
