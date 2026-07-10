// Package plan implements the tchori plan engine and the schema-versioned,
// deterministic plan.json document it produces.
package plan

import (
	"encoding/json"
	"fmt"
	"os"
)

// FormatVersion is the plan.json schema version this engine writes and reads.
const FormatVersion = "1.0"

type Change struct {
	Address         string          `json:"address"`
	Action          string          `json:"action"`                  // "create","update","delete","replace","no-op"
	Before          json.RawMessage `json:"before"`                  // JSON null for create
	After           json.RawMessage `json:"after"`                   // unknowns rendered as JSON null; JSON null for delete
	UnknownAfter    []string        `json:"unknown_after,omitempty"` // dotted attr paths
	RequiresReplace []string        `json:"requires_replace,omitempty"`
	PlannedRaw      []byte          `json:"planned_raw,omitempty"` // msgpack of planned state incl. unknowns
	Private         []byte          `json:"private,omitempty"`
}

type Summary struct {
	Create  int `json:"create"`
	Update  int `json:"update"`
	Delete  int `json:"delete"`
	Replace int `json:"replace"`
}

type Plan struct {
	FormatVersion string    `json:"format_version"` // "1.0"
	EngineVersion string    `json:"engine_version"`
	StateSerial   uint64    `json:"state_serial"`
	Changes       []*Change `json:"changes"` // sorted by Address
	Summary       Summary   `json:"summary"`
}

// HasChanges reports whether the plan contains any non-no-op change. It
// drives the CLI exit code: 0 = no changes, 2 = changes present.
func (pl *Plan) HasChanges() bool {
	s := pl.Summary
	return s.Create+s.Update+s.Delete+s.Replace > 0
}

// Write serializes the plan deterministically: encoding/json MarshalIndent
// with two-space indent plus a trailing newline. Map-free struct encoding and
// address-sorted Changes make the same plan produce byte-identical files.
func Write(pl *Plan, path string) error {
	b, err := json.MarshalIndent(pl, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o600)
}

// Read loads a plan.json written by Write, rejecting unknown format versions.
func Read(path string) (*Plan, error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: path is operator-supplied (CLI flag / fixed plan.json location), not attacker-controlled
	if err != nil {
		return nil, err
	}
	pl := &Plan{}
	if err := json.Unmarshal(b, pl); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if pl.FormatVersion != FormatVersion {
		return nil, fmt.Errorf("unsupported plan format_version %q (want %q)", pl.FormatVersion, FormatVersion)
	}
	return pl, nil
}
