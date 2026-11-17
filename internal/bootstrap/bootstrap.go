// Package bootstrap is the start sequence of design #1702 §5.2 and §12: verify the cache, resolve, download when
// needed, verify, place in the cache, and hand a verified open binary to the caller for the exec.
//
// Exit codes: 78 for every definitive refusal or configuration error (never retried, the platform's restart will
// fail the same way); 1 when the retry budget runs out (the platform restarts the container); 143 on SIGTERM.
package bootstrap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/KainosSoftwareLtd/sparkhub-konect-deploy/internal/cache"
	"github.com/KainosSoftwareLtd/sparkhub-konect-deploy/internal/fetch"
	"github.com/KainosSoftwareLtd/sparkhub-konect-deploy/internal/resolve"
	"github.com/KainosSoftwareLtd/sparkhub-konect-deploy/internal/verify"
)

// Exit codes.
const (
	ExitRetry  = 1  // retry budget spent, offline without a cache, rate limited past the budget
	ExitConfig = 78 // EX_CONFIG: definitive refusal, bad configuration, verification failure
)

// DownloadTimeout bounds one download attempt (a release object is about 35 MB).
const DownloadTimeout = 10 * time.Minute

// Exit is a terminal outcome with its exit code. Event names it in the log.
type Exit struct {
	Code  int
	Event string
	Err   error
}

func (e *Exit) Error() string { return e.Err.Error() }
func (e *Exit) Unwrap() error { return e.Err }

func exitf(code int, event, format string, a ...any) *Exit {
	return &Exit{Code: code, Event: event, Err: fmt.Errorf(format, a...)}
}

// Policy is the retry schedule for network failures and 5xx: 5, 10, 20 s, then every 60 s, 15 minutes in total.
type Policy struct {
	Delays []time.Duration
	Every  time.Duration
	Budget time.Duration
	Scale  time.Duration // divides Retry-After waits (1 in a release build; the e2e build speeds time up)
	Now    func() time.Time
	Sleep  func(ctx context.Context, d time.Duration) error
}

// DefaultPolicy is §5.2's schedule.
func DefaultPolicy() Policy {
	s := timeScale()
	return Policy{
		Delays: []time.Duration{5 * time.Second / s, 10 * time.Second / s, 20 * time.Second / s},
		Every:  60 * time.Second / s,
		Budget: 15 * time.Minute / s,
		Scale:  s,
		Now:    time.Now,
		Sleep:  sleepCtx,
	}
}

func (p Policy) delay(attempt int) time.Duration {
	if attempt < len(p.Delays) {
		return p.Delays[attempt]
	}
	return p.Every
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Resolver is the resolve endpoint (resolve.Client, or a stub in tests).
type Resolver interface {
	Resolve(ctx context.Context, req resolve.Request) (*resolve.Answer, error)
}

// Options configure one run.
type Options struct {
	Version       string
	Prod          bool
	Platform      string
	Client        string // bootstrap | install-script | build
	ClientVersion string
	TokenLabel    string // the redacted token, for messages
	Org           string
	Keys          []verify.Key
	Resolver      Resolver
	Download      *http.Client
	Policy        Policy
	Log           *slog.Logger
}

func (o *Options) expect() verify.Expect {
	env := "preprod"
	if o.Prod {
		env = "prod"
	}
	return verify.Expect{Version: o.Version, Environment: env, Platform: o.Platform}
}

func (o *Options) request(cached string) resolve.Request {
	return resolve.Request{Version: o.Version, Platform: o.Platform, Client: o.Client, ClientVersion: o.ClientVersion, CachedSHA256: cached}
}

// Start runs §5.2 steps 2 to 4 and returns a verified entry of the cache, open for the exec.
func Start(ctx context.Context, o *Options, c cache.Cache) (*cache.Entry, error) {
	if err := c.Ensure(); err != nil {
		return nil, exitf(ExitConfig, "cache_unwritable", "the cache directory %s is not writable (set KONECT_BOOTSTRAP_CACHE to a writable path on the data volume): %v", c.Dir, err)
	}
	entry, err := c.Load(o.Version, o.Keys, o.expect())
	var mm *cache.MismatchError
	switch {
	case errors.As(err, &mm):
		o.Log.Warn(mm.Error(), "event", "cache_mismatch", "version", o.Version, "reason", mm.Reason, "want", mm.Want, "got", mm.Got)
	case err != nil:
		return nil, exitf(ExitRetry, "cache_error", "reading the cache %s: %v", c.Dir, err)
	}
	if entry != nil {
		if err := o.checkCached(ctx, entry); err != nil {
			entry.File.Close()
			return nil, err
		}
		return entry, nil
	}
	return o.acquire(ctx, func(ctx context.Context, ans *resolve.Answer, res *verify.Result) (*cache.Entry, error) {
		tmp, err := c.NewTemp()
		if err != nil {
			return nil, err
		}
		if err := o.download(ctx, ans, res, tmp); err != nil {
			_ = os.RemoveAll(tmp)
			return nil, err
		}
		e, err := c.Commit(tmp, o.Version, o.Keys, o.expect())
		if err != nil {
			_ = os.RemoveAll(tmp)
			return nil, err
		}
		if e == nil {
			return nil, errors.New("the cache lost the verified binary")
		}
		return e, nil
	})
}

// Fetch resolves, downloads and verifies into outDir: konect (0755), manifest.json, manifest.sig.json (0644).
// It returns the verified entry of outDir, open.
func Fetch(ctx context.Context, o *Options, outDir string) (*cache.Entry, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, exitf(ExitConfig, "out_dir", "cannot create %s: %v", outDir, err)
	}
	return o.acquire(ctx, func(ctx context.Context, ans *resolve.Answer, res *verify.Result) (*cache.Entry, error) {
		tmp, err := os.MkdirTemp(outDir, ".tmp-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(tmp)
		if err := o.download(ctx, ans, res, tmp); err != nil {
			return nil, err
		}
		for name, mode := range map[string]os.FileMode{cache.BinaryName: 0o755, cache.ManifestName: 0o644, cache.SignatureName: 0o644} {
			if err := os.Chmod(filepath.Join(tmp, name), mode); err != nil {
				return nil, err
			}
		}
		for _, name := range []string{cache.ManifestName, cache.SignatureName, cache.BinaryName} {
			if err := os.Rename(filepath.Join(tmp, name), filepath.Join(outDir, name)); err != nil {
				return nil, err
			}
		}
		_ = cache.SyncDir(outDir)
		e, mm, err := cache.LoadDir(outDir, filepath.Join(outDir, cache.BinaryName), o.Keys, o.expect())
		if mm != nil {
			return nil, exitf(ExitRetry, "fetch_mismatch", "%s changed after it was written: %s", outDir, mm.Error())
		}
		if err != nil {
			return nil, err
		}
		e.Version = o.Version
		return e, nil
	})
}

// checkCached is §12's "cache verified" branch: one resolve with cachedSha256; anything but a definitive refusal or
// a changed manifest starts the cached binary.
func (o *Options) checkCached(ctx context.Context, e *cache.Entry) error {
	ans, err := o.Resolver.Resolve(ctx, o.request(e.SHA256()))
	var ref *resolve.Refusal
	switch {
	case err == nil:
		if !bytes.Equal(ans.Manifest, e.Manifest) {
			return exitf(ExitConfig, "manifest_changed", "SparkHub's manifest for Konect %s differs from the verified cached one; a version's manifest never changes, so nothing was run (report this to Kainos)", o.Version)
		}
		o.warn(ans.Warning)
		o.Log.Info("starting the verified cached Konect", "event", "cache_hit", "version", o.Version, "sha256", e.SHA256())
		return nil
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.As(err, &ref) && ref.Withdrawn():
		o.Log.Error(fmt.Sprintf("Konect %s was WITHDRAWN by Kainos; starting the verified cached copy. Move to a newer version (latest: %s) soon: set KONECT_VERSION and redeploy.", o.Version, orUnknown(ref.Latest)),
			"event", "version_withdrawn", "version", o.Version, "latest", ref.Latest)
		return nil
	case errors.As(err, &ref):
		return o.refusal(ref)
	default:
		o.Log.Warn("SparkHub did not answer; starting the verified cached Konect (offline start)", "event", "offline_start", "version", o.Version, "error", err.Error())
		return nil
	}
}

type placeFunc func(ctx context.Context, ans *resolve.Answer, res *verify.Result) (*cache.Entry, error)

// acquire is §12's "no cache" branch with the retry budget.
func (o *Options) acquire(ctx context.Context, place placeFunc) (*cache.Entry, error) {
	p := o.Policy
	if p.Scale <= 0 {
		p.Scale = 1
	}
	start := p.Now()
	deadline := start.Add(p.Budget)
	expiredRetried := false
	for attempt := 0; ; {
		e, err := o.tryOnce(ctx, place)
		if err == nil {
			return e, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var exit *Exit
		if errors.As(err, &exit) {
			return nil, exit
		}
		if errors.Is(err, fetch.ErrExpired) && !expiredRetried {
			expiredRetried = true
			o.Log.Info("the download link expired; resolving once more", "event", "url_expired")
			continue
		}
		wait := p.delay(attempt)
		now := p.Now()
		var tr *resolve.Transient
		if errors.As(err, &tr) && tr.RateLimited && tr.RetryAfter > 0 {
			clears := now.Add(tr.RetryAfter)
			wait = tr.RetryAfter / p.Scale
			if now.Add(wait).After(deadline) {
				return nil, exitf(ExitRetry, "rate_limited", "rate limited until %s (token %s); see SparkHub → Konect → Install",
					clears.UTC().Format(time.RFC3339), o.TokenLabel)
			}
		}
		if now.Add(wait).After(deadline) {
			return nil, exitf(ExitRetry, "gave_up", "could not get Konect %s within %s: %v (the platform restarts the bootstrap)", o.Version, p.Budget*p.Scale, err)
		}
		o.Log.Warn("could not get Konect yet; retrying", "event", "retry", "attempt", attempt+1, "in", wait.String(), "error", err.Error())
		if err := p.Sleep(ctx, wait); err != nil {
			return nil, err
		}
		attempt++
	}
}

func (o *Options) tryOnce(ctx context.Context, place placeFunc) (*cache.Entry, error) {
	ans, err := o.Resolver.Resolve(ctx, o.request(""))
	if err != nil {
		var ref *resolve.Refusal
		if errors.As(err, &ref) {
			return nil, o.refusal(ref)
		}
		return nil, err
	}
	o.warn(ans.Warning)
	res, err := verify.Release(ans.Manifest, ans.Signature, o.Keys, o.expect())
	if err != nil {
		return nil, exitf(ExitConfig, "verify_failed", "the release SparkHub offered for Konect %s failed verification (%s); nothing was run", o.Version, verify.RuleOf(err))
	}
	if ans.Artifact == nil || ans.Artifact.URL == "" {
		return nil, &resolve.Transient{Status: http.StatusOK, Err: errors.New("the answer carried no download link")}
	}
	return place(ctx, ans, res)
}

// download fetches the object into dir and leaves dir/konect (0500), manifest.json and manifest.sig.json, all
// checked against the verified manifest entry (the answer's own sizes and hashes are never used).
func (o *Options) download(ctx context.Context, ans *resolve.Answer, res *verify.Result, dir string) error {
	a := res.Artifact
	gz := filepath.Join(dir, cache.BinaryName+".gz")
	bin := filepath.Join(dir, cache.BinaryName)
	o.Log.Info("downloading Konect", "event", "download", "version", o.Version, "platform", o.Platform, "bytes", a.ObjectSize, "keyId", res.KeyID)
	dctx, cancel := context.WithTimeout(ctx, DownloadTimeout)
	defer cancel()
	if err := fetch.Download(dctx, o.Download, ans.Artifact.URL, gz, a.ObjectSize, a.ObjectSHA256); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if dctx.Err() != nil {
			return &fetch.NetError{Err: errors.New("download timed out")}
		}
		var ie *fetch.IntegrityError
		if errors.As(err, &ie) {
			o.Log.Warn("downloaded bytes do not match the signed manifest; discarded", "event", "download_mismatch", "what", ie.What, "want", ie.Want, "got", ie.Got)
		}
		return err
	}
	if err := fetch.Gunzip(gz, bin, a.Size, a.SHA256); err != nil {
		var ie *fetch.IntegrityError
		if errors.As(err, &ie) {
			o.Log.Warn("unpacked bytes do not match the signed manifest; discarded", "event", "download_mismatch", "what", ie.What, "want", ie.Want, "got", ie.Got)
		}
		return err
	}
	if err := os.Remove(gz); err != nil {
		return err
	}
	if err := os.Chmod(bin, 0o500); err != nil {
		return err
	}
	if err := cache.WriteRelease(dir, ans.Manifest, ans.Signature); err != nil {
		return err
	}
	return cache.SyncDir(dir)
}

func (o *Options) warn(w *resolve.Warning) {
	if w == nil {
		return
	}
	msg := fmt.Sprintf("SparkHub warns: %s", orUnknown(w.Code))
	if w.Code == "bootstrap_deprecated" {
		msg = fmt.Sprintf("THIS BOOTSTRAP (%s) IS OLDER THAN SPARKHUB'S MINIMUM %s", o.ClientVersion, orUnknown(w.Minimum))
		if w.Deadline != "" {
			msg += fmt.Sprintf(" (it will be refused from %s)", w.Deadline)
		}
		msg += ": update the image to the digest shown on SparkHub → Konect → Install"
	}
	o.Log.Warn(msg, "event", "warning", "code", w.Code, "minimum", w.Minimum, "deadline", w.Deadline)
}

func (o *Options) refusal(r *resolve.Refusal) *Exit {
	var msg string
	switch r.Code {
	case "invalid_token":
		msg = fmt.Sprintf("SparkHub does not accept the install token %s. Create a token in SparkHub → Konect → Install (organization %s) and set KONECT_INSTALL_TOKEN", o.TokenLabel, o.Org)
	case "token_revoked":
		msg = fmt.Sprintf("the install token %s%s was revoked. Create a new one in SparkHub → Konect → Install and set KONECT_INSTALL_TOKEN", o.TokenLabel, labelSuffix(r.Label))
	case "not_entitled":
		msg = fmt.Sprintf("organization %s is not entitled to Konect in SparkHub (or is not active)", o.Org)
	case "version_unknown":
		msg = fmt.Sprintf("Konect %s is not available; set KONECT_VERSION to a version listed on SparkHub → Konect → Install", o.Version)
	case "platform_unknown":
		msg = fmt.Sprintf("Konect is not published for %s", o.Platform)
	case "version_withdrawn":
		msg = fmt.Sprintf("Konect %s was withdrawn (latest: %s); set KONECT_VERSION to a newer version", o.Version, orUnknown(r.Latest))
	case "bootstrap_too_old":
		msg = fmt.Sprintf("this bootstrap (%s) is older than SparkHub's minimum %s; update the image to the digest shown on SparkHub → Konect → Install", o.ClientVersion, orUnknown(r.Minimum))
	default:
		msg = r.Error()
	}
	if r.Message != "" && r.Code != "" {
		msg += " (SparkHub: " + r.Message + ")"
	}
	return &Exit{Code: ExitConfig, Event: "refused_" + r.Code, Err: errors.New(msg)}
}

func labelSuffix(l string) string {
	if l == "" {
		return ""
	}
	return " (\"" + l + "\")"
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
