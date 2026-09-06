//go:build unix

// Regular CLI commands must cancel and reap providers on interrupt.
package main_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestValidateInterruptKillsLaunchedProvider(t *testing.T) {
	work := t.TempDir()
	writeConfig(t, work, "cancel-test")

	pidFile := filepath.Join(t.TempDir(), "provider.pid")

	cmd := exec.Command(tchoriBin, "--plugin-dir="+pluginDir, "validate") //nolint:gosec // binary built by TestMain into a temp dir
	cmd.Dir = work
	cmd.Env = append(os.Environ(),
		"TCHORITEST_STALL_STARTUP=1",
		"TCHORITEST_PID_FILE="+pidFile,
	)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start tchori validate: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	providerPID := waitForCLIProviderPID(t, pidFile, 5*time.Second)
	if providerPID <= 1 {
		t.Fatal("invalid provider PID")
	}
	process, err := os.FindProcess(providerPID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = process.Kill(); _ = process.Release() })

	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("sending SIGINT to tchori process: %v", err)
	}

	waitErrCh := make(chan error, 1)
	go func() { waitErrCh <- cmd.Wait() }()

	select {
	case err := <-waitErrCh:
		var ee *exec.ExitError
		if err != nil && !errors.As(err, &ee) {
			t.Fatalf("tchori validate did not exit cleanly: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("tchori validate did not exit within 5s of SIGINT (the stalled provider sleeps 24h); stdout: %s\nstderr: %s",
			stdout.String(), stderr.String())
	}

	waitForCLIProviderExit(t, providerPID, 5*time.Second)
}

func waitForCLIProviderPID(t *testing.T, path string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		raw, err := os.ReadFile(path) //nolint:gosec // G304: path is the t.TempDir PID artifact this test wrote
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(raw)))
			if parseErr != nil {
				t.Fatalf("parsing provider PID %q: %v", raw, parseErr)
			}
			return pid
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("reading provider PID file: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("provider did not write PID file within %v", timeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForCLIProviderExit(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		alive, err := cliProviderProcessAlive(pid)
		if err != nil {
			t.Fatalf("checking provider process %d: %v", pid, err)
		}
		if !alive {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("provider process %d launched by tchori validate is still running %v after SIGINT; "+
				"tchori orphaned it instead of canceling provider.Launch", pid, timeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// cliProviderProcessAlive mirrors internal/provider's unix liveness probe
// (unexported there, so not importable from this package).
func cliProviderProcessAlive(pid int) (bool, error) {
	err := syscall.Kill(pid, 0)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.ESRCH):
		return false, nil
	case errors.Is(err, syscall.EPERM):
		return true, nil
	default:
		return false, err
	}
}

func TestStateShowDiscoverSensitiveInterruptKillsLaunchedProvider(t *testing.T) {
	work := t.TempDir()
	writeConfig(t, work, "cancel-test")
	stateDoc := `{"format_version":"1.0","serial":1,"resources":{"tchoritest_thing.demo":{"type":"tchoritest_thing","provider":"tchoritest","attributes":{"id":"t-id-cancel-test","name":"cancel-test"}}}}`
	if err := os.WriteFile(filepath.Join(work, "state.json"), []byte(stateDoc), 0o600); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(t.TempDir(), "provider.pid")

	cmd := exec.Command(tchoriBin, "--plugin-dir="+pluginDir, "state", "show", "--discover-sensitive", "tchoritest_thing.demo") //nolint:gosec // binary built by TestMain into a temp dir
	cmd.Dir = work
	cmd.Env = append(os.Environ(),
		"TCHORITEST_STALL_STARTUP=1",
		"TCHORITEST_PID_FILE="+pidFile,
	)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start tchori state show --discover-sensitive: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	providerPID := waitForCLIProviderPID(t, pidFile, 5*time.Second)
	process, err := os.FindProcess(providerPID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = process.Kill(); _ = process.Release() })

	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("sending SIGINT to tchori process: %v", err)
	}
	waitErrCh := make(chan error, 1)
	go func() { waitErrCh <- cmd.Wait() }()
	select {
	case err := <-waitErrCh:
		var ee *exec.ExitError
		if err != nil && !errors.As(err, &ee) {
			t.Fatalf("tchori state show --discover-sensitive did not exit cleanly: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("tchori state show --discover-sensitive did not exit within 5s of SIGINT; stdout: %s\nstderr: %s", stdout.String(), stderr.String())
	}
	waitForCLIProviderExit(t, providerPID, 5*time.Second)
}
