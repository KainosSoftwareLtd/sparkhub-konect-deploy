package bootstrap

import (
	"runtime"
	"strings"

	"github.com/KainosSoftwareLtd/sparkhub-konect-deploy/internal/token"
	"github.com/KainosSoftwareLtd/sparkhub-konect-deploy/internal/verify"
)

// Environment variables (design #1702 §5.1).
const (
	EnvToken    = "KONECT_INSTALL_TOKEN"
	EnvVersion  = "KONECT_VERSION"
	EnvSparkHub = "KONECT_SPARKHUB"
	EnvCache    = "KONECT_BOOTSTRAP_CACHE"
	// Set for Konect after the handover.
	EnvBootstrap      = "KONECT_BOOTSTRAP"
	EnvArtifactSHA256 = "KONECT_ARTIFACT_SHA256"

	DefaultCache = "/data/.konect-bootstrap"
)

// Platform is this binary's platform in the manifest's naming ("" when the manifest has none for it).
func Platform() string { return platformOf(runtime.GOOS, runtime.GOARCH) }

func platformOf(goos, goarch string) string {
	return map[string]string{
		"linux/amd64":  "linux-x64",
		"linux/arm64":  "linux-arm64",
		"darwin/arm64": "macos-arm64",
		"darwin/amd64": "macos-x64",
	}[goos+"/"+goarch]
}

// KonectEnv is the environment handed to Konect: the original MINUS the install token, every KONECT_BOOTSTRAP_*
// variable and any value that holds an install token, PLUS KONECT_BOOTSTRAP and KONECT_ARTIFACT_SHA256 (review M2:
// after the exec, /proc/1/environ holds no token).
func KonectEnv(environ []string, bootstrapVersion, artifactSHA256 string) []string {
	out := make([]string, 0, len(environ)+2)
	for _, kv := range environ {
		k, v, _ := strings.Cut(kv, "=")
		switch {
		case k == EnvToken, k == EnvBootstrap, k == EnvArtifactSHA256, strings.HasPrefix(k, "KONECT_BOOTSTRAP_"):
			continue
		case strings.Contains(v, "kit_") && token.Pattern.MatchString(v):
			continue
		}
		out = append(out, kv)
	}
	return append(out, EnvBootstrap+"="+bootstrapVersion, EnvArtifactSHA256+"="+artifactSHA256)
}

// ValidVersion reports whether v is an exact version KONECT_VERSION may hold.
// It is the train's KONECT_VERSION_PATTERN: X.Y.Z or X.Y.Z-<pre-release> (preprod only), at most 64 characters; no
// "latest", no "v", nothing path-like.
func ValidVersion(v string) bool { return verify.ValidVersion(v) }
