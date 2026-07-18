// Package state implements tchori's deterministic, git-diffable state
// file: crash-durable atomic saves with flock locking and backup-on-write.
package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
)

// formatVersion is the only state file schema version the MVP understands.
const formatVersion = "1.0"

// ErrConcurrentModification indicates that state changed on disk after it was
// loaded. The caller should reload state and re-run its operation to reconcile
// with the other process's committed changes.
var ErrConcurrentModification = errors.New("state was modified by another process since it was loaded; re-run the command to reconcile the latest state")

// lockTimeout bounds how long Save waits to acquire path+".lock" before
// giving up. State files are local and short-lived; a lock should never be
// held for long.
const lockTimeout = 10 * time.Second

// Filesystem seams keep Save's failure paths deterministic in tests while
// production uses the real durability and atomic-replacement operations.
var (
	fsyncFile  = (*os.File).Sync
	closeFile  = (*os.File).Close
	renameFile = os.Rename
	syncDir    = defaultSyncDir
)

// ResourceState is the persisted state of a single managed resource.
type ResourceState struct {
	Type       string          `json:"type"`
	Provider   string          `json:"provider"`
	Attributes json.RawMessage `json:"attributes"`        // ctyjson-encoded object
	Private    []byte          `json:"private,omitempty"` // std base64 via encoding/json
}

// State is the top-level state document persisted to state.json.
type State struct {
	FormatVersion string                    `json:"format_version"` // "1.0"
	Serial        uint64                    `json:"serial"`
	Resources     map[string]*ResourceState `json:"resources"` // key = address
	baseSerial    uint64                    `json:"-"`
}

// Load returns an empty state (FormatVersion "1.0", Serial 0, empty map)
// when path does not exist. When path does exist, its format_version must be
// "1.0" (matching plan.Read's rejection of unsupported plan format
// versions) — this includes a missing/empty format_version, since a state
// file we ourselves wrote always carries "1.0" (see Save); anything else is
// a state file this engine did not write and should not guess about.
func Load(path string) (*State, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is operator-supplied (CLI flag / fixed state.json location), not attacker-controlled
	if err != nil {
		if os.IsNotExist(err) {
			return &State{
				FormatVersion: formatVersion,
				Serial:        0,
				Resources:     map[string]*ResourceState{},
			}, nil
		}
		return nil, fmt.Errorf("read state %s: %w", path, err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse state %s: %w", path, err)
	}
	if s.FormatVersion != formatVersion {
		return nil, fmt.Errorf("unsupported state format_version %q (want %q)", s.FormatVersion, formatVersion)
	}
	if s.Resources == nil {
		s.Resources = map[string]*ResourceState{}
	}
	s.baseSerial = s.Serial
	return &s, nil
}

// Save acquires path+".lock" via flock and compares the on-disk serial with
// the base serial observed by Load or the preceding successful Save. If they
// differ, Save returns ErrConcurrentModification without changing the state or
// its backup. Otherwise, Save writes path+".backup" (copy of the existing file,
// if any) before overwriting, increments Serial, then commits crash-durably:
// MarshalIndent with two-space indent plus a trailing newline to a temp file in
// the same directory, fsync the complete temp file, close it, atomically rename
// it over path, then fsync the containing directory before reporting success.
// Failures before rename remove the temp file and leave Serial unchanged. A
// directory-sync failure is returned without removing the state file because
// the rename already took effect; Serial and the compare-and-swap base advance
// to match that visible replacement, allowing a caller to retry safely. Save
// reports success only after the directory sync completes.
func (s *State) Save(path string) error {
	lock := flock.New(path + ".lock")
	defer func() { _ = lock.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), lockTimeout)
	defer cancel()

	locked, err := lock.TryLockContext(ctx, 50*time.Millisecond)
	if err != nil {
		return fmt.Errorf("acquire lock %s: %w", path+".lock", err)
	}
	if !locked {
		return fmt.Errorf("timed out acquiring lock %s", path+".lock")
	}

	onDiskSerial, err := readSerial(path)
	if err != nil {
		return err
	}
	if onDiskSerial != s.baseSerial {
		return fmt.Errorf("%s: on-disk serial %d does not match loaded serial %d: %w", path, onDiskSerial, s.baseSerial, ErrConcurrentModification)
	}

	if err := backupExisting(path); err != nil {
		return err
	}

	s.FormatVersion = formatVersion
	if s.Resources == nil {
		s.Resources = map[string]*ResourceState{}
	}
	// Marshal a copy with the next serial so pre-rename failures do not mutate
	// the caller's serial. Once rename succeeds, the replacement is visible and
	// both Serial and baseSerial must advance even if the durability barrier
	// below subsequently fails.
	next := *s
	next.Serial++

	data, err := json.MarshalIndent(&next, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp state file: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = closeFile(tmp)
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write temp state file: %w", err)
	}
	if err := fsyncFile(tmp); err != nil {
		_ = closeFile(tmp)
		_ = os.Remove(tmpPath)
		return fmt.Errorf("sync temp state file: %w", err)
	}
	if err := closeFile(tmp); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close temp state file: %w", err)
	}
	if err := renameFile(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename temp state file: %w", err)
	}
	s.Serial = next.Serial
	s.baseSerial = next.Serial
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("sync state directory %s: %w", dir, err)
	}
	return nil
}

// defaultSyncDir persists a completed rename's directory entry before Save
// reports success.
func defaultSyncDir(dir string) error {
	f, err := os.Open(dir) //nolint:gosec // G304: dir is derived from the operator-supplied state path, not attacker-controlled
	if err != nil {
		return fmt.Errorf("open directory: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync directory: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close directory: %w", err)
	}
	return nil
}

// readSerial returns the serial currently committed at path. A missing or
// empty file is the serial-zero state expected by a fresh State.
func readSerial(path string) (uint64, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is operator-supplied (CLI flag / fixed state.json location), not attacker-controlled
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read state serial %s: %w", path, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return 0, nil
	}
	var header struct {
		Serial uint64 `json:"serial"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return 0, fmt.Errorf("parse state serial %s: %w", path, err)
	}
	return header.Serial, nil
}

// backupExisting copies the current file at path to path+".backup" before it
// is overwritten, forcing mode 0600 where permissions are supported. It is a
// no-op when path does not yet exist — there is nothing to back up on first save.
func backupExisting(path string) error {
	src, err := os.Open(path) //nolint:gosec // G304: path is operator-supplied (CLI flag / fixed state.json location), not attacker-controlled
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open state for backup %s: %w", path, err)
	}
	defer func() { _ = src.Close() }()

	dst, err := os.OpenFile(path+".backup", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // G304: path is operator-supplied (CLI flag / fixed state.json location), not attacker-controlled
	if err != nil {
		return fmt.Errorf("create backup %s: %w", path+".backup", err)
	}
	if err := dst.Chmod(0o600); err != nil {
		_ = dst.Close()
		return fmt.Errorf("chmod backup %s: %w", path+".backup", err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return fmt.Errorf("copy backup %s: %w", path+".backup", err)
	}
	return dst.Close()
}
