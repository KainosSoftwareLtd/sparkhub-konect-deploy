// Command fakekonect stands in for the Konect release binary in the e2e test: it reports how it was started
// (argv, /proc/self/environ, /proc/self/exe, pid) to $FAKE_KONECT_REPORT and exits 0.
package main

import (
	"encoding/json"
	"os"
	"strings"
)

func main() {
	exe, _ := os.Readlink("/proc/self/exe")
	environ, _ := os.ReadFile("/proc/self/environ")
	report := map[string]any{
		"args":    os.Args,
		"environ": strings.Split(strings.TrimRight(string(environ), "\x00"), "\x00"),
		"exe":     exe,
		"pid":     os.Getpid(),
	}
	if p := os.Getenv("FAKE_KONECT_REPORT"); p != "" {
		b, _ := json.Marshal(report)
		_ = os.WriteFile(p, b, 0o600)
	}
	os.Exit(0)
}
