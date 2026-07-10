package state

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadMissing verifies Load returns an empty, well-formed state when
// no state file exists yet: FormatVersion "1.0", Serial 0, empty Resources.
func TestLoadMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load(%q) = %v, want nil error", path, err)
	}
	if s.FormatVersion != "1.0" {
		t.Fatalf("FormatVersion = %q, want %q", s.FormatVersion, "1.0")
	}
	if s.Serial != 0 {
		t.Fatalf("Serial = %d, want 0", s.Serial)
	}
	if s.Resources == nil {
		t.Fatal("Resources is nil, want empty non-nil map")
	}
	if len(s.Resources) != 0 {
		t.Fatalf("len(Resources) = %d, want 0", len(s.Resources))
	}
}

// TestSaveLoadRoundTrip verifies Save persists a state that Load can read
// back, and that each Save increments Serial (both in memory on the saved
// *State and on the value obtained from a subsequent Load).
func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load(missing) = %v", err)
	}
	if s.Serial != 0 {
		t.Fatalf("Load(missing).Serial = %d, want 0", s.Serial)
	}

	s.Resources["null_resource.demo"] = &ResourceState{
		Type:       "null_resource",
		Provider:   "null",
		Attributes: json.RawMessage(`{"id":"1"}`),
	}
	if err := s.Save(path); err != nil {
		t.Fatalf("Save #1 = %v", err)
	}
	if s.Serial != 1 {
		t.Fatalf("Serial after first Save = %d, want 1", s.Serial)
	}

	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load after first Save = %v", err)
	}
	if reloaded.FormatVersion != "1.0" {
		t.Fatalf("Load after first Save: FormatVersion = %q, want %q", reloaded.FormatVersion, "1.0")
	}
	if reloaded.Serial != 1 {
		t.Fatalf("Load after first Save: Serial = %d, want 1", reloaded.Serial)
	}
	if len(reloaded.Resources) != 1 {
		t.Fatalf("Load after first Save: len(Resources) = %d, want 1", len(reloaded.Resources))
	}

	if err := reloaded.Save(path); err != nil {
		t.Fatalf("Save #2 = %v", err)
	}
	if reloaded.Serial != 2 {
		t.Fatalf("Serial after second Save = %d, want 2", reloaded.Serial)
	}

	reloaded2, err := Load(path)
	if err != nil {
		t.Fatalf("Load after second Save = %v", err)
	}
	if reloaded2.Serial != 2 {
		t.Fatalf("Load after second Save: Serial = %d, want 2", reloaded2.Serial)
	}
}

// TestSaveDeterministicAcrossInsertionOrder is the determinism golden test:
// two logically identical states, whose Resources maps are populated in a
// different Go-source insertion order, must Save to byte-identical files.
// encoding/json sorts map keys during marshaling, so this holds regardless
// of insertion order — this test pins that guarantee down at the state
// package's own serialization boundary.
func TestSaveDeterministicAcrossInsertionOrder(t *testing.T) {
	data := map[string]*ResourceState{
		"aaa_thing.alpha": {
			Type:       "aaa_thing",
			Provider:   "aaa",
			Attributes: json.RawMessage(`{"name":"alpha"}`),
		},
		"bbb_thing.beta": {
			Type:       "bbb_thing",
			Provider:   "bbb",
			Attributes: json.RawMessage(`{"name":"beta"}`),
			Private:    []byte("secret"),
		},
		"ccc_thing.gamma": {
			Type:       "ccc_thing",
			Provider:   "ccc",
			Attributes: json.RawMessage(`{"name":"gamma"}`),
		},
	}
	build := func(order []string) *State {
		s := &State{
			FormatVersion: "1.0",
			Resources:     map[string]*ResourceState{},
		}
		for _, k := range order {
			s.Resources[k] = data[k]
		}
		return s
	}

	stateA := build([]string{"aaa_thing.alpha", "bbb_thing.beta", "ccc_thing.gamma"})
	stateB := build([]string{"ccc_thing.gamma", "aaa_thing.alpha", "bbb_thing.beta"})

	dir := t.TempDir()
	pathA := filepath.Join(dir, "a", "state.json")
	pathB := filepath.Join(dir, "b", "state.json")
	if err := os.MkdirAll(filepath.Dir(pathA), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(pathB), 0o750); err != nil {
		t.Fatal(err)
	}

	if err := stateA.Save(pathA); err != nil {
		t.Fatalf("Save(pathA) = %v", err)
	}
	if err := stateB.Save(pathB); err != nil {
		t.Fatalf("Save(pathB) = %v", err)
	}

	bytesA, err := os.ReadFile(pathA) //nolint:gosec // G304: test-controlled path under t.TempDir(), not attacker input
	if err != nil {
		t.Fatal(err)
	}
	bytesB, err := os.ReadFile(pathB) //nolint:gosec // G304: test-controlled path under t.TempDir(), not attacker input
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytesA, bytesB) {
		t.Fatalf("state files differ despite identical logical content:\n--- A ---\n%s\n--- B ---\n%s", bytesA, bytesB)
	}
}

// TestSaveWritesBackupOnSecondSave verifies path+".backup" is absent after
// the first Save (nothing existed yet to back up) and present — holding
// the pre-overwrite content — after the second Save.
func TestSaveWritesBackupOnSecondSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	backupPath := path + ".backup"

	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load(missing) = %v", err)
	}
	if err := s.Save(path); err != nil {
		t.Fatalf("Save #1 = %v", err)
	}
	if _, err := os.Stat(backupPath); !os.IsNotExist(err) {
		t.Fatalf("os.Stat(backup) after first Save: err = %v, want IsNotExist", err)
	}

	firstSaveContent, err := os.ReadFile(path) //nolint:gosec // G304: test-controlled path under t.TempDir(), not attacker input
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Save(path); err != nil {
		t.Fatalf("Save #2 = %v", err)
	}
	if _, err := os.Stat(backupPath); err != nil {
		t.Fatalf("os.Stat(backup) after second Save = %v, want file to exist", err)
	}

	gotBackup, err := os.ReadFile(backupPath) //nolint:gosec // G304: test-controlled path under t.TempDir(), not attacker input
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotBackup, firstSaveContent) {
		t.Fatalf("backup content = %q, want first save's content %q", gotBackup, firstSaveContent)
	}
}

// TestLoadRejectsUnsupportedFormatVersion mirrors plan.Read's rejection of a
// plan.json whose format_version isn't the one this engine understands:
// Load must refuse an existing state.json with a foreign format_version
// rather than silently proceeding against a document it may not be able to
// interpret correctly.
func TestLoadRejectsUnsupportedFormatVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte(`{"format_version":"2.0","serial":1,"resources":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load accepted an unsupported format_version")
	}
	if !strings.Contains(err.Error(), `"2.0"`) {
		t.Errorf("error %q does not name the unsupported format_version", err.Error())
	}
}

// TestLoadRejectsMissingFormatVersion verifies an existing state.json with
// no format_version field at all is also rejected — this is a state file
// tchori did not write (Save always stamps "1.0"), so Load should not treat
// it as compatible just because a fresh (nonexistent) state file is fine.
func TestLoadRejectsMissingFormatVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte(`{"serial":1,"resources":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted a state.json with no format_version")
	}
}
