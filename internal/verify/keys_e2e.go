//go:build e2e

package verify

import "os"

// E2EKeyID is the key id the e2e harness signs with. Only a binary built with `-tags e2e` trusts it, and only
// from the environment of the test that started it; the release workflow refuses a binary that lists it.
const E2EKeyID = "e2e-test-key-not-for-release"

func init() {
	if p := os.Getenv("KONECT_BOOTSTRAP_E2E_KEY_PEM"); p != "" {
		extraKeys = append(extraKeys, struct{ ID, PEM string }{E2EKeyID, p})
	}
}
