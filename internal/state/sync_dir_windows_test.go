//go:build windows

package state

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

// TestDefaultSyncDirNoOpOnWindows is the symptom-is-gone assertion: before
// the fix, defaultSyncDir opened the directory read-only and called Sync,
// which maps to FlushFileBuffers and fails with access-denied on a
// read-only handle. The Windows implementation is a documented no-op and
// must return nil for any directory, including a real one.
func TestDefaultSyncDirNoOpOnWindows(t *testing.T) {
	dir := t.TempDir()
	if err := defaultSyncDir(dir); err != nil {
		t.Fatalf("defaultSyncDir(%q) = %v, want nil (documented no-op on Windows)", dir, err)
	}
}

// TestSaveSucceedsOnWindowsWithProductionSeams reproduces the original
// symptom end-to-end with zero seam injection: before the fix, Save
// committed the atomic rename and then returned a non-nil
// "sync state directory ..." error on every call. It must now return nil,
// and the saved state must round-trip via Load.
func TestSaveSucceedsOnWindowsWithProductionSeams(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load(missing) = %v", err)
	}
	s.Resources["null_resource.demo"] = &ResourceState{
		Type:       "null_resource",
		Provider:   "null",
		Attributes: json.RawMessage(`{"id":"1"}`),
	}

	if err := s.Save(path); err != nil {
		t.Fatalf("Save on Windows with production seams = %v, want nil", err)
	}

	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load after Save = %v", err)
	}
	if reloaded.Serial != 1 {
		t.Fatalf("Load after Save: Serial = %d, want 1", reloaded.Serial)
	}
	if len(reloaded.Resources) != 1 {
		t.Fatalf("Load after Save: len(Resources) = %d, want 1", len(reloaded.Resources))
	}
}
