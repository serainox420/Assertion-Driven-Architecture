#!/usr/bin/env bash
# run.sh — run the ADA agent. Builds the binary first if it is missing.
#
# Usage:
#   scripts/run.sh --demo                       # offline demo, no model needed
#   scripts/run.sh -objective "..." [-model M]  # drive a local Ollama server
#
# Any flags are passed straight through to ada_agent (see: ada_agent -h).

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

if [[ ! -x "${ADA_BIN}" ]]; then
  info "binary not found; building it first"
  "${ROOT}/scripts/build.sh"
fi

# Default to the demo when called with no arguments.
if [[ $# -eq 0 ]]; then
  info "no args given — running the offline demo"
  exec "${ADA_BIN}" -demo
fi

exec "${ADA_BIN}" "$@"
