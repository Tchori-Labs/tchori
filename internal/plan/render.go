package plan

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

const sensitiveValue = "(sensitive value)"

// RenderHuman writes the stable, human-readable plan representation. The
// sensitive predicate is evaluated for every rendered leaf, including drift;
// nil means no paths are sensitive.
func RenderHuman(w io.Writer, pl *Plan, sensitive func(address, path string) bool) error {
	if err := renderDrift(w, pl.Drift, sensitive); err != nil {
		return err
	}
	if !pl.HasChanges() {
		_, err := fmt.Fprintln(w, "No changes. Configuration matches state.")
		return err
	}
	for _, change := range pl.Changes {
		if change.Action == "no-op" {
			continue
		}
		if _, err := fmt.Fprintf(w, "%s %s\n", actionSymbol(change.Action), change.Address); err != nil {
			return err
		}
		if change.Action == "delete" {
			continue
		}
		before, err := decodeJSON(change.Before)
		if err != nil {
			return fmt.Errorf("decode %s before value: %w", change.Address, err)
		}
		after, err := decodeJSON(change.After)
		if err != nil {
			return fmt.Errorf("decode %s after value: %w", change.Address, err)
		}
		if err := renderAttributes(w, change.Address, before, after, changedPaths(before, after), change.UnknownAfter, change.RequiresReplace, sensitive); err != nil {
			return err
		}
	}
	s := pl.Summary
	_, err := fmt.Fprintf(w, "Plan: %d to create, %d to update, %d to delete, %d to replace.\n",
		s.Create, s.Update, s.Delete, s.Replace)
	return err
}

func renderDrift(w io.Writer, drift []*Drift, sensitive func(address, path string) bool) error {
	if len(drift) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w, "Note: objects have changed outside tchori since the last apply."); err != nil {
		return err
	}
	entries := append([]*Drift(nil), drift...)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Address < entries[j].Address })
	for _, entry := range entries {
		after, err := decodeJSON(entry.After)
		if err != nil {
			return fmt.Errorf("decode drift for %s after value: %w", entry.Address, err)
		}
		if after == nil {
			if _, err := fmt.Fprintf(w, "  ~ %s (object no longer exists)\n", entry.Address); err != nil {
				return err
			}
			continue
		}
		before, err := decodeJSON(entry.Before)
		if err != nil {
			return fmt.Errorf("decode drift for %s before value: %w", entry.Address, err)
		}
		if _, err := fmt.Fprintf(w, "  ~ %s\n", entry.Address); err != nil {
			return err
		}
		paths := entry.Paths
		if len(paths) == 0 {
			paths = changedPaths(before, after)
		}
		if err := renderAttributes(w, entry.Address, before, after, paths, nil, nil, sensitive); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w)
	return err
}

func renderAttributes(w io.Writer, address string, before, after any, paths, unknownAfter, requiresReplace []string, sensitive func(address, path string) bool) error {
	beforeLeaves := flattenJSON(before)
	afterLeaves := flattenJSON(after)
	unknown := normalizedSet(unknownAfter)
	replace := normalizedSet(requiresReplace)
	paths = append([]string(nil), paths...)
	sort.Strings(paths)
	for _, path := range paths {
		b, beforeOK := leafAt(beforeLeaves, path)
		a, afterOK := leafAt(afterLeaves, path)
		normalized := normalizePath(path)
		// An unknown/replacement marker recorded for a whole nested attribute
		// covers every leaf beneath it; otherwise a leaf that only exists on the
		// before side would render as a removal instead of an unknown.
		isUnknown := coveredBy(unknown, normalized)
		isSensitive := sensitive != nil && sensitive(address, path)
		symbol := "~"
		switch {
		case !beforeOK:
			// The path only exists after the change. This includes an explicit
			// null leaf on create, which is still an addition rather than a removal.
			symbol = "+"
		case b == nil && (afterOK || isUnknown):
			symbol = "+"
		case !isUnknown && (!afterOK || a == nil):
			symbol = "-"
		}

		var value string
		switch symbol {
		case "+":
			value = renderedSide(a, afterOK, isUnknown, isSensitive)
		case "-":
			value = renderedSide(b, beforeOK, false, isSensitive) + " -> null"
		default:
			value = renderedSide(b, beforeOK, false, isSensitive) + " -> " + renderedSide(a, afterOK, isUnknown, isSensitive)
		}
		suffix := ""
		if coveredBy(replace, normalized) {
			suffix = " # forces replacement"
		}
		if _, err := fmt.Fprintf(w, "    %s %s = %s%s\n", symbol, path, value, suffix); err != nil {
			return err
		}
	}
	return nil
}

func renderedSide(value any, present, unknown, redacted bool) string {
	if redacted {
		return sensitiveValue
	}
	if unknown {
		return "(known after apply)"
	}
	if !present {
		return "null"
	}
	return formatValue(value)
}

func decodeJSON(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

// coveredBy reports whether normalized is in set, either exactly or as a leaf
// nested under a path the set records.
func coveredBy(set map[string]bool, normalized string) bool {
	if set[normalized] {
		return true
	}
	for candidate := range set {
		if candidate != "" && strings.HasPrefix(normalized, candidate+".") {
			return true
		}
	}
	return false
}

func normalizedSet(paths []string) map[string]bool {
	out := make(map[string]bool, len(paths))
	for _, path := range paths {
		out[normalizePath(path)] = true
	}
	return out
}

func leafAt(leaves map[string]leafValue, path string) (any, bool) {
	if leaf, ok := leaves[path]; ok {
		return leaf.value, true
	}
	normalized := normalizePath(path)
	for candidate, leaf := range leaves {
		if normalizePath(candidate) == normalized {
			return leaf.value, true
		}
	}
	return nil, false
}

func actionSymbol(action string) string {
	switch action {
	case "create":
		return "+"
	case "update":
		return "~"
	case "delete":
		return "-"
	case "replace":
		return "-/+"
	default:
		return " "
	}
}
