#!/usr/bin/env bash
# install.sh — install the dependencies ADA needs.
#
# Core (always): Go >= 1.22, git, jq, a C toolchain (for optional CGO builds).
# Optional:
#   --with-ollama   install the Ollama inference server (official script)
#   --with-python   create a venv for the distillation/training stub (§12)
#   --yes           assume "yes" / non-interactive
#
# Usage: scripts/install.sh [--with-ollama] [--with-python] [--yes]

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

WITH_OLLAMA=0
WITH_PYTHON=0
ASSUME_YES=0
for arg in "$@"; do
  case "$arg" in
    --with-ollama) WITH_OLLAMA=1 ;;
    --with-python) WITH_PYTHON=1 ;;
    --yes|-y)      ASSUME_YES=1 ;;
    -h|--help)     sed -n '2,12p' "$0"; exit 0 ;;
    *)             die "unknown flag: $arg" ;;
  esac
done

GO_MIN="1.22"

pkg_for() {
  # Map a generic need to this distro's package name (Arch/pacman listed first).
  local need="$1"
  case "${PKG_MGR}:${need}" in
    pacman:go)        echo go ;;            apt:go)        echo golang-go ;;
    dnf:go)           echo golang ;;        zypper:go)     echo go ;;        brew:go) echo go ;;
    pacman:toolchain) echo base-devel ;;    apt:toolchain) echo build-essential ;;
    dnf:toolchain)    echo "gcc make" ;;    zypper:toolchain) echo "gcc make" ;; brew:toolchain) echo "" ;;
    *:*)              echo "$need" ;;       # git, jq, curl map 1:1 everywhere
  esac
}

ensure() {
  # ensure CMD NEED — install NEED's package if CMD is missing.
  local cmd="$1" need="$2"
  if have "$cmd"; then ok "${cmd} present"; return; fi
  local pkg; pkg="$(pkg_for "$need")"
  [[ -z "$pkg" ]] && { warn "${cmd} missing and no package mapping; install it manually"; return; }
  info "installing ${pkg} (provides ${cmd})"
  # shellcheck disable=SC2086
  ${PKG_INSTALL} ${pkg}
}

detect_distro
info "distro: ${DISTRO_ID}${DISTRO_LIKE:+ (like ${DISTRO_LIKE})} — Arch is the reference target; others are supported"

info "detecting package manager"
if detect_pkg_mgr; then
  ok "using ${PKG_MGR}"
  is_arch && ok "Arch-family detected: native pacman packages preferred"
else
  warn "no supported package manager found — will only verify what's already installed"
fi

# ── Core dependencies ─────────────────────────────────────────────────────────
if [[ -n "${PKG_MGR:-}" ]]; then
  ensure git  git
  ensure jq   jq
  ensure curl curl
  have cc || have gcc || ensure gcc toolchain
  have go || ensure go go
fi

# ── Verify Go and its version ─────────────────────────────────────────────────
if have go; then
  GO_VER="$(go version | awk '{print $3}' | sed 's/^go//')"
  if version_ge "${GO_VER}" "${GO_MIN}"; then
    ok "go ${GO_VER} (>= ${GO_MIN})"
  else
    warn "go ${GO_VER} is older than required ${GO_MIN}."
    warn "Install a newer toolchain from https://go.dev/dl/ and put it ahead on PATH."
  fi
else
  die "Go is not installed and could not be installed automatically. See https://go.dev/dl/"
fi

require git
require jq

# ── Optional: Ollama ──────────────────────────────────────────────────────────
if [[ "${WITH_OLLAMA}" == 1 ]]; then
  if have ollama; then
    ok "ollama present ($(ollama --version 2>/dev/null | head -n1))"
  elif [[ "${PKG_MGR:-}" == "pacman" ]]; then
    # Arch-first: install from the official repos. Pick the GPU-matched package.
    info "installing Ollama from Arch repos"
    warn "GPU variants exist: 'ollama-rocm' (AMD), 'ollama-cuda' (NVIDIA), 'ollama' (CPU/generic)."
    warn "override the choice with: ADA_OLLAMA_PKG=ollama-rocm scripts/install.sh --with-ollama"
    # shellcheck disable=SC2086
    ${PKG_INSTALL} "${ADA_OLLAMA_PKG:-ollama}"
  else
    info "installing Ollama via the official script"
    if [[ "${ASSUME_YES}" == 1 ]]; then
      curl -fsSL https://ollama.com/install.sh | sh
    else
      warn "this will run: curl -fsSL https://ollama.com/install.sh | sh"
      read -r -p "proceed? [y/N] " a; [[ "$a" =~ ^[Yy]$ ]] && curl -fsSL https://ollama.com/install.sh | sh || warn "skipped Ollama install"
    fi
  fi
fi

# ── Optional: Python training venv (§12) ──────────────────────────────────────
if [[ "${WITH_PYTHON}" == 1 ]]; then
  require python3 "needed for --with-python"
  VENV="${ROOT}/training/.venv"
  info "creating training venv at ${VENV}"
  python3 -m venv "${VENV}"
  # shellcheck disable=SC1091
  source "${VENV}/bin/activate"
  pip install --upgrade pip
  if [[ -f "${ROOT}/training/requirements.txt" ]]; then
    pip install -r "${ROOT}/training/requirements.txt"
  else
    warn "no training/requirements.txt found; installing the lightweight base only"
    pip install transformers datasets trl peft accelerate
  fi
  warn "note: Unsloth + a GPU-matched torch (ROCm/CUDA) must be installed separately for your hardware."
  deactivate
fi

ok "dependency install complete"
info "next: scripts/build.sh   then   scripts/run.sh --demo"
