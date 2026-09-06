// Package pidfile publishes complete PID files for test-provider lifecycle checks.
package pidfile

import (
	"os"
	"path/filepath"
	"strconv"
)

// Write publishes pid using a sibling temporary file so readers never observe
// a newly created but empty PID file.
func Write(path string, pid int) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".provider-pid-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.WriteString(strconv.Itoa(pid)); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
