#!/usr/bin/env bash
set -euo pipefail

tag=${1:?usage: verify-release-artifacts.sh TAG [DIST]}
dist=${2:-dist}
version=${tag#v}
cd "$dist"
declare -A expected=()
for os in darwin linux windows; do
  extension=tar.gz
  if [ "$os" = windows ]; then extension=zip; fi
  for arch in amd64 arm64; do
    archive="tchori_${version}_${os}_${arch}.${extension}"
    test -s "$archive"
    test -s "$archive.sbom.json"
    jq -e '.spdxVersion | startswith("SPDX-")' "$archive.sbom.json" >/dev/null
    expected["$archive"]=1
    expected["$archive.sbom.json"]=1
  done
done
test -s checksums.txt
while read -r digest file; do
  file=${file#\*}
  if [[ ! $digest =~ ^[[:xdigit:]]{64}$ ]] || [[ ${expected[$file]:-0} != 1 ]]; then
    printf 'Unexpected, duplicate, or malformed checksum entry\n' >&2
    exit 1
  fi
  unset 'expected[$file]'
done < checksums.txt
if [ "${#expected[@]}" -ne 0 ]; then
  printf 'Checksum manifest omits required release artifacts\n' >&2
  exit 1
fi
sha256sum --strict -c checksums.txt
printf 'PASS: six platform archives and six SPDX SBOMs verified\n'
