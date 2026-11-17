//go:build e2e

// Package e2e runs the real bootstrap binary (built with -tags e2e: it trusts a key from its environment and may
// call a plain-HTTP stub on the loopback interface) against a stub resolve endpoint and object server, with a fake
// Konect that reports how it was started. Linux only (the bootstrap execs Konect on Linux only).
package e2e

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/KainosSoftwareLtd/sparkhub-konect-deploy/internal/testrelease"
)

const (
	tok     = "kit_e2e.0123456789abcdef0123456789abcdef.abcdefghijklmnopqrstuvwxyzABCDEFGHIJ-_01234"
	secret  = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJ-_01234"
	version = "0.7.0"
)

var bootstrapBin, fakeBin, fakeBytes = "", "", []byte(nil)

func platform() string {
	return map[string]string{"amd64": "linux-x64", "arm64": "linux-arm64"}[runtime.GOARCH]
}

func TestMain(m *testing.M) {
	if runtime.GOOS != "linux" {
		fmt.Println("e2e: Linux only")
		os.Exit(0)
	}
	dir, err := os.MkdirTemp("", "konect-bootstrap-e2e")
	if err != nil {
		panic(err)
	}
	bootstrapBin, fakeBin = filepath.Join(dir, "konect-bootstrap"), filepath.Join(dir, "fakekonect")
	for _, b := range [][]string{
		{"build", "-tags", "e2e", "-trimpath", "-ldflags", "-X main.version=1.0.0-e2e", "-o", bootstrapBin, "../cmd/konect-bootstrap"},
		{"build", "-trimpath", "-o", fakeBin, "./fakekonect"},
	} {
		cmd := exec.Command("go", b...)
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		if err := cmd.Run(); err != nil {
			panic(err)
		}
	}
	if fakeBytes, err = os.ReadFile(fakeBin); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// stub is SparkHub's resolve endpoint and the bucket.
type stub struct {
	t        *testing.T
	srv      *httptest.Server
	signer   *testrelease.Signer
	rel      *testrelease.Release
	mu       sync.Mutex
	resolves []map[string]any
	objects  int
	// onResolve may answer instead of the default (return false to fall through).
	onResolve func(n int, req map[string]any, w http.ResponseWriter) bool
	onObject  func(n int, w http.ResponseWriter, r *http.Request) bool
}

func newStub(t *testing.T) *stub {
	s := &stub{t: t, signer: testrelease.NewSigner("e2e-test-key-not-for-release")}
	s.rel = testrelease.Build(version, platform(), fakeBytes, s.signer)
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *stub) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/konect/install/v1/resolve":
		if r.Header.Get("Authorization") != "Bearer "+tok {
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"error":"invalid_token"}`)
			return
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		s.mu.Lock()
		s.resolves = append(s.resolves, req)
		n, fn := len(s.resolves), s.onResolve
		s.mu.Unlock()
		if fn != nil && fn(n, req, w) {
			return
		}
		s.answer(w, req, s.rel)
	case r.Method == http.MethodGet && r.URL.Path == "/objects/konect.gz":
		if r.Header.Get("Authorization") != "" {
			s.t.Error("the download carried an Authorization header")
		}
		s.mu.Lock()
		s.objects++
		n, fn := s.objects, s.onObject
		s.mu.Unlock()
		if fn != nil && fn(n, w, r) {
			return
		}
		_, _ = w.Write(s.rel.Object)
	default:
		w.WriteHeader(404)
	}
}

func (s *stub) answer(w http.ResponseWriter, req map[string]any, rel *testrelease.Release) {
	a := map[string]any{
		"version":    rel.Version,
		"manifest":   base64.StdEncoding.EncodeToString(rel.Manifest),
		"signatures": base64.StdEncoding.EncodeToString(rel.Signature),
		"artifact":   nil,
	}
	if req["cachedSha256"] != rel.SHA256 {
		a["artifact"] = map[string]any{"name": "konect", "url": s.srv.URL + "/objects/konect.gz?X-Amz-Signature=SECRETSIG", "expiresAt": time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(a)
}

func (s *stub) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.resolves), s.objects
}

type report struct {
	Args    []string `json:"args"`
	Environ []string `json:"environ"`
	Exe     string   `json:"exe"`
	Pid     int      `json:"pid"`
}

type result struct {
	code   int
	pid    int
	stderr string
	report *report
}

type runner struct {
	t     *testing.T
	s     *stub
	cache string
	env   map[string]string
}

func newRunner(t *testing.T, s *stub) *runner {
	dir := t.TempDir()
	return &runner{t: t, s: s, cache: filepath.Join(dir, "cache"), env: map[string]string{
		"PATH":                             "/usr/bin:/bin",
		"KONECT_INSTALL_TOKEN":             tok,
		"KONECT_VERSION":                   version,
		"KONECT_SPARKHUB":                  "preprod",
		"KONECT_BOOTSTRAP_CACHE":           filepath.Join(dir, "cache"),
		"KONECT_BOOTSTRAP_E2E_BASE_URL":    s.srv.URL,
		"KONECT_BOOTSTRAP_E2E_KEY_PEM":     s.signer.PublicPEM(),
		"KONECT_BOOTSTRAP_E2E_TIME_SCALE":  "100",
		"KONECT_CONFIG":                    "builtin:sqlite",
		"FAKE_KONECT_REPORT":               filepath.Join(dir, "report.json"),
		"SOMETHING_ELSE_HOLDING_THE_TOKEN": "x " + tok,
	}}
}

func (r *runner) command(args ...string) (*exec.Cmd, *bytes.Buffer) {
	_ = os.Remove(r.env["FAKE_KONECT_REPORT"])
	cmd := exec.Command(bootstrapBin, args...)
	for k, v := range r.env {
		if v != "" {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &stderr
	return cmd, &stderr
}

func (r *runner) finish(cmd *exec.Cmd, stderr *bytes.Buffer, err error) result {
	res := result{stderr: stderr.String()}
	if cmd.Process != nil {
		res.pid = cmd.Process.Pid
	}
	if ee, ok := err.(*exec.ExitError); ok {
		res.code = ee.ExitCode()
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			res.code = 128 + int(ws.Signal())
		}
	} else if err != nil {
		r.t.Fatal(err)
	}
	if b, err := os.ReadFile(r.env["FAKE_KONECT_REPORT"]); err == nil {
		res.report = &report{}
		if err := json.Unmarshal(b, res.report); err != nil {
			r.t.Fatal(err)
		}
	}
	// Never in any log line: the token's secret, the presigned URL.
	if strings.Contains(res.stderr, secret) || strings.Contains(res.stderr, "SECRETSIG") {
		r.t.Fatalf("a secret reached the output:\n%s", res.stderr)
	}
	return res
}

func (r *runner) run(args ...string) result {
	cmd, stderr := r.command(args...)
	return r.finish(cmd, stderr, cmd.Run())
}

func (r *runner) mustStart(args ...string) result {
	r.t.Helper()
	res := r.run(args...)
	if res.code != 0 || res.report == nil {
		r.t.Fatalf("expected Konect to start, exit %d:\n%s", res.code, res.stderr)
	}
	return res
}

func (r *runner) mustExit(code int, args ...string) result {
	r.t.Helper()
	res := r.run(args...)
	if res.code != code {
		r.t.Fatalf("exit %d, want %d:\n%s", res.code, code, res.stderr)
	}
	if res.report != nil {
		r.t.Fatalf("Konect ran although the bootstrap should have exited %d", code)
	}
	return res
}

func has(lines []string, prefix string) bool {
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}

func TestFreshStartAndHandover(t *testing.T) {
	s := newStub(t)
	r := newRunner(t, s)
	res := r.mustStart()
	rep := res.report
	if strings.Join(rep.Args, " ") != "konect serve --host 0.0.0.0 --insecure-http" {
		t.Fatalf("args %v", rep.Args)
	}
	if rep.Pid != res.pid {
		t.Fatalf("Konect ran as pid %d, the bootstrap was %d: not an exec", rep.Pid, res.pid)
	}
	if rep.Exe != filepath.Join(r.cache, version, "konect") {
		t.Fatalf("exe %s", rep.Exe)
	}
	env := strings.Join(rep.Environ, "\n")
	for _, bad := range []string{"kit_", "KONECT_INSTALL_TOKEN", "KONECT_BOOTSTRAP_"} {
		if strings.Contains(env, bad) {
			t.Fatalf("%s is in Konect's /proc/self/environ:\n%s", bad, env)
		}
	}
	if !has(rep.Environ, "KONECT_BOOTSTRAP=1.0.0-e2e") || !has(rep.Environ, "KONECT_ARTIFACT_SHA256="+s.rel.SHA256) ||
		!has(rep.Environ, "KONECT_VERSION=0.7.0") || !has(rep.Environ, "KONECT_CONFIG=builtin:sqlite") {
		t.Fatalf("environ %v", rep.Environ)
	}
	if !strings.Contains(res.stderr, `"path":"fd"`) {
		t.Fatalf("expected the fd exec path:\n%s", res.stderr)
	}
	if s.resolves[0]["client"] != "bootstrap" || s.resolves[0]["clientVersion"] != "1.0.0-e2e" || s.resolves[0]["platform"] != platform() {
		t.Fatalf("request %v", s.resolves[0])
	}

	// Second start: from the cache (cachedSha256 sent, no download), custom Konect args.
	res = r.mustStart("--", "version", "--json")
	if strings.Join(res.report.Args, " ") != "konect version --json" {
		t.Fatalf("args %v", res.report.Args)
	}
	if _, objects := s.counts(); objects != 1 || s.resolves[1]["cachedSha256"] != s.rel.SHA256 {
		t.Fatalf("objects %d, second request %v", objects, s.resolves[1])
	}
	if !strings.Contains(res.stderr, `"event":"cache_hit"`) {
		t.Fatal("no cache_hit")
	}
}

func TestCachedStartOutcomes(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		code   int // 0: Konect starts
		event  string
	}{
		{"offline", 503, ``, 0, "offline_start"},
		{"rate limited", 429, ``, 0, "offline_start"},
		{"withdrawn", 410, `{"error":"version_withdrawn","latest":"0.7.1"}`, 0, "version_withdrawn"},
		{"revoked", 401, `{"error":"token_revoked","label":"Azure prod"}`, 78, "refused_token_revoked"},
		{"not entitled", 403, `{"error":"not_entitled"}`, 78, "refused_not_entitled"},
		{"too old", 426, `{"error":"bootstrap_too_old","minimum":"2.0.0"}`, 78, "refused_bootstrap_too_old"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newStub(t)
			r := newRunner(t, s)
			r.mustStart()
			s.onResolve = func(n int, req map[string]any, w http.ResponseWriter) bool {
				if c.status == 429 {
					w.Header().Set("Retry-After", "3600")
				}
				w.WriteHeader(c.status)
				_, _ = io.WriteString(w, c.body)
				return true
			}
			var res result
			if c.code == 0 {
				res = r.mustStart()
			} else {
				res = r.mustExit(c.code)
			}
			if !strings.Contains(res.stderr, `"event":"`+c.event+`"`) {
				t.Fatalf("no %s event:\n%s", c.event, res.stderr)
			}
		})
	}
}

func TestNoCacheRefusals(t *testing.T) {
	for _, c := range []struct {
		status int
		body   string
	}{{401, `{"error":"token_revoked"}`}, {404, `{"error":"version_unknown"}`}, {410, `{"error":"version_withdrawn"}`}, {426, `{"error":"bootstrap_too_old"}`}} {
		s := newStub(t)
		s.onResolve = func(n int, req map[string]any, w http.ResponseWriter) bool {
			w.WriteHeader(c.status)
			_, _ = io.WriteString(w, c.body)
			return true
		}
		newRunner(t, s).mustExit(78)
	}
}

func TestManifestChanged(t *testing.T) {
	s := newStub(t)
	r := newRunner(t, s)
	r.mustStart()
	// A validly signed manifest for the same version, with different bytes.
	other := testrelease.Build(version, platform(), append(append([]byte(nil), fakeBytes...), 0), s.signer)
	s.onResolve = func(n int, req map[string]any, w http.ResponseWriter) bool { s.answer(w, req, other); return true }
	res := r.mustExit(78)
	if !strings.Contains(res.stderr, `"event":"manifest_changed"`) {
		t.Fatal(res.stderr)
	}
}

func TestTamperedCacheIsReplaced(t *testing.T) {
	s := newStub(t)
	r := newRunner(t, s)
	r.mustStart()
	bin := filepath.Join(r.cache, version, "konect")
	_ = os.Chmod(bin, 0o700)
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho evil\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	res := r.mustStart()
	if !strings.Contains(res.stderr, `"event":"cache_mismatch"`) {
		t.Fatal(res.stderr)
	}
	if _, objects := s.counts(); objects != 2 {
		t.Fatalf("objects %d", objects)
	}
}

func TestRetriesAndRateLimits(t *testing.T) {
	// 503 twice, then a 429 with a short Retry-After, then success.
	s := newStub(t)
	s.onResolve = func(n int, req map[string]any, w http.ResponseWriter) bool {
		switch n {
		case 1, 2:
			w.WriteHeader(503)
			return true
		case 3:
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(429)
			return true
		}
		return false
	}
	res := newRunner(t, s).mustStart()
	if strings.Count(res.stderr, `"event":"retry"`) != 3 {
		t.Fatalf("expected 3 retries:\n%s", res.stderr)
	}

	// A Retry-After past the budget: exit 1 at once, naming when it clears.
	s = newStub(t)
	s.onResolve = func(n int, req map[string]any, w http.ResponseWriter) bool {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(429)
		return true
	}
	res = newRunner(t, s).mustExit(1)
	if !strings.Contains(res.stderr, "rate limited until") || !strings.Contains(res.stderr, "kit_e2e.01234567") {
		t.Fatal(res.stderr)
	}

	// Always 503: gives up within the (scaled) 15 minutes with exit 1.
	s = newStub(t)
	s.onResolve = func(n int, req map[string]any, w http.ResponseWriter) bool { w.WriteHeader(503); return true }
	r := newRunner(t, s)
	r.env["KONECT_BOOTSTRAP_E2E_TIME_SCALE"] = "1000"
	res = r.mustExit(1)
	if n, _ := s.counts(); n < 5 || !strings.Contains(res.stderr, `"event":"gave_up"`) {
		t.Fatalf("%d resolves:\n%s", n, res.stderr)
	}
}

func TestExpiredURL(t *testing.T) {
	s := newStub(t)
	s.onObject = func(n int, w http.ResponseWriter, r *http.Request) bool {
		if n == 1 {
			w.WriteHeader(403)
			return true
		}
		return false
	}
	newRunner(t, s).mustStart()
	if n, objects := s.counts(); n != 2 || objects != 2 {
		t.Fatalf("resolves %d objects %d", n, objects)
	}
}

func TestTamperedObjectNeverRuns(t *testing.T) {
	s := newStub(t)
	s.onObject = func(n int, w http.ResponseWriter, r *http.Request) bool {
		bad := append([]byte(nil), s.rel.Object...)
		bad[len(bad)-12] ^= 1
		_, _ = w.Write(bad)
		return true
	}
	r := newRunner(t, s)
	r.env["KONECT_BOOTSTRAP_E2E_TIME_SCALE"] = "1000"
	res := r.mustExit(1)
	if !strings.Contains(res.stderr, `"event":"download_mismatch"`) {
		t.Fatal(res.stderr)
	}
}

func TestBadSignatureIsDefinitive(t *testing.T) {
	s := newStub(t)
	evil := testrelease.NewSigner(s.signer.ID) // same key id, another key
	bad := testrelease.Build(version, platform(), fakeBytes, evil)
	s.onResolve = func(n int, req map[string]any, w http.ResponseWriter) bool { s.answer(w, req, bad); return true }
	res := newRunner(t, s).mustExit(78)
	if _, objects := s.counts(); objects != 0 || !strings.Contains(res.stderr, "sig_none_valid") {
		t.Fatalf("objects %d:\n%s", objects, res.stderr)
	}
}

func TestPrerelease(t *testing.T) {
	s := newStub(t)
	s.rel = testrelease.Build("0.7.0-rc.1", platform(), fakeBytes, s.signer)
	r := newRunner(t, s)
	r.env["KONECT_VERSION"] = "0.7.0-rc.1"
	r.env["KONECT_SPARKHUB"] = "prod"
	r.mustExit(78)
	if n, _ := s.counts(); n != 0 {
		t.Fatal("a prerelease on prod reached SparkHub")
	}
	r.env["KONECT_SPARKHUB"] = "preprod"
	r.mustStart()
}

func TestSIGTERMDuringDownload(t *testing.T) {
	s := newStub(t)
	started := make(chan struct{}, 1)
	s.onObject = func(n int, w http.ResponseWriter, r *http.Request) bool {
		_, _ = w.Write(s.rel.Object[:100])
		w.(http.Flusher).Flush()
		started <- struct{}{}
		select {
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
		return true
	}
	r := newRunner(t, s)
	cmd, stderr := r.command()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(20 * time.Second):
		t.Fatal("download never started")
	}
	t0 := time.Now()
	_ = cmd.Process.Signal(syscall.SIGTERM)
	res := r.finish(cmd, stderr, cmd.Wait())
	if res.code != 143 || time.Since(t0) > 3*time.Second || res.report != nil {
		t.Fatalf("exit %d after %v:\n%s", res.code, time.Since(t0), res.stderr)
	}
}

func TestTwoReplicasShareACache(t *testing.T) {
	s := newStub(t)
	r := newRunner(t, s)
	var wg sync.WaitGroup
	results := make([]result, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rr := *r
			rr.env = map[string]string{}
			for k, v := range r.env {
				rr.env[k] = v
			}
			rr.env["FAKE_KONECT_REPORT"] = filepath.Join(t.TempDir(), "report.json")
			results[i] = rr.run()
		}(i)
	}
	wg.Wait()
	for i, res := range results {
		if res.code != 0 || res.report == nil {
			t.Fatalf("replica %d: exit %d\n%s", i, res.code, res.stderr)
		}
	}
}

func TestFetchAndVerify(t *testing.T) {
	s := newStub(t)
	r := newRunner(t, s)
	out := filepath.Join(t.TempDir(), "out")
	res := r.run("fetch", "--out", out, "--client", "build")
	if res.code != 0 {
		t.Fatalf("fetch exit %d:\n%s", res.code, res.stderr)
	}
	if s.resolves[0]["client"] != "build" {
		t.Fatalf("client %v", s.resolves[0]["client"])
	}
	for _, f := range []string{"konect", "manifest.json", "manifest.sig.json"} {
		if _, err := os.Stat(filepath.Join(out, f)); err != nil {
			t.Fatal(err)
		}
	}
	r.env["KONECT_INSTALL_TOKEN"] = "" // verify is offline and needs no token
	if res := r.run("verify", out, "--version", version, "--sparkhub", "preprod"); res.code != 0 || !strings.Contains(res.stderr, "OK: ") {
		t.Fatalf("verify exit %d:\n%s", res.code, res.stderr)
	}
	if res := r.run("verify", out, "--version", "0.7.1"); res.code != 1 || !strings.Contains(res.stderr, "version_mismatch") {
		t.Fatalf("verify of the wrong version: exit %d:\n%s", res.code, res.stderr)
	}
	if err := os.WriteFile(filepath.Join(out, "konect"), []byte("tampered"), 0o755); err != nil {
		t.Fatal(err)
	}
	if res := r.run("verify", out, "--version", version); res.code != 1 || !strings.Contains(res.stderr, "FAILED") {
		t.Fatalf("verify of a tampered binary: exit %d:\n%s", res.code, res.stderr)
	}
}

func TestHealth(t *testing.T) {
	s := newStub(t)
	r := newRunner(t, s)
	r.env["KONECT_INSTALL_TOKEN"] = ""
	if res := r.run("health"); res.code != 1 || res.report != nil {
		t.Fatalf("health without a cache: exit %d", res.code)
	}
	r.env["KONECT_INSTALL_TOKEN"] = tok
	r.mustStart()
	r.env["KONECT_INSTALL_TOKEN"] = ""
	res := r.mustStart("health")
	if strings.Join(res.report.Args, " ") != "konect health --url http://127.0.0.1:41800 --allow-locked" {
		t.Fatalf("args %v", res.report.Args)
	}
	if n, _ := s.counts(); n != 1 {
		t.Fatal("health called SparkHub")
	}
}

// E2E_NOEXEC_DIR: a noexec mount (CI mounts a tmpfs with -o noexec): the bootstrap runs Konect from a sealed memfd.
func TestNoexecCacheUsesMemfd(t *testing.T) {
	dir := os.Getenv("E2E_NOEXEC_DIR")
	if dir == "" {
		t.Skip("E2E_NOEXEC_DIR not set")
	}
	s := newStub(t)
	r := newRunner(t, s)
	cacheDir, err := os.MkdirTemp(dir, "cache")
	if err != nil {
		t.Fatal(err)
	}
	r.env["KONECT_BOOTSTRAP_CACHE"] = cacheDir
	res := r.mustStart()
	if !strings.Contains(res.stderr, `"path":"memfd"`) || !strings.HasPrefix(res.report.Exe, "/memfd:konect") {
		t.Fatalf("exe %s:\n%s", res.report.Exe, res.stderr)
	}
	if strings.Contains(strings.Join(res.report.Environ, "\n"), "kit_") {
		t.Fatal("token in the environment")
	}
}

func TestConfigErrors(t *testing.T) {
	s := newStub(t)
	for name, env := range map[string]map[string]string{
		"no token":         {"KONECT_INSTALL_TOKEN": ""},
		"bad token":        {"KONECT_INSTALL_TOKEN": "kit_nope"},
		"latest":           {"KONECT_VERSION": "latest"},
		"no version":       {"KONECT_VERSION": ""},
		"dev sparkhub":     {"KONECT_SPARKHUB": "dev"},
		"relative cache":   {"KONECT_BOOTSTRAP_CACHE": "cache"},
		"unwritable cache": {"KONECT_BOOTSTRAP_CACHE": "/proc/konect-cache"},
	} {
		r := newRunner(t, s)
		for k, v := range env {
			r.env[k] = v
		}
		res := r.run()
		if res.code != 78 || res.report != nil {
			t.Fatalf("%s: exit %d:\n%s", name, res.code, res.stderr)
		}
	}
	if n, _ := s.counts(); n != 0 {
		t.Fatalf("%d resolves on configuration errors", n)
	}
}

func TestVersionListsKeys(t *testing.T) {
	s := newStub(t)
	r := newRunner(t, s)
	res := r.run("version", "--json")
	var v struct {
		Version string `json:"version"`
		Keys    []struct {
			KeyID string `json:"keyId"`
		} `json:"keys"`
	}
	if err := json.Unmarshal([]byte(res.stderr), &v); err != nil || v.Version != "1.0.0-e2e" || len(v.Keys) == 0 || v.Keys[len(v.Keys)-1].KeyID != "e2e-test-key-not-for-release" {
		t.Fatalf("version: %v %s", err, res.stderr)
	}
}
