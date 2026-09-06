package sensitive

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

// RedactJSON provider-freely nulls matching concrete leaves in ctyjson bytes.
// It is intentionally conservative and never honors literal exemptions; use
// Spec.RedactJSON when a live schema and raw configuration are available.
func RedactJSON(attrs json.RawMessage, paths []string) (json.RawMessage, []string, error) {
	if len(paths) == 0 {
		return append(json.RawMessage(nil), attrs...), nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(attrs))
	dec.UseNumber()
	var root any
	if err := dec.Decode(&root); err != nil {
		return nil, nil, fmt.Errorf("decode attributes: %w", err)
	}
	sortedPaths := append([]string(nil), paths...)
	sort.Strings(sortedPaths)
	changed := map[string]bool{}
	root = redactJSONValue(root, "", sortedPaths, changed)
	out, err := json.Marshal(root)
	if err != nil {
		return nil, nil, fmt.Errorf("encode attributes: %w", err)
	}
	redacted := make([]string, 0, len(changed))
	for p := range changed {
		redacted = append(redacted, p)
	}
	sort.Strings(redacted)
	return out, redacted, nil
}

func redactJSONValue(v any, logical string, paths []string, changed map[string]bool) any {
	if contains(paths, logical) {
		if v == nil {
			return nil
		}
		changed[logical] = true
		return nil
	}
	switch x := v.(type) {
	case []any:
		for i := range x {
			x[i] = redactJSONValue(x[i], logical, paths, changed)
		}
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, key := range keys {
			candidate := join(logical, key)
			if pathPrefix(candidate, paths) {
				x[key] = redactJSONValue(x[key], candidate, paths, changed)
			} else {
				x[key] = redactJSONValue(x[key], logical, paths, changed)
			}
		}
	}
	return v
}

// JSONRedactor sanitizes ctyjson bytes using live schema structure.
type JSONRedactor func(attrs json.RawMessage, paths []string) (json.RawMessage, []string, error)

// Redactor binds this sensitivity specification to a concrete provider type.
func (s *Spec) Redactor(ty cty.Type) JSONRedactor {
	return func(attrs json.RawMessage, paths []string) (json.RawMessage, []string, error) {
		return s.RedactJSON(attrs, ty, paths)
	}
}

// RedactJSON decodes provider state with its live cty type before redacting.
// Structural cty paths distinguish schema attributes from map element keys.
func (s *Spec) RedactJSON(attrs json.RawMessage, ty cty.Type, paths []string) (json.RawMessage, []string, error) {
	value, err := ctyjson.Unmarshal(attrs, ty)
	if err != nil {
		return nil, nil, fmt.Errorf("decode typed attributes: %w", err)
	}
	spec := &Spec{
		paths:       sortedUnique(paths),
		exempt:      s.exempt,
		setPrefixes: s.setPrefixes,
	}
	redacted, changed, err := spec.Redact(value)
	if err != nil {
		return nil, nil, err
	}
	out, err := ctyjson.Marshal(redacted, ty)
	if err != nil {
		return nil, nil, fmt.Errorf("encode typed attributes: %w", err)
	}
	return out, changed, nil
}

func sortedUnique(paths []string) []string {
	set := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if path != "" {
			set[path] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for path := range set {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}
