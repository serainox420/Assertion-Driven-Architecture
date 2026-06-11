#!/usr/bin/env bash
# sandbox-sync.sh — re-populate /ada inside the RUNNING sandbox with the current
# host working directory, WITHOUT rebuilding the image (the `make refresh` path).
#
# It streams the working tree in over `docker exec` and unpacks it into /ada. The
# host tree is only ever read, never modified — the same one-way, host-protected
# model as the build-time COPY. Excludes mirror .dockerignore.

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

require docker "install Docker to use the sandbox"
SERVICE="${ADA_SERVICE:-arch}"

# The container must exist and be running for `exec` to land.
cid="$(docker compose ps -q "${SERVICE}" 2>/dev/null || true)"
[[ -n "${cid}" ]] || die "sandbox not created yet — run: make up"
[[ "$(docker inspect -f '{{.State.Running}}' "${cid}" 2>/dev/null)" == "true" ]] \
  || die "sandbox is stopped — start it with: make start (or make up)"

info "syncing working tree -> ${SERVICE}:/ada (host files are not touched)"
tar -C "${ROOT}" \
    --exclude='./.git' \
    --exclude='./bin' \
    --exclude='./dist' \
    --exclude='./outputs' \
    --exclude='./benchmark/results' \
    --exclude='./training/.venv' \
    --exclude='./.shared' \
    --exclude='*.gguf' \
    -cf - . \
  | docker compose exec -T "${SERVICE}" tar -C /ada -xf -

ok "/ada refreshed from $(basename "${ROOT}")"
