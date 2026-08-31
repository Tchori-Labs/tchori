package state

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestConverged(t *testing.T) {
	tests := []struct {
		name string
		st   *State
		want bool
	}{
		{name: "nil state", st: nil, want: true},
		{name: "no marker", st: &State{}, want: true},
		{name: "marker", st: &State{Incomplete: &IncompleteApply{Applied: []string{}, Remaining: []string{"thing.a"}}}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.st.Converged(); got != tt.want {
				t.Fatalf("Converged() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIncompleteWarning(t *testing.T) {
	st := &State{Incomplete: &IncompleteApply{
		FailedAddress: "thing.boom",
		Applied:       []string{"thing.alpha"},
		Remaining:     []string{"thing.boom"},
	}}
	got, ok := st.IncompleteWarning()
	if !ok {
		t.Fatal("IncompleteWarning reported no warning")
	}
	if !strings.Contains(got.Summary, "incomplete apply") || !strings.Contains(got.Detail, "thing.boom") {
		t.Fatalf("warning = %+v, want incomplete summary and failed address", got)
	}
	if _, ok := (&State{}).IncompleteWarning(); ok {
		t.Fatal("converged state returned a warning")
	}
}

func TestIncompleteApplyJSONShape(t *testing.T) {
	marked, err := json.Marshal(&State{Incomplete: &IncompleteApply{Applied: []string{}, Remaining: []string{}}})
	if err != nil {
		t.Fatal(err)
	}
	got := string(marked)
	if !strings.Contains(got, `"applied":[]`) || !strings.Contains(got, `"remaining":[]`) {
		t.Fatalf("marker slices did not marshal as arrays: %s", got)
	}

	converged, err := json.Marshal(&State{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(converged), "incomplete_apply") {
		t.Fatalf("converged state contains incomplete_apply: %s", converged)
	}
}
