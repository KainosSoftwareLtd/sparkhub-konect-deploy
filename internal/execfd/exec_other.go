//go:build !(linux && (amd64 || arm64))

package execfd

import (
	"errors"
	"os"
)

// Supported reports whether this platform can run Konect from the bootstrap (Linux amd64/arm64 only).
const Supported = false

// Exec is not available here: on macOS the bootstrap only fetches and verifies (install.sh).
func Exec(*os.File, int64, string, []string, []string, func(string)) error {
	return errors.New("running Konect through the bootstrap needs Linux (amd64 or arm64); use `konect-bootstrap fetch --out <dir>`")
}
