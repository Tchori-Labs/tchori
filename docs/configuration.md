# Configuration and environment values

tchori reads JSON configuration from files whose names end in
`*.tchori.json`. Provider declarations live under `providers`; managed
resources live under `resources`. The JSON Schema validates the file shape,
and each provider's schema validates its own `config` values.

This page is the canonical reference for supplying provider configuration from
the environment.

## Environment wrappers

A string-valued attribute in a provider's `config` block can read one
environment variable:

```json
{
  "token": {"env": "SERVICE_TOKEN"}
}
```

It can instead declare an ordered list of candidate names:

```json
{
  "token": {"env": ["SERVICE_ACCESS_TOKEN", "SERVICE_TOKEN"]}
}
```

tchori checks candidates from left to right and uses the first variable that
is set. A variable set to the empty string counts as set and therefore wins
over later candidates. Duplicate names are allowed and are checked in the
order written. A candidate list must contain at least one name, and every
candidate must be a string.

The names are chosen entirely by the configuration author. **tchori defines no
built-in or provider-specific environment variable names.** In particular, it
does not automatically try names documented by a provider or translate one
provider's naming convention into another. Use a candidate list when the same
configuration must work in environments that already export different names.

For example, a Coolify-style provider configuration can accept both an
existing workspace convention and an alternate convention without shell alias
assignments:

```json
{
  "providers": {
    "coolify": {
      "source": "example/coolify",
      "version": "1.0.0",
      "config": {
        "endpoint": {"env": ["COOLIFY_BASE_URL", "COOLIFY_ENDPOINT"]},
        "token": {"env": ["COOLIFY_ACCESS_TOKEN", "COOLIFY_TOKEN"]}
      }
    }
  }
}
```

Environment wrappers are currently accepted only in provider `config` blocks.
They are rejected in resource configuration. They are also valid only where
the provider schema expects a string; wrapping a boolean or number is an
error. Objects with additional keys are ordinary JSON objects, not wrappers.

## Commands that require provider values

`tchori validate` does more than validate the JSON file shape: it builds and
configures providers so it can validate configuration against provider
schemas. Provider-config environment wrappers must therefore be satisfied for
`validate` exactly as they must be for `plan`, `apply`, `destroy`, `import`,
and `tchori mcp`.

For example:

```sh
export COOLIFY_BASE_URL=https://coolify.example
export COOLIFY_ACCESS_TOKEN=... # supply this through your secret manager

tchori -chdir=coolify validate
```

## Unset-variable diagnostics

If no candidate is set, tchori reports every name it checked, in configuration
order, without printing any value. For an `endpoint` wrapper with two
candidates, pretty output is:

```text
Error: environment variable not set
  attribute "endpoint": none of the candidate environment variables "COOLIFY_BASE_URL", "COOLIFY_ENDPOINT" are set.
  These names come from the {"env": ...} wrapper in *.tchori.json; tchori defines no built-in or provider-specific environment variable names.
  Export one of these variables, or add the name your environment already uses to the wrapper list.
```

Machine mode (`-json`, and non-TTY stderr) emits the same summary and detail in
a compact JSON diagnostic. Fix the error by exporting one of the listed names,
or by adding the name already used by the environment to the wrapper's
candidate list. Do not put the credential itself in configuration.

## Sensitive artifacts

An environment wrapper keeps the source value out of `*.tchori.json` and out
of tchori's wrapper diagnostics. It does not guarantee that a provider will
never return or copy that value into resource data. Resolved values may be
recorded in `state.json` or `plan.json`; protect and review those files as
sensitive artifacts. See [plan and state formats](formats.md), including the
sensitive-attribute behavior and override mechanism.
