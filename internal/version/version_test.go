package version

import "testing"

func TestVersionIsSet(t *testing.T) {
	if Version == "" {
		t.Fatal("Version must not be empty")
	}
	if Version != "0.1.0-dev" {
		t.Fatalf("Version = %q, want %q", Version, "0.1.0-dev")
	}
}
