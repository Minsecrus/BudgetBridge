#!/usr/bin/env bash
set -euo pipefail

# BudgetBridge Interactive Deployment Script
# Usage: curl -sSL ... | bash  or  ./scripts/deploy.sh

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

log()  { echo -e "${GREEN}[✓]${NC} $*"; }
warn() { echo -e "${YELLOW}[!]${NC} $*"; }
err()  { echo -e "${RED}[✗]${NC} $*" >&2; }
info() { echo -e "${BLUE}[i]${NC} $*"; }

# ── Preflight checks ──────────────────────────────────────────
check_cmd() {
    if ! command -v "$1" &>/dev/null; then
        err "$1 is required but not installed."
        exit 1
    fi
}

check_cmd docker
check_cmd git

if ! docker compose version &>/dev/null 2>&1; then
    err "docker compose v2 is required. Install: https://docs.docker.com/compose/install/"
    exit 1
fi

# ── Project directory ─────────────────────────────────────────
echo ""
echo -e "${BLUE}═══════════════════════════════════════════${NC}"
echo -e "${BLUE}    BudgetBridge Deployment Setup${NC}"
echo -e "${BLUE}═══════════════════════════════════════════${NC}"
echo ""

if [ -d "BudgetBridge" ]; then
    info "BudgetBridge directory already exists, pulling latest..."
    cd BudgetBridge
    git pull
else
    read -rp "Git repository URL [skip to use current directory]: " REPO_URL
    if [ -z "$REPO_URL" ]; then
        if [ ! -f "docker-compose.yml" ]; then
            err "Not in a BudgetBridge directory and no repo URL given."
            exit 1
        fi
        info "Using current directory."
    else
        git clone "$REPO_URL" BudgetBridge
        cd BudgetBridge
    fi
fi

# ── Configuration ─────────────────────────────────────────────
echo ""
info "Configuring BudgetBridge..."
echo ""

# Listen port - read from config.yaml if exists
DEFAULT_BACKEND_PORT="8080"
DEFAULT_FRONTEND_PORT="5173"
if [ -f "config.yaml" ]; then
    EXISTING_BACKEND_PORT=$(grep -E "^listen:" config.yaml 2>/dev/null | grep -oE '[0-9]+' || true)
    [ -n "$EXISTING_BACKEND_PORT" ] && DEFAULT_BACKEND_PORT="$EXISTING_BACKEND_PORT"
    EXISTING_FRONTEND_PORT=$(grep -E "^frontend_port:" config.yaml 2>/dev/null | grep -oE '[0-9]+' || true)
    [ -n "$EXISTING_FRONTEND_PORT" ] && DEFAULT_FRONTEND_PORT="$EXISTING_FRONTEND_PORT"
fi

read -rp "Backend port [${DEFAULT_BACKEND_PORT}]: " LISTEN_PORT
LISTEN_PORT="${LISTEN_PORT:-${DEFAULT_BACKEND_PORT}}"

read -rp "Frontend port [${DEFAULT_FRONTEND_PORT}]: " FRONTEND_PORT
FRONTEND_PORT="${FRONTEND_PORT:-${DEFAULT_FRONTEND_PORT}}"

# Public URL
read -rp "Public URL (e.g. https://api.example.com, leave empty for auto): " PUBLIC_URL

# Upstream
read -rp "Upstream API URL [https://dashscope.aliyuncs.com/compatible-mode/v1]: " UPSTREAM_URL
UPSTREAM_URL="${UPSTREAM_URL:-https://dashscope.aliyuncs.com/compatible-mode/v1}"

# Admin password — protects /admin/*. Leaving it empty would leave the admin API
# fully unauthenticated, so we always set one (auto-generate if the user skips).
read -rsp "Admin password (leave empty to auto-generate): " ADMIN_PASSWORD
echo ""
if [ -z "$ADMIN_PASSWORD" ]; then
    ADMIN_PASSWORD="$(openssl rand -hex 18 2>/dev/null || tr -dc 'A-Za-z0-9' </dev/urandom | head -c 32)"
    info "Auto-generated admin password: ${ADMIN_PASSWORD}"
    warn "Save it now — it is bcrypt-hashed on first start and cannot be recovered."
fi
# Escape single quotes for safe YAML single-quoted embedding.
ADMIN_PASSWORD_ESC="${ADMIN_PASSWORD//\'/\'\'}"

# Generate config.yaml (accounts managed via frontend)
cat > config.yaml << EOF
listen: ":${LISTEN_PORT}"
frontend_port: ${FRONTEND_PORT}
upstream_url: "${UPSTREAM_URL}"
admin_password: '${ADMIN_PASSWORD_ESC}'
${PUBLIC_URL:+public_url: "${PUBLIC_URL}"}
accounts:
  []
EOF

log "Generated config.yaml"

# ── Caddy setup ────────────────────────────────────────────────
echo ""
info "Caddy is included in Docker Compose and handles reverse proxying."
info "Leave domain empty to use HTTP-only mode (:80) — suitable for internal/private deployments."
read -rp "Domain for automatic HTTPS (leave empty for HTTP-only): " DOMAIN

if [ -n "$DOMAIN" ] && [ -z "$PUBLIC_URL" ]; then
    # Auto-set public_url when domain is provided
    sed -i "s|^listen: .*|listen: \":${LISTEN_PORT}\"\npublic_url: \"https://${DOMAIN}\"|" config.yaml
    awk '!seen[$0]++' config.yaml > /tmp/bb_config && mv /tmp/bb_config config.yaml
    log "public_url set to https://${DOMAIN}"
fi

# ── Docker Compose ─────────────────────────────────────────────
echo ""
info "Starting BudgetBridge with Docker Compose..."

# Write .env so docker compose injects DOMAIN/BACKEND_PORT into the caddy
# service (the Caddyfile substitutes them via {$DOMAIN}/{$BACKEND_PORT}). The
# Caddyfile stays a clean template — no in-place sed, no git-checkout — so
# container restarts keep reading valid config.
CADDYFILE_HOST="${DOMAIN:-:80}"
cat > .env << EOF
DOMAIN=${CADDYFILE_HOST}
BACKEND_PORT=${LISTEN_PORT}
EOF
log "Wrote .env (DOMAIN=${CADDYFILE_HOST}, BACKEND_PORT=${LISTEN_PORT})"

docker compose up -d --build

echo ""
echo -e "${GREEN}═══════════════════════════════════════════${NC}"
echo -e "${GREEN}    BudgetBridge is running!${NC}"
echo -e "${GREEN}═══════════════════════════════════════════${NC}"
echo ""

if [ -n "$DOMAIN" ]; then
    echo -e "  Frontend:  ${BLUE}https://${DOMAIN}${NC}"
    echo -e "  OpenAI:    ${BLUE}https://${DOMAIN}/v1${NC}"
    echo -e "  Anthropic: ${BLUE}https://${DOMAIN}${NC}"
else
    IP=$(hostname -I 2>/dev/null | awk '{print $1}' || echo "localhost")
    echo -e "  Frontend:  ${BLUE}http://${IP}${NC}"
    echo -e "  OpenAI:    ${BLUE}http://${IP}/v1${NC}"
    echo -e "  Anthropic: ${BLUE}http://${IP}${NC}"
fi

echo ""
echo "  Manage:    docker compose logs -f"
echo "  Stop:      docker compose down"
echo ""
