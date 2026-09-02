# Continuous integration

Pull-request CI keeps provider-registry coverage deterministic by separating
local protocol tests from the live public-registry smoke.

## Runners

Every job in `.github/workflows/ci.yml` and `.github/workflows/pr-source.yml`
runs on GitHub-hosted `ubuntu-latest` runners. Self-hosted runners
(`tchori-runner-1`, `tchori-runner-2`) are reserved for the organization's
private repositories: the runner group does not admit public repositories,
and fork pull-request code must never reach those machines.

## Required PR suites

The required `check` job runs the repository checks from `AGENTS.md`, including
`go test ./...`. It also cross-vets the entire workspace with
`GOOS=windows go vet ./...`, catching POSIX-only syscalls in shared and test
code before they reach Windows users. This class of regression has reached the
tree twice: TC-028 in `internal/state` and TC-043 in `internal/provider`.

The `internal/ci` workflow policy requires the Windows vet to remain an
unconditional, executable command in the required `check` job; comments,
`echo` output, command chaining, conditions, and ignored failures cannot
satisfy the guard. The job also uses the pinned actionlint release to
statically validate every `.yml` and `.yaml` file under `.github/workflows`;
the shared verification script self-tests syntax and expression detection and
a clean control before the required context can pass. The Go test run covers:

- `tchori providers install` through an in-process registry fixture, including
  registry metadata, archive download, SHA256SUMS verification, cache layout,
  and executable permissions;
- deterministic rejection of a protocol-5-only provider by the tfplugin6
  client; and
- the `internal/ci` policy guard that prevents the live smoke from gaining a
  `pull_request` or `push` trigger and prevents public-registry references from
  entering non-smoke e2e sources; and
- the `internal/security` disclosure-policy guard over `SECURITY.md` and the
  README, plus its stubbed-`gh` matrix harness for
  `scripts/verify-security-contact.sh`.

The required `e2e` job runs checkout, Go setup, and module download with normal
network access. Its e2e test step runs the complete CLI lifecycle,
fixture-registry install, and protocol-negotiation failure under dead HTTP and
HTTPS proxies. `NO_PROXY=127.0.0.1,localhost` permits only the in-process
`httptest` registry. A public-network dependency therefore fails fast instead
of making PR results depend on DNS, CDN, or registry availability.

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
