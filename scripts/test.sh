#!/usr/bin/env bash
# test.sh — format check, vet, and the test suite.
#
# Usage: scripts/test.sh [--race]

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

RACE=0
[[ "${1:-}" == "--race" ]] && RACE=1

require go
cd "${ROOT}"

info "gofmt check"
unformatted="$(gofmt -l ada cmd 2>/dev/null || true)"
if [[ -n "${unformatted}" ]]; then
  warn "these files are not gofmt-clean:"
  printf '  %s\n' ${unformatted}
  die "run: gofmt -w ada cmd"
fi
ok "gofmt clean"

info "go vet"
go vet ./...
ok "vet clean"

if [[ "${RACE}" == 1 ]]; then
  info "go test -race"
  go test -race ./...
else
  info "go test"
  go test ./...
fi
ok "all tests passed"
