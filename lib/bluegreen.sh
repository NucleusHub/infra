#!/usr/bin/env bash
# infra/lib/bluegreen.sh — shared helpers for blue/green deployment.
#
# Sourced by infra/production, infra/rollback and infra/bluegreen-init. Holds the
# topology knowledge (two color stacks, one shared data stack, one edge proxy) so
# the entry scripts read as a linear flow. Everything here is plain Docker Compose
# + shell — no Anchor — but is structured so Anchor can call the same primitives
# later (each helper is independent and side-effect-scoped).
#
# Layout it manages:
#   nucleus-data           always-on: mongo/redis/minio (+ existing volumes)
#   nucleus-blue|green     the swappable app stacks (servers + a web nginx)
#   nucleus-edge           always-on: nginx on 80/443, forwards to the active color

# ── Paths (anchored on this file, not the caller's CWD) ───────────────────────
LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" && pwd)"
INFRA="$(cd "$LIB_DIR/.." && pwd)"
ROOT="$(cd "$INFRA/.." && pwd)"
STACKS_DIR="$INFRA/.stacks"          # per-color static snapshots + state file
STATE_FILE="$STACKS_DIR/active"      # which color currently serves traffic
EDGE_ACTIVE="$INFRA/nginx/edge/active.inc"

# nvm's shell init isn't loaded in non-interactive / non-login shells (e.g. an
# `ssh host './production'` invocation), so node/npm — needed by `nucleus build`
# and the health checks — may be off PATH. Put the newest installed node on PATH
# if it's missing. Harmless when node is already available.
if ! command -v node >/dev/null 2>&1; then
  _node_bin="$(ls -d "$HOME"/.nvm/versions/node/*/bin 2>/dev/null | sort -V | tail -n1 || true)"
  [ -n "${_node_bin:-}" ] && export PATH="$_node_bin:$PATH"
fi

# ── Topology constants ────────────────────────────────────────────────────────
DATA_NET="nucleus-data-net"
EDGE_NET="nucleus-edge-net"
BLUE_PORT=8081                       # debug host port for the blue web nginx
GREEN_PORT=8082                      # debug host port for the green web nginx
HEALTH_TIMEOUT="${HEALTH_TIMEOUT:-300}"  # seconds to wait for a stack to go healthy
                                         # (generous: a small box's health checks
                                         # flap under the mass-startup load spike)

# ── Logging ───────────────────────────────────────────────────────────────────
_BOLD="$(printf '\033[1m')"; _GREEN="$(printf '\033[32m')"; _YELLOW="$(printf '\033[33m')"
_RED="$(printf '\033[31m')"; _DIM="$(printf '\033[2m')"; _RESET="$(printf '\033[0m')"

log_stage() { printf '\n%s%s▶ %s%s\n' "$_BOLD" "$_GREEN" "$*" "$_RESET"; }
log_info()  { printf '  %s%s%s\n' "$_DIM" "$*" "$_RESET"; }
log_ok()    { printf '\n%s%s✓ %s%s\n' "$_BOLD" "$_GREEN" "$*" "$_RESET"; }
log_warn()  { printf '%s⚠  %s%s\n' "$_YELLOW" "$*" "$_RESET"; }
log_err()   { printf '%s✗ %s%s\n' "$_RED" "$*" "$_RESET" >&2; }
die()       { log_err "$*"; exit 1; }

# ── Compose wrappers ──────────────────────────────────────────────────────────
# Each targets one project with its generated file. dc_stack injects the two
# variables the color template interpolates (project suffix, alias, debug port).
dc_data() { docker compose -p nucleus-data -f "$INFRA/docker-compose.data.yml" "$@"; }
dc_edge() { docker compose -p nucleus-edge -f "$INFRA/docker-compose.edge.yml" "$@"; }
dc_stack() {
  local color="$1"; shift
  NUCLEUS_COLOR="$color" NUCLEUS_WEB_PORT="$(color_port "$color")" \
    docker compose -p "nucleus-$color" -f "$INFRA/docker-compose.stack.yml" "$@"
}

# ── Color / state helpers ─────────────────────────────────────────────────────
color_port() { case "$1" in blue) echo "$BLUE_PORT" ;; green) echo "$GREEN_PORT" ;; *) echo 8080 ;; esac; }
other_color() { case "$1" in blue) echo green ;; green) echo blue ;; *) echo blue ;; esac; }
read_active()  { [ -f "$STATE_FILE" ] && cat "$STATE_FILE" || true; }
write_active() { mkdir -p "$STACKS_DIR"; printf '%s\n' "$1" > "$STATE_FILE"; }
# The color to deploy into: the one NOT currently serving (blue on a fresh install).
target_color() { other_color "$(read_active)"; }

# ── MongoDB image selection (mirrors infra/nucleus: MongoDB 5+ needs AVX) ──────
set_mongo_image() {
  if grep -q ' avx ' /proc/cpuinfo 2>/dev/null; then
    MONGO_IMAGE="mongo:7"
  else
    MONGO_IMAGE="mongo:4.4"
    log_warn "No AVX detected — using mongo:4.4"
  fi
  export MONGO_IMAGE
  # Persist so bare `docker compose` invocations pick the same image.
  if [ -f "$INFRA/.env" ] && grep -q '^MONGO_IMAGE=' "$INFRA/.env"; then
    sed -i "s|^MONGO_IMAGE=.*|MONGO_IMAGE=$MONGO_IMAGE|" "$INFRA/.env"
  else
    echo "MONGO_IMAGE=$MONGO_IMAGE" >> "$INFRA/.env"
  fi
}

# ── Bootstrap (idempotent; safe to call every deploy) ─────────────────────────
ensure_networks() {
  local n
  for n in "$DATA_NET" "$EDGE_NET"; do
    docker network inspect "$n" >/dev/null 2>&1 || {
      log_info "Creating network $n"
      docker network create "$n" >/dev/null
    }
  done
}

ensure_data() {
  log_info "Ensuring shared data stack (mongo/redis/minio) is up…"
  dc_data up -d --wait --wait-timeout "$HEALTH_TIMEOUT" \
    || die "Shared data stack failed to become healthy."
}

ensure_edge() {
  # The edge won't start if its active include is missing (nginx errors on a
  # missing include), so seed a default before first boot.
  [ -f "$EDGE_ACTIVE" ] || printf 'set $active web-blue;\n' > "$EDGE_ACTIVE"
  log_info "Ensuring edge proxy is up…"
  dc_edge up -d || die "Edge proxy failed to start."
}

# ── Static snapshot (per-color isolation) ─────────────────────────────────────
# Copies the freshly-built dist trees into this color's own srv dir so building
# the new color never disturbs the dist the live color is serving. The route→dir
# mapping mirrors generateStackCompose: source is always <manifestdir>/client/dist,
# dest is /srv/<route>. Read the route with node (robust JSON parse).
_cp_dist() { # src dest
  [ -d "$1" ] || { log_warn "missing build output: $1 (skipping)"; return 0; }
  mkdir -p "$2"
  cp -a "$1/." "$2/"
}

snapshot_dist() {
  local color="$1" srv mf dir route dist
  srv="$STACKS_DIR/$color/srv"
  rm -rf "$srv"; mkdir -p "$srv"

  _cp_dist "$ROOT/hub/dist"   "$srv/hub"
  _cp_dist "$ROOT/hub/public" "$srv/static"

  for mf in "$ROOT"/apps/*/nucleus.app.json "$ROOT"/widgets/*/nucleus.widget.json; do
    [ -f "$mf" ] || continue
    dir="$(dirname "$mf")"
    route="$(node -e "try{process.stdout.write((require(process.argv[1]).route)||'')}catch(e){}" "$mf")"
    [ -n "$route" ] || continue
    dist="$dir/client/dist"
    [ -d "$dist" ] || continue
    _cp_dist "$dist" "$srv/${route#/}"
  done
}

# ── Health ────────────────────────────────────────────────────────────────────
# Wait until every service in a color stack is running and (if it has a
# healthcheck) healthy. Parses `compose ps --format json` with node (handles both
# the NDJSON and JSON-array shapes different compose versions emit).
wait_stack_healthy() {
  local color="$1" timeout="${2:-$HEALTH_TIMEOUT}" elapsed=0 json
  while :; do
    json="$(dc_stack "$color" ps --format json 2>/dev/null || true)"
    if printf '%s' "$json" | node "$LIB_DIR/healthy.js"; then
      return 0
    fi
    [ "$elapsed" -ge "$timeout" ] && return 1
    sleep 3; elapsed=$((elapsed + 3))
  done
}

# End-to-end probe through the color's own debug port (auth is the deepest core
# dependency: reaching it proves web→server→data wiring for this color).
health_probe() {
  local port; port="$(color_port "$1")"
  node -e "require('http').get('http://localhost:$port/api/auth/health',r=>process.exit(r.statusCode<500?0:1)).on('error',()=>process.exit(1))"
}

# ── Traffic switch (the only step that briefly interrupts requests) ────────────
# Rewrites the edge's active include and applies it with a graceful reload — no
# restart, so in-flight connections drain. Validates the config first and never
# leaves the edge pointing at a config nginx rejects.
switch_traffic() {
  local color="$1"
  printf 'set $active web-%s;\n' "$color" > "$EDGE_ACTIVE"
  if ! dc_edge exec -T nginx nginx -t >/dev/null 2>&1; then
    die "Edge nginx rejected the config after switching to $color — traffic NOT switched."
  fi
  dc_edge exec -T nginx nginx -s reload \
    || die "Edge reload failed — traffic NOT switched to $color."
}

# ── Stop a color (kept intact for rollback/debug — never `down`) ───────────────
stop_stack() {
  local color="$1"
  [ -n "$color" ] || return 0
  log_info "Stopping $color stack (containers kept for rollback/debug)…"
  dc_stack "$color" stop || log_warn "Could not stop $color stack cleanly (continuing)."
}
