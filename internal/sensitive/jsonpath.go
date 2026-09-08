package sensitive

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/zclconf/go-cty/cty"
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
// generationVersion and generationPaths describe the authenticated projection
// being opened; currentPaths describe the policy to emit.
type JSONSanitizer func(attrs json.RawMessage, recovery []byte, generationVersion int, generationPaths, currentPaths []string) (json.RawMessage, []string, []byte, error)

// Sanitizer binds this sensitivity specification to a concrete provider type.
func (s *Spec) Sanitizer(ty cty.Type) JSONSanitizer {
	return func(attrs json.RawMessage, recovery []byte, generationVersion int, generationPaths, currentPaths []string) (json.RawMessage, []string, []byte, error) {
		return s.SanitizeJSON(attrs, recovery, ty, generationVersion, generationPaths, currentPaths)
	}
}

// SanitizeJSON opens the persisted projection under its generation-time
// contract, then emits a fresh projection and recovery payload under the
// current policy. A new policy may therefore withhold additional fields
// without weakening the authenticated association of existing set recovery.
func (s *Spec) SanitizeJSON(attrs json.RawMessage, recovery []byte, ty cty.Type, generationVersion int, generationPaths, currentPaths []string) (json.RawMessage, []string, []byte, error) {
	effectivePaths := sortedUnique(currentPaths)
	spec := &Spec{
		paths:          effectivePaths,
		exempt:         s.exempt,
		setPrefixes:    affectedSets(s.allSetPrefixes, effectivePaths),
		allSetPrefixes: s.allSetPrefixes,
	}
	value, err := spec.restoreGeneration(attrs, recovery, ty, generationPaths, generationVersion)
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
