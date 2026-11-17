//go:build e2e

package bootstrap

import (
	"os"
	"strconv"
	"time"
)

// timeScale lets the e2e binary run the retry schedule faster (KONECT_BOOTSTRAP_E2E_TIME_SCALE=100: 15 min → 9 s).
func timeScale() time.Duration {
	if n, err := strconv.Atoi(os.Getenv("KONECT_BOOTSTRAP_E2E_TIME_SCALE")); err == nil && n > 0 {
		return time.Duration(n)
	}
	return 1
}
