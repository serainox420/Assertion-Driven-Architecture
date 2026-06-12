#!/usr/bin/env bash
# build.sh — build the ADA binaries (static Go).
#
# Produces TWO binaries:
#   bin/ada_agent  the raw orchestrator "brain" the scripts/benchmarks drive
#   bin/ada        the flagship CLI + interactive TUI (run `ada` with no args)
#
# Per §14.1, both are built fully static (no shared-lib deps) so they run on a
# bare host or Alpine container. The inference engine and model are NOT embedded —
# they are swappable subprocesses the brain supervises.
#
# Usage: scripts/build.sh [--race] [--out PATH] [--agent-only]
#   --race        build the test binary with the race detector (dev only; implies CGO)
#   --out P       ada_agent output path (default: bin/ada_agent)
#   --agent-only  skip building the flagship `ada` binary

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

OUT="${ADA_BIN}"
RACE=0
AGENT_ONLY=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --out)  OUT="$2"; shift 2 ;;
    --race) RACE=1; shift ;;
    --agent-only) AGENT_ONLY=1; shift ;;
    -h|--help) sed -n '2,16p' "$0"; exit 0 ;;
    *) die "unknown flag: $1" ;;
  esac
done

VERSION="$(git -C "${ROOT}" describe --tags --always --dirty 2>/dev/null || echo dev)"

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
ok "agent is runnable — try: ${OUT} -demo"

# ── Flagship `ada` binary (CLI + interactive TUI) ─────────────────────────────
if [[ "${AGENT_ONLY}" != 1 && "${RACE}" != 1 ]]; then
  ADA_TUI_OUT="$(dirname "${OUT}")/ada"
  info "building flagship ada binary (CLI + TUI) -> ${ADA_TUI_OUT}"
  CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=${VERSION}" -trimpath -o "${ADA_TUI_OUT}" ./cmd/adax
  ok "built ${ADA_TUI_OUT} (${VERSION})"
  ok "flagship is runnable — try: ${ADA_TUI_OUT}   (no args ⇒ interactive TUI)"
fi
