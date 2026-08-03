package apply

import (
	"github.com/zclconf/go-cty/cty"

	"github.com/tchori-labs/tchori/internal/diag"
	"github.com/tchori-labs/tchori/internal/provider"
)

// composeConfig resolves and validates a resource config before applyChange
// selects an apply leg. In particular, TC-048 requires this to run before a
// replace destroys the prior object. Compose cannot attach a resource address,
// so this apply layer stamps unresolved-reference diagnostics while preserving
// every other diagnostic unchanged.
func (ex *executor) composeConfig(addr string, ty cty.Type) (cty.Value, diag.Diagnostics) {
	if ex.cfg == nil || ex.cfg.Resources[addr] == nil {
		return cty.NilVal, diag.Diagnostics{diag.Errorf(addr, "resource missing from configuration",
			"the planned change address is not in the loaded configuration")}
	}
	value, ds := provider.Compose(ex.cfg.Resources[addr].Config, ty, provider.EnvResolve, ex.resolveRef)
	for i := range ds {
		if ds[i].Summary == "unresolved reference" && ds[i].Address == "" {
			ds[i].Address = addr
		}
	}
	return value, ds
}
