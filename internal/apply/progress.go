package apply

import (
	"fmt"
	"strings"

	"github.com/tchori-labs/tchori/internal/diag"
	"github.com/tchori-labs/tchori/internal/plan"
)

type stateMutation struct {
	addr, action string
	removed      bool
}

func abortSummary(statePath string, mutations []stateMutation, failed *plan.Change, notAttempted []*plan.Change) diag.Diagnostics {
	var detail strings.Builder
	_, _ = fmt.Fprintf(&detail, "apply failed at %s while attempting action %s\n", failed.Address, failed.Action)
	if len(mutations) == 0 {
		_, _ = fmt.Fprintf(&detail, "nothing was applied to %s before the failure\n", statePath)
	} else {
		_, _ = fmt.Fprintf(&detail, "changes persisted to %s before the failure:\n", statePath)
		for _, mutation := range mutations {
			result := "recorded in state"
			if mutation.removed {
				result = "removed from state"
			}
			_, _ = fmt.Fprintf(&detail, "  %s (%s): %s\n", mutation.addr, mutation.action, result)
		}
	}
	if len(notAttempted) == 0 {
		detail.WriteString("no further changes were pending; nothing was left unattempted\n")
	} else {
		detail.WriteString("changes not attempted after the failure:\n")
		for _, change := range notAttempted {
			_, _ = fmt.Fprintf(&detail, "  %s (%s)\n", change.Address, change.Action)
		}
	}
	detail.WriteString("state.json has advanced, so the saved plan no longer matches it; run tchori plan again before the next apply")
	return diag.Diagnostics{diag.Warnf(failed.Address, "apply aborted", detail.String())}
}
