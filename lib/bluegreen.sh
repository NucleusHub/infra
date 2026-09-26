#!/usr/bin/env bash
LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" && pwd)"
INFRA="$(cd "$LIB_DIR/.." && pwd)"
ROOT="$(cd "$INFRA/.." && pwd)"
STACKS_DIR="$INFRA/.stacks"
STATE_FILE="$STACKS_DIR/active"
EDGE_ACTIVE="$INFRA/nginx/edge/active.inc"

# nvm is not loaded in non-interactive shells (e.g. ssh host ./production).
if ! command -v node >/dev/null 2>&1; then
  _node_bin="$(ls -d "$HOME"/.nvm/versions/node/*/bin 2>/dev/null | sort -V | tail -n1 || true)"
  [ -n "${_node_bin:-}" ] && export PATH="$_node_bin:$PATH"
fi

DATA_NET="nucleus-data-net"
EDGE_NET="nucleus-edge-net"
BLUE_PORT=8081
GREEN_PORT=8082
HEALTH_TIMEOUT="${HEALTH_TIMEOUT:-300}"

_BOLD="$(printf '\033[1m')"; _GREEN="$(printf '\033[32m')"; _YELLOW="$(printf '\033[33m')"
_RED="$(printf '\033[31m')"; _DIM="$(printf '\033[2m')"; _RESET="$(printf '\033[0m')"

log_stage() { printf '\n%s%s▶ %s%s\n' "$_BOLD" "$_GREEN" "$*" "$_RESET"; }
log_info()  { printf '  %s%s%s\n' "$_DIM" "$*" "$_RESET"; }
log_ok()    { printf '\n%s%s✓ %s%s\n' "$_BOLD" "$_GREEN" "$*" "$_RESET"; }
log_warn()  { printf '%s⚠  %s%s\n' "$_YELLOW" "$*" "$_RESET"; }
log_err()   { printf '%s✗ %s%s\n' "$_RED" "$*" "$_RESET" >&2; }
die()       { log_err "$*"; exit 1; }

dc_data() { docker compose -p nucleus-data -f "$INFRA/docker-compose.data.yml" "$@"; }
dc_edge() { docker compose -p nucleus-edge -f "$INFRA/docker-compose.edge.yml" "$@"; }
dc_stack() {
  local color="$1"; shift
  NUCLEUS_COLOR="$color" NUCLEUS_WEB_PORT="$(color_port "$color")" \
    docker compose -p "nucleus-$color" -f "$INFRA/docker-compose.stack.yml" "$@"
}

color_port() { case "$1" in blue) echo "$BLUE_PORT" ;; green) echo "$GREEN_PORT" ;; *) echo 8080 ;; esac; }
other_color() { case "$1" in blue) echo green ;; green) echo blue ;; *) echo blue ;; esac; }
read_active()  { [ -f "$STATE_FILE" ] && cat "$STATE_FILE" || true; }
write_active() { mkdir -p "$STACKS_DIR"; printf '%s\n' "$1" > "$STATE_FILE"; }
target_color() { other_color "$(read_active)"; }

set_mongo_image() {
  if grep -q ' avx ' /proc/cpuinfo 2>/dev/null; then
    MONGO_IMAGE="mongo:7"
  else
    MONGO_IMAGE="mongo:4.4"
    log_warn "No AVX detected — using mongo:4.4"
  fi
  export MONGO_IMAGE
  if [ -f "$INFRA/.env" ] && grep -q '^MONGO_IMAGE=' "$INFRA/.env"; then
    awk -v img="$MONGO_IMAGE" '/^MONGO_IMAGE=/ { $0 = "MONGO_IMAGE=" img } 1' "$INFRA/.env" > "$INFRA/.env.tmp" && mv "$INFRA/.env.tmp" "$INFRA/.env"
  else
    echo "MONGO_IMAGE=$MONGO_IMAGE" >> "$INFRA/.env"
  fi
}

# Leaked docker-proxy processes run as root; reach root via sudo or a --pid=host container.
_as_host_root() {
  sudo -n true 2>/dev/null && { sudo -n sh -c "$1"; return; }
  local img
  for img in alpine:latest busybox:latest nginx:alpine; do
    docker image inspect "$img" >/dev/null 2>&1 || continue
    docker run --rm --pid=host --privileged --entrypoint sh "$img" -c "$1"
    return
  done
  docker run --rm --pid=host --privileged --entrypoint sh alpine:latest -c "$1"
}

# Reaps a docker-proxy leaked by a failed container network setup (Docker bug).
reap_stale_port_proxy() {
  local port="$1" own="${2:-}"

  ss -ltnH 2>/dev/null | awk '{print $4}' | grep -qE "[:.]${port}\$" || return 0

  local owners
  owners="$(docker ps -q | xargs -r docker inspect --format \
    '{{range .NetworkSettings.Networks}}{{.IPAddress}}|{{index $.Config.Labels "com.docker.compose.project"}}|{{$.Name}}
{{end}}' 2>/dev/null | grep -v '^|')"

  local found=0 stale=0 ours=0 pid cip owner proj name reported=""
  while read -r pid cip; do
    [ -n "$pid" ] || continue
    found=1
    owner="$(printf '%s\n' "$owners" | grep -m1 "^${cip}|" || true)"
    if [ -n "$owner" ]; then
      case " $reported " in *" $cip "*) continue ;; esac
      reported="$reported $cip"
      proj="$(printf '%s' "$owner" | cut -d'|' -f2)"
      name="$(printf '%s' "$owner" | cut -d'|' -f3 | sed 's|^/||')"
      if [ -n "$own" ] && [ "$proj" = "$own" ]; then
        log_info "Port $port is held by ${name} (this stack) — compose will replace it."
        ours=1
        continue
      fi
      log_warn "Port $port is held by ${name} (project ${proj:-none}), a live container — not touching it."
      continue
    fi
    log_warn "Reaping leaked docker-proxy (pid $pid) holding port $port for $cip — no such container."
    # Re-check cmdline at kill time to guard against PID reuse.
    _as_host_root "
      cmd=\$(tr '\\0' ' ' < /proc/$pid/cmdline 2>/dev/null)
      case \"\$cmd\" in
        *docker-proxy*'-host-port $port '*'-container-ip $cip '*) ;;
        *) echo 'cmdline no longer matches — refusing to kill'; exit 1 ;;
      esac
      kill $pid 2>/dev/null
      i=0; while [ -d /proc/$pid ] && [ \$i -lt 10 ]; do sleep 0.5; i=\$((i+1)); done
      [ -d /proc/$pid ] && kill -9 $pid 2>/dev/null
      sleep 0.5
      [ -d /proc/$pid ] && exit 1
      exit 0
    " >/dev/null 2>&1 && stale=1 || log_warn "Could not reap pid $pid."
  done < <(ps -eo pid=,args= \
             | grep -E "docker-proxy .*-host-port ${port}( |\$)" \
             | grep -v grep \
             | sed -nE 's/^[[:space:]]*([0-9]+).*-container-ip ([0-9.]+).*/\1 \2/p')

  if [ "$ours" = 1 ]; then return 0; fi

  if [ "$stale" = 1 ]; then sleep 1; fi
  if ss -ltnH 2>/dev/null | awk '{print $4}' | grep -qE "[:.]${port}\$"; then
    if [ "$found" = 0 ]; then
      log_warn "Port $port is in use by something that is not a docker-proxy:"
      ss -ltnp 2>/dev/null | grep -E "[:.]${port}[[:space:]]" | sed 's/^/    /' || true
    fi
    return 1
  fi
  log_info "Port $port is free."
  return 0
}

ensure_port_free() {
  local color="$1" port; port="$(color_port "$color")"
  reap_stale_port_proxy "$port" "nucleus-${color}" && return 0
  die "Debug port $port for the ${color} stack is still in use (see above) — refusing to start ${color}.
    Nothing was changed; the active color is still serving.
    Identify the holder with:  sudo ss -ltnp | grep :$port"
}

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
  [ -f "$EDGE_ACTIVE" ] || printf 'set $active web-blue;\n' > "$EDGE_ACTIVE"
  log_info "Ensuring edge proxy is up…"
  dc_edge up -d || die "Edge proxy failed to start."
}

_cp_dist() {
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

health_probe() {
  local port; port="$(color_port "$1")"
  node -e "require('http').get('http://localhost:$port/api/auth/health',r=>process.exit(r.statusCode<500?0:1)).on('error',()=>process.exit(1))"
}

switch_traffic() {
  local color="$1"
  printf 'set $active web-%s;\n' "$color" > "$EDGE_ACTIVE"
  if ! dc_edge exec -T nginx nginx -t >/dev/null 2>&1; then
    die "Edge nginx rejected the config after switching to $color — traffic NOT switched."
  fi
  dc_edge exec -T nginx nginx -s reload \
    || die "Edge reload failed — traffic NOT switched to $color."
}

stop_stack() {
  local color="$1"
  [ -n "$color" ] || return 0
  log_info "Stopping $color stack (containers kept for rollback/debug)…"
  dc_stack "$color" stop || log_warn "Could not stop $color stack cleanly (continuing)."
}
