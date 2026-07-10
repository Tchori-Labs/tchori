package provider_test

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestBuildTestProvider compiles the fake tfprotov6 test provider to a real
// binary. Later tasks (6, 8) launch this same build artifact as a go-plugin
// subprocess to exercise the engine's provider client end to end.
func TestBuildTestProvider(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "terraform-provider-tchoritest")
	//nolint:gosec // G204: fixed "go build" argv; only variable part is t.TempDir(), not external input.
	cmd := exec.Command("go", "build", "-o", bin, "./internal/provider/testprovider")
	// go test runs with cwd = this package's source dir (internal/provider);
	// the build must run from the module root two levels up.
	cmd.Dir = filepath.Join("..", "..")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build failed: %v\n%s", err, out)
	}
}
