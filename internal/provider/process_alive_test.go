package provider

import (
	"os"
	"os/exec"
	"testing"
)

func TestProviderProcessAlive(t *testing.T) {
	alive, err := providerProcessAlive(os.Getpid())
	if err != nil {
		t.Fatalf("probe current process: %v", err)
	}
	if !alive {
		t.Fatal("current process reported as exited")
	}

	cmd := exec.Command(os.Args[0], "-test.run=^$") //nolint:gosec // os.Args[0] is this trusted test binary, not external input.
	if err := cmd.Run(); err != nil {
		t.Fatalf("run short-lived test process: %v", err)
	}

	// A theoretical PID-reuse race remains, as it did in the pre-existing
	// waitForProviderExit helper that polls a process identifier.
	alive, err = providerProcessAlive(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("probe reaped process: %v", err)
	}
	if alive {
		t.Fatal("reaped process reported as alive")
	}
}
