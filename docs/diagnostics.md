# Diagnostic contract

Tchori reports actionable failures and warnings as structured diagnostics. A
diagnostic identifies its severity, preserves the message supplied by the
component that detected the condition, and, when the condition belongs to a
provider RPC, identifies the provider or resource that RPC concerned.

Source of truth: `internal/diag/diag.go`, `internal/provider/hints.go`, and the
provider-RPC call sites in plan, apply, validate, import, and runtime setup.

## Machine mode

When output is not attached to a TTY, and when `-json` requests machine output,
each tchori diagnostic is one compact JSON object on its own stderr line:

```json
{"severity":"error","summary":"Error reading project","detail":"decoding response: invalid character '\u003c' looking for beginning of value","address":"coolify_project.web"}
```

The fields are:

| Field | Values | Meaning |
| --- | --- | --- |
| `severity` | `error` or `warning` | Errors make the command fail; warnings do not. |
| `summary` | string | Short description of the condition. |
| `detail` | string, omitted when empty | Full explanation. It may contain newlines. |
| `address` | string, omitted when empty | Engine context for the condition, using the rules below. |

A command can emit more than one object. Consumers should parse stderr one
JSON diagnostic line at a time rather than treating the stream as one JSON
array.

## Pretty mode

On an interactive stderr TTY, the same diagnostic is rendered as text:

```text
Error: Error reading project (coolify_project.web)
  decoding response: invalid character '<' looking for beginning of value
```

The first line is `<Severity>: <summary> (<address>)`; the parenthesized
address is omitted only when the diagnostic has none. Every source line in a
multi-line `detail` is rendered on its own line with two spaces of indentation.
The structured values and command outcome are the same in both modes.

## Provider diagnostic addresses

Every diagnostic leaving a provider RPC is attributed exactly once, where the
engine knows the context that issued the RPC:

- resource-scoped RPCs use the resource address, for example
  `coolify_project.web`;
- if the provider also supplied an attribute path, tchori qualifies it after
  the resource address, for example `coolify_project.web.endpoint`;
- provider-scoped RPCs such as schema loading and configuration use
  `provider.<local name>`, for example `provider.coolify`.

This applies to refresh, validation, planning, apply (including both legs of a
replacement), import and post-import refresh, schema loading, and provider
configuration. Tchori relays a provider's `summary` and `detail` **verbatim**:
it does not rewrite, truncate, translate, or downgrade third-party errors.
Address attribution and a separately appended advisory warning are the only
additions made at this boundary.

## Non-JSON response advisory

When an error contains Go `encoding/json`'s
`invalid character '<'` signature, tchori appends exactly one warning to that
RPC batch:

```text
Warning: provider received a non-JSON response (HTML) (coolify_project.web)
```

The warning explains the common cause classes:

- an identity-aware proxy such as Cloudflare Access returned a login page to
  an unauthenticated request;
- the endpoint names a web UI, redirect, or error page instead of the API base
  URL; or
- a missing, expired, or rejected API token produced an HTML error page.

The hint is advisory. Tchori sees only the provider's decode failure, not the
HTTP response, response body, endpoint, token, or environment values. Confirm
the endpoint with a direct credentialed request that returns JSON before
re-running tchori. The original error remains present, `HasErrors()` remains
true, and command exit codes do not change.

For an Access-gated endpoint, making the request succeed requires the provider
to support and send the proxy's credential headers (for example,
`CF-Access-Client-Id` and `CF-Access-Client-Secret`). For Coolify that support
belongs upstream in `coolify-terraform/coolify`; tchori cannot add headers to
HTTP requests made inside a third-party provider process.
