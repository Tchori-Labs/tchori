package plan

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const maxRenderedValueRunes = 120

type leafValue struct {
	value any
}

// flattenJSON walks a value decoded by encoding/json and returns its leaves in
// stable dotted/indexed path notation. Empty collections are leaves so a
// transition to or from an empty value remains visible.
func flattenJSON(v any) map[string]leafValue {
	out := make(map[string]leafValue)
	// A top-level null means the resource is absent, not an attribute named "".
	if v != nil {
		flattenInto(out, "", v)
	}
	return out
}

func flattenInto(out map[string]leafValue, path string, v any) {
	switch value := v.(type) {
	case map[string]any:
		if len(value) == 0 {
			out[path] = leafValue{value: value}
			return
		}
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			child := key
			if path != "" {
				child = path + "." + key
			}
			flattenInto(out, child, value[key])
		}
	case []any:
		if len(value) == 0 {
			out[path] = leafValue{value: value}
			return
		}
		for i, item := range value {
			flattenInto(out, fmt.Sprintf("%s[%d]", path, i), item)
		}
	default:
		out[path] = leafValue{value: value}
	}
}

// changedPaths returns the sorted JSON-derived paths whose leaf values differ.
func changedPaths(before, after any) []string {
	beforeLeaves := flattenJSON(before)
	afterLeaves := flattenJSON(after)
	paths := make(map[string]struct{}, len(beforeLeaves)+len(afterLeaves))
	for path, b := range beforeLeaves {
		a, ok := afterLeaves[path]
		if !ok || !reflect.DeepEqual(b.value, a.value) {
			paths[path] = struct{}{}
		}
	}
	for path := range afterLeaves {
		if _, ok := beforeLeaves[path]; !ok {
			paths[path] = struct{}{}
		}
	}
	out := make([]string, 0, len(paths))
	for path := range paths {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

// normalizePath makes provider path notation and JSON-walk notation
// comparable. In particular, tags["parent"] == tags.parent and rules.0.name
// == rules[0].name.
func normalizePath(path string) string {
	var parts []string
	for i := 0; i < len(path); {
		switch path[i] {
		case '.':
			i++
		case '[':
			end := strings.IndexByte(path[i:], ']')
			if end < 0 {
				parts = append(parts, path[i:])
				i = len(path)
				continue
			}
			part := path[i+1 : i+end]
			if unquoted, err := strconv.Unquote(part); err == nil {
				part = unquoted
			}
			parts = append(parts, part)
			i += end + 1
		default:
			start := i
			for i < len(path) && path[i] != '.' && path[i] != '[' {
				i++
			}
			parts = append(parts, path[start:i])
		}
	}
	return strings.Join(parts, ".")
}

func formatValue(v any) string {
	switch value := v.(type) {
	case nil:
		return "null"
	case string:
		count := utf8.RuneCountInString(value)
		if count > maxRenderedValueRunes {
			runes := []rune(value)
			return fmt.Sprintf("%s… (%d runes)", strconv.Quote(string(runes[:maxRenderedValueRunes])), count)
		}
		return strconv.Quote(value)
	case json.Number:
		return value.String()
	case float64:
		return strconv.FormatFloat(value, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(value)
	default:
		b, err := json.Marshal(value)
		if err != nil {
			return fmt.Sprintf("%v", value)
		}
		return string(b)
	}
}
