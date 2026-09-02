#!/usr/bin/env bash
# Prints the total statement coverage of a Go coverage profile, excluding code
# that can never be covered in-process:
#
#   - internal/provider/proto/tfplugin{5,6}: generated protobuf/gRPC stubs.
#     4886 of ~7000 profile blocks, permanently at 0%, which drags an
#     unfiltered total from ~70% down to ~26% and makes the number useless as
#     a trend signal.
#   - internal/provider/testprovider*: fake provider binaries that exist to be
#     compiled and executed as subprocesses by the tests.
#
# Coverage of code the CLI runs in a subprocess (cmd/tchori) is still
# understated by design: `go test` only attributes statements executed in the
# test process. The filtered number is a floor, not a score, and nothing gates
# on it.
set -euo pipefail

profile=${1:-cover.out}
filtered=${profile%.out}.filtered.out

if [ ! -f "$profile" ]; then
  printf 'coverage profile %s does not exist; run go test -coverprofile=%s ./... first\n' "$profile" "$profile" >&2
  exit 1
fi

# The "mode:" header does not match either pattern, so it survives the filter.
grep -v -e '/internal/provider/proto/tfplugin[56]/' \
  -e '/internal/provider/testprovider' \
  "$profile" >"$filtered"

go tool cover -func="$filtered" | tail -n 1
