#!/usr/bin/env bash
# mycelium installer / updater
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/Lotho33/mycelium-core/main/install.sh | bash
#   bash install.sh --update          # aggiorna binary e riavvia servizio
#   bash install.sh --update v1.3.0   # aggiorna a versione specifica

set -euo pipefail

REPO="Lotho33/mycelium-core"
INSTALL_DIR="/opt/mycelium"
BIN_PATH="/usr/local/bin/mycelium"
UPDATE_SCRIPT="/usr/local/bin/mycelium-update"
SERVICE_NAME="mycelium"
DATA_DIR="/var/lib/mycelium"
LOG_DIR="/var/log/mycelium"

# ── colori ──────────────────────────────────────────────────────────────────
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; CYAN='\033[0;36m'; NC='\033[0m'
info()    { echo -e "${CYAN}[mycelium]${NC} $*"; }
success() { echo -e "${GREEN}[mycelium]${NC} $*"; }
warn()    { echo -e "${YELLOW}[mycelium]${NC} $*"; }
die()     { echo -e "${RED}[mycelium] ERRORE:${NC} $*" >&2; exit 1; }

# ── modalità update ──────────────────────────────────────────────────────────
MODE="install"
TARGET_VERSION=""
if [[ "${1:-}" == "--update" ]]; then
  MODE="update"
  TARGET_VERSION="${2:-}"
fi

# ── root check ───────────────────────────────────────────────────────────────
[[ $EUID -ne 0 ]] && die "esegui con sudo o come root"

# ── detect architettura ──────────────────────────────────────────────────────
detect_arch() {
  local arch
  arch=$(uname -m)
  case "$arch" in
    x86_64)  echo "linux-amd64" ;;
    aarch64) echo "linux-arm64" ;;
    armv7*)  echo "linux-armv7" ;;
    armv6*)  echo "linux-armv6" ;;
    *)       die "architettura non supportata: $arch" ;;
  esac
}

ARCH=$(detect_arch)
info "Architettura rilevata: $ARCH"

# ── risolvi versione target ──────────────────────────────────────────────────
resolve_version() {
  if [[ -n "$TARGET_VERSION" ]]; then
    echo "$TARGET_VERSION"
    return
  fi
  info "Recupero ultima versione da GitHub..."
  local tag
  tag=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
    | grep '"tag_name"' | head -1 | sed 's/.*"tag_name": *"\([^"]*\)".*/\1/')
  [[ -z "$tag" ]] && die "impossibile recuperare la versione da GitHub"
  echo "$tag"
}

VERSION=$(resolve_version)
info "Versione target: $VERSION"

BINARY_URL="https://github.com/${REPO}/releases/download/${VERSION}/mycelium-${ARCH}"

# ── download binary ──────────────────────────────────────────────────────────
download_binary() {
  local tmp
  tmp=$(mktemp)
  info "Download $BINARY_URL ..."
  if ! curl -fsSL --progress-bar "$BINARY_URL" -o "$tmp"; then
    rm -f "$tmp"
    die "download fallito — verifica che la versione $VERSION esista per $ARCH"
  fi
  chmod +x "$tmp"
  echo "$tmp"
}

# ── modalità update ──────────────────────────────────────────────────────────
if [[ "$MODE" == "update" ]]; then
  info "Aggiornamento mycelium → $VERSION"
  TMP_BIN=$(download_binary)
  systemctl stop "$SERVICE_NAME" 2>/dev/null || true
  mv "$TMP_BIN" "$BIN_PATH"
  systemctl start "$SERVICE_NAME"
  success "mycelium aggiornato a $VERSION e riavviato."
  exit 0
fi

# ────────────────────────────────────────────────────────────────────────────
# INSTALLAZIONE COMPLETA
# ────────────────────────────────────────────────────────────────────────────

echo ""
echo -e "${CYAN}╔══════════════════════════════════════╗${NC}"
echo -e "${CYAN}║      mycelium — Installazione        ║${NC}"
echo -e "${CYAN}╚══════════════════════════════════════╝${NC}"
echo ""

# ── porta ────────────────────────────────────────────────────────────────────
read -rp "Porta HTTP [default: 8000]: " PORT
PORT="${PORT:-8000}"
[[ "$PORT" =~ ^[0-9]+$ && "$PORT" -ge 1 && "$PORT" -le 65535 ]] || die "porta non valida: $PORT"

# ── crea directory ────────────────────────────────────────────────────────────
info "Creazione directory..."
mkdir -p "$INSTALL_DIR"/{plugins,web} "$DATA_DIR" "$LOG_DIR"

# ── download binary ───────────────────────────────────────────────────────────
TMP_BIN=$(download_binary)
mv "$TMP_BIN" "$BIN_PATH"
success "Binary installato in $BIN_PATH"

# ── installa browser ──────────────────────────────────────────────────────────
install_browser() {
  if command -v chromium-browser &>/dev/null || command -v chromium &>/dev/null || command -v google-chrome &>/dev/null; then
    success "Browser Chromium già installato."
    return
  fi

  info "Browser Chromium non trovato — installazione in corso..."
  if command -v apt-get &>/dev/null; then
    apt-get update -qq
    apt-get install -y -qq chromium-browser 2>/dev/null \
      || apt-get install -y -qq chromium 2>/dev/null \
      || {
        true  # no embedded browser: the optional companion browser service runs separately
        warn "Su ARM installa manualmente: sudo apt install chromium-browser"
      }
  elif command -v dnf &>/dev/null; then
    dnf install -y chromium
  elif command -v pacman &>/dev/null; then
    pacman -Sy --noconfirm chromium
  else
    warn "Gestore pacchetti non riconosciuto. Installa Chromium manualmente."
    warn "Su ARM è obbligatorio: sudo apt install chromium-browser"
  fi
}
install_browser

# ── config.json iniziale ──────────────────────────────────────────────────────
CONFIG_FILE="$DATA_DIR/config.json"
if [[ ! -f "$CONFIG_FILE" ]]; then
  cat > "$CONFIG_FILE" <<CONFIGEOF
{
    "server_port": "${PORT}"
}
CONFIGEOF
  success "Configurazione iniziale scritta in $CONFIG_FILE"
fi

# ── systemd service ───────────────────────────────────────────────────────────
cat > "/etc/systemd/system/${SERVICE_NAME}.service" <<UNITEOF
[Unit]
Description=mycelium Media Server
After=network.target

[Service]
Type=simple
ExecStart=${BIN_PATH}
WorkingDirectory=${INSTALL_DIR}
Environment="MYCELIUM_DATA_DIR=${DATA_DIR}"
Restart=on-failure
RestartSec=5
StandardOutput=append:${LOG_DIR}/mycelium.log
StandardError=append:${LOG_DIR}/mycelium.log

[Install]
WantedBy=multi-user.target
UNITEOF

systemctl daemon-reload
systemctl enable "$SERVICE_NAME"
systemctl start "$SERVICE_NAME"
success "Servizio systemd avviato."

# ── installa script di aggiornamento ──────────────────────────────────────────
cat > "$UPDATE_SCRIPT" <<UPDATEEOF
#!/usr/bin/env bash
# Usato da mycelium admin dashboard per aggiornamenti automatici.
# Viene anche invocato da: bash install.sh --update [versione]
set -euo pipefail
TARGET_VERSION="\${1:-}"
exec bash <(curl -fsSL https://raw.githubusercontent.com/${REPO}/main/install.sh) --update "\$TARGET_VERSION"
UPDATEEOF
chmod +x "$UPDATE_SCRIPT"
success "Script aggiornamento installato in $UPDATE_SCRIPT"

# ── rileva IP locale ──────────────────────────────────────────────────────────
LOCAL_IP=$(hostname -I 2>/dev/null | awk '{print $1}' || echo "127.0.0.1")

# ── riepilogo ─────────────────────────────────────────────────────────────────
echo ""
echo -e "${GREEN}╔══════════════════════════════════════════════════╗${NC}"
echo -e "${GREEN}║         mycelium installato con successo!        ║${NC}"
echo -e "${GREEN}╚══════════════════════════════════════════════════╝${NC}"
echo ""
echo -e "  URL IP diretto  : ${CYAN}http://${LOCAL_IP}:${PORT}${NC}"
echo -e "  Ricerca da Pileus: apri l'app, il server si trova da solo sulla rete locale (discovery UDP)."
echo -e "  Setup admin     : ${CYAN}http://${LOCAL_IP}:${PORT}/setup${NC}"
echo ""
echo -e "  ${YELLOW}Kodi add-on${NC}: inserisci uno degli URL sopra come indirizzo server."
echo ""
echo -e "  Comandi utili:"
echo -e "    sudo systemctl status mycelium"
echo -e "    sudo journalctl -u mycelium -f"
echo -e "    sudo bash install.sh --update        # aggiorna all'ultima versione"
echo ""
