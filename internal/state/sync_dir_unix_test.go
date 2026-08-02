//go:build !windows

package state

import (
	"strings"
	"testing"
)

// TestDefaultSyncDirSucceedsOnRealDirectory proves the POSIX defaultSyncDir
// is a real, working directory-fsync barrier: given a real directory it
// returns nil (as opposed to the documented Windows no-op).
func TestDefaultSyncDirSucceedsOnRealDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := defaultSyncDir(dir); err != nil {
		t.Fatalf("defaultSyncDir(%q) = %v, want nil", dir, err)
	}
}

// TestDefaultSyncDirNonexistentDirectory proves defaultSyncDir still
// reports a real, actionable error when the directory cannot be opened —
// unlike the Windows no-op, which never fails.
func TestDefaultSyncDirNonexistentDirectory(t *testing.T) {
	dir := t.TempDir() + "/does-not-exist"
	err := defaultSyncDir(dir)
	if err == nil {
		t.Fatalf("defaultSyncDir(%q) = nil, want error", dir)
	}
	if !strings.Contains(err.Error(), "open directory") {
		t.Fatalf("defaultSyncDir(%q) error = %q, want it to contain %q", dir, err.Error(), "open directory")
	}
}
