//go:build windows

package provider

import (
	"errors"
	"fmt"
	"syscall"
)

// providerProcessAlive probes process liveness using a Windows process handle.
// Windows has no POSIX signal-0 liveness probe, and os.Process.Signal is not a
// portable substitute because Windows rejects every signal except os.Kill.
// Opening a query/synchronize handle and polling it with WaitForSingleObject is
// the equivalent non-destructive check: a timeout means the process is still
// running, while a signaled handle means it exited. CI cross-compiles and vets
// this implementation, but no current CI lane exercises its runtime behavior
// on Windows.
func providerProcessAlive(pid int) (bool, error) {
	const errorInvalidParameter = syscall.Errno(87)

	handle, err := syscall.OpenProcess(
		syscall.PROCESS_QUERY_INFORMATION|syscall.SYNCHRONIZE,
		false,
		uint32(pid),
	)
	if err != nil {
		switch {
		case errors.Is(err, errorInvalidParameter):
			return false, nil
		case errors.Is(err, syscall.ERROR_ACCESS_DENIED):
			return true, nil
		default:
			return false, fmt.Errorf("open process %d: %w", pid, err)
		}
	}
	defer syscall.CloseHandle(handle) //nolint:errcheck // Nothing actionable remains after a read-only liveness probe.

	event, err := syscall.WaitForSingleObject(handle, 0)
	if err != nil {
		return false, fmt.Errorf("wait for process %d: %w", pid, err)
	}
	switch event {
	case syscall.WAIT_TIMEOUT:
		return true, nil
	case syscall.WAIT_OBJECT_0:
		return false, nil
	default:
		return false, fmt.Errorf("WaitForSingleObject on pid %d returned unexpected event 0x%X", pid, event)
	}
}
