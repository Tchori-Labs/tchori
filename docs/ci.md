# Continuous integration

Pull-request CI keeps provider-registry coverage deterministic by separating
local protocol tests from the live public-registry smoke.

## Required PR suites

The required `check` job runs the repository checks from `AGENTS.md`, including
`go test ./...`. That test run covers:

- `tchori providers install` through an in-process registry fixture, including
  registry metadata, archive download, SHA256SUMS verification, cache layout,
  and executable permissions;
- deterministic rejection of a protocol-5-only provider by the tfplugin6
  client; and
- the `internal/ci` policy guard that prevents the live smoke from gaining a
  `pull_request` or `push` trigger and prevents public-registry references from
  entering non-smoke e2e sources.

The required `e2e` job runs the complete CLI lifecycle, fixture-registry
install, and protocol-negotiation failure under dead HTTP and HTTPS proxies.
`NO_PROXY=127.0.0.1,localhost` permits only the in-process `httptest` registry.
A public-network dependency therefore fails fast instead of making PR results
depend on DNS, CDN, or registry availability.

Run the hermetic suite locally:

```sh
go test -tags e2e ./e2e -v
```

To reproduce CI's network-denial proof (after Go modules are available in the
local module cache):

```sh
HTTPS_PROXY=http://127.0.0.1:1 \
HTTP_PROXY=http://127.0.0.1:1 \
NO_PROXY=127.0.0.1,localhost \
go test -count=1 -tags e2e ./e2e -v
```

The `registry_install` and `protocol5_graceful_failure` subtests must pass;
they must not skip.

## Registry mirrors and test fixtures

`providers install` uses `https://registry.opentofu.org` by default. Set
`TCHORI_REGISTRY_URL` to redirect the same registry protocol to an air-gapped
mirror or local fixture:

```sh
TCHORI_REGISTRY_URL=https://registry-mirror.example \
  tchori providers install NAMESPACE/NAME VERSION
```

The override changes only the registry base URL. Version checks, archive
layout validation, and SHA256SUMS checksum verification remain unchanged.
Leaving the variable unset or empty preserves the public-registry default.

## Live registry smoke

`.github/workflows/registry-smoke.yml` preserves real-world coverage against
`registry.opentofu.org`. It runs on a daily schedule or explicit
`workflow_dispatch`; it never runs for `pull_request` or `push` and is not a
required PR status check. The job has read-only repository permissions, a
finite timeout, immutable action pins, and its own concurrency group.

Run the live smoke locally when outbound access is available:

```sh
go test -tags smoke ./e2e -v
```

A genuine network outage skips this non-blocking smoke. Registry protocol,
checksum, cache-layout, or provider-negotiation regressions still fail it.
