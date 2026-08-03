package state

import (
	"fmt"

	"github.com/tchori-labs/tchori/internal/diag"
)

// IncompleteApply records that the apply run which last wrote state.json did
// not complete. It is the durable state-publication guarantee introduced for
// issue #56 / TC-049. It contains resource addresses only: attribute values,
// provider responses, diagnostics that might echo values, and private data
// must never be stored here. It deliberately carries no timestamp so the
// state document remains byte-deterministic and suitable for git diffs.
type IncompleteApply struct {
	FailedAddress string   `json:"failed_address,omitempty"`
	Applied       []string `json:"applied"`
	Remaining     []string `json:"remaining"`
}

// Converged reports whether state is free of an incomplete-apply marker. A nil
// State represents no persisted apply and is therefore converged.
func (s *State) Converged() bool {
	return s == nil || s.Incomplete == nil
}

// IncompleteWarning returns the shared warning used by state readers when the
// loaded artifact records an apply that did not complete.
func (s *State) IncompleteWarning() (diag.Diagnostic, bool) {
	if s.Converged() {
		return diag.Diagnostic{}, false
	}
	failed := s.Incomplete.FailedAddress
	if failed == "" {
		failed = "unknown (apply may still have been in flight)"
	}
	return diag.Warnf(s.Incomplete.FailedAddress, "loaded state is from an incomplete apply", fmt.Sprintf(
		"failed address: %s; %d applied, %d remaining; run tchori state status for details",
		failed, len(s.Incomplete.Applied), len(s.Incomplete.Remaining))), true
}
