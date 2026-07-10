// Package state implements tchori's deterministic, git-diffable state
// file: atomic load/save with flock-based locking and backup-on-write.
package state

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
)

// formatVersion is the only state file schema version the MVP understands.
const formatVersion = "1.0"

// lockTimeout bounds how long Save waits to acquire path+".lock" before
// giving up. State files are local and short-lived; a lock should never be
// held for long.
const lockTimeout = 10 * time.Second

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
}

// Load returns an empty state (FormatVersion "1.0", Serial 0, empty map)
// when path does not exist.
func Load(path string) (*State, error) {
	data, err := os.ReadFile(path)
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
	if s.Resources == nil {
		s.Resources = map[string]*ResourceState{}
	}
	return &s, nil
}

// Save acquires path+".lock" via flock, writes path+".backup" (copy of the
// existing file, if any) before overwriting, increments Serial, then writes
// atomically: MarshalIndent with two-space indent plus a trailing newline
// to a temp file in the same directory, followed by os.Rename.
func (s *State) Save(path string) error {
	lock := flock.New(path + ".lock")
	defer lock.Close()

	ctx, cancel := context.WithTimeout(context.Background(), lockTimeout)
	defer cancel()

	locked, err := lock.TryLockContext(ctx, 50*time.Millisecond)
	if err != nil {
		return fmt.Errorf("acquire lock %s: %w", path+".lock", err)
	}
	if !locked {
		return fmt.Errorf("timed out acquiring lock %s", path+".lock")
	}

	if err := backupExisting(path); err != nil {
		return err
	}

	s.FormatVersion = formatVersion
	if s.Resources == nil {
		s.Resources = map[string]*ResourceState{}
	}
	s.Serial++

	data, err := json.MarshalIndent(s, "", "  ")
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
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write temp state file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close temp state file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename temp state file: %w", err)
	}
	return nil
}

// backupExisting copies the current file at path to path+".backup" before
// it is overwritten. It is a no-op when path does not yet exist — there is
// nothing to back up on the first save.
func backupExisting(path string) error {
	src, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open state for backup %s: %w", path, err)
	}
	defer src.Close()

	dst, err := os.Create(path + ".backup")
	if err != nil {
		return fmt.Errorf("create backup %s: %w", path+".backup", err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return fmt.Errorf("copy backup %s: %w", path+".backup", err)
	}
	return dst.Close()
}
