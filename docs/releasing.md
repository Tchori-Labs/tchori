# Releasing and verifying tchori

This document describes the approved release path and how consumers verify its
artifacts. Preparing this workflow does not authorize a release.

## Release policy and publish gate

Per [`CLAUDE.md`](../CLAUDE.md), no release may be tagged or published until a
human board decision explicitly approves it and that decision is recorded in
the `Tchori-Labs/main` repository. Agents may prepare a release change, but they
must not create or push the tag, dispatch the release workflow, approve its
deployment, or publish the release.

Pushing a `v*` tag automatically starts only the release workflow's
**dry-run** job in the public `Tchori-Labs/tchori` repository. A tag push never
starts `publish`, never creates or modifies a GitHub Release, and never exposes
release assets. Its dry-run checks out the existing tag, validates that the tag
is in reviewed `origin/main` history, signs the reviewable artifacts, generates
GitHub build provenance, and uploads them for review. The jobs are disabled in
forks and in `tchori-internal`; internally prepared changes must first be
transferred through review to the public repository. Accepted tags are
`v`-prefixed semantic versions whose commits belong to reviewed `origin/main`
history.

`workflow_dispatch` is the only path to `publish`. An authorized initiator
must run it from `main`, enter the existing approved `v*` tag, select
`mode: publish`, and do so only after the dry-run artifact and its required
Environment review have been examined and approved. Neither dispatch mode
creates or moves a tag. The protected `release` Environment remains a required
gate for both dry-run and publish jobs.

The ancestry check prevents an accidental release from an unreviewed commit; it
does not authenticate the tagger or make historical workflow revisions safe.
Anyone allowed to create or move a matching `v*` tag can trigger a dry-run, but
that permission alone cannot publish. A human repository administrator must
still restrict and audit the live tag-permission boundary. It is not encoded by
this repository.

The live enforcement mechanism is the GitHub Environment named `release`.
The reviewable source of truth is
[`.github/environments/release.json`](../.github/environments/release.json),
with its exact custom policies declared in
[`.github/environments/release-deployment-branch-policies.json`](../.github/environments/release-deployment-branch-policies.json).
Committing these files does not apply the Environment.

### Release Environment requirements

| Requirement | Declared policy |
| --- | --- |
| One human reviews every selected release job. | The only required reviewer is `@VictorCano` (GitHub user id `6369606`), matching [`.github/CODEOWNERS`](../.github/CODEOWNERS). |
| The actor who initiated a deployment cannot approve it. | `prevent_self_review` is `true`. |
| Only the release branch and release tags may deploy. | Custom deployment policies contain exactly branch `main` and tag pattern `v*`; protected-branches mode is disabled because it cannot express this branch-and-tag pair. |
| Both release modes remain gated. | The `dry-run` and `publish` jobs in `.github/workflows/release.yml` both declare `environment: release`; neither may be ungated. |
| Publish access remains least-privileged. | `dry-run` keeps `contents: read`; only `publish` receives `contents: write`. Both jobs retain their job-scoped OIDC and attestation permissions. |

GitHub's Environment REST API exposes `prevent_self_review`, but it does not
expose a per-Environment administrator-bypass toggle. No unsupported field is
invented in the payload. The related no-standing-admin-bypass control is the
empty `bypass_actors` list in the protected-main ruleset documented in
[`branch-protection.md`](branch-protection.md); it is a branch-ruleset control,
not an Environment property.

### Apply (repository admin only)

These commands mutate live repository settings. They are exclusively a human
repository-administrator step. An agent whose permissions do not show
`admin: true` must stop after preparing the payload and must never claim that
protection is active.

From the repository root, confirm repository administration, create or update
the Environment from the committed PUT body, then create the two declared
custom deployment policies:

```sh
gh api repos/Tchori-Labs/tchori --jq .permissions
# Continue only when the response contains: "admin": true

gh api --method PUT repos/Tchori-Labs/tchori/environments/release \
  --input .github/environments/release.json

gh api --method POST \
  repos/Tchori-Labs/tchori/environments/release/deployment-branch-policies \
  -f name=main -f type=branch

gh api --method POST \
  repos/Tchori-Labs/tchori/environments/release/deployment-branch-policies \
  -f 'name=v*' -f type=tag
```

If policies already exist, reconcile them to the exact declared pair rather
than creating duplicates. Preserve the API responses as evidence. A failed or
partial response is not proof of protection.

### Audit and verify

Read the live Environment and its independently managed custom policies, then
run the fail-closed verifier from the repository root:

```sh
gh api repos/Tchori-Labs/tchori/environments/release
gh api \
  repos/Tchori-Labs/tchori/environments/release/deployment-branch-policies
scripts/verify-release-environment.sh
```

The verifier prints a PASS or FAIL for each machine-auditable criterion and
exits non-zero when the Environment is absent, weakened, or unreadable. An
all-PASS run proves live structure only: it does not replace the board
sign-off gate or the per-deployment `@VictorCano` approval in maintainer
runbook step 4 below. Merely declaring `environment: release` in workflow YAML
does not create or protect the Environment.

### Current application status

**Public repository audited on 2026-09-05: PASS.** The live `release`
Environment requires `@VictorCano`, prevents self-review, and restricts custom
deployment policies to exactly branch `main` and tag `v*`.
`scripts/verify-release-environment.sh` passed against `Tchori-Labs/tchori`.
This is structural evidence, not board sign-off or deployment approval; rerun
the verifier before publishing. It does not certify the private archive's
Environment. Historical administrative work remains recorded in
[internal issue #77](https://github.com/Tchori-Labs/tchori-internal/issues/77);
the first-release decision is tracked in
[internal issue #66](https://github.com/Tchori-Labs/tchori-internal/issues/66).

Board approval for `v0.1.0` is recorded in
[ADR-0012](https://github.com/Tchori-Labs/main/blob/main/decisions/0012-tchori-v0.1.0-release.md).
It covers a reviewed promotion on public `main`, not unreviewed local changes.
Because `@VictorCano` is the sole reviewer and self-review is forbidden, the
decision delegates the first tag push to Tchorizo and deployment approval to
Victor. A workflow triggered by Victor cannot also be approved by Victor;
future releases need a distinct authorized initiator or a reviewed change to
the reviewer policy.

**Existing first-release attempt:** `v0.1.0` already resolves to public commit
`74af4ff52ddaf0c07771865bafa60630bc5b7c6e`.
[Run 33652904208](https://github.com/Tchori-Labs/tchori/actions/runs/33652904208)
was observed waiting for approval; approving it would publish that old
commit, not subsequent security fixes. Do not move or recreate the tag to
hide this difference.
[The existing board decision issue](https://github.com/Tchori-Labs/main/issues/163)
records `v0.1.1` as the next release version. That choice does not authorize
tag creation, release dispatch, deployment approval, or publication.

Release-readiness also requires non-secret evidence of credential revocation
and history remediation for
[internal #72](https://github.com/Tchori-Labs/tchori-internal/issues/72) /
[infra #138](https://github.com/Tchori-Labs/infra/issues/138).
The current product writes `plan.json`, `state.json`, and `state.json.backup`
in format `1.2`; it reads `1.0`, `1.1`, and `1.2` as documented in
[formats.md](formats.md). Consumers such as
[infra #150](https://github.com/Tchori-Labs/infra/issues/150) must plan this
migration to the current `1.2` write format; `1.1` is only a legacy input
format supported for reading.
When existing state or its backup has no canonical `provider_source`, run
`tchori state sanitize` with the matching configuration, provider schemas, and
artifact key before planning or applying. When an existing plan lacks complete
resource identity, or provider routing/configuration changed, recompute the
plan before apply; state sanitization does not rewrite a plan. Previously
leaked values still require credential rotation and history cleanup. Local
tests and a clean working-tree secret scan do not certify revocation,
historical cleanup, consumer key provisioning, migration, or deployment
approval.

Both release jobs reference this Environment, so once it is correctly applied
GitHub pauses the selected job for required human review. The `dry-run` job
cannot create or modify a GitHub Release; the `publish` job alone receives
job-scoped `contents: write`. No other workflow or job receives publish
permissions.

## Maintainer runbook

After the board decision and normal CODEOWNERS review have landed:

1. The board-authorized tag actor creates and pushes the approved `v*` tag.
   This triggers **dry-run only**. For `v0.1.0`, follow ADR-0012's scoped
   delegation above. Do not tag from an unreviewed commit.
2. The tag-triggered dry-run checks out that existing tag and runs GoReleaser
   with `--skip=publish`, while retaining real keyless signing and GitHub
   provenance generation. It does not create a tag, GitHub Release, or public
   release asset. Instead, it uploads a 30-day Actions artifact named
   `release-dry-run-<run-id>-<run-attempt>` containing the archives, SBOMs,
   checksum manifest, detached signature and certificate, and the local
   `provenance.intoto.jsonl` bundle for board review.
3. When a dry-run must be repeated without another tag push, an authorized
   initiator distinct from the required reviewer may use
   **Actions → Release → Run workflow** from `main`, enter the same existing
   approved `v*` tag, and leave `mode` at its safe `dry-run` default. This
   dispatch follows the same validation and artifact path, but its workflow
   identity is the reviewed `main` ref.
4. Only after the dry-run artifact has been reviewed and its required
   Environment deployment has been approved may an authorized initiator use
   **Run workflow** from `main` with `mode: publish`. Publish checks out the
   same existing tag; it never creates or moves one.
5. For every mode, `@VictorCano` reviews the pending `release` Environment
   deployment against the recorded board decision and approves or rejects it.
   In publish mode, GoReleaser uploads the signed artifacts to a **draft**
   GitHub Release. `scripts/verify-release-artifacts.sh` requires all six
   platform archives, their six SPDX SBOMs, a complete checksum manifest whose
   digests match those files. `scripts/verify-release-signature.sh` then runs
   Cosign verification against the current workflow/ref certificate identity
   and GitHub Actions OIDC issuer; missing, malformed, or invalid signatures
   stop both dry-run and publish acceptance.
   The workflow then records GitHub build-provenance attestations for every
   archive and `checksums.txt`. Only after attestation succeeds does the
   final step make the draft public. If output validation or attestation
   fails, the release remains a non-public draft for maintainer inspection;
   the workflow never exposes archives before their required provenance exists.
6. A board-approved retry for the same tag safely replaces that incomplete
   draft before uploading a fresh artifact set. Workflow concurrency serializes
   tag-push and manual runs by tag, so one attempt cannot replace or publish
   another attempt's draft.

GoReleaser invokes Cosign v2.6.4 with GitHub's ambient OpenID Connect identity
(the workflow pins this v2 release because the required detached `.sig`/`.pem`
output was replaced by bundles in Cosign v3). Fulcio issues an ephemeral
certificate for this exact workflow invocation; there is no private signing
key, PAT, or other long-lived signing credential to store or rotate.
`secrets.GITHUB_TOKEN` is GitHub's short-lived per-run token and is used only to
create the GitHub Release.

### Local preparation without publication

With the pinned Go toolchain, GoReleaser v2, and Syft installed:

```sh
scripts/check.sh
go test -count=1 -tags e2e ./e2e
govulncheck ./...
goreleaser check
goreleaser release --snapshot --clean --skip=sign,before
```

The snapshot builds all six platforms and generates SBOMs and checksums
without publishing. `before` skips `go mod tidy` so validation does not alter
the dependency manifest. Local unsigned snapshots cannot prove Cosign OIDC
signing, GitHub provenance issuance, or consumer verification of a published
release; those require the approved live workflow.

## Published artifact names

For a release such as `v0.1.0`, GoReleaser publishes:

- Unix archives: `tchori_0.1.0_<os>_<arch>.tar.gz`
- Windows archives: `tchori_0.1.0_windows_<arch>.zip`
- One SPDX JSON SBOM beside each archive, formed by appending `.sbom.json`, for
  example `tchori_0.1.0_linux_amd64.tar.gz.sbom.json`
- `checksums.txt`, which covers the archives and their SBOMs
- `checksums.txt.sig` and `checksums.txt.pem`, the keyless Cosign signature and
  Fulcio certificate for the checksum manifest

The checksum manifest is the only GoReleaser artifact signed directly. Its
verified signature authenticates every digest in the manifest, including all
archives and SBOMs. GitHub provenance additionally attests each archive and the
manifest directly.

## Consumer verification

Install [GitHub CLI](https://cli.github.com/),
[Cosign](https://docs.sigstore.dev/cosign/system_config/installation/),
`sha256sum`, and `jq`. The following example verifies the Linux amd64 archive;
change `TAG`, operating system, and architecture as needed:

```sh
TAG=v0.1.0
VERSION="${TAG#v}"
ARCHIVE="tchori_${VERSION}_linux_amd64.tar.gz"

# Download the archive, its SBOM, and the signed checksum manifest.
gh release download "$TAG" --repo tchori-labs/tchori \
  --pattern "$ARCHIVE" \
  --pattern "$ARCHIVE.sbom.json" \
  --pattern checksums.txt \
  --pattern checksums.txt.sig \
  --pattern checksums.txt.pem
```

### 1. Verify the keyless signature

Verify the certificate's GitHub Actions issuer and restrict its identity to
the workflow/ref that produced the artifact. A published release can only come
from the explicit manual `publish` dispatch, so its identity must end in
`@refs/heads/main`:

```sh
cosign verify-blob \
  --certificate checksums.txt.pem \
  --signature checksums.txt.sig \
  --certificate-identity-regexp '^https://github\.com/Tchori-Labs/tchori/\.github/workflows/release\.yml@refs/heads/main$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
```

A tag-triggered dry-run artifact instead has the exact tag identity
`@refs/tags/v...`; a manual dry-run has the exact `@refs/heads/main` identity.
Those artifacts are review evidence, not published releases. Do not use a
repository-wide or issuer-only identity expression.

### 2. Verify archive and SBOM checksums

Select exactly the expected archive and companion SBOM from the authenticated
manifest, assert that both entries exist, then verify them:

```sh
awk -v archive="$ARCHIVE" \
  '$2 == archive || $2 == archive ".sbom.json"' \
  checksums.txt > selected-checksums.txt
test "$(wc -l < selected-checksums.txt)" -eq 2
sha256sum -c selected-checksums.txt
rm selected-checksums.txt
```

Both lines must report `OK`. For a Windows archive, set `ARCHIVE` to the exact
`.zip` name; its SBOM remains `${ARCHIVE}.sbom.json`.

### 3. Verify GitHub build provenance

GitHub verifies the subject digest and confirms that the provenance belongs to
this repository:

```sh
gh attestation verify "$ARCHIVE" --repo tchori-labs/tchori
gh attestation verify checksums.txt --repo tchori-labs/tchori
```

### 4. Inspect the SBOM

The companion document is SPDX JSON. Review its document identity and package
inventory before using the binary:

```sh
jq '{name, creationInfo, packages: [.packages[] | {name, versionInfo, supplier}]}' \
  "$ARCHIVE.sbom.json" | less
```

Verification must fail closed: do not install the binary if the Cosign
identity/issuer check, either checksum, or the archive provenance check fails.
