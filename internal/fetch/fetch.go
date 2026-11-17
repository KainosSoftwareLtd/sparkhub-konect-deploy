// Package fetch downloads a release object from a presigned URL and unpacks it, with every size capped by the
// verified manifest (design #1702 §5.2 step 4): the download at objectSize, the gunzip at size.
package fetch

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
)

// ErrExpired is an S3 403 on the download: the presigned URL expired (or its signer did). Resolve once more.
var ErrExpired = errors.New("download URL expired")

// IntegrityError is bytes that do not match the verified manifest. They are never run.
type IntegrityError struct {
	What      string // object_size, object_sha256, size, sha256, gzip
	Want, Got string
}

func (e *IntegrityError) Error() string {
	if e.Want == "" {
		return "release bytes do not match the manifest: " + e.What
	}
	return fmt.Sprintf("release bytes do not match the manifest: %s (want %s, got %s)", e.What, e.Want, e.Got)
}

// NetError is a network failure or an unexpected status: worth a retry.
type NetError struct {
	Status int
	Err    error
}

func (e *NetError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("download answered HTTP %d", e.Status)
	}
	return "download failed: " + e.Err.Error()
}

func (e *NetError) Unwrap() error { return e.Err }

// Download streams rawURL (no auth header: the URL is presigned) to dst, created exclusively with mode 0600,
// capped at objectSize, and checks objectSHA256. The URL never appears in an error.
func Download(ctx context.Context, client *http.Client, rawURL, dst string, objectSize int64, objectSHA256 string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return &NetError{Err: errors.New("invalid download URL")}
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &NetError{Err: scrub(err)}
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusForbidden:
		return ErrExpired
	case resp.StatusCode != http.StatusOK:
		return &NetError{Status: resp.StatusCode}
	}
	if resp.ContentLength > objectSize {
		return &IntegrityError{What: "object_size", Want: fmt.Sprint(objectSize), Got: fmt.Sprint(resp.ContentLength)}
	}
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, objectSize+1))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &NetError{Err: scrub(err)}
	}
	if n > objectSize {
		return &IntegrityError{What: "object_size", Want: fmt.Sprint(objectSize), Got: fmt.Sprintf("more than %d", objectSize)}
	}
	if n < objectSize {
		return &NetError{Err: fmt.Errorf("download ended after %d of %d bytes", n, objectSize)}
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != objectSHA256 {
		return &IntegrityError{What: "object_sha256", Want: objectSHA256, Got: got}
	}
	return f.Close()
}

// Gunzip unpacks src into dst (created exclusively, mode 0600), capped at size, and checks sha256.
func Gunzip(src, dst string, size int64, sha string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	zr, err := gzip.NewReader(in)
	if err != nil {
		return &IntegrityError{What: "gzip"}
	}
	defer zr.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(zr, size+1))
	if err != nil {
		return &IntegrityError{What: "gzip"}
	}
	if n != size {
		return &IntegrityError{What: "size", Want: fmt.Sprint(size), Got: fmt.Sprint(n)}
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sha {
		return &IntegrityError{What: "sha256", Want: sha, Got: got}
	}
	if err := out.Sync(); err != nil {
		return err
	}
	return out.Close()
}

func scrub(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s: %w", ue.Op, ue.Err)
	}
	return err
}
