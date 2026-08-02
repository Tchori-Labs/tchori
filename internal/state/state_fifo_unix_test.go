//go:build unix

package state

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

func TestSaveRejectsFifoLockSidecar(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	lockPath := path + ".lock"
	if err := syscall.Mkfifo(lockPath, 0o600); err != nil {
		t.Skipf("mkfifo unsupported: %v", err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Save(path)
	if err == nil || !strings.Contains(err.Error(), lockPath) {
		t.Fatalf("Save error = %v, want error naming %q", err, lockPath)
	}
}

func TestLockOpenFlagsIncludeUnixGuards(t *testing.T) {
	flags := lockOpenFlags()
	if flags&syscall.O_NOFOLLOW == 0 {
		t.Fatal("lockOpenFlags lacks O_NOFOLLOW")
	}
	if flags&syscall.O_NONBLOCK == 0 {
		t.Fatal("lockOpenFlags lacks O_NONBLOCK")
	}
}

func TestLockOpenFlagsDoNotBlockOnFifo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json.lock")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unsupported: %v", err)
	}
	lock := flock.New(path, flock.SetFlag(lockOpenFlags()))
	defer func() { _ = lock.Close() }()
	type result struct {
		locked bool
		err    error
	}
	resultCh := make(chan result, 1)
	go func() {
		locked, err := lock.TryLock()
		resultCh <- result{locked: locked, err: err}
	}()
	select {
	case got := <-resultCh:
		if got.err != nil || !got.locked {
			t.Fatalf("TryLock = %v, %v, want acquired FIFO descriptor", got.locked, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("lock open blocked on filesystem FIFO")
	}

	heldInfo, err := lock.Stat()
	if err != nil {
		t.Fatal(err)
	}
	pathInfo, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(heldInfo, pathInfo) {
		t.Fatal("FIFO descriptor and lock path differ; want SameFile true")
	}
	if heldInfo.Mode().IsRegular() {
		t.Fatalf("held FIFO mode = %v, want non-regular", heldInfo.Mode())
	}
	if err := verifyLockedSidecar(lock, path); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("verifyLockedSidecar error = %v, want error naming %q", err, path)
	}
}
