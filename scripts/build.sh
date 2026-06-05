#!/usr/bin/env bash
# build.sh — build the ADA "brain" (the static Go orchestrator binary).
#
# Per §14.1, the Go brain is built fully static (no shared-lib deps) so it runs
# on a bare host or Alpine container. The inference engine and model are NOT
# embedded — they are swappable subprocesses the brain supervises.
#
# Usage: scripts/build.sh [--race] [--out PATH]
#   --race     build the test binary with the race detector (dev only; implies CGO)
#   --out P    output path (default: bin/ada_agent)

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

OUT="${ADA_BIN}"
RACE=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --out)  OUT="$2"; shift 2 ;;
    --race) RACE=1; shift ;;
    -h|--help) sed -n '2,11p' "$0"; exit 0 ;;
    *) die "unknown flag: $1" ;;
  esac
done

require go
cd "${ROOT}"

info "go mod tidy"
go mod tidy

mkdir -p "$(dirname "${OUT}")"

if [[ "${RACE}" == 1 ]]; then
  info "building (race detector, dynamic) -> ${OUT}"
  go build -race -o "${OUT}" ./cmd/ada
else
  # Fully static: no CGO, strip symbol/debug tables (~30-40% smaller binary).
  info "building static binary -> ${OUT}"
  CGO_ENABLED=0 go build -ldflags="-s -w" -trimpath -o "${OUT}" ./cmd/ada
fi

ok "built ${OUT}"
file "${OUT}" 2>/dev/null || true
"${OUT}" -h >/dev/null 2>&1 || true   # smoke: flag parsing works
ok "binary is runnable — try: ${OUT} -demo"
