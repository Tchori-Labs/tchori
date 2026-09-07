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

For provider/resource environment wrappers, the names are chosen entirely by
the configuration author. **tchori defines no built-in provider credential
names.** It does not automatically try names documented by a provider or
translate one provider's naming convention into another. Use a candidate list
when environments already export different names.

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

Environment wrappers are accepted in provider and resource `config` blocks.
They are valid only where the provider schema expects a string; wrapping a
boolean or number is an error. Objects with additional keys are ordinary JSON
objects, not wrappers. Resource wrappers resolve from the environment at
execution time and are not treated as literal exemptions from sensitivity
redaction. JSON numbers retain their precision when loaded for provider RPCs,
including integers above JavaScript's safe-integer range.

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

## Artifact encryption key

`TCHORI_ARTIFACT_KEY` is an engine setting, separate from provider environment
wrappers. Supply exactly 32 random bytes encoded with standard base64 through
the environment. Tchori uses AES-256-GCM to protect opaque provider private
data in state, backups, and plans. State uses a distinct authenticated envelope
for the full membership of sets containing sensitive leaves; the engine opens
that recovery only before provider operations and never renders it.

Generate a workspace key once and store it in your secret manager. Inject
that same key into subsequent CLI/MCP sessions and automation. Do not place
it in config, command-line arguments, source control, logs, or alongside the
artifacts it protects. Key loss prevents recovery of encrypted provider-private
and sensitive-set data. There is no plaintext fallback or automatic replacement
key.

Apply and import check the key before resource mutations, even when the
provider has not yet returned encrypted data. Reading encrypted artifacts and
writing either nonempty private data class also require the key. Validation and
reads of artifacts without encrypted fields do not require it merely to parse
those artifacts. Invalid keys, failed authentication, and address/type/provider
source/purpose tampering produce errors without revealing protected values.

New state writes use format `1.3`; plan writes remain format `1.2`. This build
reads state `1.2` recovery envelopes, `1.1` encrypted-private artifacts, and
legacy `1.0` artifacts for migration. Older engines reject newer formats
instead of applying a plan or rewriting state without the required sensitive
recovery generation boundary. `tchori state sanitize` upgrades prior state and
protects its backup without applying infrastructure changes.
Previously leaked values still require credential rotation and history
cleanup; rewriting the working tree does not erase existing commits.
Legacy plans carrying private bytes without recorded resource identity must
be recomputed before apply; reading them does not authenticate which provider
should receive their private data. Current plans are refused if their recorded
type/provider differs from the live execution target.


Encryption protects provider-private bytes and sensitive-set identity, not the
entire document. Resource attributes still rely on provider sensitivity
metadata and explicit `sensitive_attributes`; treat artifacts as sensitive and
review them before publishing.
