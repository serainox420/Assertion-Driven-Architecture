#!/usr/bin/env bash
# create-model.sh — build the ADA-tuned Ollama models from modelfiles/.
#
# Usage:
#   scripts/create-model.sh                      # build every modelfiles/*.Modelfile
#   scripts/create-model.sh ada-qwen3-coder      # build one (basename, extension optional)
#
# Each modelfiles/<name>.Modelfile becomes the local Ollama model <name>.
# Run the agent against one with:  ADA_MODEL=<name> make bench
#
# Honors OLLAMA_HOST_URL; starts a local 'ollama serve' if none is reachable.

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

require ollama "install with: scripts/install.sh --with-ollama"

MFDIR="${ROOT}/modelfiles"
[[ -d "${MFDIR}" ]] || die "missing ${MFDIR}"

# ── Ensure a server is reachable (same dance as model.sh) ─────────────────────
if ! curl -fsS "${OLLAMA_HOST_URL}/api/tags" >/dev/null 2>&1; then
  info "no server at ${OLLAMA_HOST_URL}; starting 'ollama serve' in the background"
  ( ollama serve >/tmp/ollama-serve.log 2>&1 & )
  for _ in $(seq 1 60); do
    curl -fsS "${OLLAMA_HOST_URL}/api/tags" >/dev/null 2>&1 && break
    sleep 0.5
  done
  curl -fsS "${OLLAMA_HOST_URL}/api/tags" >/dev/null 2>&1 \
    || die "ollama did not become ready (see /tmp/ollama-serve.log)"
fi

# ── Resolve which Modelfiles to build ─────────────────────────────────────────
declare -a FILES=()
if [[ $# -gt 0 ]]; then
  for name in "$@"; do
    f="${MFDIR}/${name%.Modelfile}.Modelfile"
    [[ -f "${f}" ]] || die "no such modelfile: ${f}"
    FILES+=("${f}")
  done
else
  for f in "${MFDIR}"/*.Modelfile; do
    [[ -e "${f}" ]] || die "no *.Modelfile in ${MFDIR}"
    FILES+=("${f}")
  done
fi

# ── Build ─────────────────────────────────────────────────────────────────────
built=0
for f in "${FILES[@]}"; do
  name="$(basename "${f}" .Modelfile)"
  info "creating ${name} from ${f#"${ROOT}"/} (pulls the base tag on first run — can take a while)"
  if ollama create "${name}" -f "${f}"; then
    ok "created ${name}"
    built=$((built + 1))
  else
    warn "failed to create ${name} — is the FROM tag available on this server?"
  fi
done

[[ ${built} -gt 0 ]] || die "no models built"
name0="$(basename "${FILES[0]}" .Modelfile)"
ok "done. run the agent with:  ADA_MODEL=${name0} make bench"
