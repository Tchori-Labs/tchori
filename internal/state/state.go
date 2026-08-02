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
	"runtime"
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
// its backup. Otherwise, Save copies an existing state through a fresh temporary
// file and renames it to path+".backup" before overwriting, so a planted backup
// symlink is replaced rather than followed. It then increments Serial and
// commits crash-durably: MarshalIndent with two-space indent plus a trailing
// newline to a temp file in the same directory, fsync the complete temp file,
// close it, atomically rename it over path, then runs the platform's
// directory-durability barrier before reporting success. On POSIX this fsyncs
// the containing directory; on Windows, where directory fsync is not supported,
// this barrier is a documented no-op. Failures before rename remove the temp
// file and leave Serial unchanged. A directory-sync failure is returned without
// removing the state file because the rename already took effect; Serial and the
// compare-and-swap base advance to match that visible replacement, allowing a
// caller to retry safely. Save reports success only after the durability barrier
// completes.
func (s *State) Save(path string) error {
	lockPath := path + ".lock"
	// Preflight rejects ordinary non-regular entries with an operator-facing
	// error before a lock is taken. The open-time flags below, not this check,
	// close the POSIX race window and raced-filesystem-FIFO hang.
	info, err := os.Lstat(lockPath)
	if err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("lock path %s is not a regular file", lockPath)
	}
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("inspect lock path %s: %w", lockPath, err)
	}

	// Existing regular lock files are reused exactly as-is: replacing,
	// truncating, or chmod'ing an inode another process may hold would weaken
	// flock synchronization, and the lock contains no sensitive data.
	lock := flock.New(lockPath, flock.SetFlag(lockOpenFlags()))
	defer func() { _ = lock.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), lockTimeout)
	defer cancel()

	locked, err := lock.TryLockContext(ctx, 50*time.Millisecond)
	if err != nil {
		return fmt.Errorf("acquire lock %s: %w", lockPath, err)
	}
	if !locked {
		return fmt.Errorf("timed out acquiring lock %s", lockPath)
	}
	if err := verifyLockedSidecar(lock, lockPath); err != nil {
		return err
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

// lockOpenFlags mirrors gofrs/flock v0.13.0 flock.go:62: O_CREATE plus
// O_RDWR on aix/solaris/illumos and O_RDONLY elsewhere. Unix builds add
// O_NOFOLLOW and O_NONBLOCK; the latter guarantee is measured for filesystem
// FIFOs on the shipped Linux and Darwin targets, not arbitrary device nodes.
func lockOpenFlags() int {
	flags := os.O_CREATE | os.O_RDONLY
	switch runtime.GOOS {
	case "aix", "solaris", "illumos":
		flags = os.O_CREATE | os.O_RDWR
	}
	return flags | lockGuardFlags
}

// verifyLockedSidecar checks the held descriptor rather than trusting the
// preflight path lookup. Regularity rejects a raced FIFO even though SameFile
// is true; identity rejects a followed symlink on platforms whose guard flag
// is zero even though its target is regular.
//
// Hardlinks and symlinked parent directories are outside this mechanism. On
// Windows, a raced symlink may be traversed before this check and create an
// empty target, but O_TRUNC is absent so existing data is not destroyed;
// filesystem-path FIFOs do not exist there. O_NONBLOCK's claim is limited to
// filesystem FIFOs on shipped POSIX targets, not arbitrary devices. The
// unshipped aix/solaris/illumos targets use O_RDWR, whose nonblocking FIFO-open
// behavior is POSIX-undefined.
func verifyLockedSidecar(lock *flock.Flock, lockPath string) error {
	heldInfo, err := lock.Stat()
	if err != nil {
		return fmt.Errorf("verify lock path %s: stat held descriptor: %w", lockPath, err)
	}
	pathInfo, err := os.Lstat(lockPath)
	if err != nil {
		return fmt.Errorf("verify lock path %s: lstat: %w", lockPath, err)
	}
	if !heldInfo.Mode().IsRegular() || !os.SameFile(heldInfo, pathInfo) {
		return fmt.Errorf("verify lock path %s: held descriptor is not the same regular file", lockPath)
	}
	return nil
}

// backupExisting copies the current file at path to path+".backup" before it
// is overwritten. The source is opened first: when it does not exist, the
// backup is a no-op without inspecting a hostile entry at the backup path.
// Once a source exists, a directory at the backup path is rejected because it
// cannot be renamed over. Every other entry is safely replaced via a fresh
// same-directory temporary file and os.Rename. This replaces the name without
// writing through it (an atomic replace on POSIX), so rejecting symlinks would
// only enable denial of service. CreateTemp requests mode 0600; on POSIX umask
// can only narrow that to owner-only bits, while Windows uses its own permission
// semantics. No chmod is needed because the renamed file is always a new inode.
func backupExisting(path string) error {
	src, err := os.Open(path) //nolint:gosec // G304: path is operator-supplied (CLI flag / fixed state.json location), not attacker-controlled
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open state for backup %s: %w", path, err)
	}
	defer func() { _ = src.Close() }()

	backupPath := path + ".backup"
	info, err := os.Lstat(backupPath)
	if err == nil && info.IsDir() {
		return fmt.Errorf("backup path %s is a directory", backupPath)
	}
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("inspect backup path %s: %w", backupPath, err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-backup-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary backup %s: %w", backupPath, err)
	}
	tmpPath := tmp.Name()
	cleanupTemp := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}
	if _, err := io.Copy(tmp, src); err != nil {
		cleanupTemp()
		return fmt.Errorf("copy backup %s: %w", backupPath, err)
	}
	if err := tmp.Close(); err != nil {
		cleanupTemp()
		return fmt.Errorf("close backup %s: %w", backupPath, err)
	}
	if err := os.Rename(tmpPath, backupPath); err != nil {
		cleanupTemp()
		return fmt.Errorf("rename backup %s: %w", backupPath, err)
	}
	return nil
}
