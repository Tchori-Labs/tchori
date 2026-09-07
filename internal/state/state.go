// Package state implements tchori's crash-durable state file with flock
// locking, backup-on-write, and authenticated private-data encryption.
package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"github.com/gofrs/flock"

	"github.com/tchori-labs/tchori/internal/privateblob"
	"github.com/tchori-labs/tchori/internal/sensitive"
)

const (
	formatVersion          = "1.2"
	encryptedFormatVersion = "1.1"
	legacyFormatVersion    = "1.0"
)

// ErrConcurrentModification indicates that state changed on disk after it was
// loaded. The caller should reload state and re-run its operation to reconcile
// with the other process's committed changes.
var ErrConcurrentModification = errors.New("state was modified by another process since it was loaded; re-run the command to reconcile the latest state")

// lockTimeout bounds acquisition, not the duration of an apply transaction.
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
	Type                     string          `json:"type"`
	Provider                 string          `json:"provider"`
	ProviderSource           string          `json:"provider_source,omitempty"`
	Attributes               json.RawMessage `json:"attributes"` // ctyjson-encoded object
	Private                  []byte          `json:"-"`
	SensitiveSetRecovery     []byte          `json:"-"`
	SensitiveRecoveryVersion int             `json:"-"`
	Redacted                 []string        `json:"redacted,omitempty"`
	SensitivePaths           []string        `json:"sensitive_paths,omitempty"`
	SensitiveScanned         bool            `json:"sensitive_scanned,omitempty"`
}

// Resolution is a live schema+config sensitivity lookup result. An empty Paths
// slice is a valid, definitive non-sensitive result when the resolver's ok is
// true; nil-vs-empty is never used to signal resolvability. SanitizeAttributes
// applies live literal exemptions; SanitizeBackup deliberately omits them.
// Both restore sensitive-set identity before typed decoding.
type Resolution struct {
	Paths              []string
	ProviderSource     string
	SanitizeAttributes sensitive.JSONSanitizer
	SanitizeBackup     sensitive.JSONSanitizer
}

// SensitiveResolver reports sensitivity for one entry. ok=false means the
// address is unresolvable and callers must conservatively use persisted hints.
type SensitiveResolver func(addr string, rs *ResourceState) (resolution Resolution, ok bool)

// State is the top-level state document persisted to state.json.
type State struct {
	FormatVersion  string                    `json:"format_version"`
	Serial         uint64                    `json:"serial"`
	Resources      map[string]*ResourceState `json:"resources"` // key = address
	Incomplete     *IncompleteApply          `json:"incomplete_apply,omitempty"`
	baseSerial     uint64                    `json:"-"`
	resolver       SensitiveResolver         `json:"-"`
	sensitiveHints map[string][]string       `json:"-"`
	unresolved     []string                  `json:"-"`
	lock           *flock.Flock              `json:"-"`
	lockedPath     string                    `json:"-"`
}

type resourceDocument struct {
	Type                     string          `json:"type"`
	Provider                 string          `json:"provider"`
	ProviderSource           string          `json:"provider_source,omitempty"`
	Attributes               json.RawMessage `json:"attributes"`
	Private                  json.RawMessage `json:"private,omitempty"`
	SensitiveSetRecovery     json.RawMessage `json:"sensitive_set_recovery,omitempty"`
	SensitiveRecoveryVersion int             `json:"sensitive_recovery_version,omitempty"`
	Redacted                 []string        `json:"redacted,omitempty"`
	SensitivePaths           []string        `json:"sensitive_paths,omitempty"`
	SensitiveScanned         bool            `json:"sensitive_scanned,omitempty"`
}

type stateDocument struct {
	FormatVersion string                       `json:"format_version"`
	Serial        uint64                       `json:"serial"`
	Resources     map[string]*resourceDocument `json:"resources"`
	Incomplete    *IncompleteApply             `json:"incomplete_apply,omitempty"`
}

type legacyResourceDocument struct {
	Type             string          `json:"type"`
	Provider         string          `json:"provider"`
	Attributes       json.RawMessage `json:"attributes"`
	Private          []byte          `json:"private,omitempty"`
	Redacted         []string        `json:"redacted,omitempty"`
	SensitivePaths   []string        `json:"sensitive_paths,omitempty"`
	SensitiveScanned bool            `json:"sensitive_scanned,omitempty"`
}

type legacyStateDocument struct {
	FormatVersion string                             `json:"format_version"`
	Serial        uint64                             `json:"serial"`
	Resources     map[string]*legacyResourceDocument `json:"resources"`
	Incomplete    *IncompleteApply                   `json:"incomplete_apply,omitempty"`
}

// MarshalJSON emits format 1.2 and seals provider-private bytes and sensitive
// set recovery in distinct resource-identity-bound envelopes. ResourceState
// omits both plaintext fields outside this address-aware whole-state boundary.
func (s State) MarshalJSON() ([]byte, error) {
	doc := stateDocument{
		FormatVersion: formatVersion,
		Serial:        s.Serial,
		Resources:     make(map[string]*resourceDocument, len(s.Resources)),
		Incomplete:    s.Incomplete,
	}
	for addr, rs := range s.Resources {
		if rs == nil {
			return nil, fmt.Errorf("invalid state: resource %q is null", addr)
		}
		private, err := sealResourcePrivate(addr, rs)
		if err != nil {
			return nil, err
		}
		recovery, err := sealSensitiveSetRecovery(addr, rs)
		if err != nil {
			return nil, err
		}
		doc.Resources[addr] = &resourceDocument{
			Type: rs.Type, Provider: rs.Provider, ProviderSource: rs.ProviderSource, Attributes: rs.Attributes,
			Private: private, SensitiveSetRecovery: recovery, SensitiveRecoveryVersion: rs.SensitiveRecoveryVersion,
			Redacted: rs.Redacted, SensitivePaths: rs.SensitivePaths, SensitiveScanned: rs.SensitiveScanned,
		}
	}
	return json.Marshal(doc)
}

// UnmarshalJSON accepts legacy 1.0 plaintext/base64 private fields, reads 1.1
// encrypted provider-private fields, and requires authenticated envelopes for
// both private data classes in format 1.2.
func (s *State) UnmarshalJSON(data []byte) error {
	var header struct {
		FormatVersion string `json:"format_version"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return err
	}
	switch header.FormatVersion {
	case legacyFormatVersion:
		var legacy legacyStateDocument
		if err := json.Unmarshal(data, &legacy); err != nil {
			return err
		}
		resources := make(map[string]*ResourceState, len(legacy.Resources))
		for addr, rs := range legacy.Resources {
			if rs == nil {
				resources[addr] = nil
				continue
			}
			resources[addr] = &ResourceState{
				Type: rs.Type, Provider: rs.Provider, Attributes: rs.Attributes, Private: rs.Private,
				Redacted: rs.Redacted, SensitivePaths: rs.SensitivePaths, SensitiveScanned: rs.SensitiveScanned,
			}
		}
		*s = State{FormatVersion: legacy.FormatVersion, Serial: legacy.Serial, Resources: resources, Incomplete: legacy.Incomplete}
		return nil
	case encryptedFormatVersion, formatVersion:
		var doc stateDocument
		if err := json.Unmarshal(data, &doc); err != nil {
			return err
		}
		resources := make(map[string]*ResourceState, len(doc.Resources))
		for addr, persisted := range doc.Resources {
			if persisted == nil {
				resources[addr] = nil
				continue
			}
			rs := &ResourceState{
				Type: persisted.Type, Provider: persisted.Provider, ProviderSource: persisted.ProviderSource, Attributes: persisted.Attributes,
				SensitiveRecoveryVersion: persisted.SensitiveRecoveryVersion,
				Redacted:                 persisted.Redacted, SensitivePaths: persisted.SensitivePaths, SensitiveScanned: persisted.SensitiveScanned,
			}
			if len(persisted.Private) != 0 {
				private, err := privateblob.Open(persisted.Private, resourcePrivateContext("state", addr, rs.Provider, rs.ProviderSource, rs.Type))
				if err != nil {
					return fmt.Errorf("open private state for %s: %w", addr, err)
				}
				rs.Private = private
			}
			if len(persisted.SensitiveSetRecovery) != 0 {
				if rs.SensitiveRecoveryVersion != 0 && rs.SensitiveRecoveryVersion != sensitive.RecoveryVersion {
					return fmt.Errorf("open sensitive set recovery for %s: unsupported projection version %d", addr, rs.SensitiveRecoveryVersion)
				}
				recovery, err := privateblob.Open(persisted.SensitiveSetRecovery, sensitiveRecoveryContext(addr, rs))
				if err != nil {
					return fmt.Errorf("open sensitive set recovery for %s: %w", addr, err)
				}
				rs.SensitiveSetRecovery = recovery
			}
			resources[addr] = rs
		}
		*s = State{FormatVersion: doc.FormatVersion, Serial: doc.Serial, Resources: resources, Incomplete: doc.Incomplete}
		return nil
	default:
		return fmt.Errorf("unsupported state format_version %q (supported: %q, %q, and %q)", header.FormatVersion, legacyFormatVersion, encryptedFormatVersion, formatVersion)
	}
}

func sealResourcePrivate(addr string, rs *ResourceState) (json.RawMessage, error) {
	if len(rs.Private) == 0 {
		return nil, nil
	}
	if rs.Type == "" || rs.Provider == "" || rs.ProviderSource == "" {
		return nil, fmt.Errorf("seal private state for %s: type, provider, and provider source are required", addr)
	}
	sealed, err := privateblob.Seal(rs.Private, resourcePrivateContext("state", addr, rs.Provider, rs.ProviderSource, rs.Type))
	if err != nil {
		return nil, fmt.Errorf("seal private state for %s: %w", addr, err)
	}
	return json.RawMessage(sealed), nil
}

func sealSensitiveSetRecovery(addr string, rs *ResourceState) (json.RawMessage, error) {
	if len(rs.SensitiveSetRecovery) == 0 {
		return nil, nil
	}
	if rs.Type == "" || rs.Provider == "" || rs.ProviderSource == "" {
		return nil, fmt.Errorf("seal sensitive set recovery for %s: type, provider, and provider source are required", addr)
	}
	if rs.SensitiveRecoveryVersion != 0 && rs.SensitiveRecoveryVersion != sensitive.RecoveryVersion {
		return nil, fmt.Errorf("seal sensitive set recovery for %s: unsupported projection version %d", addr, rs.SensitiveRecoveryVersion)
	}
	sealed, err := privateblob.Seal(rs.SensitiveSetRecovery, sensitiveRecoveryContext(addr, rs))
	if err != nil {
		return nil, fmt.Errorf("seal sensitive set recovery for %s: %w", addr, err)
	}
	return json.RawMessage(sealed), nil
}

func sensitiveRecoveryContext(addr string, rs *ResourceState) string {
	kind := "state-sensitive-set-recovery"
	if rs.SensitiveRecoveryVersion != 0 {
		kind = fmt.Sprintf("%s-v%d", kind, rs.SensitiveRecoveryVersion)
	}
	return resourcePrivateContext(kind, addr, rs.Provider, rs.ProviderSource, rs.Type)
}

func resourcePrivateContext(kind, addr, provider, providerSource, resourceType string) string {
	if providerSource == "" {
		// Compatibility path for already-persisted 1.1 artifacts. State
		// sanitize must bind a live source before the artifact can be saved.
		return kind + "\x00" + addr + "\x00" + provider + "\x00" + resourceType
	}
	return kind + "\x00" + addr + "\x00" + provider + "\x00" + providerSource + "\x00" + resourceType
}

// Load returns an empty format 1.2 state when path does not exist. Existing
// 1.0 and 1.1 documents remain readable for migration; encrypted fields are
// opened only after authenticating their resource identity and purpose.
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
	if s.FormatVersion != legacyFormatVersion && s.FormatVersion != encryptedFormatVersion && s.FormatVersion != formatVersion {
		return nil, fmt.Errorf("unsupported state format_version %q", s.FormatVersion)
	}
	if s.Resources == nil {
		s.Resources = map[string]*ResourceState{}
	}
	for addr, rs := range s.Resources {
		if rs == nil {
			return nil, fmt.Errorf("invalid state: resource %q is null", addr)
		}
	}
	s.baseSerial = s.Serial
	return &s, nil
}

// SetSensitiveResolver registers the live schema+raw-config resolver used by Save.
func (s *State) SetSensitiveResolver(r SensitiveResolver) { s.resolver = r }

// NoteSensitive records the full effective path set for backup sanitization.
func (s *State) NoteSensitive(addr string, paths []string) {
	if s.sensitiveHints == nil {
		s.sensitiveHints = map[string][]string{}
	}
	s.sensitiveHints[addr] = unionStrings(s.sensitiveHints[addr], paths)
}

// UnresolvedSensitiveAddresses returns addresses the most recent Save could not inspect.
func (s *State) UnresolvedSensitiveAddresses() []string {
	return append([]string(nil), s.unresolved...)
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
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if s.lock != nil {
		if s.lockedPath != path {
			return fmt.Errorf("state is locked for a different path")
		}
	} else {
		lock, err := acquireLock(context.Background(), path)
		if err != nil {
			return err
		}
		defer func() { _ = lock.Close() }()
	}

	onDiskSerial, err := readSerial(path)
	if err != nil {
		return err
	}
	if onDiskSerial != s.baseSerial {
		return fmt.Errorf("%s: on-disk serial %d does not match loaded serial %d: %w", path, onDiskSerial, s.baseSerial, ErrConcurrentModification)
	}

	if s.Resources == nil {
		s.Resources = map[string]*ResourceState{}
	}
	// Prepare the recovery artifact and the replacement completely before
	// committing either. In particular, key/envelope errors cannot replace a
	// previously valid backup.
	backupData, hasBackup, err := s.prepareBackup(path)
	if err != nil {
		return err
	}
	if err := s.sanitizeAll(); err != nil {
		return err
	}

	// Marshal a copy with the next serial so pre-rename failures do not mutate
	// the caller's serial or format version. Once rename succeeds, the visible
	// replacement and in-memory CAS fields advance together.
	next := *s
	next.FormatVersion = formatVersion
	next.Serial++
	data, err := json.MarshalIndent(&next, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	data = append(data, '\n')
	if hasBackup {
		if err := writeBackup(path, backupData); err != nil {
			return err
		}
	}

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
	s.FormatVersion = formatVersion
	s.Serial = next.Serial
	s.baseSerial = next.Serial
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("sync state directory %s: %w", dir, err)
	}
	return nil
}

// Lock holds the state sidecar across a complete mutating operation. Save on
// this State reuses the lock; other writers cannot race remote side effects.
// The caller must release it and must not share this State across goroutines.
func (s *State) Lock(ctx context.Context, path string) (func(), error) {
	if s.lock != nil {
		return nil, fmt.Errorf("state operation already holds a lock")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	lock, err := acquireLock(ctx, path)
	if err != nil {
		return nil, err
	}
	serial, err := readSerial(path)
	if err == nil && serial != s.baseSerial {
		err = fmt.Errorf("%s: %w", path, ErrConcurrentModification)
	}
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	s.lock, s.lockedPath = lock, path
	return func() {
		s.lock, s.lockedPath = nil, ""
		_ = lock.Close()
	}, nil
}

func acquireLock(ctx context.Context, path string) (*flock.Flock, error) {
	lockPath := path + ".lock"
	// Never replace an existing lock inode; reject symlinks and special files.
	info, err := os.Lstat(lockPath)
	if err == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("lock path %s is not a regular file", lockPath)
	}
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("inspect lock path %s: %w", lockPath, err)
	}
	lock := flock.New(lockPath, flock.SetFlag(lockOpenFlags()))
	ctx, cancel := context.WithTimeout(ctx, lockTimeout)
	defer cancel()
	locked, err := lock.TryLockContext(ctx, 50*time.Millisecond)
	if err == nil && !locked {
		err = fmt.Errorf("timed out acquiring lock %s", lockPath)
	}
	if err == nil {
		err = verifyLockedSidecar(lock, lockPath)
	}
	if err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("acquire lock %s: %w", lockPath, err)
	}
	return lock, nil
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

// prepareBackup returns a sanitized recovery copy of the previous document
// without changing the backup path. Existing 1.2 bytes are preserved exactly
// when no attribute sanitization is needed. Earlier documents are rewritten as
// 1.2 so neither encrypted data class can be lost or left plaintext.
func (s *State) prepareBackup(path string) ([]byte, bool, error) {
	data, err := os.ReadFile(path) //nolint:gosec // operator-selected state path
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("open state for backup %s: %w", path, err)
	}
	var previous State
	if err := json.Unmarshal(data, &previous); err != nil {
		return nil, false, fmt.Errorf("parse state for backup %s: %w", path, err)
	}
	if previous.Resources == nil {
		previous.Resources = map[string]*ResourceState{}
	}
	changedDocument := previous.FormatVersion != formatVersion
	for addr, prior := range previous.Resources {
		generationPaths := append([]string(nil), prior.SensitivePaths...)
		paths := unionStrings(generationPaths, prior.Redacted, s.sensitiveHints[addr])
		if current := s.Resources[addr]; current != nil {
			paths = unionStrings(paths, current.SensitivePaths)
			if prior.ProviderSource == "" && current.ProviderSource != "" &&
				prior.Type == current.Type && prior.Provider == current.Provider {
				prior.ProviderSource = current.ProviderSource
				changedDocument = true
			}
		}
		var backupSanitizer sensitive.JSONSanitizer
		if s.resolver != nil {
			if resolution, ok := s.resolver(addr, prior); ok {
				if err := bindResolvedProviderSource(addr, prior, resolution.ProviderSource); err != nil {
					return nil, false, err
				}
				paths = unionStrings(paths, resolution.Paths)
				backupSanitizer = resolution.SanitizeBackup
			}
		}
		if len(paths) == 0 {
			continue
		}
		if backupSanitizer != nil {
			attrs, changed, recovery, err := backupSanitizer(
				prior.Attributes, prior.SensitiveSetRecovery, prior.SensitiveRecoveryVersion, generationPaths, paths,
			)
			if err != nil {
				return nil, false, fmt.Errorf("sanitize backup attributes for %s: %w", addr, err)
			}
			prior.Attributes = attrs
			prior.SensitiveSetRecovery = recovery
			if len(recovery) != 0 {
				prior.SensitiveRecoveryVersion = sensitive.RecoveryVersion
			} else {
				prior.SensitiveRecoveryVersion = 0
			}
			prior.Redacted = unionStrings(prior.Redacted, changed)
			changedDocument = true
			continue
		}
		if len(prior.SensitiveSetRecovery) != 0 {
			// Without a schema, changing either half would invalidate the
			// authenticated projection/recovery pair. Preserve both verbatim.
			continue
		}
		changedDocument = true
		// Conservative by design: a backup is a recovery/reporting copy never
		// read by the engine, so under-scrubbing is a leak and over-scrubbing a
		// raw literal is safe.
		attrs, changed, err := sensitive.RedactJSON(prior.Attributes, paths)
		if err != nil {
			return nil, false, fmt.Errorf("sanitize backup attributes for %s: %w", addr, err)
		}
		prior.Attributes = attrs
		prior.Redacted = unionStrings(prior.Redacted, changed)
	}
	if changedDocument {
		data, err = json.MarshalIndent(&previous, "", "  ")
		if err != nil {
			return nil, false, fmt.Errorf("marshal sanitized backup: %w", err)
		}
		data = append(data, '\n')
	}
	return data, true, nil
}

func writeBackup(path string, data []byte) error {
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
	cleanup := func() { _ = tmp.Close(); _ = os.Remove(tmpPath) }
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("chmod backup %s: %w", backupPath, err)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("copy backup %s: %w", backupPath, err)
	}
	if err := fsyncFile(tmp); err != nil {
		cleanup()
		return fmt.Errorf("sync temporary backup %s: %w", backupPath, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close backup %s: %w", backupPath, err)
	}
	if err := os.Rename(tmpPath, backupPath); err != nil {
		cleanup()
		return fmt.Errorf("rename backup %s: %w", backupPath, err)
	}
	return nil
}

func (s *State) sanitizeAll() error {
	s.unresolved = nil
	for addr, rs := range s.Resources {
		if rs == nil {
			continue
		}
		generationPaths := append([]string(nil), rs.SensitivePaths...)
		paths := unionStrings(generationPaths, rs.Redacted, s.sensitiveHints[addr])
		sanitizer := sensitive.JSONSanitizer(nil)
		resolved := false
		if s.resolver != nil {
			if resolution, ok := s.resolver(addr, rs); ok {
				resolved = true
				// Removing a config declaration must not declassify a stored secret.
				paths = unionStrings(paths, resolution.Paths)
				sanitizer = resolution.SanitizeAttributes
				if err := bindResolvedProviderSource(addr, rs, resolution.ProviderSource); err != nil {
					return err
				}
				rs.SensitivePaths = append([]string(nil), paths...)
				rs.SensitiveScanned = true
				rs.Redacted = intersectStrings(rs.Redacted, paths)
			} else if len(paths) == 0 {
				s.unresolved = append(s.unresolved, addr)
			}
		}
		if len(paths) == 0 {
			if resolved {
				rs.SensitivePaths = nil
				rs.Redacted = nil
			}
			continue
		}
		var (
			attrs    json.RawMessage
			changed  []string
			recovery []byte
			err      error
		)
		if resolved && sanitizer != nil {
			attrs, changed, recovery, err = sanitizer(rs.Attributes, rs.SensitiveSetRecovery, rs.SensitiveRecoveryVersion, generationPaths, paths)
		} else {
			if len(rs.SensitiveSetRecovery) != 0 {
				return fmt.Errorf("sanitize state attributes for %s: live schema is required to preserve the authenticated sensitive set projection", addr)
			}
			// Orphan/config-unavailable state has no trustworthy schema or
			// literal binding, so provider-free sanitization fails closed.
			attrs, changed, err = sensitive.RedactJSON(rs.Attributes, paths)
		}
		if err != nil {
			return fmt.Errorf("sanitize state attributes for %s: %w", addr, err)
		}
		rs.Attributes = attrs
		if resolved && sanitizer != nil {
			rs.SensitiveSetRecovery = recovery
			if len(recovery) != 0 {
				rs.SensitiveRecoveryVersion = sensitive.RecoveryVersion
			} else {
				rs.SensitiveRecoveryVersion = 0
			}
		}
		rs.Redacted = unionStrings(rs.Redacted, changed)
	}
	sort.Strings(s.unresolved)
	return nil
}

func bindResolvedProviderSource(addr string, rs *ResourceState, source string) error {
	if source == "" {
		return nil
	}
	if rs.ProviderSource != "" && rs.ProviderSource != source {
		return fmt.Errorf("state resource %s provider source %q does not match resolved source %q", addr, rs.ProviderSource, source)
	}
	rs.ProviderSource = source
	return nil
}

func sortedUnique(parts []string) []string {
	set := map[string]bool{}
	for _, p := range parts {
		if p != "" {
			set[p] = true
		}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
func unionStrings(groups ...[]string) []string {
	var all []string
	for _, g := range groups {
		all = append(all, g...)
	}
	return sortedUnique(all)
}
func intersectStrings(a, b []string) []string {
	keep := map[string]bool{}
	for _, p := range b {
		keep[p] = true
	}
	var out []string
	for _, p := range a {
		if keep[p] {
			out = append(out, p)
		}
	}
	return sortedUnique(out)
}
