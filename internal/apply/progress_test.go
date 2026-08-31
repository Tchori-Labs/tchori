package apply

import (
	"strings"
	"testing"

	"github.com/tchori-labs/tchori/internal/diag"
	"github.com/tchori-labs/tchori/internal/plan"
)

func TestAbortSummaryEmptyProgress(t *testing.T) {
	failed := &plan.Change{Address: "test.failed", Action: "update"}
	ds := abortSummary("custom-state.json", nil, failed, nil)
	if len(ds) != 1 || ds[0].Severity != diag.Warning || ds[0].Summary != "apply aborted" || ds[0].Address != failed.Address {
		t.Fatalf("diagnostics = %#v", ds)
	}
	for _, want := range []string{
		"apply failed at test.failed while attempting action update",
		"nothing was applied to custom-state.json before the failure",
		"no further changes were pending; nothing was left unattempted",
		"state.json has advanced",
		"run tchori plan again before the next apply",
	} {
		if !strings.Contains(ds[0].Detail, want) {
			t.Fatalf("detail = %q; want %q", ds[0].Detail, want)
		}
	}
}

func TestAbortSummaryRecordedAndRemovedMutations(t *testing.T) {
	failed := &plan.Change{Address: "test.replaced", Action: "replace"}
	mutations := []stateMutation{
		{addr: "test.updated", action: "update"},
		{addr: "test.replaced", action: "replace", removed: true},
	}
	ds := abortSummary("state.json", mutations, failed, nil)
	if len(ds) != 1 || !strings.Contains(ds[0].Detail, "test.updated (update): recorded in state") ||
		!strings.Contains(ds[0].Detail, "test.replaced (replace): removed from state") {
		t.Fatalf("diagnostics = %#v", ds)
	}
}

func TestAbortSummaryListsNotAttemptedChanges(t *testing.T) {
	failed := &plan.Change{Address: "test.failed", Action: "update"}
	pending := []*plan.Change{
		{Address: "test.next", Action: "create"},
		{Address: "test.last", Action: "delete"},
	}
	ds := abortSummary("state.json", []stateMutation{{addr: "test.first", action: "update"}}, failed, pending)
	if len(ds) != 1 || !strings.Contains(ds[0].Detail, "changes not attempted after the failure:") ||
		!strings.Contains(ds[0].Detail, "test.next (create)") || !strings.Contains(ds[0].Detail, "test.last (delete)") {
		t.Fatalf("diagnostics = %#v", ds)
	}
}
