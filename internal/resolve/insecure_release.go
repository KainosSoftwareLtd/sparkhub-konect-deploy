//go:build !e2e

package resolve

import "net/url"

// allowHTTP is false in every release build: resolve and downloads are HTTPS only.
func allowHTTP(*url.URL) bool { return false }

// E2EBaseURL is empty in a release build: the resolve host always comes from the token's org and KONECT_SPARKHUB.
func E2EBaseURL() string { return "" }
