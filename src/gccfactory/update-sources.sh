#!/usr/bin/env bash
# Re-pin internal/sources/sources.json.
#
# Downloads every URL in sources.json, recomputes each sha256 and the expected
# top-level directory, and rewrites the file in place. Checksums are only ever
# taken from bytes actually pulled over the network -- nothing is hand-edited.
#
#   ./update-sources.sh                 refresh every source
#   ./update-sources.sh --only gcc      refresh one source
#   ./update-sources.sh --check         verify only; exit 1 if anything drifted
#
# A clean run of --check is what proves the embedded checksum DB still matches
# upstream; wire it into CI, not into the build.
set -euo pipefail

cd "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

json="internal/sources/sources.json"
[ -f "$json" ] || { echo "update-sources: missing $json" >&2; exit 1; }
command -v go >/dev/null || { echo "update-sources: go is not on PATH" >&2; exit 1; }
command -v tar >/dev/null || { echo "update-sources: tar is not on PATH" >&2; exit 1; }

args=()
while [ $# -gt 0 ]; do
  case "$1" in
    --only) args+=(-only "$2"); shift 2 ;;
    --only=*) args+=(-only "${1#*=}"); shift ;;
    --check) args+=(-check); shift ;;
    -h|--help) sed -n '2,12p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *) echo "update-sources: unknown argument '$1' (try --help)" >&2; exit 2 ;;
  esac
done

exec go run ./internal/sources/cmd/update -json "$json" ${args[@]+"${args[@]}"}
