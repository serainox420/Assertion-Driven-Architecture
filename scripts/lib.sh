#!/usr/bin/env bash
# lib.sh — shared helpers sourced by the other scripts. Not meant to be run directly.

set -euo pipefail

# ── Resolve repo root from this file's location (works regardless of CWD) ──────
LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${LIB_DIR}/.." && pwd)"
export ROOT

# ── Pretty logging (colors only when stdout is a TTY) ─────────────────────────
if [[ -t 1 ]]; then
  _C_RST=$'\033[0m'; _C_BLU=$'\033[34m'; _C_GRN=$'\033[32m'; _C_YLW=$'\033[33m'; _C_RED=$'\033[31m'
else
  _C_RST=""; _C_BLU=""; _C_GRN=""; _C_YLW=""; _C_RED=""
fi

info() { printf '%s==>%s %s\n' "${_C_BLU}" "${_C_RST}" "$*"; }
ok()   { printf '%s ok %s %s\n' "${_C_GRN}" "${_C_RST}" "$*"; }
warn() { printf '%swarn%s %s\n' "${_C_YLW}" "${_C_RST}" "$*" >&2; }
die()  { printf '%sfail%s %s\n' "${_C_RED}" "${_C_RST}" "$*" >&2; exit 1; }

# ── Capability checks ─────────────────────────────────────────────────────────
have() { command -v "$1" >/dev/null 2>&1; }

# require CMD [hint] — die with a helpful message if CMD is missing.
require() {
  local cmd="$1" hint="${2:-}"
  have "$cmd" || die "missing '${cmd}'.${hint:+ }${hint}"
}

# version_ge A B — true if version A >= version B (dotted numeric compare).
version_ge() {
  [[ "$(printf '%s\n%s\n' "$2" "$1" | sort -V | head -n1)" == "$2" ]]
}

# detect_distro — read /etc/os-release into DISTRO_ID / DISTRO_LIKE (best-effort).
# Arch is the project's reference distro; everything else is "compatible".
detect_distro() {
  DISTRO_ID="unknown"; DISTRO_LIKE=""
  if [[ -r /etc/os-release ]]; then
    # shellcheck disable=SC1091
    . /etc/os-release
    DISTRO_ID="${ID:-unknown}"
    DISTRO_LIKE="${ID_LIKE:-}"
  fi
  export DISTRO_ID DISTRO_LIKE
}

# is_arch — true on Arch or any Arch-derived distro (Manjaro, EndeavourOS, …).
is_arch() {
  detect_distro
  [[ "${DISTRO_ID}" == "arch" || "${DISTRO_LIKE}" == *arch* ]] || have pacman
}

# ── Package manager detection (best-effort, multi-distro) ─────────────────────
# Sets PKG_MGR and PKG_INSTALL; returns non-zero if none found. Arch's pacman is
# probed FIRST (reference platform); apt/dnf/zypper/brew follow for compatibility.
detect_pkg_mgr() {
  if   have pacman;  then PKG_MGR=pacman; PKG_INSTALL="sudo pacman -S --noconfirm --needed"
  elif have apt-get; then PKG_MGR=apt;    PKG_INSTALL="sudo apt-get install -y"
  elif have dnf;     then PKG_MGR=dnf;    PKG_INSTALL="sudo dnf install -y"
  elif have zypper;  then PKG_MGR=zypper; PKG_INSTALL="sudo zypper install -y"
  elif have brew;    then PKG_MGR=brew;   PKG_INSTALL="brew install"
  else return 1; fi
  export PKG_MGR PKG_INSTALL
}

# ── Defaults shared across scripts (override via environment) ─────────────────
: "${ADA_MODEL:=qwen2.5-coder:14b}"        # worker model tag
: "${OLLAMA_HOST_URL:=http://localhost:11434}"
: "${ADA_BIN:=${ROOT}/bin/ada_agent}"      # built binary path
export ADA_MODEL OLLAMA_HOST_URL ADA_BIN
