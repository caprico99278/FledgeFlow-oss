#!/usr/bin/env bash
# leakcheck.sh — fail if the working tree contains things that must never be
# published from this repository:
#
#   1. denylisted words (names of private projects, products or services);
#   2. strings that look like private keys or API tokens;
#   3. key / env files and raster image files (all test images are synthetic
#      and generated at run time, so no image file should ever be committed);
#   4. the repository name spelled in a non-canonical way.
#
# The built-in denylist is stored base64-encoded so that the names themselves
# do not appear as plain text (and are not found by code search) in this
# public repository. Decode it with:  echo "$DENY_B64" | base64 -d
#
# Extend it without committing anything:
#   * LEAKCHECK_EXTRA='word1|word2' bash scripts/leakcheck.sh
#   * one extended-regex per line in ./.leakcheck-denylist (git-ignored)
#
# Files scanned: everything git would commit (tracked + untracked, honouring
# .gitignore), or every file outside .git/ and target/ when git is absent.
# This script and the CI workflow are excluded because they contain the
# patterns. Usage: bash scripts/leakcheck.sh   (exit 0 = clean, 1 = findings)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

SELF="scripts/leakcheck.sh"
WORKFLOW=".github/workflows/ci.yml"

# Case-insensitive denylist (extended regex), base64-encoded.
DENY_B64="Y3lwaG9lbml4fG1pbm5lfGluc3RhZ3JhbXxvcGVuYWl8dHdpdHRlcg=="
deny="$(printf '%s' "$DENY_B64" | base64 -d)"
if [ -n "${LEAKCHECK_EXTRA:-}" ]; then
  deny="$deny|$LEAKCHECK_EXTRA"
fi
if [ -f .leakcheck-denylist ]; then
  while IFS= read -r line || [ -n "$line" ]; do
    line="${line%$'\r'}"
    if [ -z "$line" ] || [ "${line:0:1}" = "#" ]; then
      continue
    fi
    deny="$deny|$line"
  done < .leakcheck-denylist
fi

# Case-sensitive secret patterns (extended regex).
secrets='-----BEGIN ([A-Z0-9]+ )*PRIVATE KEY-----'
secrets+='|AKIA[0-9A-Z]{16}'
secrets+='|gh[pousr]_[A-Za-z0-9]{36,}'
secrets+='|github_pat_[A-Za-z0-9_]{22,}'
secrets+='|xox[abprs]-[A-Za-z0-9-]{10,}'
secrets+='|sk-[A-Za-z0-9_-]{20,}'
secrets+='|AIza[0-9A-Za-z_-]{35}'

# Forbidden file names (matched against the path, case-insensitive).
bad_files='(^|/)(\.env(\..+)?|id_rsa|id_ecdsa|id_ed25519|[^/]+\.(pem|key|p12|pfx|jks|keystore))$'
bad_files+='|\.(jpe?g|png|webp|gif|bmp|tiff?|psd|heic|heif|avif|raw|cr2|nef|arw|dng)$'

# Non-canonical spellings of the repository name (case-sensitive).
naming='fledgeflow-oss|Fledgeflow-oss|FledgeFlow-OSS|FledgeFlow-Oss|FLEDGEFLOW-OSS'

files=()
if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  while IFS= read -r -d '' f; do
    files+=("$f")
  done < <(git ls-files -z --cached --others --exclude-standard)
else
  while IFS= read -r -d '' f; do
    files+=("${f#./}")
  done < <(find . -type f -not -path './.git/*' -not -path './target/*' -print0)
fi

scan=()
for f in "${files[@]}"; do
  case "$f" in
    "$SELF" | "$WORKFLOW" | .leakcheck-denylist) continue ;;
  esac
  [ -f "$f" ] && scan+=("$f")
done

status=0
report() {
  echo "leakcheck: $1" >&2
  printf '%s\n' "$2" | sed 's/^/  /' >&2
  status=1
}

if [ "${#scan[@]}" -gt 0 ]; then
  hits="$(printf '%s\0' "${scan[@]}" | xargs -0 grep -I -n -i -E -e "$deny" -- 2>/dev/null || true)"
  [ -z "$hits" ] || report "denylisted words found:" "$hits"

  hits="$(printf '%s\0' "${scan[@]}" | xargs -0 grep -I -n -E -e "$secrets" -- 2>/dev/null || true)"
  [ -z "$hits" ] || report "secret-looking strings found:" "$hits"

  hits="$(printf '%s\0' "${scan[@]}" | xargs -0 grep -I -n -E -e "$naming" -- 2>/dev/null || true)"
  [ -z "$hits" ] || report "use the exact repository name 'FledgeFlow-oss':" "$hits"

  hits="$(printf '%s\n' "${scan[@]}" | grep -i -E -e "$bad_files" || true)"
  [ -z "$hits" ] || report "forbidden files (keys, env files or images) found:" "$hits"
fi

if [ "$status" -eq 0 ]; then
  echo "leakcheck: OK (${#scan[@]} files scanned)"
fi
exit "$status"
