package verify

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

var platforms = []string{"linux-x64", "linux-arm64", "macos-arm64", "macos-x64", "windows-x64", "node"}

func hex64(c byte) string { return strings.Repeat(string(c), 64) }

func manifestMap(version string) map[string]any {
	channel := "release"
	if strings.Contains(version, "-") {
		channel = "prerelease"
	}
	var arts []any
	for i, p := range platforms {
		arts = append(arts, map[string]any{
			"name": ArtifactName(p), "platform": p,
			"sha256": hex64("0123456789abcdef"[i]), "size": 100000 + i,
			"object": ArtifactName(p) + ".gz", "objectSha256": hex64("fedcba9876543210"[i]), "objectSize": 40000 + i,
			"encoding": "gzip",
		})
	}
	return map[string]any{
		"schema": ManifestSchema, "version": version, "tag": "v" + version,
		"commit": strings.Repeat("a", 40), "channel": channel, "builtAt": "2026-11-17T09:00:00.000Z",
		"artifacts": arts,
		"embedded":  map[string]any{"templates": []string{"sqlite", "postgres"}, "ca": map[string]any{"azure-postgres": hex64('1')}},
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func newKey(t *testing.T, id string) (*ecdsa.PrivateKey, Key) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv, Key{ID: id, Pub: &priv.PublicKey}
}

func sign(t *testing.T, priv *ecdsa.PrivateKey, msg []byte) string {
	t.Helper()
	d := sha256.Sum256(msg)
	sig, err := ecdsa.SignASN1(rand.Reader, priv, d[:])
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(sig)
}

func sigFile(t *testing.T, entries ...map[string]any) []byte {
	t.Helper()
	list := []any{}
	for _, e := range entries {
		list = append(list, e)
	}
	return mustJSON(t, map[string]any{"schema": SignatureSchema, "signatures": list})
}

func TestReleaseOK(t *testing.T) {
	priv, key := newKey(t, "konect-release-a")
	m := mustJSON(t, manifestMap("0.7.0"))
	s := sigFile(t, map[string]any{"keyId": "konect-release-a", "sig": sign(t, priv, m)})
	for _, p := range platforms {
		r, err := Release(m, s, []Key{key}, Expect{Version: "0.7.0", Prod: true, Platform: p})
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if r.Artifact.Platform != p || r.KeyID != "konect-release-a" || r.Artifact.Name != ArtifactName(p) {
			t.Fatalf("%s: wrong result %+v", p, r)
		}
	}
}

func TestReleaseSignatureRules(t *testing.T) {
	privA, keyA := newKey(t, "konect-release-a")
	privB, keyB := newKey(t, "konect-release-b")
	privX, _ := newKey(t, "attacker")
	m := mustJSON(t, manifestMap("0.7.0"))
	good := map[string]any{"keyId": "konect-release-a", "sig": sign(t, privA, m)}
	exp := Expect{Version: "0.7.0", Prod: true, Platform: "linux-x64"}
	keys := []Key{keyA, keyB}

	cases := []struct {
		name string
		sig  []byte
		want string
	}{
		{"not json", []byte("nope"), RuleSigInvalid},
		{"json array", []byte(`[]`), RuleSigInvalid},
		{"trailing data", append(sigFile(t, good), []byte(` {}`)...), RuleSigInvalid},
		{"duplicate key", []byte(`{"schema":"konect-release-signature/1","schema":"x","signatures":[]}`), RuleSigInvalid},
		{"schema missing", mustJSON(t, map[string]any{"signatures": []any{good}}), RuleSigSchema},
		{"schema wrong", mustJSON(t, map[string]any{"schema": "konect-release-signature/2", "signatures": []any{good}}), RuleSigSchema},
		{"schema case", []byte(`{"Schema":"konect-release-signature/1","signatures":[]}`), RuleSigSchema},
		{"signatures missing", mustJSON(t, map[string]any{"schema": SignatureSchema}), RuleSigCount},
		{"signatures null", []byte(`{"schema":"konect-release-signature/1","signatures":null}`), RuleSigCount},
		{"signatures object", []byte(`{"schema":"konect-release-signature/1","signatures":{}}`), RuleSigCount},
		{"signatures empty", sigFile(t), RuleSigCount},
		{"five entries", sigFile(t, good, good, good, good, good), RuleSigCount},
		{"only unknown ids", sigFile(t, map[string]any{"keyId": "attacker", "sig": sign(t, privX, m)}), RuleSigNoneValid},
		{"trusted id, wrong key", sigFile(t, map[string]any{"keyId": "konect-release-a", "sig": sign(t, privX, m)}), RuleSigNoneValid},
		{"id of A, signed by B", sigFile(t, map[string]any{"keyId": "konect-release-a", "sig": sign(t, privB, m)}), RuleSigNoneValid},
		{"signature over other bytes", sigFile(t, map[string]any{"keyId": "konect-release-a", "sig": sign(t, privA, append(m, ' '))}), RuleSigNoneValid},
		{"junk entries only", []byte(`{"schema":"konect-release-signature/1","signatures":[1,"x",null,{"keyId":5}]}`), RuleSigNoneValid},
		{"bad base64", sigFile(t, map[string]any{"keyId": "konect-release-a", "sig": "%%%"}), RuleSigNoneValid},
		{"raw r||s instead of DER", sigFile(t, map[string]any{"keyId": "konect-release-a", "sig": base64.StdEncoding.EncodeToString(make([]byte, 64))}), RuleSigNoneValid},
		{"key id case", sigFile(t, map[string]any{"keyId": "Konect-Release-A", "sig": good["sig"]}), RuleSigNoneValid},
	}
	for _, c := range cases {
		_, err := Release(m, c.sig, keys, exp)
		if got := RuleOf(err); got != c.want {
			t.Errorf("%s: got %q (%v), want %q", c.name, got, err, c.want)
		}
	}

	// Rule 3: unknown and junk entries are ignored when a valid one is present; an "alg" field selects nothing.
	okCases := map[string][]byte{
		"unknown then valid": sigFile(t, map[string]any{"keyId": "attacker", "sig": sign(t, privX, m)}, good),
		"junk then valid":    []byte(`{"schema":"konect-release-signature/1","signatures":[1,{"keyId":"konect-release-a","sig":"AAAA"},` + string(mustJSON(t, good)) + `]}`),
		"slot B only":        sigFile(t, map[string]any{"keyId": "konect-release-b", "sig": sign(t, privB, m)}),
		"alg ignored":        sigFile(t, map[string]any{"keyId": "konect-release-a", "sig": good["sig"], "alg": "none"}),
		"extra top field":    []byte(`{"schema":"konect-release-signature/1","note":"x","signatures":[` + string(mustJSON(t, good)) + `]}`),
	}
	for name, s := range okCases {
		if _, err := Release(m, s, keys, exp); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// No trusted keys at all: nothing verifies.
	if got := RuleOf(func() error { _, err := Release(m, sigFile(t, good), nil, exp); return err }()); got != RuleSigNoneValid {
		t.Errorf("no keys: got %q", got)
	}
}

func TestReleaseManifestRules(t *testing.T) {
	priv, key := newKey(t, "konect-release-a")
	run := func(m []byte, exp Expect) error {
		s := sigFile(t, map[string]any{"keyId": "konect-release-a", "sig": sign(t, priv, m)})
		_, err := Release(m, s, []Key{key}, exp)
		return err
	}
	prod := Expect{Version: "0.7.0", Prod: true, Platform: "linux-x64"}
	mut := func(version string, f func(m map[string]any)) []byte {
		m := manifestMap(version)
		f(m)
		return mustJSON(t, m)
	}
	art := func(m map[string]any, i int) map[string]any { return m["artifacts"].([]any)[i].(map[string]any) }

	cases := []struct {
		name string
		m    []byte
		exp  Expect
		want string
	}{
		{"not json", []byte("{"), prod, RuleManifestInvalid},
		{"duplicate key", []byte(`{"schema":"konect-release-manifest/1","version":"0.7.0","version":"0.5.0"}`), prod, RuleManifestInvalid},
		{"schema", mut("0.7.0", func(m map[string]any) { m["schema"] = "konect-release-manifest/2" }), prod, RuleManifestSchema},
		{"version not semver", mut("0.7.0", func(m map[string]any) { m["version"] = "latest" }), prod, RuleManifestInvalid},
		{"commit", mut("0.7.0", func(m map[string]any) { m["commit"] = "abc" }), prod, RuleManifestInvalid},
		{"builtAt", mut("0.7.0", func(m map[string]any) { m["builtAt"] = "yesterday" }), prod, RuleManifestInvalid},
		{"five artifacts", mut("0.7.0", func(m map[string]any) { m["artifacts"] = m["artifacts"].([]any)[:5] }), prod, RuleManifestInvalid},
		{"size float", mut("0.7.0", func(m map[string]any) { art(m, 0)["size"] = 1.5 }), prod, RuleManifestInvalid},
		{"size zero", mut("0.7.0", func(m map[string]any) { art(m, 0)["size"] = 0 }), prod, RuleManifestInvalid},
		{"size string", mut("0.7.0", func(m map[string]any) { art(m, 0)["objectSize"] = "40000" }), prod, RuleManifestInvalid},
		{"size too large", mut("0.7.0", func(m map[string]any) { art(m, 0)["size"] = MaxArtifactSize + 1 }), prod, RuleManifestInvalid},
		{"hash upper case", mut("0.7.0", func(m map[string]any) { art(m, 0)["sha256"] = strings.Repeat("A", 64) }), prod, RuleManifestInvalid},
		{"encoding", mut("0.7.0", func(m map[string]any) { art(m, 0)["encoding"] = "zstd" }), prod, RuleManifestInvalid},
		{"name for platform", mut("0.7.0", func(m map[string]any) { art(m, 0)["name"] = "konect-linux-arm64" }), prod, RuleManifestInvalid},
		{"unknown platform", mut("0.7.0", func(m map[string]any) { art(m, 5)["platform"] = "freebsd-x64" }), prod, RuleManifestInvalid},
		{"embedded missing", mut("0.7.0", func(m map[string]any) { delete(m, "embedded") }), prod, RuleManifestInvalid},
		{"version mismatch", mustJSON(t, manifestMap("0.6.0")), prod, RuleVersionMismatch},
		{"tag mismatch", mut("0.7.0", func(m map[string]any) { m["tag"] = "v0.7.1" }), prod, RuleTagMismatch},
		{"channel unknown", mut("0.7.0", func(m map[string]any) { m["channel"] = "beta" }), prod, RuleChannelInvalid},
		{"rc labelled release", mut("0.7.0-rc.1", func(m map[string]any) { m["channel"] = "release" }), Expect{Version: "0.7.0-rc.1", Platform: "linux-x64"}, RuleChannelInvalid},
		{"release labelled prerelease", mut("0.7.0", func(m map[string]any) { m["channel"] = "prerelease" }), Expect{Version: "0.7.0", Platform: "linux-x64"}, RuleChannelInvalid},
		{"prerelease on prod", mustJSON(t, manifestMap("0.7.0-rc.1")), Expect{Version: "0.7.0-rc.1", Prod: true, Platform: "linux-x64"}, RuleChannelRefused},
		{"platform missing", mut("0.7.0", func(m map[string]any) {
			a := m["artifacts"].([]any)
			a[0] = a[1]
		}), prod, RulePlatformMissing},
		{"platform duplicate", mut("0.7.0", func(m map[string]any) {
			a := m["artifacts"].([]any)
			a[0] = a[1]
		}), Expect{Version: "0.7.0", Prod: true, Platform: "linux-arm64"}, RulePlatformDuplicate},
		{"platform not in contract", mustJSON(t, manifestMap("0.7.0")), Expect{Version: "0.7.0", Prod: true, Platform: "linux-riscv64"}, RulePlatformMissing},
	}
	for _, c := range cases {
		if got := RuleOf(run(c.m, c.exp)); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}

	ok := map[string]struct {
		m   []byte
		exp Expect
	}{
		"prerelease on preprod": {mustJSON(t, manifestMap("0.7.0-rc.1")), Expect{Version: "0.7.0-rc.1", Platform: "linux-arm64"}},
		"unknown field ignored": {mut("0.7.0", func(m map[string]any) { m["future"] = map[string]any{"x": 1} }), prod},
		// Exact key matching: "Version" is an unknown field, not a second "version".
		"case variant ignored": {mut("0.7.0", func(m map[string]any) { m["Version"] = "0.5.0" }), prod},
	}
	for name, c := range ok {
		if err := run(c.m, c.exp); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The verifier never puts file bytes into an error.
func TestErrorsNameTheRuleOnly(t *testing.T) {
	_, key := newKey(t, "konect-release-a")
	secret := "kit_acme.0123456789abcdef0123456789abcdef.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	_, err := Release([]byte(secret), []byte(fmt.Sprintf(`{"schema":%q}`, secret)), []Key{key}, Expect{Version: "0.7.0", Platform: "linux-x64"})
	if err == nil || strings.Contains(err.Error(), "kit_") {
		t.Fatalf("error leaks bytes or is nil: %v", err)
	}
}

func TestParseKey(t *testing.T) {
	priv, _ := newKey(t, "x")
	_ = priv
	if _, err := ParseKeyPEM("a", "garbage"); err == nil {
		t.Fatal("garbage parsed")
	}
	// P-384 is refused: the algorithm is fixed.
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	der, _ := marshalPKIX(&p384.PublicKey)
	if _, err := ParseKeyDER("a", der); err == nil {
		t.Fatal("P-384 accepted")
	}
	der, _ = marshalPKIX(&priv.PublicKey)
	k, err := ParseKeyDER("a", der)
	if err != nil || len(Fingerprint(k)) != 64 {
		t.Fatalf("P-256: %v", err)
	}
}

// The compiled keys parse (TrustedKeys panics otherwise), with unique ids.
func TestCompiledKeys(t *testing.T) {
	seen := map[string]bool{}
	for _, k := range TrustedKeys() {
		if seen[k.ID] {
			t.Fatalf("duplicate key id %s", k.ID)
		}
		seen[k.ID] = true
	}
}
