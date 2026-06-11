#!/usr/bin/env bash
# sync.sh — update the local checkout from a fresh clone, interactively.
#
# Clones the repo fresh into a tmp dir, compares every tracked file against your
# local copy, prints exactly what differs, and (after you confirm) overwrites the
# local files in place. One command instead of stash/pull/resolve gymnastics.
#
# Direct usage (pass flags straight — NO "FLAGS="):
#   scripts/sync.sh                 # sync current branch from origin
#   scripts/sync.sh --branch main   # a specific branch
#   scripts/sync.sh --remote URL    # a specific remote URL
#   scripts/sync.sh --dry-run       # show differences, never write
#   scripts/sync.sh --yes           # don't prompt, just apply
#
# Via make (this is where FLAGS= belongs):
#   make sync FLAGS="--remote URL --dry-run"
#
# Env overrides: ADA_SYNC_REMOTE, ADA_SYNC_BRANCH.
# Only files TRACKED in the fresh clone are considered, so build artifacts and
# gitignored files are never touched. Local-only files are reported, never deleted.

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

# Last-resort remote when none is configured and none is passed.
DEFAULT_REMOTE="${ADA_SYNC_REMOTE:-https://github.com/serainox420/Assertion-Driven-Architecture.git}"

main() {
  require git

  # Run git against the (possibly other-owned) local repo without tripping the
  # "dubious ownership" guard — the usual reason detection fails when run as root.
  git_local() { git -C "${ROOT}" -c safe.directory="${ROOT}" "$@"; }

  local branch="" remote="" dry_run=0 assume_yes=0

  # Parse flags first so an explicit --remote/--branch wins over detection.
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --branch) branch="$2"; shift 2 ;;
      --remote) remote="$2"; shift 2 ;;
      --dry-run) dry_run=1; shift ;;
      --yes|-y) assume_yes=1; shift ;;
      -h|--help) sed -n '2,21p' "$0"; exit 0 ;;
      *=*) die "'$1' looks like make syntax. Use:  make sync FLAGS='--remote URL'  — or call the script directly:  scripts/sync.sh --remote URL" ;;
      *) die "unknown flag: $1 (try --help)" ;;
    esac
  done

  # Resolve the remote: explicit flag > origin > first remote > env/default.
  [[ -z "${remote}" ]] && remote="$(git_local remote get-url origin 2>/dev/null || true)"
  if [[ -z "${remote}" ]]; then
    local first; first="$(git_local remote 2>/dev/null | head -n1)"
    [[ -n "${first}" ]] && remote="$(git_local remote get-url "${first}" 2>/dev/null || true)"
  fi
  [[ -z "${remote}" ]] && remote="${DEFAULT_REMOTE}"
  [[ -n "${remote}" ]] || die "no remote URL; pass --remote URL or set ADA_SYNC_REMOTE"

  # Resolve the branch: explicit flag > env > current branch > remote's default > main.
  [[ -z "${branch}" ]] && branch="${ADA_SYNC_BRANCH:-}"
  [[ -z "${branch}" ]] && branch="$(git_local symbolic-ref --short -q HEAD 2>/dev/null || true)"
  if [[ -z "${branch}" ]]; then
    branch="$(git ls-remote --symref "${remote}" HEAD 2>/dev/null \
      | awk '/^ref:/{sub(/refs\/heads\//,"",$2); print $2; exit}')"
  fi
  [[ -n "${branch}" ]] || branch="main"

  local tmp; tmp="$(mktemp -d)"
  # shellcheck disable=SC2064
  trap "rm -rf '${tmp}'" EXIT

  info "cloning ${branch} from ${remote%%@*}…"   # hide any embedded creds in logs
  git clone --quiet --depth 1 --branch "${branch}" "${remote}" "${tmp}/repo" \
    || die "clone failed — check the remote is reachable and branch '${branch}' exists (pass --branch NAME)"

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
    ok "already up to date — no tracked file differs from ${branch}"
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
  warn "'changed' files include any LOCAL edits you have not pushed — review before replacing."

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
