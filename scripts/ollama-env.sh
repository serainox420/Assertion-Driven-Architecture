#!/usr/bin/env bash
# ollama-env.sh — canonical Ollama environment. Source this, then start the server.
#
#   source scripts/ollama-env.sh
#   ollama serve            # foreground, for logs
#
# Values are tuned for a single-GPU local box (see the research notes §2). Adjust
# the GPU lines for your hardware; the defaults assume an AMD ROCm card. On NVIDIA
# or CPU-only boxes, drop the HSA/HIP lines.

# ── GPU detection (AMD / ROCm) ────────────────────────────────────────────────
# Set HSA_OVERRIDE_GFX_VERSION ONLY if `ollama serve` misdetects your card. Read
# its startup line first — do not cargo-cult an override.
# export HSA_OVERRIDE_GFX_VERSION=12.0.0
export HIP_VISIBLE_DEVICES="${HIP_VISIBLE_DEVICES:-0}"   # single GPU, index 0
export OLLAMA_VULKAN="${OLLAMA_VULKAN:-false}"           # prefer ROCm/HIP over Vulkan

# ── Loading / lifetime ────────────────────────────────────────────────────────
export OLLAMA_MAX_LOADED_MODELS="${OLLAMA_MAX_LOADED_MODELS:-1}"  # 32 GiB is tight
export OLLAMA_NUM_PARALLEL="${OLLAMA_NUM_PARALLEL:-1}"            # MoE rejects parallel reqs
export OLLAMA_KEEP_ALIVE="${OLLAMA_KEEP_ALIVE:-5m}"              # keep warm during a loop
export OLLAMA_LOAD_TIMEOUT="${OLLAMA_LOAD_TIMEOUT:-10m}"

# ── Memory / context ──────────────────────────────────────────────────────────
export OLLAMA_FLASH_ATTENTION="${OLLAMA_FLASH_ATTENTION:-true}"
export OLLAMA_KV_CACHE_TYPE="${OLLAMA_KV_CACHE_TYPE:-q8_0}"      # drop to q4_0 only if VRAM-starved
export OLLAMA_CONTEXT_LENGTH="${OLLAMA_CONTEXT_LENGTH:-16384}"
# export OLLAMA_GPU_OVERHEAD=2048   # reserve 2 GiB to dodge OOM on a tight card

# ── Storage ───────────────────────────────────────────────────────────────────
# export OLLAMA_MODELS=/path/to/your/models
export OLLAMA_NO_CLOUD="${OLLAMA_NO_CLOUD:-true}"               # local only

echo "→ ollama env loaded (single-load / quantized KV / ctx=${OLLAMA_CONTEXT_LENGTH})"
