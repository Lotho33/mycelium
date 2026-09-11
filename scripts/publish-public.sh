#!/usr/bin/env bash
# Pubblica lo stato attuale di `dev` (la fonte di verità, storia completa)
# come un nuovo commit su `main` (storia corta e curata — solo gli snapshot
# che scegli di pubblicare, non ogni commit di dev). Analogo allo script
# gemello in pileus-player: dev e main vivono nello STESSO remote (`origin`,
# GitHub) — niente più Forgejo, un solo repo per tutto lo stack.
#
#   scripts/publish-public.sh "messaggio del commit"
#   scripts/publish-public.sh "messaggio" -n      # dry-run: mostra cosa farebbe
#
# Cosa fa: prende l'albero ESATTO di dev (HEAD) così com'è ora e lo scrive
# come commit nuovo in cima alla storia esistente di main — non uno squash
# ogni volta, main resta e cresce come sequenza di snapshot pubblici. dev
# resta invariato.
#
# Come lo scrive: via plumbing (git commit-tree + push diretto del branch
# remoto), MAI checkout/rm/add sul working tree. Motivo: alcuni path del
# repo possono finire di proprietà di root (es. plugins/jellyfin, se creati
# da un container avviato come root) — un `git rm -r .` ci si pianta sopra
# con "Permesso negato" anche quando quei file non cambiano affatto tra dev
# e main. Costruendo il commit direttamente dal tree già committato di dev,
# i file non toccati restano semplicemente intoccati, di chiunque siano.
#
# Gate di build/test: gira in un container (nessun tool Go richiesto
# sull'host, coerente con .github/workflows/ci.yml) — gofmt + go vet/build/test.
# Richiede docker o podman nel PATH; salta con MYCELIUM_SKIP_GATE=1.
#
# Override via env:
#   MYCELIUM_PUBLISH_REMOTE   remote del repo pubblico          (default: origin)
#   MYCELIUM_PUBLISH_BRANCH   branch pubblico                   (default: main)
#   MYCELIUM_SOURCE_REMOTE    remote della fonte di verità       (default: origin)
#   MYCELIUM_SOURCE_BRANCH    branch sorgente da pubblicare     (default: dev)
#   MYCELIUM_GATE_IMAGE       immagine container per il gate    (default: golang:1.26)
#   MYCELIUM_SKIP_GATE        1 = salta gofmt/vet/build/test
set -euo pipefail

PUB_REMOTE=${MYCELIUM_PUBLISH_REMOTE:-origin}
PUB_BRANCH=${MYCELIUM_PUBLISH_BRANCH:-main}
SRC_REMOTE=${MYCELIUM_SOURCE_REMOTE:-origin}
SRC_BRANCH=${MYCELIUM_SOURCE_BRANCH:-dev}
GATE_IMAGE=${MYCELIUM_GATE_IMAGE:-golang:1.26}
SKIP_GATE=${MYCELIUM_SKIP_GATE:-0}

msg=${1:-}
dry=false
[[ "${2:-}" == "-n" || "${2:-}" == "--dry-run" ]] && dry=true

if [[ -z "$msg" ]]; then
  echo "uso: $0 \"messaggio del commit\" [-n]   (es. $0 \"v1.4.0 — hardening proxy + refactor admin\")" >&2
  exit 2
fi

cd "$(git rev-parse --show-toplevel)"

# --- controlli ---------------------------------------------------------------
cur_branch=$(git branch --show-current)
[[ "$cur_branch" == "$SRC_BRANCH" ]] || { echo "sei su '$cur_branch', non '$SRC_BRANCH'." >&2; exit 1; }

dirty=$(git status --porcelain --untracked-files=no || true)
[[ -z "$dirty" ]] || { echo "albero di lavoro sporco su $SRC_BRANCH — commit o stash prima:"; echo "$dirty"; exit 1; }

git fetch "$SRC_REMOTE" "$SRC_BRANCH" -q || true
git fetch "$PUB_REMOTE" "$PUB_BRANCH" -q || true

# dev locale deve essere allineato al remote — non pubblichiamo uno
# snapshot che non è (ancora) pushato come storia reale.
src_remote_head=$(git rev-parse "$SRC_REMOTE/$SRC_BRANCH" 2>/dev/null || echo "")
src_local_head=$(git rev-parse "$SRC_BRANCH")
if [[ -n "$src_remote_head" && "$src_local_head" != "$src_remote_head" ]]; then
  echo "$SRC_BRANCH locale ($src_local_head) è diverso da $SRC_REMOTE/$SRC_BRANCH ($src_remote_head)." >&2
  echo "pusha/aggiorna $SRC_BRANCH su $SRC_REMOTE prima di pubblicare." >&2
  exit 1
fi

src_tree=$(git rev-parse "$SRC_BRANCH^{tree}")
pub_tree=$(git rev-parse "$PUB_REMOTE/$PUB_BRANCH^{tree}" 2>/dev/null || echo "")
if [[ "$src_tree" == "$pub_tree" ]]; then
  echo "$SRC_BRANCH e $PUB_REMOTE/$PUB_BRANCH hanno già lo stesso identico albero — niente da pubblicare."
  exit 0
fi

# --- gate: gofmt + go vet/build/test, in container (niente Go sull'host) ----
if [[ "$SKIP_GATE" != "1" ]]; then
  runtime=""
  command -v docker >/dev/null 2>&1 && runtime=docker
  [[ -z "$runtime" ]] && command -v podman >/dev/null 2>&1 && runtime=podman
  [[ -n "$runtime" ]] || { echo "né docker né podman in PATH — usa MYCELIUM_SKIP_GATE=1 per saltare il gate (sconsigliato)." >&2; exit 1; }

  echo "gate ($runtime, $GATE_IMAGE): gofmt + go vet/build/test"
  if ! $dry; then
    "$runtime" run --rm -v "$PWD":/src -w /src -e GOCACHE=/tmp/gocache "$GATE_IMAGE" bash -c '
      set -e
      git config --global --add safe.directory /src
      unformatted=$(gofmt -l $(git ls-files "*.go" | grep -v "^third_party/"))
      if [ -n "$unformatted" ]; then echo "gofmt needed on:"; echo "$unformatted"; exit 1; fi
      go vet ./...
      go build ./...
      go test ./...
    '
  fi
else
  echo "gate saltato (MYCELIUM_SKIP_GATE=1)"
fi

pub_head=$(git rev-parse "$PUB_REMOTE/$PUB_BRANCH")
echo "pubblico $SRC_BRANCH ($src_tree) su $PUB_REMOTE/$PUB_BRANCH (attuale $pub_head): \"$msg\""
if $dry; then echo "(dry-run, nulla eseguito — nessun oggetto creato)"; exit 0; fi

# --- pubblicazione -----------------------------------------------------------
# Un commit nuovo, tree = quello di dev, parent = l'HEAD attuale di main:
# è un fast-forward puro in cima alla storia esistente di main, mai una
# riscrittura. Il working tree locale (qualunque branch sia checkato ora)
# non viene toccato.
new_commit=$(git commit-tree "$src_tree" -p "$pub_head" -m "$msg")
echo "nuovo commit: $new_commit"
git push "$PUB_REMOTE" "$new_commit:refs/heads/$PUB_BRANCH"

# se esiste un branch locale con lo stesso nome del branch pubblico, allinealo
# (fast-forward-only: se non lo fosse, meglio fermarsi che clobberare a caso).
if git show-ref --verify --quiet "refs/heads/$PUB_BRANCH"; then
  git fetch . "$new_commit:$PUB_BRANCH" -q 2>/dev/null || \
    echo "nota: refs/heads/$PUB_BRANCH locale non aggiornato automaticamente (non era un fast-forward) — aggiornalo a mano se ti serve." >&2
fi

echo "fatto: $PUB_BRANCH pubblicato su $PUB_REMOTE ($pub_head -> $new_commit)."
