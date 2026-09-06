// Package plan implements the tchori plan engine and its schema-versioned
// plan.json document.
package plan

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tchori-labs/tchori/internal/privateblob"
)

const (
	FormatVersion          = "1.2"
	encryptedFormatVersion = "1.1"
	legacyFormatVersion    = "1.0"
)

type Change struct {
	Address         string          `json:"address"`
	Type            string          `json:"type,omitempty"`
	Provider        string          `json:"provider,omitempty"`
	ProviderSource  string          `json:"provider_source,omitempty"`
	Action          string          `json:"action"`                  // "create","update","delete","replace","no-op"
	Before          json.RawMessage `json:"before"`                  // JSON null for create
	After           json.RawMessage `json:"after"`                   // unknowns rendered as JSON null; JSON null for delete
	UnknownAfter    []string        `json:"unknown_after,omitempty"` // dotted attr paths
	RequiresReplace []string        `json:"requires_replace,omitempty"`
	PlannedRaw      []byte          `json:"planned_raw,omitempty"` // msgpack of planned state incl. unknowns
	Private         []byte          `json:"-"`
}

type Drift struct {
	Address string          `json:"address"`
	Before  json.RawMessage `json:"before"`
	After   json.RawMessage `json:"after"`
	Paths   []string        `json:"paths,omitempty"`
}

type Summary struct {
	Create  int `json:"create"`
	Update  int `json:"update"`
	Delete  int `json:"delete"`
	Replace int `json:"replace"`
}

type Plan struct {
	FormatVersion string    `json:"format_version"`
	EngineVersion string    `json:"engine_version"`
	StateSerial   uint64    `json:"state_serial"`
	Changes       []*Change `json:"changes"` // sorted by Address
	Drift         []*Drift  `json:"drift,omitempty"`
	Summary       Summary   `json:"summary"`
}

type changeDocument struct {
	Address         string          `json:"address"`
	Type            string          `json:"type,omitempty"`
	Provider        string          `json:"provider,omitempty"`
	ProviderSource  string          `json:"provider_source,omitempty"`
	Action          string          `json:"action"`
	Before          json.RawMessage `json:"before"`
	After           json.RawMessage `json:"after"`
	UnknownAfter    []string        `json:"unknown_after,omitempty"`
	RequiresReplace []string        `json:"requires_replace,omitempty"`
	PlannedRaw      []byte          `json:"planned_raw,omitempty"`
	Private         json.RawMessage `json:"private,omitempty"`
}

type planDocument struct {
	FormatVersion string            `json:"format_version"`
	EngineVersion string            `json:"engine_version"`
	StateSerial   uint64            `json:"state_serial"`
	Changes       []*changeDocument `json:"changes"`
	Drift         []*Drift          `json:"drift,omitempty"`
	Summary       Summary           `json:"summary"`
}

type legacyChangeDocument struct {
	Address         string          `json:"address"`
	Action          string          `json:"action"`
	Before          json.RawMessage `json:"before"`
	After           json.RawMessage `json:"after"`
	UnknownAfter    []string        `json:"unknown_after,omitempty"`
	RequiresReplace []string        `json:"requires_replace,omitempty"`
	PlannedRaw      []byte          `json:"planned_raw,omitempty"`
	Private         []byte          `json:"private,omitempty"`
}

type legacyPlanDocument struct {
	FormatVersion string                  `json:"format_version"`
	EngineVersion string                  `json:"engine_version"`
	StateSerial   uint64                  `json:"state_serial"`
	Changes       []*legacyChangeDocument `json:"changes"`
	Drift         []*Drift                `json:"drift,omitempty"`
	Summary       Summary                 `json:"summary"`
}

// MarshalJSON emits format 1.2 and seals each non-empty provider private
// payload using change metadata as authenticated context. Version 1.2 also
// tells older engines not to apply set-sensitive plans they cannot persist.
func (pl Plan) MarshalJSON() ([]byte, error) {
	var changes []*changeDocument
	if pl.Changes != nil {
		changes = make([]*changeDocument, len(pl.Changes))
	}
	for i, ch := range pl.Changes {
		if ch == nil {
			return nil, fmt.Errorf("invalid plan: change %d is null", i)
		}
		var private json.RawMessage
		if len(ch.Private) != 0 {
			if ch.Type == "" || ch.Provider == "" || ch.ProviderSource == "" {
				return nil, fmt.Errorf("seal private plan change for %s: type, provider, and provider source are required", ch.Address)
			}
			sealed, err := privateblob.Seal(ch.Private, privateContext("plan", ch.Address, ch.Provider, ch.ProviderSource, ch.Type))
			if err != nil {
				return nil, fmt.Errorf("seal private plan change for %s: %w", ch.Address, err)
			}
			private = json.RawMessage(sealed)
		}
		changes[i] = &changeDocument{
			Address: ch.Address, Type: ch.Type, Provider: ch.Provider, ProviderSource: ch.ProviderSource, Action: ch.Action,
			Before: ch.Before, After: ch.After, UnknownAfter: ch.UnknownAfter,
			RequiresReplace: ch.RequiresReplace, PlannedRaw: ch.PlannedRaw, Private: private,
		}
	}
	return json.Marshal(planDocument{
		FormatVersion: FormatVersion, EngineVersion: pl.EngineVersion, StateSerial: pl.StateSerial,
		Changes: changes, Drift: pl.Drift, Summary: pl.Summary,
	})
}

// UnmarshalJSON accepts legacy 1.0 plaintext/base64 private fields and
// authenticated 1.1 and 1.2 envelopes.
func (pl *Plan) UnmarshalJSON(data []byte) error {
	var header struct {
		FormatVersion string `json:"format_version"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return err
	}
	switch header.FormatVersion {
	case legacyFormatVersion:
		var legacy legacyPlanDocument
		if err := json.Unmarshal(data, &legacy); err != nil {
			return err
		}
		var changes []*Change
		if legacy.Changes != nil {
			changes = make([]*Change, len(legacy.Changes))
		}
		for i, ch := range legacy.Changes {
			if ch == nil {
				return fmt.Errorf("invalid plan: change %d is null", i)
			}
			changes[i] = &Change{
				Address: ch.Address, Action: ch.Action, Before: ch.Before, After: ch.After,
				UnknownAfter: ch.UnknownAfter, RequiresReplace: ch.RequiresReplace,
				PlannedRaw: ch.PlannedRaw, Private: ch.Private,
			}
		}
		*pl = Plan{
			FormatVersion: legacy.FormatVersion, EngineVersion: legacy.EngineVersion,
			StateSerial: legacy.StateSerial, Changes: changes, Drift: legacy.Drift, Summary: legacy.Summary,
		}
		return nil
	case encryptedFormatVersion, FormatVersion:
		var doc planDocument
		if err := json.Unmarshal(data, &doc); err != nil {
			return err
		}
		var changes []*Change
		if doc.Changes != nil {
			changes = make([]*Change, len(doc.Changes))
		}
		for i, persisted := range doc.Changes {
			if persisted == nil {
				return fmt.Errorf("invalid plan: change %d is null", i)
			}
			ch := &Change{
				Address: persisted.Address, Type: persisted.Type, Provider: persisted.Provider, ProviderSource: persisted.ProviderSource,
				Action: persisted.Action, Before: persisted.Before, After: persisted.After,
				UnknownAfter: persisted.UnknownAfter, RequiresReplace: persisted.RequiresReplace,
				PlannedRaw: persisted.PlannedRaw,
			}
			if len(persisted.Private) != 0 {
				if ch.Type == "" || ch.Provider == "" {
					return fmt.Errorf("open private plan change for %s: type and provider are required", ch.Address)
				}
				private, err := privateblob.Open(persisted.Private, privateContext("plan", ch.Address, ch.Provider, ch.ProviderSource, ch.Type))
				if err != nil {
					return fmt.Errorf("open private plan change for %s: %w", ch.Address, err)
				}
				ch.Private = private
			}
			changes[i] = ch
		}
		*pl = Plan{
			FormatVersion: doc.FormatVersion, EngineVersion: doc.EngineVersion,
			StateSerial: doc.StateSerial, Changes: changes, Drift: doc.Drift, Summary: doc.Summary,
		}
		return nil
	default:
		return fmt.Errorf("unsupported plan format_version %q (supported: %q, %q, and %q)", header.FormatVersion, legacyFormatVersion, encryptedFormatVersion, FormatVersion)
	}
}

func privateContext(kind, addr, provider, providerSource, resourceType string) string {
	if providerSource == "" {
		// Compatibility path for already-persisted 1.1 artifacts. Apply
		// refuses these unbound changes; a new plan performs the migration.
		return kind + "\x00" + addr + "\x00" + provider + "\x00" + resourceType
	}
	return kind + "\x00" + addr + "\x00" + provider + "\x00" + providerSource + "\x00" + resourceType
}

// HasChanges reports whether the plan contains any non-no-op change. It
// drives the CLI exit code: 0 = no changes, 2 = changes present.
func (pl *Plan) HasChanges() bool {
	s := pl.Summary
	return s.Create+s.Update+s.Delete+s.Replace > 0
}

// Write serializes the plan with two-space indentation and a trailing newline.
// Plans without private payloads remain byte-deterministic; encrypted private
// envelopes intentionally use fresh nonces. Write atomically replaces path
// with a regular owner-only (0600) file, so an existing permissive file is
// tightened and a symlink at path is not followed.
func Write(pl *Plan, path string) error {
	b, err := json.MarshalIndent(pl, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), ".plan-*.tmp") //nolint:gosec // G304: path is operator-supplied via the CLI -out flag, not attacker-controlled
	if err != nil {
		return fmt.Errorf("create temp plan file: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write temp plan file: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("chmod temp plan file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close temp plan file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename temp plan file: %w", err)
	}
	pl.FormatVersion = FormatVersion
	return nil
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
	if pl.FormatVersion != legacyFormatVersion && pl.FormatVersion != FormatVersion {
		return nil, fmt.Errorf("unsupported plan format_version %q", pl.FormatVersion)
	}
	return pl, nil
}
