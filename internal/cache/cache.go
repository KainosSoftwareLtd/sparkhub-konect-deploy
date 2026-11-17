// Package cache keeps verified Konect binaries on the data volume (design #1702 §5.2):
//
//	<dir>/<version>/konect             0500, the release binary
//	<dir>/<version>/manifest.json      0400, the exact signed bytes
//	<dir>/<version>/manifest.sig.json  0400
//	<dir>/state.json                   the pinned version and the previous one (pruning keeps both)
//	<dir>/.tmp-*                       downloads in progress, renamed into place when complete
//
// Every load re-runs the verifier contract and hashes the binary THROUGH the file descriptor that is then executed,
// so a rename or a new file between the check and the exec cannot change what runs.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/KainosSoftwareLtd/sparkhub-konect-deploy/internal/verify"
)

// File names inside a version directory.
const (
	BinaryName    = "konect"
	ManifestName  = "manifest.json"
	SignatureName = "manifest.sig.json"
	stateName     = "state.json"
	tmpPrefix     = ".tmp-"
)

// Cache is a cache directory.
type Cache struct{ Dir string }

// Entry is a verified binary, open read-only. The caller executes File or closes it.
type Entry struct {
	Version   string
	Path      string
	File      *os.File
	Manifest  []byte
	Signature []byte
	Result    *verify.Result
}

// SHA256 is the binary's hash (the manifest's, which the fd hash matched).
func (e *Entry) SHA256() string { return e.Result.Artifact.SHA256 }

// MismatchError is a cache entry that failed verification. The entry has been deleted.
type MismatchError struct {
	Version string
	Reason  string // a verifier rule, "missing_file", "size", "sha256", "not_regular"
	Want    string
	Got     string
}

func (e *MismatchError) Error() string {
	s := fmt.Sprintf("cached Konect %s failed verification (%s)", e.Version, e.Reason)
	if e.Want != "" {
		s += fmt.Sprintf(": want %s, got %s", e.Want, e.Got)
	}
	return s + "; deleted"
}

// Ensure creates the directory (0700) and checks that it is writable.
func (c Cache) Ensure() error {
	if err := os.MkdirAll(c.Dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(c.Dir, tmpPrefix+"probe-")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

// Load returns the verified entry for version, (nil, nil) when none is cached, or a *MismatchError after deleting a
// bad entry.
func (c Cache) Load(version string, keys []verify.Key, exp verify.Expect) (*Entry, error) {
	dir := filepath.Join(c.Dir, version)
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	e, mismatch, err := LoadDir(dir, filepath.Join(dir, BinaryName), keys, exp)
	if mismatch != nil {
		mismatch.Version = version
		_ = removeAll(dir)
		return nil, mismatch
	}
	if err != nil {
		return nil, err
	}
	e.Version = version
	return e, nil
}

// LoadDir verifies manifest.json + manifest.sig.json in dir and the binary at bin (opened without following a
// symlink, hashed through the returned fd). A verification failure is a *MismatchError; I/O errors are plain errors.
func LoadDir(dir, bin string, keys []verify.Key, exp verify.Expect) (*Entry, *MismatchError, error) {
	m, err := readCapped(filepath.Join(dir, ManifestName))
	if err != nil {
		return nil, &MismatchError{Reason: "missing_file"}, nil
	}
	s, err := readCapped(filepath.Join(dir, SignatureName))
	if err != nil {
		return nil, &MismatchError{Reason: "missing_file"}, nil
	}
	res, err := verify.Release(m, s, keys, exp)
	if err != nil {
		return nil, &MismatchError{Reason: verify.RuleOf(err)}, nil
	}
	f, err := os.OpenFile(bin, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, &MismatchError{Reason: "missing_file"}, nil
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, &MismatchError{Reason: "not_regular"}, nil
	}
	want := res.Artifact
	if fi.Size() != want.Size {
		f.Close()
		return nil, &MismatchError{Reason: "size", Want: fmt.Sprint(want.Size), Got: fmt.Sprint(fi.Size())}, nil
	}
	got, err := HashFD(f, want.Size)
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if got != want.SHA256 {
		f.Close()
		return nil, &MismatchError{Reason: "sha256", Want: want.SHA256, Got: got}, nil
	}
	return &Entry{Path: bin, File: f, Manifest: m, Signature: s, Result: res}, nil, nil
}

// HashFD hashes exactly the first size bytes of f with positional reads (the fd offset is not used) and fails
// when the file holds more or fewer.
func HashFD(f *os.File, size int64) (string, error) {
	h := sha256.New()
	n, err := io.Copy(h, io.NewSectionReader(f, 0, size+1))
	if err != nil {
		return "", err
	}
	if n != size {
		return fmt.Sprintf("size %d", n), nil
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// NewTemp creates a private temporary directory inside the cache, for a download.
func (c Cache) NewTemp() (string, error) { return os.MkdirTemp(c.Dir, tmpPrefix) }

// Commit renames a completed temporary directory to <dir>/<version> and loads it. If another replica won the
// rename, its entry is verified and used; a bad one is replaced by ours.
func (c Cache) Commit(tmp, version string, keys []verify.Key, exp verify.Expect) (*Entry, error) {
	final := filepath.Join(c.Dir, version)
	for attempt := 0; attempt < 2; attempt++ {
		err := os.Rename(tmp, final)
		if err == nil {
			_ = syncDir(c.Dir)
			return c.Load(version, keys, exp)
		}
		if !errors.Is(err, os.ErrExist) && !errors.Is(err, syscall.ENOTEMPTY) && !errors.Is(err, syscall.EEXIST) {
			return nil, err
		}
		theirs, lerr := c.Load(version, keys, exp)
		if lerr == nil && theirs != nil {
			_ = removeAll(tmp)
			return theirs, nil
		}
		// theirs was bad (and is now deleted) or vanished: try our rename again.
	}
	return nil, fmt.Errorf("could not place the verified binary in the cache")
}

type state struct {
	Current  string `json:"current"`
	Previous string `json:"previous,omitempty"`
}

// Prune records version as the current one and deletes every other version except the previous current one
// (rollback within a migration-compatible range starts without a download), and stale temporary directories.
// It returns the versions it deleted.
func (c Cache) Prune(version string) ([]string, error) {
	var st state
	if b, err := readCapped(filepath.Join(c.Dir, stateName)); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	if st.Current != version {
		if st.Current != "" {
			st.Previous = st.Current
		}
		st.Current = version
		if err := writeAtomic(filepath.Join(c.Dir, stateName), mustJSON(st)); err != nil {
			return nil, err
		}
	}
	entries, err := os.ReadDir(c.Dir)
	if err != nil {
		return nil, err
	}
	var deleted []string
	for _, e := range entries {
		name := e.Name()
		switch {
		case name == stateName || name == st.Current || (st.Previous != "" && name == st.Previous):
		case strings.HasPrefix(name, tmpPrefix):
			if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > time.Hour {
				_ = removeAll(filepath.Join(c.Dir, name))
			}
		case e.IsDir():
			if removeAll(filepath.Join(c.Dir, name)) == nil {
				deleted = append(deleted, name)
			}
		}
	}
	return deleted, nil
}

// WriteRelease writes the binary's companions into dir (0400 files) and syncs them.
func WriteRelease(dir string, manifest, signature []byte) error {
	for name, b := range map[string][]byte{ManifestName: manifest, SignatureName: signature} {
		if err := writeSynced(filepath.Join(dir, name), b, 0o400); err != nil {
			return err
		}
	}
	return nil
}

func writeSynced(path string, b []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func writeAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	_ = os.Remove(tmp)
	if err := writeSynced(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// SyncDir flushes a directory entry (after writing files into it).
func SyncDir(dir string) error { return syncDir(dir) }

func readCapped(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, verify.MaxFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(b) > verify.MaxFileSize {
		return nil, errors.New("file too large")
	}
	return b, nil
}

// removeAll deletes a directory whose files may be read-only (0500/0400): it restores owner write on the
// directories first, which unlink needs on some filesystems' permission checks.
func removeAll(path string) error {
	_ = filepath.Walk(path, func(p string, info os.FileInfo, err error) error {
		if err == nil && info.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	return os.RemoveAll(path)
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
