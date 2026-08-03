package provider

import (
	"strings"

	"github.com/tchori-labs/tchori/internal/diag"
)

// nonJSONResponseMarker is encoding/json's stable SyntaxError wording when a
// decoder encounters '<' where a JSON value must begin. At that position '<'
// essentially identifies a markup document (HTML or XML), so it is safe to
// classify the response without inspecting or relaying its body.
const nonJSONResponseMarker = "invalid character '<'"

const nonJSONResponseHintDetail = `The response begins with <, so the endpoint returned HTML where JSON was expected. Tchori sees only the provider's decode failure, so this hint is advisory.

Common causes are:
- an identity-aware proxy such as Cloudflare Access answering an unauthenticated request with a login page;
- an endpoint pointing at the web UI, a redirect, or an error page rather than the API base URL; or
- a missing, expired, or rejected API token answered with an HTML error page.

Verify the endpoint with a direct credentialed request that returns JSON before re-running tchori.`

// Context attributes provider RPC diagnostics to addr and appends one advisory
// warning when an error indicates that the provider decoded markup as JSON.
// Existing diagnostics remain ordered and their provider-supplied text is
// preserved verbatim.
func Context(addr string, ds diag.Diagnostics) diag.Diagnostics {
	out := ds.InContext(addr)
	matched := false
	for _, d := range out {
		if d.Severity == diag.Error && (strings.Contains(d.Summary, nonJSONResponseMarker) || strings.Contains(d.Detail, nonJSONResponseMarker)) {
			matched = true
			break
		}
	}
	if !matched {
		return out
	}

	// InContext deliberately returns its input unchanged for an empty context.
	// Copy here before append so Context never writes into input backing storage.
	if addr == "" {
		copied := make(diag.Diagnostics, len(out))
		copy(copied, out)
		out = copied
	}
	return append(out, diag.Warnf(addr,
		"provider received a non-JSON response (HTML)",
		nonJSONResponseHintDetail))
}
