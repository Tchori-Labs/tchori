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

// JSONSanitizer sanitizes ctyjson bytes with a live schema while retaining
// authenticated sensitive-set recovery separately from the public projection.
type JSONSanitizer func(attrs json.RawMessage, recovery []byte, paths []string) (json.RawMessage, []string, []byte, error)

// Sanitizer binds this sensitivity specification to a concrete provider type.
func (s *Spec) Sanitizer(ty cty.Type) JSONSanitizer {
	return func(attrs json.RawMessage, recovery []byte, paths []string) (json.RawMessage, []string, []byte, error) {
		return s.SanitizeJSON(attrs, recovery, ty, paths)
	}
}

// SanitizeJSON restores authoritative set identity before decoding and emits a
// fresh public projection and recovery payload. A non-empty affected set with
// no recovery is rejected because its redacted elements may already have
// coalesced in an older artifact.
func (s *Spec) SanitizeJSON(attrs json.RawMessage, recovery []byte, ty cty.Type, paths []string) (json.RawMessage, []string, []byte, error) {
	effectivePaths := sortedUnique(paths)
	spec := &Spec{
		paths:          effectivePaths,
		exempt:         s.exempt,
		setPrefixes:    affectedSets(s.allSetPrefixes, effectivePaths),
		allSetPrefixes: s.allSetPrefixes,
	}
	var (
		value cty.Value
		err   error
	)
	if spec.HasSensitiveSets() {
		value, err = spec.Restore(attrs, recovery, ty)
	} else {
		value, err = ctyjson.Unmarshal(attrs, ty)
	}
	if err != nil {
		return nil, nil, nil, fmt.Errorf("decode typed attributes: %w", err)
	}
	out, changed, nextRecovery, err := spec.Project(value)
	if err != nil {
		return nil, nil, nil, err
	}
	return out, changed, nextRecovery, nil
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
