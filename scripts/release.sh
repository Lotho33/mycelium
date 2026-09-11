#!/usr/bin/env bash
# Rilascia una nuova versione di mycelium.
#
#   scripts/release.sh 0.1.4            # bump + commit + tag v0.1.4 + push su GitHub
#   scripts/release.sh 0.1.4 -n        # dry-run: mostra cosa farebbe e basta
#
# Il push del tag vX.Y.Z fa partire .github/workflows/release.yml, che builda
# l'immagine Docker multi-arch (VERSION iniettata dal tag) e la pubblica su
# ghcr.io/lotho33/mycelium:<ver> + :latest — runner GitHub-hosted, nessun
# secret da configurare (GITHUB_TOKEN copre push GHCR + creazione release).
#
# Override: MYCELIUM_RELEASE_REMOTE (default: origin), MYCELIUM_RELEASE_BRANCH
# (default: main).
set -euo pipefail

REMOTE=${MYCELIUM_RELEASE_REMOTE:-origin}
BRANCH=${MYCELIUM_RELEASE_BRANCH:-main}
VERSION_FILE=internal/core/version.go

ver=${1:-}
dry=false
[[ "${2:-}" == "-n" || "${2:-}" == "--dry-run" ]] && dry=true

if [[ ! "$ver" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "uso: $0 X.Y.Z [-n]   (es. $0 0.1.4)" >&2
  exit 2
fi
tag="v$ver"

cd "$(git rev-parse --show-toplevel)"

# --- controlli -------------------------------------------------------------
cur_branch=$(git branch --show-current)
[[ "$cur_branch" == "$BRANCH" ]] || { echo "sei su '$cur_branch', non '$BRANCH'." >&2; exit 1; }

git fetch -q "$REMOTE" --tags || true
if git rev-parse -q --verify "refs/tags/$tag" >/dev/null || \
   git ls-remote --exit-code --tags "$REMOTE" "$tag" >/dev/null 2>&1; then
  echo "il tag $tag esiste gia (locale o su $REMOTE)." >&2
  exit 1
fi

# Solo il file versione puo essere gia modificato tra i file tracciati
# (i non tracciati non bloccano il rilascio).
dirty=$(git status --porcelain --untracked-files=no | grep -v " ${VERSION_FILE}\$" || true)
[[ -z "$dirty" ]] || { echo "albero di lavoro sporco:"; echo "$dirty"; exit 1; }

# --- bump versione -------------------------------------------------------
new_line="var Version = \"${ver}\""
old_line=$(grep -E '^var Version = ' "$VERSION_FILE" || true)
echo "${VERSION_FILE}: '${old_line}'  ->  '${new_line}'"
echo "commit + tag $tag, poi push '$BRANCH' e '$tag' su '$REMOTE'"

if $dry; then echo "(dry-run, nulla eseguito)"; exit 0; fi

sed -i -E "s/^var Version = .*/${new_line}/" "$VERSION_FILE"

git add "$VERSION_FILE"
git commit -m "$tag" -m "" \
  -m "Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>"
git tag -a "$tag" -m "$tag"

git push "$REMOTE" "$BRANCH"
git push "$REMOTE" "$tag"

echo
echo "fatto. il workflow di release parte dal tag appena pushato."
