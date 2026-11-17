//go:build linux && (amd64 || arm64)

package execfd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func fileHash(t *testing.T, path string) (int64, string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := sha256.Sum256(b)
	return int64(len(b)), hex.EncodeToString(s[:])
}

// TestHelperExec runs in a child process: it execs the binary named by EXECFD_BIN.
func TestHelperExec(t *testing.T) {
	if os.Getenv("EXECFD_HELPER") == "" {
		t.Skip("helper process")
	}
	path := os.Getenv("EXECFD_BIN")
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(3)
	}
	size, sha := fileHash(t, path)
	forceMemfd = os.Getenv("EXECFD_MEMFD") == "1"
	err = Exec(f, size, sha, []string{"echo", "hello-from-exec"}, []string{"PATH=/usr/bin:/bin"}, func(p string) {
		fmt.Fprintln(os.Stderr, "path="+p)
	})
	fmt.Fprintln(os.Stderr, "exec failed:", err)
	os.Exit(3)
}

func TestExec(t *testing.T) {
	echo, err := exec.LookPath("echo")
	if err != nil {
		t.Skip("no echo binary")
	}
	src, err := os.ReadFile(echo)
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "konect")
	if err := os.WriteFile(bin, src, 0o500); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []struct{ memfd, path string }{{"0", "path=fd"}, {"1", "path=memfd"}} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperExec$")
		cmd.Env = append(os.Environ(), "EXECFD_HELPER=1", "EXECFD_BIN="+bin, "EXECFD_MEMFD="+mode.memfd)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("memfd=%s: %v\n%s", mode.memfd, err, stderr.String())
		}
		if stdout.String() != "hello-from-exec\n" {
			t.Fatalf("memfd=%s: stdout %q, stderr %s", mode.memfd, stdout.String(), stderr.String())
		}
		if !strings.Contains(stderr.String(), mode.path) {
			t.Fatalf("memfd=%s: expected %s in %q", mode.memfd, mode.path, stderr.String())
		}
	}
}

// The memory copy is hashed again: a hash that does not match is never executed.
func TestSealedCopyChecksHash(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "konect")
	if err := os.WriteFile(bin, []byte("not the verified bytes"), 0o500); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(bin)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := sealedCopy(f, 22, strings.Repeat("0", 64)); err == nil {
		t.Fatal("a copy with the wrong hash was accepted")
	}
	size, sha := fileHash(t, bin)
	ro, err := sealedCopy(f, size, sha)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	// Sealed: no write can change it any more, even through a new writable open.
	if w, err := os.OpenFile(fmt.Sprintf(procSelfFDFormat, ro.Fd()), os.O_WRONLY, 0); err == nil {
		_, werr := w.Write([]byte("x"))
		w.Close()
		if werr == nil {
			t.Fatal("sealed memfd accepted a write")
		}
	}
}
