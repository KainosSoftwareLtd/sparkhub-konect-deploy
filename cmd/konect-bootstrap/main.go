// Command konect-bootstrap gets a signed Konect release from SparkHub, verifies it and runs it
// (design #1702 §5). It holds no Konect code.
//
//	konect-bootstrap [konect args…]            resolve, fetch or reuse, verify, exec Konect (default: serve --host 0.0.0.0 --insecure-http)
//	konect-bootstrap health [args]             exec the cached Konect's health check; no network
//	konect-bootstrap fetch --out <dir>         resolve, download, verify, write konect + manifest.json + manifest.sig.json
//	konect-bootstrap verify <dir>              verify a fetched directory offline
//	konect-bootstrap version [--json]          this bootstrap's version and the release keys it trusts
//
// Use `konect-bootstrap -- <args>` to pass arguments to Konect that start with one of the words above.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/KainosSoftwareLtd/sparkhub-konect-deploy/internal/bootstrap"
	"github.com/KainosSoftwareLtd/sparkhub-konect-deploy/internal/cache"
	"github.com/KainosSoftwareLtd/sparkhub-konect-deploy/internal/execfd"
	"github.com/KainosSoftwareLtd/sparkhub-konect-deploy/internal/resolve"
	"github.com/KainosSoftwareLtd/sparkhub-konect-deploy/internal/token"
	"github.com/KainosSoftwareLtd/sparkhub-konect-deploy/internal/verify"
)

// version is set at release build time: -ldflags "-X main.version=1.0.0".
var version = "0.0.0-dev"

var defaultKonectArgs = []string{"serve", "--host", "0.0.0.0", "--insecure-http"}

var log = slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("component", "konect-bootstrap")

func main() {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "health":
			os.Exit(cmdHealth(args[1:]))
		case "fetch":
			os.Exit(withSignals(func(ctx context.Context) int { return cmdFetch(ctx, args[1:]) }))
		case "verify":
			os.Exit(cmdVerify(args[1:]))
		case "version":
			os.Exit(cmdVersion(args[1:], os.Stdout))
		case "help", "--help", "-h":
			fmt.Fprint(os.Stdout, usage)
			os.Exit(0)
		case "--":
			args = args[1:]
		}
	}
	if len(args) == 0 {
		args = defaultKonectArgs
	}
	os.Exit(withSignals(func(ctx context.Context) int { return cmdRun(ctx, args) }))
}

const usage = `konect-bootstrap: get a signed Konect release from SparkHub, verify it and run it.

  konect-bootstrap [konect args…]      resolve, fetch or reuse, verify, exec Konect
                                       (default args: serve --host 0.0.0.0 --insecure-http)
  konect-bootstrap health [args]       run the cached Konect's health check (no network)
  konect-bootstrap fetch --out <dir> [--client install-script|build]
  konect-bootstrap verify <dir> [--binary <file>] [--version X.Y.Z] [--platform p] [--sparkhub prod|preprod]
  konect-bootstrap version [--json]

Environment: KONECT_INSTALL_TOKEN (required, never logged, removed before Konect starts), KONECT_VERSION (required,
exact), KONECT_SPARKHUB (prod | preprod, default prod), KONECT_BOOTSTRAP_CACHE (default /data/.konect-bootstrap),
HTTPS_PROXY / NO_PROXY, SSL_CERT_FILE / SSL_CERT_DIR.
`

// withSignals runs fn with a context that SIGTERM / SIGINT cancel; a cancelled run exits 143 / 130.
func withSignals(fn func(ctx context.Context) int) int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGTERM, syscall.SIGINT)
	var got atomic.Value
	go func() {
		if s, ok := <-sigc; ok {
			got.Store(s)
			cancel()
		}
	}()
	code := fn(ctx)
	if ctx.Err() != nil {
		if s, _ := got.Load().(os.Signal); s == syscall.SIGINT {
			return 130
		}
		return 143
	}
	return code
}

type config struct {
	tok      token.Token
	version  string
	env      string
	cacheDir string
	platform string
}

// readConfig checks the environment (§12: "env ok? no → exit 78"). needToken: the run and fetch commands.
func readConfig(needToken bool) (*config, *bootstrap.Exit) {
	c := &config{
		version:  os.Getenv(bootstrap.EnvVersion),
		env:      os.Getenv(bootstrap.EnvSparkHub),
		cacheDir: os.Getenv(bootstrap.EnvCache),
		platform: bootstrap.Platform(),
	}
	bad := func(format string, a ...any) *bootstrap.Exit {
		return &bootstrap.Exit{Code: bootstrap.ExitConfig, Event: "config", Err: fmt.Errorf(format, a...)}
	}
	if c.env == "" {
		c.env = "prod"
	}
	if c.cacheDir == "" {
		c.cacheDir = bootstrap.DefaultCache
	}
	if _, ok := resolve.Hosts[c.env]; !ok {
		return nil, bad("%s must be prod or preprod", bootstrap.EnvSparkHub)
	}
	if !bootstrap.ValidVersion(c.version) {
		return nil, bad("%s must be an exact Konect version such as 0.7.0 (no \"latest\")", bootstrap.EnvVersion)
	}
	if c.env == "prod" && strings.Contains(c.version, "-") {
		return nil, bad("%s=%s is a pre-release; pre-releases run against preprod only", bootstrap.EnvVersion, c.version)
	}
	if !filepath.IsAbs(c.cacheDir) {
		return nil, bad("%s must be an absolute path", bootstrap.EnvCache)
	}
	if c.platform == "" {
		return nil, bad("no Konect build for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	if needToken {
		raw := os.Getenv(bootstrap.EnvToken)
		_ = os.Unsetenv(bootstrap.EnvToken)
		if raw == "" {
			return nil, bad("%s is not set: create an install token in SparkHub → Konect → Install", bootstrap.EnvToken)
		}
		t, err := token.Parse(strings.TrimSpace(raw))
		if err != nil {
			return nil, bad("%v", err)
		}
		c.tok = t
	}
	return c, nil
}

func (c *config) options(client string, keys []verify.Key) (*bootstrap.Options, error) {
	rc, err := resolve.New(c.tok, c.env)
	if err != nil {
		return nil, err
	}
	if u := resolve.E2EBaseURL(); u != "" { // e2e build only; empty in a release build
		rc.BaseURL = strings.TrimRight(u, "/")
	}
	dl := resolve.NewHTTPClient(0)
	dl.CheckRedirect = nil // S3 never redirects a presigned GET, but a mirror in front of it may
	if tr, ok := dl.Transport.(*http.Transport); ok {
		tr.ResponseHeaderTimeout = 30 * time.Second
	}
	return &bootstrap.Options{
		Version: c.version, Prod: c.env == "prod", Platform: c.platform, Client: client, ClientVersion: version,
		TokenLabel: c.tok.Redacted(), Org: c.tok.Org, Keys: keys, Resolver: rc, Download: dl,
		Policy: bootstrap.DefaultPolicy(), Log: log,
	}, nil
}

func fatal(err error) int {
	var ex *bootstrap.Exit
	if errors.As(err, &ex) {
		log.Error(ex.Error(), "event", ex.Event, "exit", ex.Code)
		return ex.Code
	}
	log.Error(err.Error(), "event", "error", "exit", 1)
	return 1
}

func trustedKeys() ([]verify.Key, *bootstrap.Exit) {
	keys := verify.TrustedKeys()
	if len(keys) == 0 {
		return nil, &bootstrap.Exit{Code: bootstrap.ExitConfig, Event: "no_keys", Err: errors.New("this bootstrap build trusts no release key (a build defect); use a released image")}
	}
	return keys, nil
}

func cmdRun(ctx context.Context, konectArgs []string) int {
	if !execfd.Supported {
		return fatal(&bootstrap.Exit{Code: bootstrap.ExitConfig, Event: "platform", Err: errors.New("running Konect through the bootstrap needs Linux (amd64 or arm64); on a laptop use install.sh or `konect-bootstrap fetch`")})
	}
	cfg, ex := readConfig(true)
	if ex != nil {
		return fatal(ex)
	}
	keys, ex := trustedKeys()
	if ex != nil {
		return fatal(ex)
	}
	o, err := cfg.options("bootstrap", keys)
	if err != nil {
		return fatal(err)
	}
	c := cache.Cache{Dir: cfg.cacheDir}
	log.Info("starting", "event", "start", "bootstrap", version, "version", cfg.version, "platform", cfg.platform, "sparkhub", cfg.env, "token", cfg.tok.Redacted())
	entry, err := bootstrap.Start(ctx, o, c)
	if err != nil {
		if ctx.Err() != nil {
			log.Warn("stopped by a signal", "event", "signal")
			return 0 // withSignals turns this into 143 / 130
		}
		return fatal(err)
	}
	if deleted, err := c.Prune(cfg.version); err != nil {
		log.Warn("could not prune the cache", "event", "prune", "error", err.Error())
	} else if len(deleted) > 0 {
		log.Info("pruned old versions from the cache", "event", "prune", "deleted", deleted)
	}
	return execKonect(entry, append([]string{"konect"}, konectArgs...), cfg.version, "starting Konect")
}

func execKonect(entry *cache.Entry, argv []string, ver, msg string) int {
	env := bootstrap.KonectEnv(os.Environ(), version, entry.SHA256())
	log.Info(msg, "event", "exec", "version", ver, "sha256", entry.SHA256(), "keyId", entry.Result.KeyID)
	signal.Reset(syscall.SIGTERM, syscall.SIGINT)
	err := execfd.Exec(entry.File, entry.Result.Artifact.Size, entry.SHA256(), argv, env, func(path string) {
		log.Info("exec", "event", "exec_path", "path", path)
	})
	return fatal(&bootstrap.Exit{Code: bootstrap.ExitRetry, Event: "exec_failed", Err: err})
}

func cmdHealth(args []string) int {
	log = slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})).With("component", "konect-bootstrap")
	if !execfd.Supported {
		return fatal(errors.New("health needs Linux"))
	}
	cfg, ex := readConfig(false)
	if ex != nil {
		return fatal(ex)
	}
	c := cache.Cache{Dir: cfg.cacheDir}
	entry, err := c.Load(cfg.version, verify.TrustedKeys(), verify.Expect{Version: cfg.version, Prod: cfg.env == "prod", Platform: cfg.platform})
	if err != nil {
		return fatal(&bootstrap.Exit{Code: 1, Event: "health_cache", Err: err})
	}
	if entry == nil {
		fmt.Fprintf(os.Stderr, "konect-bootstrap health: Konect %s is not cached yet (still starting?)\n", cfg.version)
		return 1
	}
	if len(args) == 0 {
		args = []string{"--url", "http://127.0.0.1:41800", "--allow-locked"}
	}
	return execKonect(entry, append([]string{"konect", "health"}, args...), cfg.version, "health")
}

func cmdFetch(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("fetch", flag.ContinueOnError)
	out := fs.String("out", "", "directory to write konect, manifest.json and manifest.sig.json into")
	client := fs.String("client", "install-script", "install-script | build")
	if err := fs.Parse(args); err != nil || *out == "" || fs.NArg() != 0 {
		fmt.Fprint(os.Stderr, "usage: konect-bootstrap fetch --out <dir> [--client install-script|build]\n")
		return bootstrap.ExitConfig
	}
	if *client != "install-script" && *client != "build" {
		return fatal(&bootstrap.Exit{Code: bootstrap.ExitConfig, Event: "config", Err: errors.New("--client must be install-script or build")})
	}
	cfg, ex := readConfig(true)
	if ex != nil {
		return fatal(ex)
	}
	keys, ex := trustedKeys()
	if ex != nil {
		return fatal(ex)
	}
	o, err := cfg.options(*client, keys)
	if err != nil {
		return fatal(err)
	}
	entry, err := bootstrap.Fetch(ctx, o, *out)
	if err != nil {
		if ctx.Err() != nil {
			return 0
		}
		return fatal(err)
	}
	entry.File.Close()
	log.Info("fetched and verified", "event", "fetched", "version", cfg.version, "platform", cfg.platform, "sha256", entry.SHA256(), "keyId", entry.Result.KeyID, "out", *out)
	return 0
}

func cmdVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	binary := fs.String("binary", "", "the binary (default <dir>/konect)")
	ver := fs.String("version", os.Getenv(bootstrap.EnvVersion), "the expected Konect version (default $KONECT_VERSION)")
	platform := fs.String("platform", bootstrap.Platform(), "linux-x64 | linux-arm64 | macos-arm64 | macos-x64")
	env := fs.String("sparkhub", envOr(bootstrap.EnvSparkHub, "prod"), "prod | preprod (prod refuses a pre-release)")
	var dir string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		dir, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil || fs.NArg() > 1 || (dir == "" && fs.NArg() == 0) {
		fmt.Fprint(os.Stderr, "usage: konect-bootstrap verify <dir> [--binary <file>] [--version X.Y.Z] [--platform p] [--sparkhub prod|preprod]\n")
		return bootstrap.ExitConfig
	}
	if dir == "" {
		dir = fs.Arg(0)
	}
	if *binary == "" {
		*binary = filepath.Join(dir, cache.BinaryName)
	}
	if !bootstrap.ValidVersion(*ver) {
		return fatal(&bootstrap.Exit{Code: bootstrap.ExitConfig, Event: "config", Err: errors.New("pass --version X.Y.Z (or set KONECT_VERSION)")})
	}
	keys, ex := trustedKeys()
	if ex != nil {
		return fatal(ex)
	}
	e, mm, err := cache.LoadDir(dir, *binary, keys, verify.Expect{Version: *ver, Prod: *env == "prod", Platform: *platform})
	if mm != nil {
		fmt.Fprintf(os.Stderr, "FAILED: %s (%s)\n", *binary, strings.TrimSuffix(strings.TrimPrefix(mm.Error(), "cached "), "; deleted"))
		return 1
	}
	if err != nil {
		return fatal(err)
	}
	e.File.Close()
	fmt.Printf("OK: %s is Konect %s for %s, sha256 %s, signed by %s\n", *binary, *ver, *platform, e.SHA256(), e.Result.KeyID)
	return 0
}

func cmdVersion(args []string, w io.Writer) int {
	asJSON := len(args) == 1 && args[0] == "--json"
	type keyInfo struct {
		KeyID       string `json:"keyId"`
		Fingerprint string `json:"fingerprint"`
	}
	var keys []keyInfo
	for _, k := range verify.TrustedKeys() {
		keys = append(keys, keyInfo{k.ID, "sha256:" + verify.Fingerprint(k)})
	}
	if asJSON {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": "konect-bootstrap", "version": version, "go": runtime.Version(), "platform": runtime.GOOS + "/" + runtime.GOARCH, "keys": keys,
		})
		return 0
	}
	fmt.Fprintf(w, "konect-bootstrap %s (%s, %s/%s)\n", version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
	if len(keys) == 0 {
		fmt.Fprintln(w, "trusted release keys: NONE (this build cannot verify a release)")
	} else {
		fmt.Fprintln(w, "trusted release keys:")
		for _, k := range keys {
			fmt.Fprintf(w, "  %s  %s\n", k.KeyID, k.Fingerprint)
		}
	}
	return 0
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
