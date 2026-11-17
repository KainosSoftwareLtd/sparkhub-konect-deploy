//go:build !e2e

package bootstrap

import "time"

// timeScale is 1 in a release build: §5.2's timings are real.
func timeScale() time.Duration { return 1 }
