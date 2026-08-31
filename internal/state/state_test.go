package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gofrs/flock"
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
	data, err := os.ReadFile(path) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("incomplete_apply")) {
		t.Fatalf("converged state contains incomplete_apply: %s", data)
	}
}

func TestIncompleteApplyRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := &State{
		Resources: map[string]*ResourceState{},
		Incomplete: &IncompleteApply{
			FailedAddress: "thing.boom",
			Applied:       []string{"thing.alpha"},
			Remaining:     []string{"thing.boom"},
		},
	}
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Incomplete == nil || got.Incomplete.FailedAddress != "thing.boom" ||
		len(got.Incomplete.Applied) != 1 || got.Incomplete.Applied[0] != "thing.alpha" ||
		len(got.Incomplete.Remaining) != 1 || got.Incomplete.Remaining[0] != "thing.boom" {
		t.Fatalf("Incomplete after round trip = %+v", got.Incomplete)
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
	assertOwnerReadWriteOnly(t, backupPath)
}

// TestSaveRetightensPermissiveBackup verifies Save overwrites stale backup
// content and forces a pre-existing permissive backup back to mode 0600.
func TestSaveRetightensPermissiveBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	backupPath := path + ".backup"

	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load(missing) = %v", err)
	}
	s.Resources["thing.example"] = &ResourceState{
		Type:       "thing",
		Provider:   "test",
		Attributes: json.RawMessage(`{"value":"before"}`),
		Private:    []byte("sensitive state"),
	}
	if err := s.Save(path); err != nil {
		t.Fatalf("Save #1 = %v", err)
	}
	firstSaveContent, err := os.ReadFile(path) //nolint:gosec // G304: test-controlled path under t.TempDir(), not attacker input
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(backupPath, []byte("stale backup"), 0o666); err != nil { //nolint:gosec // G306: permissive mode reproduces the regression
		t.Fatalf("seed permissive backup = %v", err)
	}
	if err := os.Chmod(backupPath, 0o666); err != nil { //nolint:gosec // G302: explicit chmod defeats the process umask in this regression test
		t.Fatalf("chmod seeded backup = %v", err)
	}

	s.Resources["thing.example"].Attributes = json.RawMessage(`{"value":"after"}`)
	if err := s.Save(path); err != nil {
		t.Fatalf("Save #2 = %v", err)
	}
	gotBackup, err := os.ReadFile(backupPath) //nolint:gosec // G304: test-controlled path under t.TempDir(), not attacker input
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotBackup, firstSaveContent) {
		t.Fatalf("backup content = %q, want first save's content %q", gotBackup, firstSaveContent)
	}
	assertOwnerReadWriteOnly(t, backupPath)
}

func assertOwnerReadWriteOnly(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Log("skipping backup permission assertion on Windows")
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("os.Stat(%q) = %v", path, err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("backup permissions = %o, want 600", got)
	}
}

// TestSaveDurabilityBarrierOrder proves Save does not report success until the
// temp file has been synced, the rename has completed, and the directory has
// been synced, in that order.
func TestSaveDurabilityBarrierOrder(t *testing.T) {
	originalFsyncFile := fsyncFile
	originalCloseFile := closeFile
	originalRenameFile := renameFile
	originalSyncDir := syncDir
	defer func() {
		fsyncFile = originalFsyncFile
		closeFile = originalCloseFile
		renameFile = originalRenameFile
		syncDir = originalSyncDir
	}()

	var operations []string
	fsyncFile = func(f *os.File) error {
		operations = append(operations, "temp-file-sync")
		return originalFsyncFile(f)
	}
	renameFile = func(oldPath, newPath string) error {
		operations = append(operations, "rename")
		return originalRenameFile(oldPath, newPath)
	}
	syncDir = func(dir string) error {
		operations = append(operations, "dir-sync")
		return originalSyncDir(dir)
	}

	path := filepath.Join(t.TempDir(), "state.json")
	s := &State{Resources: map[string]*ResourceState{
		"thing.example": {
			Type:       "thing",
			Provider:   "test",
			Attributes: json.RawMessage(`{"value":"durable"}`),
		},
	}}
	if err := s.Save(path); err != nil {
		t.Fatalf("Save = %v", err)
	}

	want := "temp-file-sync,rename,dir-sync"
	if got := strings.Join(operations, ","); got != want {
		t.Fatalf("durability operations = %q, want %q", got, want)
	}
}

// TestSaveTempSyncFailure verifies a failed pre-rename data barrier is
// actionable and leaves the old state untouched with no temporary file.
func TestSaveTempSyncFailure(t *testing.T) {
	originalFsyncFile := fsyncFile
	originalCloseFile := closeFile
	originalRenameFile := renameFile
	originalSyncDir := syncDir
	defer func() {
		fsyncFile = originalFsyncFile
		closeFile = originalCloseFile
		renameFile = originalRenameFile
		syncDir = originalSyncDir
	}()

	path := filepath.Join(t.TempDir(), "state.json")
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(path); err != nil {
		t.Fatalf("seed Save = %v", err)
	}
	before, err := os.ReadFile(path) //nolint:gosec // G304: test-controlled path under t.TempDir(), not attacker input
	if err != nil {
		t.Fatal(err)
	}

	s.Resources["thing.new"] = &ResourceState{Type: "thing", Provider: "test", Attributes: json.RawMessage(`{}`)}
	syncFailure := errors.New("injected temp fsync failure")
	fsyncFile = func(*os.File) error { return syncFailure }
	err = s.Save(path)
	if !errors.Is(err, syncFailure) || !strings.Contains(err.Error(), "sync temp state file") {
		t.Fatalf("Save error = %v, want actionable temp sync failure", err)
	}
	if s.Serial != 1 {
		t.Fatalf("Serial after pre-rename failure = %d, want unchanged serial 1", s.Serial)
	}
	assertStateFileUnchanged(t, path, before)
	assertNoTempStateFiles(t, filepath.Dir(path))
}

// TestSaveCloseFailure verifies a failed close after fsync is surfaced and the
// still-pre-rename temporary file is removed without replacing old state.
func TestSaveCloseFailure(t *testing.T) {
	originalFsyncFile := fsyncFile
	originalCloseFile := closeFile
	originalRenameFile := renameFile
	originalSyncDir := syncDir
	defer func() {
		fsyncFile = originalFsyncFile
		closeFile = originalCloseFile
		renameFile = originalRenameFile
		syncDir = originalSyncDir
	}()

	path := filepath.Join(t.TempDir(), "state.json")
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(path); err != nil {
		t.Fatalf("seed Save = %v", err)
	}
	before, err := os.ReadFile(path) //nolint:gosec // G304: test-controlled path under t.TempDir(), not attacker input
	if err != nil {
		t.Fatal(err)
	}

	closeFailure := errors.New("injected temp close failure")
	closeFile = func(f *os.File) error {
		if err := originalCloseFile(f); err != nil {
			return err
		}
		return closeFailure
	}
	err = s.Save(path)
	if !errors.Is(err, closeFailure) || !strings.Contains(err.Error(), "close temp state file") {
		t.Fatalf("Save error = %v, want actionable temp close failure", err)
	}
	if s.Serial != 1 {
		t.Fatalf("Serial after pre-rename failure = %d, want unchanged serial 1", s.Serial)
	}
	assertStateFileUnchanged(t, path, before)
	assertNoTempStateFiles(t, filepath.Dir(path))
}

// TestSaveRenameFailure verifies an atomic replacement failure leaves the old
// state intact and removes the already-synced temporary file.
func TestSaveRenameFailure(t *testing.T) {
	originalFsyncFile := fsyncFile
	originalCloseFile := closeFile
	originalRenameFile := renameFile
	originalSyncDir := syncDir
	defer func() {
		fsyncFile = originalFsyncFile
		closeFile = originalCloseFile
		renameFile = originalRenameFile
		syncDir = originalSyncDir
	}()

	path := filepath.Join(t.TempDir(), "state.json")
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(path); err != nil {
		t.Fatalf("seed Save = %v", err)
	}
	before, err := os.ReadFile(path) //nolint:gosec // G304: test-controlled path under t.TempDir(), not attacker input
	if err != nil {
		t.Fatal(err)
	}

	renameFailure := errors.New("injected rename failure")
	renameFile = func(string, string) error { return renameFailure }
	err = s.Save(path)
	if !errors.Is(err, renameFailure) || !strings.Contains(err.Error(), "rename temp state file") {
		t.Fatalf("Save error = %v, want actionable rename failure", err)
	}
	if s.Serial != 1 {
		t.Fatalf("Serial after pre-rename failure = %d, want unchanged serial 1", s.Serial)
	}
	assertStateFileUnchanged(t, path, before)
	assertNoTempStateFiles(t, filepath.Dir(path))
}

// TestSaveDirectorySyncFailure proves the post-rename barrier gates reported
// success without rolling back a state file whose rename already took effect.
func TestSaveDirectorySyncFailure(t *testing.T) {
	originalFsyncFile := fsyncFile
	originalCloseFile := closeFile
	originalRenameFile := renameFile
	originalSyncDir := syncDir
	defer func() {
		fsyncFile = originalFsyncFile
		closeFile = originalCloseFile
		renameFile = originalRenameFile
		syncDir = originalSyncDir
	}()

	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	s := &State{Resources: map[string]*ResourceState{
		"thing.renamed": {Type: "thing", Provider: "test", Attributes: json.RawMessage(`{}`)},
	}}
	dirSyncFailure := errors.New("injected directory fsync failure")
	syncDir = func(gotDir string) error {
		if gotDir != dir {
			t.Fatalf("syncDir(%q), want %q", gotDir, dir)
		}
		return dirSyncFailure
	}

	err := s.Save(path)
	if !errors.Is(err, dirSyncFailure) || !strings.Contains(err.Error(), "sync state directory "+dir) {
		t.Fatalf("Save error = %v, want actionable directory sync failure", err)
	}
	persisted, loadErr := Load(path)
	if loadErr != nil {
		t.Fatalf("Load renamed state = %v", loadErr)
	}
	if persisted.Serial != 1 || persisted.Resources["thing.renamed"] == nil {
		t.Fatalf("renamed state not preserved after directory sync failure: %#v", persisted)
	}
	if s.Serial != 1 {
		t.Fatalf("Serial after post-rename failure = %d, want visible serial 1", s.Serial)
	}

	// The rename already committed serial 1. The in-memory CAS base must match
	// it so a retry can complete normally rather than report a false concurrent
	// modification.
	syncDir = originalSyncDir
	if err := s.Save(path); err != nil {
		t.Fatalf("Save retry after directory sync failure = %v", err)
	}
	if s.Serial != 2 {
		t.Fatalf("Serial after retry = %d, want 2", s.Serial)
	}
	assertNoTempStateFiles(t, dir)
}

func assertStateFileUnchanged(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path) //nolint:gosec // G304: test-controlled path under t.TempDir(), not attacker input
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("state file changed on failed Save:\n--- before ---\n%s\n--- after ---\n%s", want, got)
	}
}

func assertNoTempStateFiles(t *testing.T, dir string) {
	t.Helper()
	temps, err := filepath.Glob(filepath.Join(dir, ".state-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 0 {
		t.Fatalf("temporary state files remain: %v", temps)
	}
}

// TestSaveRejectsConcurrentModification reproduces the lost-update race with
// two independently loaded State values. The stale writer must fail without
// changing the winner, its backup, file modes, or its own serial.
func TestSaveRejectsConcurrentModification(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	backupPath := path + ".backup"

	seed, err := Load(path)
	if err != nil {
		t.Fatalf("Load(missing) = %v", err)
	}
	if err := seed.Save(path); err != nil {
		t.Fatalf("seed Save = %v", err)
	}

	winner, err := Load(path)
	if err != nil {
		t.Fatalf("Load(winner) = %v", err)
	}
	stale, err := Load(path)
	if err != nil {
		t.Fatalf("Load(stale) = %v", err)
	}
	winner.Resources["thing.x"] = &ResourceState{
		Type:       "thing",
		Provider:   "test",
		Attributes: json.RawMessage(`{"name":"x"}`),
	}
	if err := winner.Save(path); err != nil {
		t.Fatalf("winner Save = %v", err)
	}
	if winner.Serial != 2 {
		t.Fatalf("winner.Serial = %d, want 2", winner.Serial)
	}

	stateBefore, err := os.ReadFile(path) //nolint:gosec // G304: test-controlled path under t.TempDir(), not attacker input
	if err != nil {
		t.Fatal(err)
	}
	stateInfoBefore, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	backupInfoBefore, err := os.Stat(backupPath)
	if err != nil {
		t.Fatalf("os.Stat(backup before stale Save) = %v", err)
	}
	backupBefore, err := os.ReadFile(backupPath) //nolint:gosec // G304: test-controlled path under t.TempDir(), not attacker input
	if err != nil {
		t.Fatal(err)
	}

	stale.Resources["thing.y"] = &ResourceState{
		Type:       "thing",
		Provider:   "test",
		Attributes: json.RawMessage(`{"name":"y"}`),
	}
	err = stale.Save(path)
	if !errors.Is(err, ErrConcurrentModification) {
		t.Fatalf("stale Save error = %v, want ErrConcurrentModification", err)
	}
	if stale.Serial != 1 {
		t.Fatalf("stale.Serial after rejected Save = %d, want unchanged serial 1", stale.Serial)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name state path %q", err, path)
	}
	if !strings.Contains(err.Error(), "another process") || !strings.Contains(err.Error(), "re-run") {
		t.Errorf("error %q is not actionable", err)
	}

	persisted, err := Load(path)
	if err != nil {
		t.Fatalf("Load after stale Save = %v", err)
	}
	if persisted.Serial != 2 {
		t.Errorf("persisted.Serial = %d, want winner serial 2", persisted.Serial)
	}
	if persisted.Resources["thing.x"] == nil {
		t.Error("winner resource thing.x is missing after stale Save")
	}
	if persisted.Resources["thing.y"] != nil {
		t.Error("stale resource thing.y was persisted")
	}

	stateAfter, err := os.ReadFile(path) //nolint:gosec // G304: test-controlled path under t.TempDir(), not attacker input
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stateAfter, stateBefore) {
		t.Fatalf("state file changed during rejected Save:\n--- before ---\n%s\n--- after ---\n%s", stateBefore, stateAfter)
	}
	backupAfter, err := os.ReadFile(backupPath) //nolint:gosec // G304: test-controlled path under t.TempDir(), not attacker input
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(backupAfter, backupBefore) {
		t.Fatalf("backup changed during rejected Save:\n--- before ---\n%s\n--- after ---\n%s", backupBefore, backupAfter)
	}
	stateInfoAfter, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	backupInfoAfter, err := os.Stat(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if stateInfoAfter.Mode().Perm() != stateInfoBefore.Mode().Perm() {
		t.Errorf("state permissions changed from %o to %o", stateInfoBefore.Mode().Perm(), stateInfoAfter.Mode().Perm())
	}
	if stateInfoAfter.Mode().Perm() != 0o600 {
		t.Errorf("state permissions = %o, want 600", stateInfoAfter.Mode().Perm())
	}
	if backupInfoAfter.Mode().Perm() != backupInfoBefore.Mode().Perm() {
		t.Errorf("backup permissions changed from %o to %o", backupInfoBefore.Mode().Perm(), backupInfoAfter.Mode().Perm())
	}
	temps, err := filepath.Glob(filepath.Join(dir, ".state-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 0 {
		t.Errorf("rejected Save created temp files: %v", temps)
	}
}

// TestSaveAdvancesBaseSerial verifies the same State can be saved repeatedly,
// as apply does after each resource operation.
func TestSaveAdvancesBaseSerial(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load(missing) = %v", err)
	}
	want := uint64(0)
	for _, addr := range []string{"thing.a", "thing.b", "thing.c"} {
		want++
		s.Resources[addr] = &ResourceState{
			Type:       "thing",
			Provider:   "test",
			Attributes: json.RawMessage(`{}`),
		}
		if err := s.Save(path); err != nil {
			t.Fatalf("Save serial %d = %v", want, err)
		}
		if s.Serial != want {
			t.Fatalf("Serial after Save %d = %d, want %d", want, s.Serial, want)
		}
	}
}

// TestSaveFreshStates verifies both Load's missing-file state and a directly
// constructed zero-value-base State can perform their first save and round-trip.
func TestSaveFreshStates(t *testing.T) {
	tests := map[string]func(string) (*State, error){
		"loaded missing file": Load,
		"direct construction": func(string) (*State, error) {
			return &State{
				Resources: map[string]*ResourceState{
					"thing.direct": {
						Type:       "thing",
						Provider:   "test",
						Attributes: json.RawMessage(`{}`),
					},
				},
			}, nil
		},
	}
	for name, build := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			s, err := build(path)
			if err != nil {
				t.Fatalf("build fresh State = %v", err)
			}
			if err := s.Save(path); err != nil {
				t.Fatalf("Save = %v", err)
			}
			if s.Serial != 1 {
				t.Fatalf("Serial = %d, want 1", s.Serial)
			}
			reloaded, err := Load(path)
			if err != nil {
				t.Fatalf("Load after Save = %v", err)
			}
			if reloaded.Serial != 1 {
				t.Errorf("reloaded.Serial = %d, want 1", reloaded.Serial)
			}
			if len(reloaded.Resources) != len(s.Resources) {
				t.Errorf("reloaded resources = %d, want %d", len(reloaded.Resources), len(s.Resources))
			}
		})
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
func assertOwnerOnly(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Log("skipping POSIX permission assertion on Windows")
		return
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("os.Lstat(%q) = %v", path, err)
	}
	if got := info.Mode().Perm() &^ 0o600; got != 0 {
		t.Errorf("permissions outside owner read/write = %o, want 0", got)
	}
}

func mustSymlink(t *testing.T, oldname, newname string) {
	t.Helper()
	if err := os.Symlink(oldname, newname); err != nil {
		t.Skipf("symlink creation unsupported on this platform: %v", err)
	}
}

// TestSaveBackupReplacesPlantedSymlinkWithoutClobbering reproduces the
// arbitrary-file truncation reported in Tchori-Labs/tchori#34.
func TestSaveBackupReplacesPlantedSymlinkWithoutClobbering(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	backupPath := path + ".backup"
	victim := filepath.Join(dir, "victim.txt")
	if err := os.WriteFile(victim, []byte("PRECIOUS DATA"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(path); err != nil {
		t.Fatalf("Save #1 = %v", err)
	}
	before, err := os.ReadFile(path) //nolint:gosec // G304: test-controlled path under t.TempDir(), not attacker input
	if err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, victim, backupPath)
	if err := s.Save(path); err != nil {
		t.Fatalf("Save #2 = %v, want nil", err)
	}
	gotVictim, err := os.ReadFile(victim) //nolint:gosec // G304: test-controlled path under t.TempDir(), not attacker input
	if err != nil {
		t.Fatal(err)
	}
	if string(gotVictim) != "PRECIOUS DATA" {
		t.Fatalf("victim content = %q, want %q", gotVictim, "PRECIOUS DATA")
	}
	info, err := os.Lstat(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("backup mode = %v, want regular file", info.Mode())
	}
	gotBackup, err := os.ReadFile(backupPath) //nolint:gosec // G304: test-controlled path under t.TempDir(), not attacker input
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotBackup, before) {
		t.Fatalf("backup content = %q, want %q", gotBackup, before)
	}
	assertOwnerOnly(t, backupPath)
}

func TestSaveLockRejectsNonRegularSidecar(t *testing.T) {
	t.Run("symlink to victim", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "state.json")
		lockPath := path + ".lock"
		victim := filepath.Join(dir, "victim")
		if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		mustSymlink(t, victim, lockPath)
		s, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		err = s.Save(path)
		if err == nil || !strings.Contains(err.Error(), lockPath) {
			t.Fatalf("Save error = %v, want error naming %q", err, lockPath)
		}
		got, readErr := os.ReadFile(victim) //nolint:gosec // G304: test-controlled path under t.TempDir(), not attacker input
		if readErr != nil || string(got) != "keep" {
			t.Fatalf("victim = %q, err = %v, want keep", got, readErr)
		}
		info, lstatErr := os.Lstat(lockPath)
		if lstatErr != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("lock path mode = %v, err = %v, want unchanged symlink", info, lstatErr)
		}
		assertNoStateOrBackup(t, path)
	})

	t.Run("dangling symlink", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "state.json")
		lockPath := path + ".lock"
		target := filepath.Join(dir, "missing-target")
		mustSymlink(t, target, lockPath)
		s, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		err = s.Save(path)
		if err == nil || !strings.Contains(err.Error(), lockPath) {
			t.Fatalf("Save error = %v, want error naming %q", err, lockPath)
		}
		if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
			t.Fatalf("os.Stat(target) error = %v, want IsNotExist", statErr)
		}
		info, lstatErr := os.Lstat(lockPath)
		if lstatErr != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("lock path mode = %v, err = %v, want unchanged symlink", info, lstatErr)
		}
		assertNoStateOrBackup(t, path)
	})
}

func TestSaveBackupReplacesDanglingSymlink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	backupPath := path + ".backup"
	target := filepath.Join(dir, "missing-target")
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path) //nolint:gosec // G304: test-controlled path under t.TempDir(), not attacker input
	if err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, target, backupPath)
	if err := s.Save(path); err != nil {
		t.Fatalf("Save = %v, want nil", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("os.Stat(target) error = %v, want IsNotExist", err)
	}
	info, err := os.Lstat(backupPath)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("os.Lstat(backup) = %v, %v, want regular file", info, err)
	}
	got, err := os.ReadFile(backupPath) //nolint:gosec // G304: test-controlled path under t.TempDir(), not attacker input
	if err != nil || !bytes.Equal(got, before) {
		t.Fatalf("backup = %q, err = %v, want %q", got, err, before)
	}
	assertOwnerOnly(t, backupPath)
}

func TestSaveRejectsBackupDirectoryWithoutChangingState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	backupPath := path + ".backup"
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path) //nolint:gosec // G304: test-controlled path under t.TempDir(), not attacker input
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(backupPath, 0o700); err != nil {
		t.Fatal(err)
	}
	err = s.Save(path)
	if err == nil || !strings.Contains(err.Error(), backupPath) {
		t.Fatalf("Save error = %v, want error naming %q", err, backupPath)
	}
	assertStateFileUnchanged(t, path, before)
	info, statErr := os.Stat(backupPath)
	if statErr != nil || !info.IsDir() {
		t.Fatalf("backup directory = %v, %v, want directory", info, statErr)
	}
	assertNoSidecarTempFiles(t, dir)
}

func TestSaveFirstSaveSucceedsWithDirectoryAtBackupPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	backupPath := path + ".backup"
	if err := os.Mkdir(backupPath, 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(path); err != nil {
		t.Fatalf("Save = %v, want nil", err)
	}
	if s.Serial != 1 {
		t.Fatalf("Serial = %d, want 1", s.Serial)
	}
	info, err := os.Stat(backupPath)
	if err != nil || !info.IsDir() {
		t.Fatalf("backup path = %v, %v, want directory", info, err)
	}
}

func TestSaveLeavesExistingRegularLockFileUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	lockPath := path + ".lock"
	if err := os.WriteFile(lockPath, []byte("keep"), 0o644); err != nil { //nolint:gosec // G306: permissive mode verifies an existing lock is untouched
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(lockPath, 0o644); err != nil { //nolint:gosec // G302: test deliberately seeds a permissive lock
			t.Fatal(err)
		}
	}
	before, err := os.Lstat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(path); err != nil {
		t.Fatalf("Save = %v, want nil", err)
	}
	got, err := os.ReadFile(lockPath) //nolint:gosec // G304: test-controlled path under t.TempDir(), not attacker input
	if err != nil || string(got) != "keep" {
		t.Fatalf("lock content = %q, err = %v, want keep", got, err)
	}
	after, err := os.Lstat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("regular lock inode was replaced")
	}
	if runtime.GOOS != "windows" && after.Mode().Perm() != before.Mode().Perm() {
		t.Fatalf("lock mode changed from %o to %o", before.Mode().Perm(), after.Mode().Perm())
	}
}

func TestSaveRejectsDirectoryLockSidecar(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	lockPath := path + ".lock"
	if err := os.Mkdir(lockPath, 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Save(path)
	if err == nil || !strings.Contains(err.Error(), lockPath) {
		t.Fatalf("Save error = %v, want error naming %q", err, lockPath)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("os.Stat(state) error = %v, want IsNotExist", err)
	}
	if _, err := os.Stat(path + ".backup"); !os.IsNotExist(err) {
		t.Fatalf("os.Stat(backup) error = %v, want IsNotExist", err)
	}
}

func TestVerifyLockedSidecar(t *testing.T) {
	t.Run("regular", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state.json.lock")
		lock := flock.New(path, flock.SetFlag(os.O_CREATE|os.O_RDONLY))
		locked, err := lock.TryLock()
		defer func() { _ = lock.Close() }()
		if err != nil || !locked {
			t.Fatalf("TryLock = %v, %v", locked, err)
		}
		if err := verifyLockedSidecar(lock, path); err != nil {
			t.Fatalf("verifyLockedSidecar = %v, want nil", err)
		}
	})

	t.Run("followed symlink", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "state.json.lock")
		target := filepath.Join(dir, "target")
		if err := os.WriteFile(target, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		mustSymlink(t, target, path)
		lock := flock.New(path, flock.SetFlag(os.O_CREATE|os.O_RDONLY))
		locked, err := lock.TryLock()
		defer func() { _ = lock.Close() }()
		if err != nil || !locked {
			t.Fatalf("TryLock = %v, %v", locked, err)
		}
		err = verifyLockedSidecar(lock, path)
		if err == nil || !strings.Contains(err.Error(), path) {
			t.Fatalf("verifyLockedSidecar error = %v, want error naming %q", err, path)
		}
	})
}

func TestLockOpenFlags(t *testing.T) {
	if lockOpenFlags()&os.O_CREATE == 0 {
		t.Fatal("lockOpenFlags lacks O_CREATE")
	}
	if runtime.GOOS != "windows" && lockGuardFlags == 0 {
		t.Fatal("lockGuardFlags = 0 on POSIX")
	}
}

func assertNoStateOrBackup(t *testing.T, path string) {
	t.Helper()
	for _, candidate := range []string{path, path + ".backup"} {
		if _, err := os.Stat(candidate); !os.IsNotExist(err) {
			t.Fatalf("os.Stat(%q) error = %v, want IsNotExist", candidate, err)
		}
	}
}

func assertNoSidecarTempFiles(t *testing.T, dir string) {
	t.Helper()
	for _, pattern := range []string{".state-*.tmp", ".state-backup-*.tmp"} {
		matches, err := filepath.Glob(filepath.Join(dir, pattern))
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) != 0 {
			t.Fatalf("temporary files remain for %q: %v", pattern, matches)
		}
	}
}

func TestSaveSanitizesSensitiveStateAndBackup(t *testing.T) {
	const sentinel = "tchori-e2e-super-secret-value"
	path := filepath.Join(t.TempDir(), "state.json")
	legacy := `{"format_version":"1.0","serial":0,"resources":{"secret.a":{"type":"secret","provider":"test","attributes":{"client_secret":"` + sentinel + `"}}}}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	s.SetSensitiveResolver(func(string, *ResourceState) (Resolution, bool) {
		return Resolution{Paths: []string{"client_secret"}}, true
	})
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, path + ".backup"} {
		b, err := os.ReadFile(p) //nolint:gosec // test-controlled state and backup paths under t.TempDir()
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte(sentinel)) {
			t.Fatalf("sentinel persisted in %s", p)
		}
	}
	if !s.Resources["secret.a"].SensitiveScanned || fmt.Sprint(s.Resources["secret.a"].SensitivePaths) != "[client_secret]" {
		t.Fatalf("metadata = %#v", s.Resources["secret.a"])
	}
}

func TestSaveSanitizesBackupFromEffectiveHintWhenValueNowNull(t *testing.T) {
	const sentinel = "tchori-e2e-super-secret-value"
	path := filepath.Join(t.TempDir(), "state.json")
	legacy := `{"format_version":"1.0","serial":0,"resources":{"secret.a":{"type":"secret","provider":"test","attributes":{"write_only_secret":"` + sentinel + `"}}}}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Resources["secret.a"].Attributes = json.RawMessage(`{"write_only_secret":null}`)
	s.NoteSensitive("secret.a", []string{"write_only_secret"})
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path + ".backup") //nolint:gosec // test-controlled backup path under t.TempDir()
	if bytes.Contains(b, []byte(sentinel)) {
		t.Fatal("legacy write-only secret copied to backup")
	}
}

func TestSavePreservesLiteralAndCurrentResolutionClearsRemovedPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	initial := `{"format_version":"1.0","serial":0,"resources":{` +
		`"secret.literal":{"type":"secret","provider":"test","attributes":{"token":"literal-token-ok"},"sensitive_paths":["token"]},` +
		`"secret.removed":{"type":"secret","provider":"test","attributes":{"note":null},"sensitive_paths":["note"],"redacted":["note"]}}}`
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Resources["secret.removed"].Attributes = json.RawMessage(`{"note":"visible-again"}`)
	s.SetSensitiveResolver(func(addr string, _ *ResourceState) (Resolution, bool) {
		if addr == "secret.literal" {
			return Resolution{Paths: []string{"token"}, ExemptInstances: []string{"token"}}, true
		}
		return Resolution{}, true
	})
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path) //nolint:gosec // test-controlled state path under t.TempDir()
	if !bytes.Contains(b, []byte("literal-token-ok")) || !bytes.Contains(b, []byte("visible-again")) {
		t.Fatalf("live state clobbered: %s", b)
	}
	rs := s.Resources["secret.removed"]
	if len(rs.SensitivePaths) != 0 || len(rs.Redacted) != 0 || !rs.SensitiveScanned {
		t.Fatalf("stale metadata = %#v", rs)
	}
}

func TestSaveResolverOutcomes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := &State{Resources: map[string]*ResourceState{"known": {Attributes: json.RawMessage(`{}`)}, "orphan": {Attributes: json.RawMessage(`{}`)}}}
	s.SetSensitiveResolver(func(addr string, _ *ResourceState) (Resolution, bool) {
		if addr == "known" {
			return Resolution{}, true
		}
		return Resolution{}, false
	})
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	if !s.Resources["known"].SensitiveScanned {
		t.Fatal("resolved non-sensitive entry not marked scanned")
	}
	if got := fmt.Sprint(s.UnresolvedSensitiveAddresses()); got != "[orphan]" {
		t.Fatalf("unresolved = %s", got)
	}
	if s.Resources["orphan"].SensitiveScanned {
		t.Fatal("unresolved entry marked scanned")
	}
}
