package sensitive

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
)

// RedactJSON provider-freely nulls matching concrete leaves in ctyjson bytes.
// Sensitivity is index-insensitive while exemptions are exact instance paths.
func RedactJSON(attrs json.RawMessage, paths []string, exemptInstances []string) (json.RawMessage, []string, error) {
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
	exempt := map[string]bool{}
	for _, p := range exemptInstances {
		exempt[p] = true
	}
	changed := map[string]bool{}
	root = redactJSONValue(root, "", "", sortedPaths, exempt, changed, false)
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

func redactJSONValue(v any, logical, instance string, paths []string, exempt map[string]bool, changed map[string]bool, ambiguous bool) any {
	if contains(paths, logical) {
		if v == nil {
			return nil
		}
		if !ambiguous && exempt[instance] {
			return v
		}
		changed[logical] = true
		return nil
	}
	switch x := v.(type) {
	case []any:
		for i := range x {
			x[i] = redactJSONValue(x[i], logical, fmt.Sprintf("%s[%d]", instance, i), paths, exempt, changed, ambiguous)
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
				_, containerMap := x[key].(map[string]any)
				_, containerList := x[key].([]any)
				x[key] = redactJSONValue(x[key], candidate, join(instance, key), paths, exempt, changed, ambiguous || (contains(paths, candidate) && (containerMap || containerList)))
			} else {
				x[key] = redactJSONValue(x[key], logical, instance+"["+strconv.Quote(key)+"]", paths, exempt, changed, ambiguous)
			}
		}
	}
	return v
}
