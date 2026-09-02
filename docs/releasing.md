# Releasing and verifying tchori

This document describes the approved release path and how consumers verify its
artifacts. Preparing this workflow does not authorize a release.

## Release policy and publish gate

Per [`AGENTS.md`](../AGENTS.md), no release may be tagged or published until a
human board decision explicitly approves it and that decision is recorded in
the `Tchori-Labs/main` repository. Agents may prepare a release change, but they
must not create or push the tag, dispatch the release workflow, approve its
deployment, or publish the release.

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

**Applied as of 2026-09-02.** Repository admin `@VictorCano` applied the
`release` Environment to the live repository and created its two custom
deployment-branch policies. `scripts/verify-release-environment.sh` returned
all PASS: the required reviewer is `@VictorCano`, `prevent_self_review` is
`true`, and the declared policies are exactly branch `main` and tag pattern
`v*`. The administrator handoff that led to this application is recorded in
[issue #77](https://github.com/Tchori-Labs/tchori-internal/issues/77#issuecomment-5165580020).
[TC-059](https://github.com/Tchori-Labs/tchori-internal/issues/66) is
unblocked on this Environment control, but a release still also requires the
separate TC-068 board-decision gate before any tag is created.

Both release jobs reference this Environment, so GitHub pauses the selected
job for required human review. The `dry-run` job
cannot create or modify a GitHub Release; the `publish` job alone receives
job-scoped `contents: write`. No other workflow or job receives publish
permissions.

## Maintainer runbook

After the board decision and normal CODEOWNERS review have landed:

1. A human maintainer creates and pushes the approved `v*` tag. This triggers
   publish mode. Do not tag from an unreviewed commit.
2. Before publishing, or when validating a workflow change, a human maintainer
   may use **Actions → Release → Run workflow** from `main`, enter an existing
   approved `v*` tag, and leave `mode` at its safe `dry-run` default. The
   workflow checks out the tag and runs GoReleaser with `--skip=publish`, while
   retaining real keyless signing and GitHub provenance generation. It does
   not create a tag, GitHub Release, or public release asset. Instead, it
   uploads a 30-day Actions artifact named
   `release-dry-run-<run-id>-<run-attempt>` containing the archives, SBOMs,
   checksum manifest, detached signature and certificate, and the local
   `provenance.intoto.jsonl` bundle for board review.
3. A board-approved manual publish or retry uses the same dispatch with
   `mode: publish`. Both manual modes require an existing tag; neither creates
   or moves one.
4. For every mode, `@VictorCano` reviews the pending `release` Environment
   deployment against the recorded board decision and approves or rejects it.
5. In publish mode, GoReleaser uploads the signed artifacts to a **draft**
   GitHub Release. The workflow checks the archive, SBOM, checksum, signature,
   and certificate outputs, then records GitHub build-provenance attestations
   for every archive and `checksums.txt`. Only after attestation succeeds does
   the final step make the draft public. If output validation or attestation
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

Verify the certificate's GitHub Actions issuer and restrict its identity to the
repository's tag-triggered release workflow:

```sh
cosign verify-blob \
  --certificate checksums.txt.pem \
  --signature checksums.txt.sig \
  --certificate-identity-regexp '^https://github\.com/tchori-labs/tchori/\.github/workflows/release\.yml@refs/tags/v.*$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
```

A manual retry checks out the same release tag but its certificate identity can
end in `@refs/heads/main`. For a board-approved manual retry only, replace the
identity regexp above with this deliberately narrow alternative:

```sh
--certificate-identity-regexp '^https://github\.com/tchori-labs/tchori/\.github/workflows/release\.yml@refs/heads/main$'
```

Do not use a repository-wide or issuer-only identity expression.

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
