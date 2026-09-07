// Package sensitive identifies and withholds sensitive resource attributes.
package sensitive

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/zclconf/go-cty/cty"
)

// AttrPath converts a dotted attribute path into GetAttr steps.
func AttrPath(dotted string) cty.Path {
	if dotted == "" {
		return nil
	}
	parts := strings.Split(dotted, ".")
	path := cty.GetAttrPath(parts[0])
	for _, part := range parts[1:] {
		path = path.GetAttr(part)
	}
	return path
}

// PathString renders a cty.Path as a dotted attribute path: "id",
// "triggers.foo", `tags["env"]`, "items[0]". Adapted from Terraform's
// internal tfdiags.FormatCtyPath (internal/tfdiags, BUSL-1.1), without the
// leading dot. An empty path renders as an empty string.
func PathString(path cty.Path) string {
	var buf strings.Builder
	for i, step := range path {
		switch ts := step.(type) {
		case cty.GetAttrStep:
			if i > 0 {
				buf.WriteByte('.')
			}
			buf.WriteString(ts.Name)
		case cty.IndexStep:
			key := ts.Key
			switch {
			case key.Type() == cty.Number && key.IsKnown() && !key.IsNull():
				_, _ = fmt.Fprintf(&buf, "[%s]", key.AsBigFloat().Text('g', -1))
			case key.Type() == cty.String && key.IsKnown() && !key.IsNull():
				_, _ = fmt.Fprintf(&buf, "[%s]", strconv.Quote(key.AsString()))
			default:
				buf.WriteString("[...]")
			}
		}
	}
	return buf.String()
}

func logicalPath(path cty.Path) string {
	var parts []string
	for _, step := range path {
		if attr, ok := step.(cty.GetAttrStep); ok {
			parts = append(parts, attr.Name)
		}
	}
	return strings.Join(parts, ".")
}
