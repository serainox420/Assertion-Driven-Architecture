#!/usr/bin/env bash
# bench.sh — run the graded benchmark objectives through the agent.
#
# Reads benchmark/objectives.tsv, drives each selected level through ada_agent,
# tees a transcript to benchmark/results/<level>.log, and prints the outcome +
# established facts. Compare against benchmark/expected.md by eye.
#
# Usage:
#   scripts/bench.sh              # run every level
#   scripts/bench.sh L3           # one level
#   scripts/bench.sh L1 L5 L8     # a subset
#
# Honors ADA_MODEL (default qwen2.5-coder:14b) and OLLAMA_HOST_URL.

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

TSV="${ROOT}/benchmark/objectives.tsv"
RESULTS="${ROOT}/benchmark/results"
[[ -f "${TSV}" ]] || die "missing ${TSV}"

# Build the binary if needed.
if [[ ! -x "${ADA_BIN}" ]]; then
  info "building agent first"
  "${ROOT}/scripts/build.sh"
fi

# Warn (don't fail) if no inference server answers — the agent will error per-run.
if ! curl -fsS "${OLLAMA_HOST_URL}/api/tags" >/dev/null 2>&1; then
  warn "no Ollama server at ${OLLAMA_HOST_URL} — start one with: scripts/model.sh"
fi

mkdir -p "${RESULTS}"

# Selected levels: args, or all levels from the TSV.
declare -a WANT=("$@")
want_level() {
  [[ ${#WANT[@]} -eq 0 ]] && return 0
  local l; for l in "${WANT[@]}"; do [[ "${l}" == "$1" ]] && return 0; done
  return 1
}

ran=0
# Read TSV: LEVEL <tab> MAX_STEPS <tab> OBJECTIVE
while IFS=$'\t' read -r level maxsteps objective; do
  [[ -z "${level}" || "${level}" == \#* ]] && continue
  want_level "${level}" || continue
  ran=$((ran+1))

  log="${RESULTS}/${level}.log"
  printf '\n%s================ %s (max_steps=%s) ================%s\n' "${_C_BLU}" "${level}" "${maxsteps}" "${_C_RST}"
  printf 'objective: %s\n' "${objective}"
  printf 'model:     %s\n' "${ADA_MODEL}"

  # Run the agent; capture everything for later inspection.
  "${ADA_BIN}" \
    -objective "${objective}" \
    -model "${ADA_MODEL}" \
    -ollama "${OLLAMA_HOST_URL}" \
    -max-steps "${maxsteps}" \
    2>&1 | tee "${log}"

  # Surface the headline outcome.
  if grep -q 'RUN COMPLETE: FINISHED' "${log}"; then
    ok "${level}: FINISHED  (transcript: ${log})"
  else
    warn "${level}: did NOT finish — see ${log} and compare with benchmark/expected.md"
  fi
done < "${TSV}"

[[ ${ran} -gt 0 ]] || die "no matching levels (have: $(awk -F'\t' '!/^#/{printf \"%s \", $1}' "${TSV}"))"
printf '\n'
ok "ran ${ran} level(s). Score them against benchmark/expected.md"
