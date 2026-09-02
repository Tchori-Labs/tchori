// Package pidfile writes the PID file used by the fake test providers
// (testprovider, testprovider5) to signal their process ID to the lifecycle
// tests in internal/provider.
package pidfile

import (
	"os"
	"strconv"
)

// Write records pid at path atomically: it stages the PID text on a
// sibling "path + .tmp" file and renames it into place. A concurrent
// reader polling path therefore either sees no file yet or the complete,
// final content — never an empty file created by a non-atomic write
// (os.WriteFile creates the file empty and then writes its content,
// exposing exactly that window).
func Write(path string, pid int) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(pid)), 0o600); err != nil { //nolint:gosec // G306: test-only path is explicitly provided by the lifecycle test
		return err
	}
	return os.Rename(tmp, path)
}
