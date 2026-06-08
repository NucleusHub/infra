#!/usr/bin/env bash
# Builds all Vue frontends for production and starts the stack via Docker Compose.
# Run from any directory — paths are resolved relative to this script.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# ── Colour helpers ────────────────────────────────────────────────────────────
bold="\033[1m"; green="\033[32m"; yellow="\033[33m"; reset="\033[0m"
step() { echo -e "\n${bold}${green}▶ $*${reset}"; }
warn() { echo -e "${yellow}⚠  $*${reset}"; }

# ── 1. Build Vue frontends ────────────────────────────────────────────────────

step "[1/4] Building hub"
cd "$REPO/hub"
npm ci --prefer-offline
npm run build

step "[2/4] Building watchlist client"
cd "$REPO/apps/watchlist/client"
npm ci --prefer-offline
npm run build

step "[3/4] Building goal-calendar client"
cd "$REPO/apps/goal-calendar/client"
npm ci --prefer-offline
npm run build

step "[4/4] Building spotify widget"
cd "$REPO/widgets"
pnpm install --frozen-lockfile
pnpm run build   # runs vite build for all workspace client packages

# ── 2. Write production nginx config ─────────────────────────────────────────

step "Writing infra/nginx/nginx.prod.conf"

cat > "$REPO/infra/nginx/nginx.prod.conf" <<'NGINX'
server {
    listen 80 default_server;
    listen 443 ssl default_server;
    server_name nucleus.home;

    ssl_certificate     /etc/ssl/certs/nucleus.crt;
    ssl_certificate_key /etc/ssl/private/nucleus.key;

    root /usr/share/nginx/html;

    # ── Backend API routes ─────────────────────────────────────────────────

    location /api/registry {
        proxy_pass         http://registry:4000;
        proxy_http_version 1.1;
        proxy_set_header   Host         $host;
        proxy_set_header   X-Real-IP    $remote_addr;
    }

    location /api/spotify {
        proxy_pass         http://spotify-server:3002;
        proxy_http_version 1.1;
        proxy_set_header   Host         $host;
        proxy_set_header   X-Real-IP    $remote_addr;
    }

    location /api/goals {
        proxy_pass         http://goal-calendar-server:3001;
        proxy_http_version 1.1;
        proxy_set_header   Host         $host;
        proxy_set_header   X-Real-IP    $remote_addr;
    }

    location /uploads {
        proxy_pass         http://watchlist-server:3000;
        proxy_http_version 1.1;
        proxy_set_header   Host         $host;
        proxy_set_header   X-Real-IP    $remote_addr;
    }

    location /api {
        proxy_pass         http://watchlist-server:3000;
        proxy_http_version 1.1;
        proxy_set_header   Host         $host;
        proxy_set_header   X-Real-IP    $remote_addr;
    }

    # ── Static SPA routes ──────────────────────────────────────────────────
    # Each app was built with its matching base path, so assets resolve
    # correctly without any rewriting.

    location /watchlist {
        try_files $uri $uri/ /watchlist/index.html;
    }

    location /goals {
        try_files $uri $uri/ /goals/index.html;
    }

    location /spotify {
        try_files $uri $uri/ /spotify/index.html;
    }

    location ~* ^(/watchlist)?/favicon\.ico$ {
        root /usr/share/nginx/static;
        try_files /favicon.ico =404;
    }

    # Hub SPA — catch-all must come last
    location / {
        try_files $uri $uri/ /index.html;
    }
}
NGINX

# ── 3. Write production docker-compose ───────────────────────────────────────

step "Writing infra/docker-compose.prod.yml"

cat > "$REPO/infra/docker-compose.prod.yml" <<'COMPOSE'
# Production stack — no Vite dev servers; nginx serves pre-built static files.
# Servers run with plain `node index.js` (no --watch).
name: nucleus

services:
  nginx:
    image: nginx:alpine
    restart: unless-stopped
    ports:
      - "80:80"
      - "443:443"
    volumes:
      - ./nginx/nginx.prod.conf:/etc/nginx/conf.d/default.conf:ro
      # Hub dist is the root; sub-app dists are mounted inside it
      - ../hub/dist:/usr/share/nginx/html:ro
      - ../apps/watchlist/client/dist:/usr/share/nginx/html/watchlist:ro
      - ../apps/goal-calendar/client/dist:/usr/share/nginx/html/goals:ro
      - ../widgets/spotify/client/dist:/usr/share/nginx/html/spotify:ro
      - ../hub/public:/usr/share/nginx/static:ro
      - /etc/ssl/certs/nucleus.crt:/etc/ssl/certs/nucleus.crt:ro
      - /etc/ssl/private/nucleus.key:/etc/ssl/private/nucleus.key:ro
    depends_on:
      - registry
      - watchlist-server
      - goal-calendar-server
      - spotify-server

  registry:
    build:
      context: ./registry
    restart: unless-stopped
    environment:
      PORT: 4000
      APPS_DIR: /apps
      WIDGETS_DIR: /widgets
    volumes:
      - ../apps:/apps:ro
      - ../widgets:/widgets:ro

  watchlist-server:
    build:
      context: ../apps/watchlist/server
    restart: unless-stopped
    command: ["node", "index.js"]
    environment:
      MONGODB_URI: mongodb://mongo:27017/nucleus
      PORT: 3000
    volumes:
      - uploads_data:/app/uploads
    depends_on:
      mongo:
        condition: service_healthy
    healthcheck:
      test: ["CMD", "node", "-e", "require('http').get('http://localhost:3000/api/watchlist',r=>process.exit(r.statusCode<500?0:1)).on('error',()=>process.exit(1))"]
      interval: 5s
      timeout: 5s
      retries: 10
      start_period: 10s

  goal-calendar-server:
    build:
      context: ../apps/goal-calendar/server
    restart: unless-stopped
    command: ["node", "index.js"]
    environment:
      MONGODB_URI: mongodb://mongo:27017/nucleus
      PORT: 3001
    depends_on:
      mongo:
        condition: service_healthy
    healthcheck:
      test: ["CMD", "node", "-e", "require('http').get('http://localhost:3001/api/goals',r=>process.exit(r.statusCode<500?0:1)).on('error',()=>process.exit(1))"]
      interval: 5s
      timeout: 5s
      retries: 10
      start_period: 10s

  spotify-server:
    build:
      context: ../widgets/spotify/server
    restart: unless-stopped
    command: ["node", "index.js"]
    environment:
      PORT: 3002
      SPOTIFY_CLIENT_ID: ${SPOTIFY_CLIENT_ID}
      SPOTIFY_CLIENT_SECRET: ${SPOTIFY_CLIENT_SECRET}
      SPOTIFY_REDIRECT_URI: ${SPOTIFY_REDIRECT_URI:-http://nucleus.home/api/spotify/callback}
      SPOTIFY_SUCCESS_REDIRECT: ${SPOTIFY_SUCCESS_REDIRECT:-http://nucleus.home/spotify}
    healthcheck:
      test: ["CMD", "node", "-e", "require('http').get('http://localhost:3002/api/spotify/status',r=>process.exit(r.statusCode<500?0:1)).on('error',()=>process.exit(1))"]
      interval: 5s
      timeout: 5s
      retries: 10
      start_period: 10s

  mongo:
    image: ${MONGO_IMAGE:-mongo:7}
    restart: unless-stopped
    volumes:
      - mongo_data:/data/db
    healthcheck:
      test: ["CMD-SHELL", "mongosh --eval 'db.adminCommand({ping:1})' --quiet 2>/dev/null || mongo --eval 'db.adminCommand({ping:1})' --quiet"]
      interval: 5s
      timeout: 5s
      retries: 10
      start_period: 10s

volumes:
  mongo_data:
  uploads_data:
COMPOSE

# ── 4. Deploy ─────────────────────────────────────────────────────────────────

step "Starting production stack"
cd "$REPO/infra"
docker compose -f docker-compose.prod.yml up -d --build

echo -e "\n${bold}${green}✓ Production stack is up.${reset}"
echo "  Hub       → http://nucleus.home/"
echo "  Watchlist → http://nucleus.home/watchlist/"
echo "  Goals     → http://nucleus.home/goals/"
echo "  Spotify   → http://nucleus.home/spotify/"
