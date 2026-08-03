package sensitive

import (
	"testing"

	"github.com/zclconf/go-cty/cty"
)

func TestPathString(t *testing.T) {
	cases := []struct {
		path cty.Path
		want string
	}{
		{nil, ""}, {cty.GetAttrPath("id"), "id"},
		{cty.GetAttrPath("items").Index(cty.NumberIntVal(0)).GetAttr("name"), "items[0].name"},
		{cty.GetAttrPath("tags").IndexString("env"), `tags["env"]`},
		{append(cty.GetAttrPath("items"), cty.IndexStep{Key: cty.UnknownVal(cty.String)}), "items[...]"},
	}
	for _, tc := range cases {
		if got := PathString(tc.path); got != tc.want {
			t.Errorf("PathString = %q, want %q", got, tc.want)
		}
	}
}
