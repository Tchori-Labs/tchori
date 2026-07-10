// Package diag provides structured diagnostics shared across the tchori
// engine. Diagnostics are emitted as one JSON object per line on stderr
// (machine mode), or as human-readable text when stderr is a TTY.
package diag

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Severity classifies a Diagnostic.
type Severity string

const (
	Error   Severity = "error"
	Warning Severity = "warning"
)

// Diagnostic is a single structured error or warning.
type Diagnostic struct {
	Severity Severity `json:"severity"`
	Summary  string   `json:"summary"`
	Detail   string   `json:"detail,omitempty"`
	Address  string   `json:"address,omitempty"`
}

// Diagnostics is an ordered list of Diagnostic values.
type Diagnostics []Diagnostic

// HasErrors reports whether ds contains at least one Error-severity
// Diagnostic.
func (ds Diagnostics) HasErrors() bool {
	for _, d := range ds {
		if d.Severity == Error {
			return true
		}
	}
	return false
}

// Errorf builds an Error-severity Diagnostic.
func Errorf(address, summary, detail string) Diagnostic {
	return Diagnostic{Severity: Error, Summary: summary, Detail: detail, Address: address}
}

// Warnf builds a Warning-severity Diagnostic.
func Warnf(address, summary, detail string) Diagnostic {
	return Diagnostic{Severity: Warning, Summary: summary, Detail: detail, Address: address}
}

// Emit writes ds to w. When pretty is false, it writes one compact JSON
// object per line (machine mode). When pretty is true, it writes
// human-readable text: "Error: <summary>" (or "Warning: ..."), optionally
// followed by " (<address>)", then each line of Detail indented by two
// spaces.
func Emit(w io.Writer, ds Diagnostics, pretty bool) {
	if !pretty {
		enc := json.NewEncoder(w)
		for _, d := range ds {
			_ = enc.Encode(d)
		}
		return
	}

	for _, d := range ds {
		label := "Warning"
		if d.Severity == Error {
			label = "Error"
		}
		line := label + ": " + d.Summary
		if d.Address != "" {
			line += " (" + d.Address + ")"
		}
		_, _ = fmt.Fprintln(w, line)
		if d.Detail != "" {
			for _, l := range strings.Split(d.Detail, "\n") {
				_, _ = fmt.Fprintln(w, "  "+l)
			}
		}
	}
}
