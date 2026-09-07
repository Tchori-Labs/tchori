#!/usr/bin/env bash
set -euo pipefail

dist=${1:-dist}
workflow_ref=${GITHUB_WORKFLOW_REF:?GITHUB_WORKFLOW_REF is required}
current_ref=${GITHUB_REF:?GITHUB_REF is required}
workflow_identity=${workflow_ref%@*}
if [ "$workflow_identity" = "$workflow_ref" ]; then
  printf 'GITHUB_WORKFLOW_REF must include the workflow ref suffix\n' >&2
  exit 1
fi
case "$current_ref" in
  refs/heads/*|refs/tags/*) ;;
  *)
    printf 'GITHUB_REF must identify a branch or tag ref\n' >&2
    exit 1
    ;;
esac

cd "$dist"
for file in checksums.txt checksums.txt.sig checksums.txt.pem; do test -s "$file"; done
certificate_identity="https://github.com/${workflow_identity}@${current_ref}"
cosign verify-blob \
  --certificate checksums.txt.pem \
  --signature checksums.txt.sig \
  --certificate-identity "$certificate_identity" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
printf 'PASS: checksum signature verified for %s\n' "$certificate_identity"
