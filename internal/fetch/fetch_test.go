package fetch

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func sum(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func gz(b []byte) []byte {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	_, _ = w.Write(b)
	_ = w.Close()
	return buf.Bytes()
}

func TestGunzipCaps(t *testing.T) {
	dir := t.TempDir()
	bomb := gz(make([]byte, 10<<20)) // 10 MB of zeros, a few KB packed
	src := filepath.Join(dir, "x.gz")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(bomb) }))
	defer srv.Close()
	if err := Download(context.Background(), srv.Client(), srv.URL+"/x?X-Amz-Signature=SECRET", src, int64(len(bomb)), sum(bomb)); err != nil {
		t.Fatal(err)
	}
	err := Gunzip(src, filepath.Join(dir, "out"), 1000, strings.Repeat("0", 64))
	var ie *IntegrityError
	if !errors.As(err, &ie) || ie.What != "size" {
		t.Fatalf("got %v", err)
	}
	// The same object with its honest size and hash unpacks.
	err = Gunzip(filepath.Join(dir, "x.gz"), filepath.Join(dir, "out2"), int64(10<<20), sum(make([]byte, 10<<20)))
	if err != nil {
		t.Fatalf("the honest size must pass: %v", err)
	}
}

func TestDownloadStatuses(t *testing.T) {
	for status, check := range map[int]func(error) bool{
		403: func(err error) bool { return errors.Is(err, ErrExpired) },
		404: func(err error) bool { var n *NetError; return errors.As(err, &n) && n.Status == 404 },
		500: func(err error) bool { var n *NetError; return errors.As(err, &n) && n.Status == 500 },
	} {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
		err := Download(context.Background(), srv.Client(), srv.URL+"/x?X-Amz-Signature=SECRET", filepath.Join(t.TempDir(), "o"), 10, strings.Repeat("0", 64))
		srv.Close()
		if !check(err) {
			t.Fatalf("%d: %v", status, err)
		}
		if err != nil && strings.Contains(err.Error(), "SECRET") {
			t.Fatal("URL in error")
		}
	}
}

func TestDownloadShortAndLong(t *testing.T) {
	body := []byte("0123456789")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(body) }))
	defer srv.Close()
	// Declared larger than served: short read, retryable.
	err := Download(context.Background(), srv.Client(), srv.URL, filepath.Join(t.TempDir(), "o"), 20, sum(body))
	var n *NetError
	if !errors.As(err, &n) {
		t.Fatalf("short: %v", err)
	}
	// Declared smaller than served: refused by the cap.
	err = Download(context.Background(), srv.Client(), srv.URL, filepath.Join(t.TempDir(), "o"), 5, sum(body[:5]))
	var ie *IntegrityError
	if !errors.As(err, &ie) || ie.What != "object_size" {
		t.Fatalf("long: %v", err)
	}
	// Right size, wrong hash.
	err = Download(context.Background(), srv.Client(), srv.URL, filepath.Join(t.TempDir(), "o"), 10, strings.Repeat("0", 64))
	if !errors.As(err, &ie) || ie.What != "object_sha256" {
		t.Fatalf("hash: %v", err)
	}
}
