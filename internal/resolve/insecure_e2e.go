//go:build e2e

package resolve

import (
	"net/url"
	"os"
)

// allowHTTP lets the e2e binary talk plain HTTP to a stub on the loopback interface only.
func allowHTTP(u *url.URL) bool {
	h := u.Hostname()
	return u.Scheme == "http" && (h == "127.0.0.1" || h == "localhost" || h == "::1")
}

// E2EBaseURL points the e2e binary at the stub resolve server.
func E2EBaseURL() string { return os.Getenv("KONECT_BOOTSTRAP_E2E_BASE_URL") }
