package resolve

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/KainosSoftwareLtd/sparkhub-konect-deploy/internal/token"
)

const tok = "kit_acme.0123456789abcdef0123456789abcdef.abcdefghijklmnopqrstuvwxyzABCDEFGHIJ-_01234"

func client(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	tk, err := token.Parse(tok)
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(tk, "preprod")
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseURL != "https://acme.sparkhub.guru" {
		t.Fatalf("base %s", c.BaseURL)
	}
	hc := srv.Client()
	hc.CheckRedirect = c.HTTP.CheckRedirect
	c.HTTP, c.BaseURL = hc, srv.URL
	c.Now = func() time.Time { return time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC) }
	return c
}

var req = Request{Version: "0.7.0", Platform: "linux-x64", Client: "bootstrap", ClientVersion: "1.0.0"}

func TestRequestShape(t *testing.T) {
	c := client(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/konect/install/v1/resolve" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer "+tok {
			t.Error("authorization header")
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["version"] != "0.7.0" || body["platform"] != "linux-x64" || body["client"] != "bootstrap" || body["clientVersion"] != "1.0.0" {
			t.Errorf("body %v", body)
		}
		if _, has := body["cachedSha256"]; has {
			t.Error("cachedSha256 sent without a cache")
		}
		w.WriteHeader(503)
	})
	_, err := c.Resolve(context.Background(), req)
	var tr *Transient
	if !errors.As(err, &tr) || tr.Status != 503 {
		t.Fatalf("got %v", err)
	}
}

func TestAnswer(t *testing.T) {
	manifest := []byte(`{"schema":"konect-release-manifest/1"}`)
	sig := []byte(`{"schema":"konect-release-signature/1","signatures":[]}`)
	for name, sigField := range map[string]any{"base64": base64.StdEncoding.EncodeToString(sig), "object": json.RawMessage(sig)} {
		c := client(t, func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"version": "0.7.0", "manifest": base64.StdEncoding.EncodeToString(manifest), "signatures": sigField,
				"artifact": map[string]any{"name": "konect-linux-x64", "url": "https://bucket.s3.amazonaws.com/x?sig=1", "expiresAt": "2026-11-17T09:05:00Z"},
				"warning":  map[string]any{"code": "bootstrap_deprecated", "minimum": "1.1.0"},
			})
		})
		a, err := c.Resolve(context.Background(), req)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(a.Manifest) != string(manifest) || a.Artifact == nil || a.Warning.Minimum != "1.1.0" {
			t.Fatalf("%s: %+v", name, a)
		}
		if strings.TrimSpace(string(a.Signature)) != string(sig) {
			t.Fatalf("%s: signature %s", name, a.Signature)
		}
	}
}

func TestAnswerRejectsPlainHTTPDownloads(t *testing.T) {
	c := client(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"manifest": base64.StdEncoding.EncodeToString([]byte("{}")), "signatures": "e30=",
			"artifact": map[string]any{"url": "http://bucket/x"},
		})
	})
	if _, err := c.Resolve(context.Background(), req); err == nil {
		t.Fatal("http download URL accepted")
	}
}

func TestRefusals(t *testing.T) {
	cases := []struct {
		status int
		body   string
		code   string
	}{
		{401, `{"error":"token_revoked","message":"revoked by x","label":"Azure prod"}`, "token_revoked"},
		{401, `{"error":{"code":"invalid_token","message":"m"}}`, "invalid_token"},
		{401, `{"code":"invalid_token"}`, "invalid_token"},
		{401, `not json`, "invalid_token"},
		{403, `{"error":"not_entitled"}`, "not_entitled"},
		{404, `{"error":"version_unknown"}`, "version_unknown"},
		{400, `{"error":"platform_unknown"}`, "platform_unknown"},
		{410, `{"error":"version_withdrawn","latest":"0.7.1"}`, "version_withdrawn"},
		{426, `{"error":"bootstrap_too_old","minimum":"1.1.0"}`, "bootstrap_too_old"},
	}
	for _, cs := range cases {
		c := client(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(cs.status)
			_, _ = io.WriteString(w, cs.body)
		})
		_, err := c.Resolve(context.Background(), req)
		var ref *Refusal
		if !errors.As(err, &ref) || ref.Code != cs.code || ref.Status != cs.status {
			t.Fatalf("%d %s: got %v", cs.status, cs.body, err)
		}
		if cs.status == 410 && (!ref.Withdrawn() || ref.Latest != "0.7.1") {
			t.Fatal("410 details")
		}
	}
}

func TestTransients(t *testing.T) {
	for _, status := range []int{500, 502, 503, 504, 302, 418} {
		c := client(t, func(w http.ResponseWriter, r *http.Request) {
			if status == 302 {
				http.Redirect(w, r, "https://evil.example/", http.StatusFound)
				return
			}
			w.WriteHeader(status)
		})
		_, err := c.Resolve(context.Background(), req)
		var tr *Transient
		if !errors.As(err, &tr) || tr.RateLimited {
			t.Fatalf("%d: got %v", status, err)
		}
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	cases := map[string]time.Duration{
		"":                              0,
		"120":                           2 * time.Minute,
		"-1":                            0,
		"999999":                        24 * time.Hour,
		"Tue, 17 Nov 2026 09:30:00 GMT": 30 * time.Minute,
		"Tue, 17 Nov 2026 08:30:00 GMT": 0,
		"soon":                          0,
	}
	for in, want := range cases {
		if got := retryAfter(in, now); got != want {
			t.Errorf("%q: %v, want %v", in, got, want)
		}
	}
	c := client(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "600")
		w.WriteHeader(429)
	})
	_, err := c.Resolve(context.Background(), req)
	var tr *Transient
	if !errors.As(err, &tr) || !tr.RateLimited || tr.RetryAfter != 10*time.Minute {
		t.Fatalf("got %v", err)
	}
}

func TestRefusalTextIsScrubbed(t *testing.T) {
	c := client(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = io.WriteString(w, `{"error":"invalid_token","message":"bad `+tok+"\\n\\u001b[31m"+`"}`)
	})
	_, err := c.Resolve(context.Background(), req)
	if err == nil || strings.Contains(err.Error(), "abcdefghij") || strings.ContainsAny(err.Error(), "\n\x1b") {
		t.Fatalf("not scrubbed: %q", err)
	}
}

func TestUnknownEnvironment(t *testing.T) {
	tk, _ := token.Parse(tok)
	if _, err := New(tk, "dev"); err == nil {
		t.Fatal("dev accepted")
	}
}
