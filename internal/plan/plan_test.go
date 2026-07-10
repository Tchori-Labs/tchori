package plan_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tchori-labs/tchori/internal/plan"
)

func TestHasChanges(t *testing.T) {
	pl := &plan.Plan{FormatVersion: "1.0"}
	if pl.HasChanges() {
		t.Fatal("empty plan: HasChanges() = true, want false")
	}
	pl.Summary.Create = 1
	if !pl.HasChanges() {
		t.Fatal("summary create=1: HasChanges() = false, want true")
	}
	pl.Summary = plan.Summary{Delete: 2}
	if !pl.HasChanges() {
		t.Fatal("summary delete=2: HasChanges() = false, want true")
	}
}

func TestPlanWriteReadDeterminism(t *testing.T) {
	pl := &plan.Plan{
		FormatVersion: "1.0",
		EngineVersion: "0.1.0-dev",
		StateSerial:   4,
		Changes: []*plan.Change{{
			Address:      "tchoritest_thing.demo",
			Action:       "create",
			Before:       json.RawMessage("null"),
			After:        json.RawMessage(`{"echo":null,"id":null,"name":"demo","replace_me":null,"tags":null}`),
			UnknownAfter: []string{"echo", "id"},
		}},
		Summary: plan.Summary{Create: 1},
	}

	path := filepath.Join(t.TempDir(), "plan.json")
	if err := plan.Write(pl, path); err != nil {
		t.Fatalf("Write: %v", err)
	}
	b1, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(b1) == 0 || b1[len(b1)-1] != '\n' {
		t.Error("plan.json must end with a trailing newline")
	}
	if !strings.Contains(string(b1), `"format_version": "1.0"`) {
		t.Errorf("plan.json missing two-space-indented format_version:\n%s", b1)
	}

	// Determinism: writing the same plan again is byte-identical.
	if err := plan.Write(pl, path); err != nil {
		t.Fatalf("second Write: %v", err)
	}
	b2, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(b1, b2) {
		t.Error("plan.json is not byte-identical across writes")
	}

	got, err := plan.Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.StateSerial != 4 || got.EngineVersion != "0.1.0-dev" || len(got.Changes) != 1 {
		t.Errorf("Read round-trip mismatch: %+v", got)
	}
	if got.Changes[0].Address != "tchoritest_thing.demo" || got.Changes[0].Action != "create" {
		t.Errorf("Read change mismatch: %+v", got.Changes[0])
	}
}

func TestReadRejectsUnknownFormatVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(path, []byte(`{"format_version":"9.9"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Read(path); err == nil {
		t.Fatal("Read accepted format_version 9.9, want error")
	}
}
