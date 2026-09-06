# Acceptance checklist: existing Cloudflare DNS import

Procedure and evidence template for the adoption criterion in
[Tchori-Labs/tchori-internal issue #4](https://github.com/Tchori-Labs/tchori-internal/issues/4).
A credentialed operator runs it in the Tchori-Labs infra repository against
`infra/cloudflare`. It remains manual until it can be automated. This criterion
is satisfied only after an operator completes the procedure, merges the config
and state changes in that repository, and records a no-op plan; this document's
existence alone is not acceptance evidence.

## Prerequisites

> **Pinned build required.** Public release `v0.1.0` includes `import`, but it
> predates the `1.1` encrypted-artifact and provider-source identity protections
> required by this acceptance procedure. `v0.1.1` is not released. Until the
> reviewed integration is promoted to `main` and receives a board-approved
> release, build the pinned public `develop` commit below rather than using
> `go install ...@latest`.
>
> **Security rationale for the pin.** This public integration commit includes
> `import`, authenticated private artifacts, provider-source binding, and the
> gRPC and `x/text` dependency upgrades that avoid GO-2026-6061 and
> GO-2026-5970. Before any future repin, prove that the new SHA (a) is an
> ancestor of `origin/develop`, (b) contains `newImportCmd`, (c) carries
> `google.golang.org/grpc` >= v1.82.1 and `golang.org/x/text` >= v0.39.0, and
> (d) retains the artifact protections; then rerun `govulncheck ./...`.

- [ ] Build commit `ea73e83f3b4adeec14e40a98e2e80efdd32fc61f` from a clean
      tchori checkout into a dedicated directory:

```bash
# Build from the pinned develop commit in a clean checkout
TCHORI_SRC=/path/to/tchori
PINNED_SHA=ea73e83f3b4adeec14e40a98e2e80efdd32fc61f
git -C "$TCHORI_SRC" fetch origin develop
git -C "$TCHORI_SRC" checkout "$PINNED_SHA"

# Record both provenance checks; the first must print nothing
git -C "$TCHORI_SRC" status --porcelain
git -C "$TCHORI_SRC" rev-parse HEAD

mkdir -p "$HOME/.local/tchori-bin"
(cd "$TCHORI_SRC" && go build -o "$HOME/.local/tchori-bin/tchori" ./cmd/tchori)

# Discipline A prepends the dedicated build directory
export PATH="$HOME/.local/tchori-bin:$PATH"

# Discipline B exports an absolute-path variable
export TCHORI_BIN="$HOME/.local/tchori-bin/tchori"
```

  Pick exactly one resolution discipline and do not mix them. **Discipline A**
  means use bare `tchori` throughout and ensure `PATH` resolves it to the fresh
  build. **Discipline B** means never type bare `tchori`; invoke every command
  as `"$TCHORI_BIN"` instead. A non-empty `git status --porcelain` or a HEAD
  different from `PINNED_SHA` is a hard stop: clean or re-checkout, then rebuild.
- [ ] Prove resolution before touching infra. Under Discipline A,
      `command -v tchori` must print `$HOME/.local/tchori-bin/tchori`; a path
      such as `~/go/bin/tchori` or `/usr/local/bin/tchori` may resolve to a
      release binary without the required artifact protections, so stop and fix
      `PATH`. Under Discipline B, verify `ls -l "$TCHORI_BIN"` and use that
      expansion everywhere. Run `tchori import --help`: it must exit 0 and
      print `tchori import ADDRESS ID`.
- [ ] Capture `tchori version` as metadata only. Source builds normally report
      `0.1.0-dev`; release builds stamp their version through linker flags.
      Neither value alone proves source provenance. Identity comes from the
      clean checkout, matching HEAD, build from that checkout, resolution
      check, and `import --help` capability probe.
- [ ] Check out Tchori-Labs/infra. Its `infra/cloudflare` directory must have
      the existing `state.json` and at least one `*.tchori.json`. Start with a
      clean `git status` so every resulting change is attributable to this run.
- [ ] Ensure the config-pinned `cloudflare/cloudflare` provider is installed.
      `tchori providers list` must show it; otherwise run `tchori providers
      install cloudflare/cloudflare <pinned-version>`. Schema inspection below
      launches the provider.
- [ ] Export a Cloudflare API token with DNS edit scope for the `tchori.com.br`
      zone as `CLOUDFLARE_API_TOKEN`, and the zone ID as
      `CLOUDFLARE_ZONE_ID`. Source the token from a secret manager or use
      `read -rs`; never type it inline or retain it in shell history.
- [ ] Inject the persistent workspace `TCHORI_ARTIFACT_KEY` from the approved
      secret manager: standard base64 encoding of 32 random bytes. Import
      refuses to mutate without a valid key. Reuse the same key for encrypted
      state, plans, and backup recovery; never print it, commit it, or generate
      a different key on each invocation. See
      [artifact key management](configuration.md#artifact-encryption-key) and
      [infra adoption #150](https://github.com/Tchori-Labs/infra/issues/150).
- [ ] Install `jq` and curl 7.x or newer; verify with `curl --version`. Keep
      shell xtrace (`set -x`) off.

## Checklist

- [ ] **1. Confirm the binary in use.** Re-run the two checkout provenance
      checks, resolution check, `tchori version` metadata capture, and `tchori
      import --help`. Stop on any mismatch. Do this again after a new shell,
      `sudo`, SSH hop, or checkout change: these can lose `PATH` or change the
      source after it was inspected.

- [ ] **2. Pin the working directory.** Run:

```bash
cd "<infra-checkout>/infra/cloudflare"
pwd
ls state.json
ls *.tchori.json
```

  `pwd` must end in `infra/cloudflare`, and both `ls` commands must succeed.
  Every later command runs here: the config glob, `state.json`, and MCP server
  workdir are all relative to the process working directory. The equivalent
  global `--chdir "<infra-checkout>/infra/cloudflare"` may be used consistently
  from the repo root. Running elsewhere can load an empty/different config and
  silently read or create the wrong state file.

- [ ] **3. Identify the exact config file and address.** Config loading globs
      `*.tchori.json`, merges files in lexical order, and keys resources as
      `type.name` under top-level `resources`. Record the exact existing file
      to edit and the address `cloudflare_dns_record.<name>` (for example,
      `cloudflare_dns_record.tchori_verification`). Prefer the existing
      Cloudflare config file. Confirm the address occurs in neither any other
      config file nor `tchori state list`; duplicate config addresses are a
      load error and import refuses to overwrite state. Record the file and
      address; that exact address is the later `ADDRESS` operand.

- [ ] **4. Identify exactly one record and retain its full object.** Run this
      exact credential-safe, URL-encoded query:

```bash
curl -sS --config - --get \
  --data-urlencode "type=TXT" \
  --data-urlencode "name=_tchori.tchori.com.br" \
  -H "Content-Type: application/json" <<EOF | jq '{success, count: (.result | length), records: .result}'
header = "Authorization: Bearer ${CLOUDFLARE_API_TOKEN}"
url = "https://api.cloudflare.com/client/v4/zones/${CLOUDFLARE_ZONE_ID}/dns_records"
EOF
```

  `success` must be true and `count` exactly `1`. Verify the returned type,
  name, and content, record its ID, and save the **whole** record JSON. A zero
  count means the zone/name is wrong. More than one is a hard stop: narrow with
  `--data-urlencode "content=<exact value>"` or select the record ID in the
  dashboard until exactly one intended object is identified.

  `--config -` reads the bearer header over stdin, so the token never enters
  curl's argv and is absent from `ps`, `/proc/<pid>/cmdline`, and per-process
  command auditing. In contrast, a curl `-H`/`--header` Authorization argument
  exposes it. Bash may materialize the heredoc in a short-lived mode-0600 file;
  this is better than world-readable argv but not zero-trace on a shared host,
  and xtrace would echo it. Do not move interpolation into another process's
  arguments: for example, inline `printf 'header = "Authorization: Bearer
  %s"' "${CLOUDFLARE_API_TOKEN}" | curl --config - …` can expose the value if
  `printf` is external. The filters are encoded rather than embedded in the
  URL, and the token travels only in a header. Returning `records: .result` is
  deliberate: projecting only id/type/name/content hides TTL, proxied,
  comment, tags, priority, settings, and other fields that can cause drift.

- [ ] **5. Enumerate every managed argument and declare live values.** Every
      argument managed by the pinned provider for `cloudflare_dns_record` must
      either be declared with the live value or be genuinely optional and
      unmanaged. Run the `provider_schema` MCP tool from this same directory
      with the token exported. It is JSON-RPC payload data, not a CLI
      subcommand or positional operand. The brief waits keep stdin open until
      the asynchronous server has emitted each response; EOF then shuts it
      down cleanly.

```bash
{
  printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"runbook","version":"0.0.1"}}}'
  sleep 1
  printf '%s\n' '{"jsonrpc":"2.0","method":"notifications/initialized"}'
  printf '%s\n' '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"provider_schema","arguments":{"name":"cloudflare"}}}'
  sleep 3
} | tchori mcp \
  | jq -r 'select(.id == 2) | .result.content[0].text' \
  | jq '.resource_types.cloudflare_dns_record.block.attributes'
```

  The `arguments.name` value is the provider **local name**, the key under
  `providers`, not the `cloudflare/cloudflare` source slug. The protocol is
  newline-delimited JSON-RPC 2.0: initialize, initialized notification, then
  tools/call. The result is a text content block whose `.text` is itself JSON,
  hence the second `jq`. Required and operator-settable optional attributes
  carrying live values must be matched; computed-only attributes such as `id`
  must not be declared. If `isError` is true, read its diagnostics: common
  causes are an uninstalled provider, wrong directory/local name, or unset
  token. If the type is under `unsupported_resource_types`, use the fallback.

  The credential-free schema source is the exact pinned-version page
  `https://search.opentofu.org/provider/cloudflare/cloudflare/v<pinned-version>/docs/resources/dns_record`.
  Verify the substituted URL returns HTTP 200. If unavailable, use
  `https://registry.terraform.io/providers/cloudflare/cloudflare/<pinned-version>/docs/resources/dns_record`.
  Record which source was used.

  Edit the chosen config file, merging rather than duplicating its top-level
  objects. This fill-in declaration illustrates the required shape:

```json
{
  "providers": {
    "cloudflare": {
      "source": "cloudflare/cloudflare",
      "version": "<pinned-version>",
      "config": { "api_token": { "env": "CLOUDFLARE_API_TOKEN" } }
    }
  },
  "resources": {
    "cloudflare_dns_record.tchori_verification": {
      "provider": "cloudflare",
      "config": {
        "zone_id": "<zone-id>",
        "name": "_tchori.tchori.com.br",
        "type": "TXT",
        "content": "<live content value>",
        "ttl": 1,
        "comment": "<live comment, or omit if none>"
      }
    }
  }
}
```

  Adapt rather than copy. TTL must equal the live numeric value (automatic is
  `1`). Proxied is not meaningful for TXT; include it only when schema and live
  object both carry it. Match comment/tags when set and omit only when absent.
  Priority applies to MX/SRV, not TXT. Environment wrappers are permitted in
  both provider and resource config when the schema expects a string. Record
  each managed argument and whether its source was a live field or deliberate
  omission.

- [ ] **6. Validate.** `tchori validate` must exit 0, proving the merged block
      parses and its provider resolves before state is touched.
- [ ] **7. Import.** Run `tchori import cloudflare_dns_record.<name>
      <record-id>` using the exact recorded values. Both operands are required.
      It must exit 0 and print `Imported cloudflare_dns_record.<name>
      (id=<record-id>).`; missing operands exit 1.
- [ ] **8. Inspect.** `tchori state show cloudflare_dns_record.<name>` must show
      the intended object. Compare every attribute with the full API object.
- [ ] **9. Prove a no-op plan.** `tchori plan` must exit 0 and print `No
      changes`. Capture output verbatim. If it differs, do not re-import or
      edit state: plan is read-only and the diff names config mismatches.
      Adjust config to live values, then repeat validate and plan until empty.
      Record adjustment rounds and corrected arguments. Roll back only if the
      wrong object was adopted or convergence is impossible.
- [ ] **10. Commit.** Commit the config declaration and generated `state.json`
      through the infra repository's normal reviewed PR flow. Attach the
      completed run record and verbatim no-op output as PR evidence.

## Safety and rollback

Never place the token in config, argv, this run record, or a PR comment. It is
referenced only as `${CLOUDFLARE_API_TOKEN}` in curl's stdin config and as
`{"env": "CLOUDFLARE_API_TOKEN"}` in provider config. Sensitive attributes are
withheld according to schema/config metadata and opaque private recovery data
is encrypted. Unmarked attributes can still carry token-derived data; inspect
diffs before committing. Neither sanitization nor encryption revokes an
already exposed credential or removes historical copies.

Import never overwrites an existing state entry, so repeating it after a
mistake exits 1. For the expected uncommitted failure (wrong object, or plan
still changing after config adjustment), do not hand-edit state. From
`infra/cloudflare`, restore the generated change with `git restore --
state.json` (or `git checkout -- state.json`), fix config or the record ID, and
restart at validation. If a wrong import was already merged, open a reverting
PR in the infra repo. Rollback is always restore from version control or revert
of a reviewed commit.
Retain the artifact key for restored encrypted versions. If a rollback brings
back legacy plaintext, sanitize the restored state and backup before
recommitting; follow the security incident's rotation/history-remediation
procedure rather than treating a clean working-tree diff as proof of cleanup.

## Run record

| Field | Value |
| --- | --- |
| Date | |
| Operator | |
| Build checkout path | |
| Pinned `develop` SHA | |
| Build checkout `git rev-parse HEAD` (must match) | |
| Build checkout `git status --porcelain` clean (yes/no) | |
| `tchori version` (metadata only; expected `0.1.0-dev`) | |
| Resolved binary path (`command -v tchori` / `TCHORI_BIN`) | |
| `tchori import --help` exit 0 (yes/no) | |
| Working directory (expected `infra/cloudflare`) | |
| Config file edited (`*.tchori.json`) | |
| Resource address | |
| Cloudflare zone ID | |
| Record ID | |
| Record query `count` | |
| Schema source (MCP tool / pinned docs URL) | |
| Provider-managed arguments matched and source | |
| Plan adjustment rounds needed | |
| `plan` result (no-op yes/no) | |
| Result (pass/fail) | |
| Notes / deviations | |
