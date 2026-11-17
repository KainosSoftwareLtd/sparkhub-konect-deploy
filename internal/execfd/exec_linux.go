//go:build linux && (amd64 || arm64)

// Package execfd replaces the process with the program behind an open, already verified file descriptor
// (design #1702 §5.2 step 7):
//
//  1. execveat(fd, "", argv, envp, AT_EMPTY_PATH): runs exactly the open file;
//  2. /proc/self/fd/<fd> when execveat is missing or filtered (ENOSYS, EPERM): the same open file;
//  3. when the cache mount is noexec (EACCES): copy the verified bytes into a memfd, hash the copy again,
//     seal it against writes and exec that.
package execfd

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

const (
	atEmptyPath      = 0x1000
	mfdCloexec       = 0x1
	mfdAllowSealing  = 0x2
	mfdExec          = 0x10
	fAddSeals        = 1033
	fSealSeal        = 0x1
	fSealShrink      = 0x2
	fSealGrow        = 0x4
	fSealWrite       = 0x8
	sealAll          = fSealSeal | fSealShrink | fSealGrow | fSealWrite
	procSelfFDFormat = "/proc/self/fd/%d"
)

// forceMemfd makes Exec skip the fd paths (tests only).
var forceMemfd bool

// Supported reports whether this platform can run Konect from the bootstrap.
const Supported = true

// Exec runs the verified file f (size bytes, SHA-256 sha) with argv and env. It returns only on failure.
// note is called with the path taken ("fd", "proc-fd", "memfd") just before each exec attempt.
func Exec(f *os.File, size int64, sha string, argv, env []string, note func(path string)) error {
	fd := int(f.Fd())
	if !forceMemfd {
		note("fd")
		err := execveat(fd, argv, env)
		if errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.EPERM) {
			note("proc-fd")
			err = syscall.Exec(fmt.Sprintf(procSelfFDFormat, fd), argv, env)
		}
		if !errors.Is(err, syscall.EACCES) {
			return fmt.Errorf("exec Konect: %w", err)
		}
	}
	ro, err := sealedCopy(f, size, sha)
	if err != nil {
		return fmt.Errorf("exec Konect from memory (the cache mount is noexec): %w", err)
	}
	note("memfd")
	err = execveat(int(ro.Fd()), argv, env)
	if errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.EPERM) {
		err = syscall.Exec(fmt.Sprintf(procSelfFDFormat, ro.Fd()), argv, env)
	}
	ro.Close()
	return fmt.Errorf("exec Konect from memory: %w", err)
}

// sealedCopy copies the verified bytes into a memfd, hashes the copy, seals it and returns a read-only fd of it
// (an exec of a file that is open for writing fails with ETXTBSY).
func sealedCopy(f *os.File, size int64, sha string) (*os.File, error) {
	mfd, err := memfdCreate("konect", mfdCloexec|mfdAllowSealing|mfdExec)
	if errors.Is(err, syscall.EINVAL) { // kernels before 6.3 do not know MFD_EXEC
		mfd, err = memfdCreate("konect", mfdCloexec|mfdAllowSealing)
	}
	if err != nil {
		return nil, fmt.Errorf("memfd_create: %w", err)
	}
	mf := os.NewFile(uintptr(mfd), "memfd:konect")
	defer mf.Close()
	if _, err := io.Copy(mf, io.NewSectionReader(f, 0, size)); err != nil {
		return nil, err
	}
	h := sha256.New()
	n, err := io.Copy(h, io.NewSectionReader(mf, 0, size+1))
	if err != nil {
		return nil, err
	}
	if n != size || hex.EncodeToString(h.Sum(nil)) != sha {
		return nil, errors.New("the memory copy does not match the verified hash")
	}
	if _, _, e := syscall.Syscall(syscall.SYS_FCNTL, uintptr(mfd), fAddSeals, sealAll); e != 0 {
		return nil, fmt.Errorf("seal: %w", e)
	}
	return os.Open(fmt.Sprintf(procSelfFDFormat, mfd))
}

func memfdCreate(name string, flags uintptr) (int, error) {
	p, err := syscall.BytePtrFromString(name)
	if err != nil {
		return -1, err
	}
	fd, _, e := syscall.Syscall(sysMemfdCreate, uintptr(unsafe.Pointer(p)), flags, 0)
	if e != 0 {
		return -1, e
	}
	return int(fd), nil
}

func execveat(fd int, argv, env []string) error {
	argvp, err := syscall.SlicePtrFromStrings(argv)
	if err != nil {
		return err
	}
	envp, err := syscall.SlicePtrFromStrings(env)
	if err != nil {
		return err
	}
	empty, err := syscall.BytePtrFromString("")
	if err != nil {
		return err
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	_, _, e := syscall.RawSyscall6(sysExecveat, uintptr(fd), uintptr(unsafe.Pointer(empty)),
		uintptr(unsafe.Pointer(&argvp[0])), uintptr(unsafe.Pointer(&envp[0])), atEmptyPath, 0)
	runtime.KeepAlive(argvp)
	runtime.KeepAlive(envp)
	runtime.KeepAlive(empty)
	if e != 0 {
		return e
	}
	return nil
}
