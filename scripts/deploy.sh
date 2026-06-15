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

# Listen port
read -rp "Backend listen port [8080]: " LISTEN_PORT
LISTEN_PORT="${LISTEN_PORT:-8080}"

# Public URL
read -rp "Public URL (e.g. https://api.example.com, leave empty for auto): " PUBLIC_URL

# Upstream
read -rp "Upstream API URL [https://dashscope.aliyuncs.com/compatible-mode/v1]: " UPSTREAM_URL
UPSTREAM_URL="${UPSTREAM_URL:-https://dashscope.aliyuncs.com/compatible-mode/v1}"

# Accounts
echo ""
info "Adding API accounts (press Enter with empty key to stop):"
echo ""

ACCOUNTS=""
ACC_COUNT=0
while true; do
    echo -e "${YELLOW}── Account $((ACC_COUNT + 1)) ──${NC}"
    read -rp "  Alias (optional): " ACC_ALIAS
    read -rp "  API Key: " ACC_KEY

    if [ -z "$ACC_KEY" ]; then
        break
    fi

    read -rp "  AccessKey ID (for balance check, optional): " ACC_AK_ID
    read -rp "  AccessKey Secret (optional): " ACC_AK_SECRET

    [ -z "$ACC_ALIAS" ] && ACC_ALIAS="账号$((ACC_COUNT + 1))"

    ACCOUNTS+="  - alias: \"$ACC_ALIAS\"
    api_key: \"$ACC_KEY\""

    if [ -n "$ACC_AK_ID" ]; then
        ACCOUNTS+="
    ak_id: \"$ACC_AK_ID\""
    fi
    if [ -n "$ACC_AK_SECRET" ]; then
        ACCOUNTS+="
    ak_secret: \"$ACC_AK_SECRET\""
    fi
    ACCOUNTS+=$'\n'

    ACC_COUNT=$((ACC_COUNT + 1))
    log "Added: $ACC_ALIAS"
done

if [ "$ACC_COUNT" -eq 0 ]; then
    warn "No accounts added. You can add them later via the web panel."
    ACCOUNTS="  []"
fi

# Generate config.yaml
cat > backend/config.yaml << EOF
listen: ":${LISTEN_PORT}"
upstream_url: "${UPSTREAM_URL}"
${PUBLIC_URL:+public_url: "${PUBLIC_URL}"}
accounts:
${ACCOUNTS}
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

    # Update upstream ports
    sed -i "s/127.0.0.1:8080/127.0.0.1:${LISTEN_PORT}/g" /etc/nginx/conf.d/budgetbridge.conf

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
      - "127.0.0.1:3000:80"
EOF
    log "Created docker-compose.override.yml (localhost-only ports)"
fi

docker compose up -d --build

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
    echo -e "  Frontend:  ${BLUE}http://${IP}:3000${NC}"
    echo -e "  OpenAI:    ${BLUE}http://${IP}:${LISTEN_PORT}/v1${NC}"
    echo -e "  Anthropic: ${BLUE}http://${IP}:${LISTEN_PORT}${NC}"
fi

echo ""
echo "  Manage:    docker compose logs -f"
echo "  Stop:      docker compose down"
echo ""
