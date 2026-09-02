// Package pidfile_test exercises the atomic PID-file writer shared by the
// fake test providers.
package pidfile_test

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/tchori-labs/tchori/internal/provider/testprovider/pidfile"
)

// TestWriteLeavesOnlyFinalFile asserts that after Write returns, path holds
// exactly the PID text and no ".tmp" sibling is left behind.
func TestWriteLeavesOnlyFinalFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "provider.pid")

	if err := pidfile.Write(path, 4242); err != nil {
		t.Fatalf("Write: %v", err)
	}

	raw, err := os.ReadFile(path) //nolint:gosec // G304: path is the t.TempDir artifact created above
	if err != nil {
		t.Fatalf("reading PID file: %v", err)
	}
	if got, want := string(raw), strconv.Itoa(4242); got != want {
		t.Errorf("PID file content = %q, want %q", got, want)
	}

	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("expected no %q.tmp sibling, stat err = %v", path, err)
	}
}

// TestWriteNeverObservedEmpty guards the root cause of the flake fixed by
// this change: a concurrent reader polling path must never observe a
// zero-length file mid-write. A non-atomic os.WriteFile-based
// implementation creates the file empty and then writes its content,
// leaving a window where a reader sees len == 0 with a nil error; Write
// must never expose that window because it stages the content on a
// separate path and renames it into place.
func TestWriteNeverObservedEmpty(t *testing.T) {
	dir := t.TempDir()
	const writes = 200

	observedZeroCh := make(chan bool, 1)
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})

	go func() {
		defer close(doneCh)
		for {
			for i := range writes {
				path := filepath.Join(dir, strconv.Itoa(i)+".pid")
				raw, err := os.ReadFile(path) //nolint:gosec // G304: path is derived from the fixed writes loop below
				if err == nil && len(raw) == 0 {
					observedZeroCh <- true
					return
				}
			}
			select {
			case <-stopCh:
				observedZeroCh <- false
				return
			default:
			}
		}
	}()

	for i := range writes {
		path := filepath.Join(dir, strconv.Itoa(i)+".pid")
		if err := pidfile.Write(path, 1000+i); err != nil {
			t.Fatalf("Write(%d): %v", i, err)
		}
	}
	close(stopCh)
	<-doneCh

	if observedZero := <-observedZeroCh; observedZero {
		t.Fatal("reader observed a zero-length PID file mid-write")
	}
}
