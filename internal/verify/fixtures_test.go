package verify

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// fixtureIndex is fixtures/index.json: the shared vectors from the train (T/releases.ts). Both verifiers must give
// the same answer for every vector: "ok", or the rule name of the refusal.
type fixtureIndex struct {
	Schema string `json:"schema"` // konect-release-fixtures/1
	Keys   []struct {
		KeyID        string `json:"keyId"`
		PublicKeyPEM string `json:"publicKeyPem"`
	} `json:"keys"`
	Vectors []struct {
		Name      string `json:"name"`
		Manifest  string `json:"manifest"`  // file, relative to fixtures/
		Signature string `json:"signature"` // file, relative to fixtures/
		Expect    struct {
			Version  string `json:"version"`
			Prod     bool   `json:"prod"`
			Platform string `json:"platform"`
		} `json:"expect"`
		Result string `json:"result"` // "ok" or a rule name
	} `json:"vectors"`
	// Release: a committed signed manifest from a real release, checked against the COMPILED keys (keys.go), so a
	// compiled key that does not match KMS fails CI (design §3.7).
	Release *struct {
		Manifest  string `json:"manifest"`
		Signature string `json:"signature"`
		Version   string `json:"version"`
	} `json:"release,omitempty"`
}

func TestFixtures(t *testing.T) {
	dir := filepath.Join("..", "..", "fixtures")
	raw, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if os.IsNotExist(err) {
		t.Skip("fixtures/index.json not present yet (train track)")
	}
	if err != nil {
		t.Fatal(err)
	}
	var idx fixtureIndex
	if err := json.Unmarshal(raw, &idx); err != nil {
		t.Fatal(err)
	}
	if idx.Schema != "konect-release-fixtures/1" || len(idx.Vectors) == 0 {
		t.Fatalf("unexpected fixture index: schema %q, %d vectors", idx.Schema, len(idx.Vectors))
	}
	var keys []Key
	for _, k := range idx.Keys {
		key, err := ParseKeyPEM(k.KeyID, k.PublicKeyPEM)
		if err != nil {
			t.Fatalf("fixture key %s: %v", k.KeyID, err)
		}
		keys = append(keys, key)
	}
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for _, v := range idx.Vectors {
		_, err := Release(read(v.Manifest), read(v.Signature), keys, Expect{Version: v.Expect.Version, Prod: v.Expect.Prod, Platform: v.Expect.Platform})
		got := "ok"
		if err != nil {
			got = RuleOf(err)
			if got == "" {
				got = err.Error()
			}
		}
		if got != v.Result {
			t.Errorf("vector %q: got %q, want %q", v.Name, got, v.Result)
		}
	}
	if idx.Release != nil {
		for _, p := range []string{"linux-x64", "linux-arm64", "macos-arm64", "macos-x64"} {
			if _, err := Release(read(idx.Release.Manifest), read(idx.Release.Signature), TrustedKeys(), Expect{Version: idx.Release.Version, Platform: p}); err != nil {
				t.Errorf("real release %s does not verify against the compiled keys (%s): %v", idx.Release.Version, p, err)
			}
		}
	}
}
