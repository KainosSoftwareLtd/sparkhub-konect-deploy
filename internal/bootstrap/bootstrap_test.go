package bootstrap

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KainosSoftwareLtd/sparkhub-konect-deploy/internal/cache"
	"github.com/KainosSoftwareLtd/sparkhub-konect-deploy/internal/resolve"
	"github.com/KainosSoftwareLtd/sparkhub-konect-deploy/internal/testrelease"
	"github.com/KainosSoftwareLtd/sparkhub-konect-deploy/internal/verify"
)

// harness: a fake resolver (scripted) and a TLS object server.
type harness struct {
	t        *testing.T
	signer   *testrelease.Signer
	rel      *testrelease.Release
	srv      *httptest.Server
	mu       sync.Mutex
	requests []resolve.Request
	answers  []func(req resolve.Request) (*resolve.Answer, error) // consumed in order; then default
	objHits  int
	objFn    func(hit int, w http.ResponseWriter) bool // true: handled
	clock    time.Time
	waits    []time.Duration
	logs     bytes.Buffer
	cacheDir string
}

const testVersion = "0.7.0"

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, signer: testrelease.NewSigner("konect-release-a"), clock: time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)}
	bin := make([]byte, 4096)
	_, _ = rand.Read(bin)
	h.rel = testrelease.Build(testVersion, "linux-x64", bin, h.signer)
	h.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.objHits++
		hit, fn := h.objHits, h.objFn
		h.mu.Unlock()
		if fn != nil && fn(hit, w) {
			return
		}
		_, _ = w.Write(h.rel.Object)
	}))
	t.Cleanup(h.srv.Close)
	h.cacheDir = filepath.Join(t.TempDir(), "cache")
	return h
}

func (h *harness) Resolve(ctx context.Context, req resolve.Request) (*resolve.Answer, error) {
	h.mu.Lock()
	h.requests = append(h.requests, req)
	var fn func(resolve.Request) (*resolve.Answer, error)
	if len(h.answers) > 0 {
		fn, h.answers = h.answers[0], h.answers[1:]
	}
	h.mu.Unlock()
	if fn != nil {
		return fn(req)
	}
	return h.ok(req)
}

func (h *harness) ok(req resolve.Request) (*resolve.Answer, error) {
	a := &resolve.Answer{Version: testVersion, Manifest: h.rel.Manifest, Signature: h.rel.Signature}
	if req.CachedSHA256 != h.rel.SHA256 {
		a.Artifact = &resolve.Artifact{Name: "konect-linux-x64", URL: h.srv.URL + "/konect-releases/v0.7.0/konect-linux-x64.gz?X-Amz-Signature=SECRETSIG"}
	}
	return a, nil
}

func (h *harness) opts() *Options {
	return &Options{
		Version: testVersion, Prod: true, Platform: "linux-x64", Client: "bootstrap", ClientVersion: "1.0.0-test",
		TokenLabel: "kit_acme.01234567…", Org: "acme",
		Keys: []verify.Key{h.signer.Key()}, Resolver: h, Download: h.srv.Client(),
		Policy: Policy{
			Delays: []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second}, Every: time.Minute, Budget: 15 * time.Minute, Scale: 1,
			Now: func() time.Time { h.mu.Lock(); defer h.mu.Unlock(); return h.clock },
			Sleep: func(_ context.Context, d time.Duration) error {
				h.mu.Lock()
				defer h.mu.Unlock()
				h.waits = append(h.waits, d)
				h.clock = h.clock.Add(d)
				return nil
			},
		},
		Log: slog.New(slog.NewJSONHandler(&h.logs, nil)),
	}
}

func (h *harness) start() (*cache.Entry, error) {
	e, err := Start(context.Background(), h.opts(), cache.Cache{Dir: h.cacheDir})
	if e != nil {
		h.t.Cleanup(func() { e.File.Close() })
	}
	return e, err
}

func exitCode(err error) int {
	var e *Exit
	if errors.As(err, &e) {
		return e.Code
	}
	return -1
}

func refusal(status int, code string) func(resolve.Request) (*resolve.Answer, error) {
	return func(resolve.Request) (*resolve.Answer, error) { return nil, &resolve.Refusal{Status: status, Code: code} }
}

func transient(status int) func(resolve.Request) (*resolve.Answer, error) {
	return func(resolve.Request) (*resolve.Answer, error) {
		return nil, &resolve.Transient{Status: status, Err: errors.New("x")}
	}
}

func TestFreshStartThenCache(t *testing.T) {
	h := newHarness(t)
	e, err := h.start()
	if err != nil {
		t.Fatal(err)
	}
	if e.SHA256() != h.rel.SHA256 || e.Path != filepath.Join(h.cacheDir, testVersion, "konect") {
		t.Fatalf("entry %+v", e)
	}
	fi, _ := os.Stat(e.Path)
	if fi.Mode().Perm() != 0o500 {
		t.Fatalf("binary mode %v", fi.Mode())
	}
	if h.requests[0].CachedSHA256 != "" || h.requests[0].Client != "bootstrap" || h.requests[0].Platform != "linux-x64" {
		t.Fatalf("first request %+v", h.requests[0])
	}
	// Second start: from the cache, with cachedSha256; no download.
	if _, err := h.start(); err != nil {
		t.Fatal(err)
	}
	if h.requests[1].CachedSHA256 != h.rel.SHA256 || h.objHits != 1 {
		t.Fatalf("second request %+v, object hits %d", h.requests[1], h.objHits)
	}
	if strings.Contains(h.logs.String(), "SECRETSIG") {
		t.Fatal("the presigned URL reached the log")
	}
}

func TestCachedBranches(t *testing.T) {
	cases := []struct {
		name   string
		answer func(resolve.Request) (*resolve.Answer, error)
		code   int // 0: starts
		event  string
	}{
		{"offline 503", transient(503), 0, "offline_start"},
		{"rate limited", func(resolve.Request) (*resolve.Answer, error) {
			return nil, &resolve.Transient{Status: 429, RateLimited: true, RetryAfter: time.Hour}
		}, 0, "offline_start"},
		{"network", func(resolve.Request) (*resolve.Answer, error) { return nil, errors.New("dial tcp: refused") }, 0, "offline_start"},
		{"withdrawn", refusal(410, "version_withdrawn"), 0, "version_withdrawn"},
		{"revoked", refusal(401, "token_revoked"), ExitConfig, ""},
		{"invalid", refusal(401, "invalid_token"), ExitConfig, ""},
		{"not entitled", refusal(403, "not_entitled"), ExitConfig, ""},
		{"unknown version", refusal(404, "version_unknown"), ExitConfig, ""},
		{"too old", refusal(426, "bootstrap_too_old"), ExitConfig, ""},
		{"manifest changed", func(req resolve.Request) (*resolve.Answer, error) {
			return &resolve.Answer{Manifest: append([]byte(" "), []byte("{}")...), Signature: []byte("{}")}, nil
		}, ExitConfig, "manifest_changed"},
		{"deprecated", nil, 0, "warning"}, // answer built below (needs the harness)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			if _, err := h.start(); err != nil {
				t.Fatal(err)
			}
			ans := c.answer
			if ans == nil {
				ans = func(req resolve.Request) (*resolve.Answer, error) {
					a, _ := h.ok(req)
					a.Warning = &resolve.Warning{Code: "bootstrap_deprecated", Minimum: "1.1.0"}
					return a, nil
				}
			}
			h.answers = append(h.answers, ans)
			e, err := h.start()
			if c.code == 0 {
				if err != nil || e == nil {
					t.Fatalf("expected a start, got %v", err)
				}
				if c.event != "" && !strings.Contains(h.logs.String(), `"event":"`+c.event+`"`) {
					t.Fatalf("no %s event in %s", c.event, h.logs.String())
				}
				return
			}
			if got := exitCode(err); got != c.code {
				t.Fatalf("exit %d (%v), want %d", got, err, c.code)
			}
			if c.event != "" && !strings.Contains(err.(*Exit).Event, c.event) {
				t.Fatalf("event %s", err.(*Exit).Event)
			}
		})
	}
}

func TestNoCacheRefusals(t *testing.T) {
	for _, c := range []struct {
		status int
		code   string
	}{{401, "invalid_token"}, {401, "token_revoked"}, {403, "not_entitled"}, {404, "version_unknown"}, {400, "platform_unknown"}, {410, "version_withdrawn"}, {426, "bootstrap_too_old"}} {
		h := newHarness(t)
		h.answers = append(h.answers, refusal(c.status, c.code))
		_, err := h.start()
		if exitCode(err) != ExitConfig {
			t.Fatalf("%s: exit %d (%v)", c.code, exitCode(err), err)
		}
		if strings.Contains(err.Error(), "kit_acme.0123456789") {
			t.Fatal("message holds more than the redacted token")
		}
	}
}

func TestRetrySchedule(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 4; i++ {
		h.answers = append(h.answers, transient(503))
	}
	if _, err := h.start(); err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, time.Minute}
	if len(h.waits) != len(want) {
		t.Fatalf("waits %v", h.waits)
	}
	for i := range want {
		if h.waits[i] != want[i] {
			t.Fatalf("waits %v, want %v", h.waits, want)
		}
	}
}

func TestRetryBudgetGivesUp(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 100; i++ {
		h.answers = append(h.answers, transient(502))
	}
	_, err := h.start()
	if exitCode(err) != ExitRetry || err.(*Exit).Event != "gave_up" {
		t.Fatalf("got %v", err)
	}
	var total time.Duration
	for _, w := range h.waits {
		total += w
	}
	// 5+10+20 s then every 60 s, never past 15 minutes.
	if total > 15*time.Minute || total < 14*time.Minute {
		t.Fatalf("waited %v in total (%v)", total, h.waits)
	}
}

func TestRateLimited(t *testing.T) {
	h := newHarness(t)
	h.answers = append(h.answers, func(resolve.Request) (*resolve.Answer, error) {
		return nil, &resolve.Transient{Status: 429, RateLimited: true, RetryAfter: 90 * time.Second}
	})
	if _, err := h.start(); err != nil {
		t.Fatal(err)
	}
	if len(h.waits) != 1 || h.waits[0] != 90*time.Second {
		t.Fatalf("waits %v", h.waits)
	}

	h = newHarness(t)
	h.answers = append(h.answers, func(resolve.Request) (*resolve.Answer, error) {
		return nil, &resolve.Transient{Status: 429, RateLimited: true, RetryAfter: 40 * time.Minute}
	})
	_, err := h.start()
	if exitCode(err) != ExitRetry || !strings.Contains(err.Error(), "rate limited until 2026-11-17T09:40:00Z (token kit_acme.01234567…)") {
		t.Fatalf("got %v", err)
	}
	if len(h.waits) != 0 {
		t.Fatalf("waited %v past the budget", h.waits)
	}
}

func TestExpiredURLResolvesAgain(t *testing.T) {
	h := newHarness(t)
	h.objFn = func(hit int, w http.ResponseWriter) bool {
		if hit == 1 {
			w.WriteHeader(http.StatusForbidden)
			return true
		}
		return false
	}
	if _, err := h.start(); err != nil {
		t.Fatal(err)
	}
	if len(h.requests) != 2 || len(h.waits) != 0 {
		t.Fatalf("%d resolves, waits %v", len(h.requests), h.waits)
	}
}

func TestTamperedObjectNeverRuns(t *testing.T) {
	h := newHarness(t)
	h.objFn = func(hit int, w http.ResponseWriter) bool {
		bad := append([]byte(nil), h.rel.Object...)
		bad[len(bad)/2] ^= 0xff
		_, _ = w.Write(bad)
		return true
	}
	_, err := h.start()
	if exitCode(err) != ExitRetry {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(h.logs.String(), `"event":"download_mismatch"`) {
		t.Fatal("no download_mismatch logged")
	}
	if _, err := os.Stat(filepath.Join(h.cacheDir, testVersion)); !os.IsNotExist(err) {
		t.Fatal("bad bytes reached the cache")
	}
}

func TestOversizedObjectIsCut(t *testing.T) {
	h := newHarness(t)
	h.objFn = func(hit int, w http.ResponseWriter) bool {
		_, _ = w.Write(append(append([]byte(nil), h.rel.Object...), make([]byte, 1<<20)...))
		return true
	}
	if _, err := h.start(); exitCode(err) != ExitRetry {
		t.Fatalf("got %v", err)
	}
}

func TestServerSignatureFailureIsDefinitive(t *testing.T) {
	h := newHarness(t)
	other := testrelease.NewSigner("konect-release-a") // same id, wrong key
	h.answers = append(h.answers, func(req resolve.Request) (*resolve.Answer, error) {
		a, _ := h.ok(req)
		a.Signature = testrelease.SignatureJSON(a.Manifest, other)
		return a, nil
	})
	_, err := h.start()
	if exitCode(err) != ExitConfig || !strings.Contains(err.Error(), "sig_none_valid") || h.objHits != 0 {
		t.Fatalf("got %v (object hits %d)", err, h.objHits)
	}
}

func TestPrereleaseRefusedOnProd(t *testing.T) {
	h := newHarness(t)
	h.rel = testrelease.Build("0.7.0-rc.1", "linux-x64", h.rel.Binary, h.signer)
	o := h.opts()
	o.Version = "0.7.0-rc.1"
	_, err := Start(context.Background(), o, cache.Cache{Dir: h.cacheDir})
	if exitCode(err) != ExitConfig || !strings.Contains(err.Error(), "channel_refused") {
		t.Fatalf("got %v", err)
	}
	o.Prod = false
	e, err := Start(context.Background(), o, cache.Cache{Dir: h.cacheDir})
	if err != nil {
		t.Fatal(err)
	}
	e.File.Close()
}

func TestTamperedCacheIsReplaced(t *testing.T) {
	h := newHarness(t)
	e, err := h.start()
	if err != nil {
		t.Fatal(err)
	}
	e.File.Close()
	_ = os.Chmod(e.Path, 0o600)
	if err := os.WriteFile(e.Path, []byte("evil"), 0o600); err != nil {
		t.Fatal(err)
	}
	e2, err := h.start()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.logs.String(), `"event":"cache_mismatch"`) || h.objHits != 2 {
		t.Fatalf("hits %d, logs %s", h.objHits, h.logs.String())
	}
	b, _ := io.ReadAll(io.NewSectionReader(e2.File, 0, 1<<20))
	if !bytes.Equal(b, h.rel.Binary) {
		t.Fatal("the replaced binary is not the release")
	}
}

func TestCacheSignedByUntrustedKeyIsReplaced(t *testing.T) {
	h := newHarness(t)
	e, err := h.start()
	if err != nil {
		t.Fatal(err)
	}
	e.File.Close()
	// A newer bootstrap that no longer trusts the old key: the entry is deleted and downloaded again.
	old := h.signer
	h.signer = testrelease.NewSigner("konect-release-b")
	h.rel = testrelease.Build(testVersion, "linux-x64", h.rel.Binary, old, h.signer)
	if _, err := h.start(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.logs.String(), `"reason":"sig_none_valid"`) {
		t.Fatalf("logs %s", h.logs.String())
	}
}

func TestCommitRaceUsesTheirs(t *testing.T) {
	h := newHarness(t)
	e, err := h.start()
	if err != nil {
		t.Fatal(err)
	}
	e.File.Close()
	// Another replica's download arrives after ours is in place.
	c := cache.Cache{Dir: h.cacheDir}
	tmp, _ := c.NewTemp()
	if err := os.WriteFile(filepath.Join(tmp, "konect"), h.rel.Binary, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := cache.WriteRelease(tmp, h.rel.Manifest, h.rel.Signature); err != nil {
		t.Fatal(err)
	}
	got, err := c.Commit(tmp, testVersion, []verify.Key{h.signer.Key()}, verify.Expect{Version: testVersion, Prod: true, Platform: "linux-x64"})
	if err != nil {
		t.Fatal(err)
	}
	got.File.Close()
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatal("the losing temp dir was left behind")
	}
}

func TestPruneKeepsCurrentAndPrevious(t *testing.T) {
	dir := t.TempDir()
	c := cache.Cache{Dir: dir}
	mk := func(vs ...string) {
		for _, v := range vs {
			_ = os.MkdirAll(filepath.Join(dir, v), 0o700)
		}
	}
	exists := func(v string) bool { _, err := os.Stat(filepath.Join(dir, v)); return err == nil }
	mk("0.5.0", "0.6.0")
	if _, err := c.Prune("0.6.0"); err != nil {
		t.Fatal(err)
	}
	if exists("0.5.0") || !exists("0.6.0") {
		t.Fatal("first prune")
	}
	mk("0.7.0", "0.4.0")
	if _, err := c.Prune("0.7.0"); err != nil {
		t.Fatal(err)
	}
	if !exists("0.7.0") || !exists("0.6.0") || exists("0.4.0") {
		t.Fatal("second prune must keep 0.7.0 and the previous 0.6.0 only")
	}
	// Rolling back to the previous version keeps both.
	if _, err := c.Prune("0.6.0"); err != nil {
		t.Fatal(err)
	}
	if !exists("0.7.0") || !exists("0.6.0") {
		t.Fatal("rollback prune")
	}
}

func TestFetch(t *testing.T) {
	h := newHarness(t)
	out := filepath.Join(t.TempDir(), "bin")
	o := h.opts()
	o.Client = "install-script"
	e, err := Fetch(context.Background(), o, out)
	if err != nil {
		t.Fatal(err)
	}
	e.File.Close()
	for name, mode := range map[string]os.FileMode{"konect": 0o755, "manifest.json": 0o644, "manifest.sig.json": 0o644} {
		fi, err := os.Stat(filepath.Join(out, name))
		if err != nil || fi.Mode().Perm() != mode {
			t.Fatalf("%s: %v %v", name, err, fi)
		}
	}
	entries, _ := os.ReadDir(out)
	if len(entries) != 3 {
		t.Fatalf("out holds %d entries", len(entries))
	}
	if h.requests[0].Client != "install-script" {
		t.Fatal("client not passed")
	}
	// Fetch again over an existing install.
	e, err = Fetch(context.Background(), o, out)
	if err != nil {
		t.Fatal(err)
	}
	e.File.Close()
}

func TestKonectEnv(t *testing.T) {
	tok := "kit_acme.0123456789abcdef0123456789abcdef.abcdefghijklmnopqrstuvwxyzABCDEFGHIJ-_01234"
	in := []string{
		"PATH=/usr/bin", "KONECT_INSTALL_TOKEN=" + tok, "KONECT_BOOTSTRAP_CACHE=/data/x", "KONECT_BOOTSTRAP_E2E_BASE_URL=x",
		"KONECT_VERSION=0.7.0", "KONECT_SPARKHUB=preprod", "COPY_OF_TOKEN=Bearer " + tok, "KONECT_BOOTSTRAP=fake", "KONECT_CONFIG=builtin:sqlite",
	}
	out := KonectEnv(in, "1.0.0", strings.Repeat("a", 64))
	joined := strings.Join(out, "\n")
	for _, bad := range []string{"kit_", "KONECT_INSTALL_TOKEN", "KONECT_BOOTSTRAP_", "KONECT_BOOTSTRAP=fake"} {
		if strings.Contains(joined, bad) {
			t.Fatalf("%q left in %v", bad, out)
		}
	}
	for _, good := range []string{"PATH=/usr/bin", "KONECT_VERSION=0.7.0", "KONECT_SPARKHUB=preprod", "KONECT_CONFIG=builtin:sqlite", "KONECT_BOOTSTRAP=1.0.0", "KONECT_ARTIFACT_SHA256=" + strings.Repeat("a", 64)} {
		if !strings.Contains(joined, good) {
			t.Fatalf("%q missing from %v", good, out)
		}
	}
}

func TestPlatformOf(t *testing.T) {
	for in, want := range map[string]string{"linux/amd64": "linux-x64", "linux/arm64": "linux-arm64", "darwin/arm64": "macos-arm64", "darwin/amd64": "macos-x64", "windows/amd64": ""} {
		goos, goarch, _ := strings.Cut(in, "/")
		if got := platformOf(goos, goarch); got != want {
			t.Fatalf("%s: %q", in, got)
		}
	}
}

func TestVersionPattern(t *testing.T) {
	for _, v := range []string{"0.7.0", "1.10.3", "0.7.0-rc.1"} {
		if !versionRe.MatchString(v) {
			t.Fatalf("%s refused", v)
		}
	}
	for _, v := range []string{"latest", "v0.7.0", "0.7", "0.7.0/../../x", "../0.7.0", "0.7.0-", "07.0.0", ""} {
		if versionRe.MatchString(v) {
			t.Fatalf("%s accepted", v)
		}
	}
}
