#!/usr/bin/env bash
# model.sh — make sure Ollama is up and the worker model is pulled.
#
# Usage: scripts/model.sh [MODEL_TAG]
#   MODEL_TAG defaults to $ADA_MODEL (qwen2.5-coder:14b).
#
# Honors scripts/ollama-env.sh if present (source it yourself before `ollama serve`).

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

MODEL="${1:-${ADA_MODEL}}"
require ollama "install with: scripts/install.sh --with-ollama"

# ── Ensure a server is reachable ──────────────────────────────────────────────
if curl -fsS "${OLLAMA_HOST_URL}/api/tags" >/dev/null 2>&1; then
  ok "ollama server reachable at ${OLLAMA_HOST_URL}"
else
  info "no server at ${OLLAMA_HOST_URL}; starting 'ollama serve' in the background"
  ( ollama serve >/tmp/ollama-serve.log 2>&1 & )
  for _ in $(seq 1 60); do
    curl -fsS "${OLLAMA_HOST_URL}/api/tags" >/dev/null 2>&1 && break
    sleep 0.5
  done
  curl -fsS "${OLLAMA_HOST_URL}/api/tags" >/dev/null 2>&1 \
    || die "ollama did not become ready (see /tmp/ollama-serve.log)"
  ok "ollama serve is up"
fi

# ── Pull the model if absent ──────────────────────────────────────────────────
if ollama list 2>/dev/null | awk '{print $1}' | grep -qx "${MODEL}"; then
  ok "model already present: ${MODEL}"
else
  info "pulling ${MODEL} (this can take a while)"
  ollama pull "${MODEL}"
  ok "pulled ${MODEL}"
fi

info "ready. run the agent with:  scripts/run.sh -objective \"...\" -model ${MODEL}"
