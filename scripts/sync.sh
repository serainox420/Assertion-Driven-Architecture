#!/usr/bin/env bash
# sync.sh — update the local checkout from a fresh clone, interactively.
#
# Clones the repo fresh into a tmp dir, compares every tracked file against your
# local copy, prints exactly what differs, and (after you confirm) overwrites the
# local files in place. One command instead of stash/pull/resolve gymnastics.
#
# Usage:
#   scripts/sync.sh                 # sync current branch from origin, prompt before writing
#   scripts/sync.sh --branch main   # sync a specific branch
#   scripts/sync.sh --remote URL    # sync from a specific remote URL
#   scripts/sync.sh --dry-run       # show differences, never write
#   scripts/sync.sh --yes           # don't prompt, just apply
#
# Only files TRACKED in the fresh clone are considered, so build artifacts and
# gitignored files are never touched. Files that exist only locally are reported
# but left alone (nothing is deleted).

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

main() {
  require git
  local branch remote dry_run=0 assume_yes=0
  branch="$(git -C "${ROOT}" rev-parse --abbrev-ref HEAD 2>/dev/null || echo HEAD)"
  remote="$(git -C "${ROOT}" remote get-url origin 2>/dev/null || true)"

  while [[ $# -gt 0 ]]; do
    case "$1" in
      --branch) branch="$2"; shift 2 ;;
      --remote) remote="$2"; shift 2 ;;
      --dry-run) dry_run=1; shift ;;
      --yes|-y) assume_yes=1; shift ;;
      -h|--help) sed -n '2,20p' "$0"; exit 0 ;;
      *) die "unknown flag: $1" ;;
    esac
  done
  [[ -n "${remote}" ]] || die "no remote URL (pass --remote URL)"

  local tmp; tmp="$(mktemp -d)"
  # shellcheck disable=SC2064
  trap "rm -rf '${tmp}'" EXIT

  info "cloning ${branch} from ${remote%%@*}…"   # hide any embedded creds in logs
  git clone --quiet --depth 1 --branch "${branch}" "${remote}" "${tmp}/repo" \
    || die "clone failed (branch '${branch}' may not exist on the remote)"

  # ── Compare tracked files ───────────────────────────────────────────────────
  local -a changed=() added=()
  local f
  while IFS= read -r f; do
    if [[ ! -e "${ROOT}/${f}" ]]; then
      added+=("${f}")
    elif ! cmp -s "${tmp}/repo/${f}" "${ROOT}/${f}"; then
      changed+=("${f}")
    fi
  done < <(git -C "${tmp}/repo" ls-files)

  local total=$(( ${#changed[@]} + ${#added[@]} ))
  if [[ ${total} -eq 0 ]]; then
    ok "already up to date — no tracked file differs"
    return 0
  fi

  # Line-count delta for a single file (fresh vs local), via git's numstat.
  numstat() {
    local local_path="$1" fresh_path="$2" a r
    read -r a r _ < <(git --no-pager diff --no-index --numstat "${local_path}" "${fresh_path}" 2>/dev/null || echo "? ? -")
    printf '+%s -%s' "${a}" "${r}"
  }

  info "files that differ from the fresh ${branch}:"
  for f in "${changed[@]}"; do
    printf '  %schanged%s  %-40s %s\n' "${_C_YLW}" "${_C_RST}" "${f}" "$(numstat "${ROOT}/${f}" "${tmp}/repo/${f}")"
  done
  for f in "${added[@]}"; do
    printf '  %snew    %s  %-40s %s\n' "${_C_GRN}" "${_C_RST}" "${f}" "$(numstat /dev/null "${tmp}/repo/${f}")"
  done

  if [[ ${dry_run} -eq 1 ]]; then
    warn "dry-run: ${total} file(s) would be updated; nothing written"
    return 0
  fi

  # ── Confirm and apply ───────────────────────────────────────────────────────
  if [[ ${assume_yes} -eq 0 ]]; then
    local ans
    read -r -p "Replace ${total} local file(s) with the fresh versions in place? (Y/n) " ans
    ans="${ans:-Y}"
    [[ "${ans}" =~ ^[Yy]$ ]] || { warn "aborted; nothing changed"; return 0; }
  fi

  for f in "${changed[@]}" "${added[@]}"; do
    mkdir -p "${ROOT}/$(dirname "${f}")"
    cp "${tmp}/repo/${f}" "${ROOT}/${f}"
  done
  ok "updated ${total} file(s) from ${branch}"
  info "review with: git -C '${ROOT}' status"
}

main "$@"
