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
if [ -f "backend/config.yaml" ]; then
    EXISTING_BACKEND_PORT=$(grep -E "^listen:" backend/config.yaml 2>/dev/null | grep -oE '[0-9]+' || true)
    [ -n "$EXISTING_BACKEND_PORT" ] && DEFAULT_BACKEND_PORT="$EXISTING_BACKEND_PORT"
    EXISTING_FRONTEND_PORT=$(grep -E "^frontend_port:" backend/config.yaml 2>/dev/null | grep -oE '[0-9]+' || true)
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

# Generate config.yaml (accounts managed via frontend)
cat > backend/config.yaml << EOF
listen: ":${LISTEN_PORT}"
frontend_port: ${FRONTEND_PORT}
upstream_url: "${UPSTREAM_URL}"
${PUBLIC_URL:+public_url: "${PUBLIC_URL}"}
accounts:
  []
EOF

log "Generated backend/config.yaml"

# ── Nginx setup ────────────────────────────────────────────────
echo ""
read -rp "Set up Nginx reverse proxy with HTTPS? (y/N): " SETUP_NGINX
SETUP_NGINX="${SETUP_NGINX:-n}"

if [[ "$SETUP_NGINX" =~ ^[Yy] ]]; then
    check_cmd nginx

    read -rp "Domain name (e.g. api.example.com): " DOMAIN

    if [ -z "$DOMAIN" ]; then
        err "Domain name is required for Nginx setup."
        exit 1
    fi

    # Update public_url in config
    if [ -z "$PUBLIC_URL" ]; then
        sed -i "s|^listen: .*|listen: \":${LISTEN_PORT}\"\npublic_url: \"https://${DOMAIN}\"|" backend/config.yaml
        # Remove duplicate listen line
        awk '!seen[$0]++' backend/config.yaml > /tmp/bb_config && mv /tmp/bb_config backend/config.yaml
    fi

    # Copy and customize nginx config
    cp nginx/budgetbridge.conf /etc/nginx/conf.d/budgetbridge.conf
    sed -i "s/your-domain.com/${DOMAIN}/g" /etc/nginx/conf.d/budgetbridge.conf

    # Update port placeholders
    sed -i "s/__BACKEND_PORT__/${LISTEN_PORT}/g" /etc/nginx/conf.d/budgetbridge.conf
    sed -i "s/__FRONTEND_PORT__/${FRONTEND_PORT}/g" /etc/nginx/conf.d/budgetbridge.conf

    # Check if certbot is available
    if command -v certbot &>/dev/null; then
        info "Obtaining SSL certificate with certbot..."
        certbot certonly --nginx -d "$DOMAIN" --non-interactive --agree-tos --register-unsafely-without-email 2>/dev/null || {
            warn "Auto cert failed. Run manually:"
            echo "  sudo certbot --nginx -d ${DOMAIN}"
        }
    else
        warn "certbot not found. Install and run:"
        echo "  sudo apt install certbot python3-certbot-nginx"
        echo "  sudo certbot --nginx -d ${DOMAIN}"
    fi

    nginx -t && systemctl reload nginx
    log "Nginx configured for ${DOMAIN}"
fi

# ── Docker Compose ─────────────────────────────────────────────
echo ""
info "Starting BudgetBridge with Docker Compose..."

# Replace port placeholders in docker-compose.yml
sed -i "s/__BACKEND_PORT__/${LISTEN_PORT}/g" docker-compose.yml
sed -i "s/__FRONTEND_PORT__/${FRONTEND_PORT}/g" docker-compose.yml

# Replace port placeholder in frontend nginx config for Docker build
sed -i "s/__BACKEND_PORT__/${LISTEN_PORT}/g" frontend/nginx.conf

# If using Nginx externally, bind to localhost only
if [[ "$SETUP_NGINX" =~ ^[Yy] ]]; then
    # Create override for production
    cat > docker-compose.override.yml << EOF
services:
  backend:
    ports:
      - "127.0.0.1:${LISTEN_PORT}:${LISTEN_PORT}"
  frontend:
    ports:
      - "127.0.0.1:${FRONTEND_PORT}:80"
EOF
    log "Created docker-compose.override.yml (localhost-only ports)"
fi

docker compose up -d --build

# Restore template files to keep repo clean
git checkout -- docker-compose.yml frontend/nginx.conf

echo ""
echo -e "${GREEN}═══════════════════════════════════════════${NC}"
echo -e "${GREEN}    BudgetBridge is running!${NC}"
echo -e "${GREEN}═══════════════════════════════════════════${NC}"
echo ""

if [[ "$SETUP_NGINX" =~ ^[Yy] ]]; then
    echo -e "  Frontend:  ${BLUE}https://${DOMAIN}${NC}"
    echo -e "  OpenAI:    ${BLUE}https://${DOMAIN}/v1${NC}"
    echo -e "  Anthropic: ${BLUE}https://${DOMAIN}${NC}"
else
    IP=$(hostname -I 2>/dev/null | awk '{print $1}' || echo "localhost")
    echo -e "  Frontend:  ${BLUE}http://${IP}:${FRONTEND_PORT}${NC}"
    echo -e "  OpenAI:    ${BLUE}http://${IP}:${LISTEN_PORT}/v1${NC}"
    echo -e "  Anthropic: ${BLUE}http://${IP}:${LISTEN_PORT}${NC}"
fi

echo ""
echo "  Manage:    docker compose logs -f"
echo "  Stop:      docker compose down"
echo ""
