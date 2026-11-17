// Package resolve is the client of SparkHub's install resolve endpoint (design #1702 §4.2, schema T/install.ts):
//
//	POST https://<org>.sparkhub.studio/api/konect/install/v1/resolve   (preprod: <org>.sparkhub.guru)
//	Authorization: Bearer kit_…
//	{ "version", "platform", "client", "clientVersion", "cachedSha256"? }
//
// An answer is one of: *Answer (200), *Refusal (a definitive 400/401/403/404/410/426), *Transient (429, 5xx,
// timeouts, network, anything unexpected). Nothing in an answer is trusted before the verifier contract has
// checked the manifest; the artifact fields of the answer are never used for integrity (the manifest's are).
package resolve

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/KainosSoftwareLtd/sparkhub-konect-deploy/internal/token"
	"github.com/KainosSoftwareLtd/sparkhub-konect-deploy/internal/verify"
)

// Timeout of one resolve call (§4.2: "5xx, timeout (10 s)").
const Timeout = 10 * time.Second

// maxAnswer caps the answer body (the manifest is a few KB).
const maxAnswer = 4 << 20

// Hosts per SparkHub environment.
var Hosts = map[string]string{"prod": "sparkhub.studio", "preprod": "sparkhub.guru"}

// Request is the body of a resolve call.
type Request struct {
	Version       string `json:"version"`
	Platform      string `json:"platform"`
	Client        string `json:"client"` // bootstrap | install-script | build
	ClientVersion string `json:"clientVersion"`
	CachedSHA256  string `json:"cachedSha256,omitempty"`
}

// Answer is a 200.
type Answer struct {
	Version   string
	Manifest  []byte    // the exact manifest.json bytes
	Signature []byte    // the manifest.sig.json bytes
	Artifact  *Artifact // nil when cachedSha256 matched
	Warning   *Warning
}

// Artifact is where to download the platform's object. Only URL is used; integrity comes from the manifest.
type Artifact struct {
	Name      string `json:"name"`
	URL       string `json:"url"`
	ExpiresAt string `json:"expiresAt"`
}

// Warning is a 200 with a deprecation notice (bootstrap_deprecated).
type Warning struct {
	Code     string `json:"code"`
	Minimum  string `json:"minimum"`
	Deadline string `json:"deadline"`
	Message  string `json:"message"`
}

// Refusal is a definitive answer: the bootstrap never retries it.
type Refusal struct {
	Status  int
	Code    string // invalid_token, token_revoked, not_entitled, version_unknown, platform_unknown, version_withdrawn, bootstrap_too_old
	Message string
	Label   string // the token's label, when SparkHub names it
	Latest  string // 410: the latest available version
	Minimum string // 426: the minimum bootstrap version
}

func (r *Refusal) Error() string {
	s := fmt.Sprintf("SparkHub refused (%d %s)", r.Status, r.Code)
	if r.Message != "" {
		s += ": " + r.Message
	}
	return s
}

// Withdrawn reports a 410: the version may still start from a verified cache.
func (r *Refusal) Withdrawn() bool { return r.Status == http.StatusGone }

// Transient is anything worth retrying (or, with a verified cache, worth an offline start).
type Transient struct {
	Status      int // 0 for network errors
	RateLimited bool
	RetryAfter  time.Duration // from Retry-After on a 429; 0 when absent
	Err         error
}

func (t *Transient) Error() string {
	switch {
	case t.RateLimited:
		return "SparkHub rate limited this install token (429)"
	case t.Status != 0:
		return fmt.Sprintf("SparkHub answered %d", t.Status)
	default:
		return "SparkHub unreachable: " + t.Err.Error()
	}
}

func (t *Transient) Unwrap() error { return t.Err }

// Client calls the resolve endpoint.
type Client struct {
	HTTP    *http.Client
	BaseURL string // https://<org>.<host>; tests point it elsewhere
	Token   token.Token
	Now     func() time.Time
}

// New returns a client for the token's org on the named environment.
func New(tok token.Token, env string) (*Client, error) {
	host, ok := Hosts[env]
	if !ok {
		return nil, fmt.Errorf("KONECT_SPARKHUB must be prod or preprod")
	}
	return &Client{HTTP: NewHTTPClient(Timeout), BaseURL: "https://" + tok.Org + "." + host, Token: tok, Now: time.Now}, nil
}

// NewHTTPClient honours HTTPS_PROXY / NO_PROXY and SSL_CERT_FILE / SSL_CERT_DIR (Go reads both) and never
// follows redirects (a redirect of the resolve call is not an answer).
func NewHTTPClient(timeout time.Duration) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = http.ProxyFromEnvironment
	return &http.Client{
		Transport:     tr,
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Resolve makes one call.
func (c *Client) Resolve(ctx context.Context, req Request) (*Answer, error) {
	body, _ := json.Marshal(req)
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/konect/install/v1/resolve", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Authorization", c.Token.Bearer())
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "application/json")
	hreq.Header.Set("User-Agent", "konect-bootstrap/"+req.ClientVersion)
	resp, err := c.HTTP.Do(hreq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &Transient{Err: scrubURLError(err)}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &Transient{Status: resp.StatusCode, Err: scrubURLError(err)}
	}
	if len(raw) > maxAnswer {
		return nil, &Transient{Status: resp.StatusCode, Err: errors.New("answer too large")}
	}
	switch s := resp.StatusCode; {
	case s == http.StatusOK:
		a, err := parseAnswer(raw)
		if err != nil {
			return nil, &Transient{Status: s, Err: err}
		}
		return a, nil
	case s == http.StatusTooManyRequests:
		return nil, &Transient{Status: s, RateLimited: true, RetryAfter: retryAfter(resp.Header.Get("Retry-After"), c.now())}
	case s == 400 || s == 401 || s == 403 || s == 404 || s == 410 || s == 426:
		return nil, parseRefusal(s, raw)
	default:
		return nil, &Transient{Status: s, Err: fmt.Errorf("HTTP %d", s)}
	}
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

type wireAnswer struct {
	Version    string          `json:"version"`
	Manifest   string          `json:"manifest"`
	Signatures json.RawMessage `json:"signatures"`
	Artifact   *Artifact       `json:"artifact"`
	Warning    *Warning        `json:"warning"`
}

func parseAnswer(raw []byte) (*Answer, error) {
	var w wireAnswer
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, errors.New("answer is not JSON")
	}
	m, err := base64.StdEncoding.DecodeString(w.Manifest)
	if err != nil || len(m) == 0 || len(m) > verify.MaxFileSize {
		return nil, errors.New("answer has no manifest")
	}
	sig, err := signatureBytes(w.Signatures)
	if err != nil {
		return nil, err
	}
	if w.Artifact != nil {
		u, err := url.Parse(w.Artifact.URL)
		if err != nil || u.Host == "" || (u.Scheme != "https" && !allowHTTP(u)) {
			return nil, errors.New("answer has an invalid download URL")
		}
	}
	return &Answer{Version: w.Version, Manifest: m, Signature: sig, Artifact: w.Artifact, Warning: w.Warning}, nil
}

// signatureBytes accepts the manifest.sig.json bytes as base64 (the exact file) or as the JSON object itself.
// The signature file is not signed, so its exact bytes do not matter; the manifest's do.
func signatureBytes(raw json.RawMessage) ([]byte, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, errors.New("answer has no signatures")
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, errors.New("answer has invalid signatures")
		}
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil || len(b) == 0 || len(b) > verify.MaxFileSize {
			return nil, errors.New("answer has invalid signatures")
		}
		return b, nil
	}
	if raw[0] == '{' && len(raw) <= verify.MaxFileSize {
		return append([]byte(nil), raw...), nil
	}
	return nil, errors.New("answer has invalid signatures")
}

func parseRefusal(status int, raw []byte) *Refusal {
	var w struct {
		Error   any    `json:"error"`
		Code    string `json:"code"`
		Message string `json:"message"`
		Label   string `json:"label"`
		Latest  string `json:"latest"`
		Minimum string `json:"minimum"`
	}
	_ = json.Unmarshal(raw, &w)
	r := &Refusal{Status: status, Code: w.Code, Message: clean(w.Message), Label: clean(w.Label), Latest: clean(w.Latest), Minimum: clean(w.Minimum)}
	switch e := w.Error.(type) {
	case string:
		if r.Code == "" {
			r.Code = e
		}
	case map[string]any: // { error: { code, message } }
		if c, ok := e["code"].(string); ok && r.Code == "" {
			r.Code = c
		}
		if m, ok := e["message"].(string); ok && r.Message == "" {
			r.Message = clean(m)
		}
	}
	r.Code = clean(r.Code)
	if r.Code == "" {
		r.Code = map[int]string{400: "bad_request", 401: "invalid_token", 403: "not_entitled", 404: "version_unknown", 410: "version_withdrawn", 426: "bootstrap_too_old"}[status]
	}
	return r
}

// clean keeps a server string printable and short before it reaches a log line.
func clean(s string) string {
	if len(s) > 300 {
		s = s[:300]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, token.Pattern.ReplaceAllString(s, "kit_[redacted]"))
}

// retryAfter reads delta-seconds or an HTTP date.
func retryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n < 0 {
			return 0
		}
		if n > 86400 {
			n = 86400
		}
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// scrubURLError drops the URL from a *url.Error (a presigned URL must never reach a log line).
func scrubURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s: %w", ue.Op, ue.Err)
	}
	return err
}
