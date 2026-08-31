//go:build !windows

package state

import (
	"fmt"
	"os"
)

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
