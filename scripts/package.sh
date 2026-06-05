#!/usr/bin/env bash
# package.sh — assemble the portable "ada_toolkit" tarball (§14.1).
#
# Produces the deployment shape: a small static Go brain that supervises
# swappable muscle (the inference engine + model) as subprocesses.
#
#   ada_toolkit/
#   ├── ada_agent        # static Go binary (built here)
#   ├── scripts/         # ollama-env.sh, run helpers
#   ├── README.md
#   ├── llama-server     # (drop in your prebuilt engine for the target GPU/CPU)
#   └── models/          # (drop in your .gguf model)
#
# llama-server and the model are NOT bundled (drivers must dynamically link the
# host's ROCm/CUDA; you cannot embed a 20 GB model). Placeholders document where
# they go for an offline/air-gapped copy (§14.4).
#
# Usage: scripts/package.sh [--out DIR]

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

OUTDIR="${ROOT}/dist"
[[ "${1:-}" == "--out" ]] && { OUTDIR="$2"; shift 2; }

STAGE="${OUTDIR}/ada_toolkit"
info "building the brain"
"${ROOT}/scripts/build.sh" --out "${STAGE}/ada_agent"

info "staging toolkit at ${STAGE}"
mkdir -p "${STAGE}/scripts" "${STAGE}/models"
cp "${ROOT}/scripts/ollama-env.sh" "${STAGE}/scripts/"
cp "${ROOT}/scripts/run.sh"        "${STAGE}/scripts/" 2>/dev/null || true
cp "${ROOT}/scripts/lib.sh"        "${STAGE}/scripts/" 2>/dev/null || true
cp "${ROOT}/README.md"             "${STAGE}/" 2>/dev/null || true

# Document where the swappable muscle goes.
cat > "${STAGE}/models/README.txt" <<'EOF'
Drop your GGUF model here, e.g. qwen-coder-14b.gguf.
Not bundled: a model can be tens of GB and is hardware/quantization specific.
EOF
cat > "${STAGE}/PLACE_ENGINE_HERE.txt" <<'EOF'
Place your prebuilt `llama-server` (llama.cpp) binary in this directory,
built for the target GPU/CPU. It is NOT bundled because the inference engine
must dynamically link the host's ROCm/CUDA libraries.
EOF

TARBALL="${OUTDIR}/ada_toolkit.tar.gz"
info "creating ${TARBALL}"
tar -C "${OUTDIR}" -czf "${TARBALL}" ada_toolkit

ok "packaged: ${TARBALL}"
info "offline deploy: copy the tarball, unpack, add llama-server + a model, run ./ada_agent"
