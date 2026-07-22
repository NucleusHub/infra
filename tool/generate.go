package main

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// anchorFallbackHTML is the degraded-mode page served when an upstream is
// unreachable. Embedded verbatim (it contains literal double quotes but no
// single quotes, backticks, or `$`) so it stays byte-identical to generate.js.
//
//go:embed fallback.html
var anchorFallbackHTML string

// defaultHost is the public hostname baked into the templates. It is replaced
// with NUCLEUS_HOST wherever it appears, so the literal here is just the
// fallback when nothing is configured.
const defaultHost = "nucleus.olm-altair.ts.net"

// Blue/green networking. The two networks are external (created once by the
// deploy bootstrap) so the always-on data stack, both color stacks, and the
// edge proxy can all attach to them and resolve each other by service name /
// alias across compose projects.
const (
	dataNet = "nucleus-data-net" // servers ↔ mongo/redis/minio
	edgeNet = "nucleus-edge-net" // edge proxy ↔ each color's web nginx
)

// dataServices are the stateful backing services. They live in the always-on
// nucleus-data stack (shared by both colors), so a color stack never declares a
// depends_on against them — it reaches them over dataNet instead.
var dataServices = map[string]bool{"mongo": true, "redis": true, "minio": true}

// nucleusHost resolves the public hostname: the NUCLEUS_HOST environment
// variable (set by loadEnv in build/dev), else the value in infra/.env, else
// the default. Single source of truth for the nginx server_name and the links.
func nucleusHost(p paths) string {
	if h := strings.TrimSpace(os.Getenv("NUCLEUS_HOST")); h != "" {
		return h
	}
	if h := envFileValue(filepath.Join(p.infra, ".env"), "NUCLEUS_HOST"); h != "" {
		return h
	}
	return defaultHost
}

func fallbackLocation() string {
	return `
    # Degraded-mode page (manual link to Anchor; never auto-routed).
    location @anchor_fallback {
        default_type text/html;
        return 503 '` + anchorFallbackHTML + `';
    }`
}

// runGenerate is the entry point for `nucleus generate`. It mirrors the main
// body of generate.js: discover, validate, then emit the four config files.
func runGenerate(p paths) error {
	apps, widgets, err := discover(p)
	if err != nil {
		return err
	}
	hubLibs, err := findHubLibraries(p)
	if err != nil {
		return err
	}

	fmt.Printf("Apps:    %s\n", joinIDsOr(apps, "none"))
	fmt.Printf("Widgets: %s\n", joinIDsOr(widgets, "none"))
	fmt.Printf("Hub libs: %s\n", joinLibsOr(hubLibs, "none"))

	validate(p, apps, widgets) // exits 1 on manifest errors

	// The nginx templates carry the public host (server_name + HTTPS redirect);
	// swap the placeholder default for the configured NUCLEUS_HOST.
	host := nucleusHost(p)
	withHost := func(s string) string { return strings.ReplaceAll(s, defaultHost, host) }

	writes := []struct {
		path, content, log string
	}{
		// nginx configs go into per-env DIRECTORIES (conf.d/ dev, prod/ prod) that
		// are bind-mounted as a whole. Mounting the directory (not the single file)
		// means a regenerated config — a new inode — is visible to a running nginx,
		// so `nginx -s reload` applies it without recreating the container. A
		// single-file bind mount pins the container to the original inode, which is
		// why a stale config survived reloads until now.
		{filepath.Join(p.infra, "nginx", "conf.d", "default.conf"), withHost(generateNginx(p, apps, widgets)), "→ nginx/conf.d/default.conf"},
		{filepath.Join(p.infra, "docker-compose.override.yml"), generateOverride(p, apps, widgets, hubLibs), "→ docker-compose.override.yml"},
		// Blue/green production: an always-on shared data stack, a color-swappable
		// app stack (run with -p nucleus-blue|nucleus-green), and an always-on edge
		// proxy that owns 80/443+TLS and forwards to whichever color is active.
		{filepath.Join(p.infra, "nginx", "stack", "default.conf"), withHost(generateStackNginx(p, apps, widgets)), "→ nginx/stack/default.conf"},
		{filepath.Join(p.infra, "nginx", "edge", "default.conf"), withHost(generateEdgeNginx(p)), "→ nginx/edge/default.conf"},
		{filepath.Join(p.infra, "docker-compose.data.yml"), generateDataCompose(p, apps, widgets), "→ docker-compose.data.yml"},
		{filepath.Join(p.infra, "docker-compose.stack.yml"), generateStackCompose(p, apps, widgets), "→ docker-compose.stack.yml"},
		{filepath.Join(p.infra, "docker-compose.edge.yml"), generateEdgeCompose(p), "→ docker-compose.edge.yml"},
	}
	for _, w := range writes {
		if err := os.MkdirAll(filepath.Dir(w.path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(w.path, []byte(w.content), 0o644); err != nil {
			return err
		}
		fmt.Println(w.log)
	}

	// active.inc is the edge proxy's live switch state (set $active web-<color>),
	// owned by the deploy/switch helper. Seed a safe default only when it doesn't
	// exist yet — never clobber the running target on a regenerate.
	activeInc := filepath.Join(p.infra, "nginx", "edge", "active.inc")
	if !exists(activeInc) {
		if err := os.WriteFile(activeInc, []byte("set $active web-blue;\n"), 0o644); err != nil {
			return err
		}
		fmt.Println("→ nginx/edge/active.inc (seeded: blue)")
	}
	return nil
}

// discover reads app + widget manifests and drops any that collide with a core
// service name (e.g. a stale apps/auth) so we never emit duplicate keys.
func discover(p paths) (apps, widgets []*Manifest, err error) {
	apps, err = readManifests(p.apps, "nucleus.app.json")
	if err != nil {
		return nil, nil, err
	}
	widgets, err = readManifests(p.widgets, "nucleus.widget.json")
	if err != nil {
		return nil, nil, err
	}

	core := map[string]bool{}
	for _, c := range coreServices(p) {
		if c.Server != nil && c.Server.Service != "" {
			core[c.Server.Service] = true
		}
	}
	drop := func(list []*Manifest) []*Manifest {
		out := list[:0]
		for _, m := range list {
			if m.Server != nil && core[m.Server.Service] {
				id := m.ID
				if id == "" {
					id = m.dir
				}
				fmt.Printf("  Skipping %s: '%s' is provided by core, not apps/widgets\n", id, m.Server.Service)
				continue
			}
			out = append(out, m)
		}
		return out
	}
	return drop(apps), drop(widgets), nil
}

func joinIDsOr(ms []*Manifest, fallback string) string {
	if len(ms) == 0 {
		return fallback
	}
	ids := make([]string, len(ms))
	for i, m := range ms {
		ids[i] = m.ID
	}
	return strings.Join(ids, ", ")
}

func joinLibsOr(libs []hubLib, fallback string) string {
	if len(libs) == 0 {
		return fallback
	}
	ids := make([]string, len(libs))
	for i, l := range libs {
		ids[i] = l.id
	}
	return strings.Join(ids, ", ")
}

// ── Nginx ────────────────────────────────────────────────────────────────────

func nginxLocation(r Route) string {
	ws := ""
	if r.Websocket {
		ws = `
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";`
	}

	fbBlock := ""
	if r.fallback {
		fbBlock = `
        error_page 502 503 504 = @anchor_fallback;`
	}

	corsBlock := ""
	if r.Cors {
		corsBlock = `
        if ($request_method = 'OPTIONS') {
            add_header 'Access-Control-Allow-Origin' '*' always;
            add_header 'Access-Control-Allow-Methods' 'GET, PUT, DELETE, HEAD, OPTIONS' always;
            add_header 'Access-Control-Allow-Headers' '*' always;
            add_header 'Access-Control-Max-Age' '3000' always;
            add_header 'Content-Length' '0' always;
            return 204;
        }
        add_header 'Access-Control-Allow-Origin' '*' always;`
	}

	sizeBlock := ""
	if r.MaxBodySize != nil {
		sizeBlock = fmt.Sprintf(`
        client_max_body_size %s;
        proxy_request_buffering off;`, *r.MaxBodySize)
	}

	return fmt.Sprintf(`
    location %s {%s%s%s
        set $upstream %s;
        proxy_pass http://$upstream;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;%s
    }`, r.Path, corsBlock, sizeBlock, fbBlock, r.Upstream, ws)
}

// collectRoutes gathers all routes across apps+widgets+core, tagging the SPA
// route for fallback, then sorts longest-path-first (stable, like JS sort).
func collectRoutes(p paths, apps, widgets []*Manifest, tagFallback bool) []Route {
	all := append(append(append([]*Manifest{}, apps...), widgets...), coreServices(p)...)
	var routes []Route
	for _, m := range all {
		for _, r := range m.routes() {
			r.fallback = tagFallback && m.Route != "" && r.Path == m.Route
			r.manifestRoute = m.Route
			routes = append(routes, r)
		}
	}
	sortByPathLenDesc(routes)
	return routes
}

// sortByPathLenDesc sorts longest path first. JS Array.sort is not guaranteed
// stable across all lengths, but V8's is stable; sort.SliceStable matches it.
func sortByPathLenDesc(routes []Route) {
	sort.SliceStable(routes, func(i, j int) bool {
		return len(routes[j].Path) < len(routes[i].Path)
	})
}

func generateNginx(p paths, apps, widgets []*Manifest) string {
	routes := collectRoutes(p, apps, widgets, true)
	var blocks []string
	for _, r := range routes {
		blocks = append(blocks, nginxLocation(r))
	}

	return `# Redirect nucleus.olm-altair.ts.net HTTP traffic to HTTPS (external access with TLS cert)
server {
    listen 80;
    server_name nucleus.olm-altair.ts.net;
    return 301 https://nucleus.olm-altair.ts.net$request_uri;
}

# Main server — HTTPS for nucleus.olm-altair.ts.net, plain HTTP for localhost / IP access
server {
    listen 80 default_server;
    listen 443 ssl;
    server_name _;

    ssl_certificate /etc/ssl/certs/nucleus.crt;
    ssl_certificate_key /etc/ssl/private/nucleus.key;

    # Docker's internal DNS — lets nginx start even when optional services aren't up yet
    resolver 127.0.0.11 valid=10s ipv6=off;

    location /api/registry {
        # Variable + resolver so nginx re-resolves the registry's IP per request
        # (survives registry container restarts without an nginx restart).
        set $registry_upstream registry:4000;
        proxy_pass http://$registry_upstream;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
    }

    location /api/plugins {
        # Plugin runtime — discovery/metadata only. Variable + resolver so nginx
        # re-resolves its IP per request (survives container restarts).
        set $plugins_upstream plugin-runtime:4100;
        proxy_pass http://$plugins_upstream;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
    }

    # Maintenance flag — a static file written by infra/maintenance and served by
    # nginx (not an app server), so it stays reachable while apps are rebuilt.
    # Absent → 204, which the client treats as "not in maintenance".
    location = /maintenance.json {
        root /srv/state;
        add_header Cache-Control "no-store" always;
        try_files /maintenance.json =204;
    }
` + strings.Join(blocks, "\n") + `

    location ~* ^(/watchlist)?/favicon\.ico$ {
        root /usr/share/nginx/static;
        try_files /favicon.ico =404;
    }

    location / {
        proxy_pass http://hub:5174;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_set_header X-Real-IP $remote_addr;
        # If the hub dev server is down, show the Anchor recovery page.
        proxy_intercept_errors on;
        error_page 502 503 504 = @anchor_fallback;
    }
` + fallbackLocation() + `
}
`
}

// ── Stack (color) Nginx ──────────────────────────────────────────────────────

// generateStackNginx emits the per-color web nginx config. It is identical to
// the old prod nginx EXCEPT it terminates no TLS and does no HTTP→HTTPS
// redirect — the always-on edge proxy owns 80/443+TLS and forwards plain HTTP to
// this server. Route upstreams are Docker service names, which resolve to *this
// color's* servers inside the color's own compose project, so the same config
// serves both blue and green unchanged.
func generateStackNginx(p paths, apps, widgets []*Manifest) string {
	all := append(append(append([]*Manifest{}, apps...), widgets...), coreServices(p)...)

	var apiRoutes, spaRoutes []Route
	for _, m := range all {
		for _, r := range m.routes() {
			r.manifestRoute = m.Route
			if m.Route != "" && r.Path == m.Route {
				spaRoutes = append(spaRoutes, r)
			} else {
				apiRoutes = append(apiRoutes, r)
			}
		}
	}
	sortByPathLenDesc(apiRoutes)
	sortByPathLenDesc(spaRoutes)

	var apiBlocks []string
	for _, r := range apiRoutes {
		apiBlocks = append(apiBlocks, nginxLocation(r))
	}

	var spaBlocks []string
	for _, r := range spaRoutes {
		dir := strings.TrimPrefix(r.Path, "/")
		spaBlocks = append(spaBlocks, fmt.Sprintf(`
    location %s {
        root /srv;
        try_files $uri $uri/ /%s/index.html;
    }`, r.Path, dir))
	}

	return `# Per-color web server. TLS + HTTP→HTTPS redirect are handled by the edge proxy;
# this server listens plain HTTP and is reached over the edge network (and a
# debug host port). Upstreams resolve to this color's own servers.
server {
    listen 80 default_server;
    server_name _;

    # Docker's internal DNS — lets nginx start even when optional services aren't up yet
    resolver 127.0.0.11 valid=10s ipv6=off;

    location /api/registry {
        # Variable + resolver so nginx re-resolves the registry's IP per request
        # (survives registry container restarts without an nginx restart).
        set $registry_upstream registry:4000;
        proxy_pass http://$registry_upstream;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
    }

    location /api/plugins {
        # Plugin runtime — discovery/metadata only. Variable + resolver so nginx
        # re-resolves its IP per request (survives container restarts).
        set $plugins_upstream plugin-runtime:4100;
        proxy_pass http://$plugins_upstream;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
    }

    # Maintenance flag — a static file written by infra/maintenance and served by
    # nginx (not an app server), so it stays reachable while apps are rebuilt.
    # Absent → 204, which the client treats as "not in maintenance".
    location = /maintenance.json {
        root /srv/state;
        add_header Cache-Control "no-store" always;
        try_files /maintenance.json =204;
    }
` + strings.Join(apiBlocks, "\n") + `
` + strings.Join(spaBlocks, "\n") + `

    location ~* ^(/watchlist)?/favicon\.ico$ {
        root /srv/static;
        try_files /favicon.ico =404;
    }

    # Hub SPA — catch-all (pre-built static files in prod)
    location / {
        root /srv/hub;
        try_files $uri $uri/ /index.html;
        error_page 502 503 504 = @anchor_fallback;
    }
` + fallbackLocation() + `
}
`
}

// ── Edge Nginx ───────────────────────────────────────────────────────────────

// generateEdgeNginx emits the always-on front proxy config: it owns 80/443+TLS
// and blindly forwards everything to the active color ($active, set by the
// separately-mounted active.inc). Switching colors = rewrite active.inc and
// `+"`nginx -s reload`"+` — no restart, no config regeneration.
//
// active.inc is deliberately named *.inc (not *.conf) so nginx's default
// `+"`include /etc/nginx/conf.d/*.conf`"+` does NOT load it at http scope; it is
// pulled in only by the explicit include inside the server block below.
func generateEdgeNginx(p paths) string {
	return `# GENERATED by 'nucleus generate' (infra/tool) — do not edit manually
# Blue/green edge proxy: TLS terminator on 80/443, forwards to the active color.

# Websocket upgrade passthrough (Echo). '' → keep-alive close for normal requests.
map $http_upgrade $connection_upgrade {
    default upgrade;
    ''      close;
}

# Redirect public-host HTTP → HTTPS (external access with TLS cert)
server {
    listen 80;
    server_name nucleus.olm-altair.ts.net;
    return 301 https://nucleus.olm-altair.ts.net$request_uri;
}

# Main server — HTTPS for the public host, plain HTTP for localhost / IP access
server {
    listen 80 default_server;
    listen 443 ssl;
    server_name _;

    ssl_certificate /etc/ssl/certs/nucleus.crt;
    ssl_certificate_key /etc/ssl/private/nucleus.key;

    # Docker DNS so the color alias re-resolves per request — a recreated color
    # container is picked up without restarting the edge.
    resolver 127.0.0.11 valid=10s ipv6=off;

    # Blind pass-through: no body-size limit, no request buffering, websocket-ready.
    client_max_body_size 0;
    proxy_request_buffering off;

    # set $active web-blue|web-green — the ONLY thing a traffic switch changes.
    include /etc/nginx/conf.d/active.inc;

    location / {
        proxy_pass http://$active;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $connection_upgrade;
        proxy_read_timeout 3600s;
        proxy_send_timeout 3600s;
        # Active color unreachable → the Anchor degraded-mode page.
        error_page 502 503 504 = @anchor_fallback;
    }
` + fallbackLocation() + `
}
`
}

// ── Prod Compose ─────────────────────────────────────────────────────────────

func labelsBlock(role, app string, depends []string) string {
	var b strings.Builder
	b.WriteString("    labels:\n")
	b.WriteString("      nucleus.managed: \"true\"\n")
	b.WriteString("      nucleus.stack: \"nucleus\"\n")
	b.WriteString(fmt.Sprintf("      nucleus.role: \"%s\"\n", role))
	if app != "" {
		b.WriteString(fmt.Sprintf("      nucleus.app: \"%s\"\n", app))
	}
	if len(depends) > 0 {
		b.WriteString(fmt.Sprintf("      nucleus.depends: \"%s\"\n", strings.Join(depends, ",")))
	}
	return b.String()
}

// relFromRoot turns an absolute manifest dir into the `../<rel>` path used in
// compose contexts and volume mounts (forward slashes, like JS).
func relFromRoot(p paths, dir string) string {
	rel := strings.TrimPrefix(dir, p.root+string(filepath.Separator))
	rel = filepath.ToSlash(rel)
	return "../" + rel
}

// stackServerBlock emits one app/auth/widget server for the color stack. It is
// the old prodServerBlock with two blue/green changes: (1) depends_on against
// stateful data services is dropped — mongo/redis/minio live in the always-on
// nucleus-data stack, reached over the shared external network, not within this
// project; (2) the service is attached to the internal + data networks.
func stackServerBlock(p paths, m *Manifest) string {
	s := m.Server
	relDir := relFromRoot(p, m.dir)
	buildContext := s.Context
	if buildContext == "" {
		buildContext = relDir + "/server"
	}
	healthURL := fmt.Sprintf("http://localhost:%d%s", s.Port, s.HealthEndpoint)
	startPeriod := s.StartPeriod
	if startPeriod == "" {
		startPeriod = "10s"
	}

	envLines := []string{fmt.Sprintf("      PORT: %d", s.Port)}
	if s.Env != nil {
		for _, k := range s.Env.Keys {
			envLines = append(envLines, fmt.Sprintf("      %s: %s", k, s.Env.Vals[k]))
		}
	}

	var volumeLines []string
	for _, v := range s.NamedVolumes {
		volumeLines = append(volumeLines, "      - "+v)
	}
	for _, v := range s.BindMounts {
		volumeLines = append(volumeLines, "      - "+v)
	}

	// Only in-project dependencies belong here; data services are external.
	var dependsLines []string
	for _, d := range s.Depends {
		if dataServices[d] {
			continue
		}
		dependsLines = append(dependsLines, fmt.Sprintf("      %s:\n        condition: service_healthy", d))
	}

	role := m.role
	if role == "" {
		role = "app-server"
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("  %s:\n", s.Service))
	b.WriteString(fmt.Sprintf("    build:\n      context: %s\n", buildContext))
	b.WriteString("    restart: unless-stopped\n")
	b.WriteString(labelsBlock(role, m.ID, s.Depends))
	b.WriteString("    command: [\"node\", \"index.js\"]\n")
	b.WriteString(fmt.Sprintf("    environment:\n%s\n", strings.Join(envLines, "\n")))
	if len(volumeLines) > 0 {
		b.WriteString(fmt.Sprintf("    volumes:\n%s\n", strings.Join(volumeLines, "\n")))
	}
	if len(dependsLines) > 0 {
		b.WriteString(fmt.Sprintf("    depends_on:\n%s\n", strings.Join(dependsLines, "\n")))
	}
	b.WriteString("    networks:\n      - internal\n      - data\n")
	b.WriteString("    healthcheck:\n")
	b.WriteString(fmt.Sprintf("      test: [\"CMD\", \"node\", \"-e\", \"require('http').get('%s',r=>process.exit(r.statusCode<500?0:1)).on('error',()=>process.exit(1))\"]\n", healthURL))
	b.WriteString("      interval: 5s\n")
	b.WriteString("      timeout: 5s\n")
	b.WriteString("      retries: 10\n")
	b.WriteString(fmt.Sprintf("      start_period: %s\n", startPeriod))
	return b.String()
}

// backingNeeds reports which stateful services any server depends on. Shared by
// the data-stack generator (which services to run) and kept in one place.
func backingNeeds(p paths, apps, widgets []*Manifest) (mongo, minio, redis bool) {
	all := append(append(append([]*Manifest{}, apps...), widgets...), coreServices(p)...)
	for _, m := range all {
		if m.Server == nil {
			continue
		}
		for _, d := range m.Server.Depends {
			switch d {
			case "mongo":
				mongo = true
			case "minio":
				minio = true
			case "redis":
				redis = true
			}
		}
	}
	return
}

// ── Data compose (always-on, shared by both colors) ──────────────────────────

// generateDataCompose emits the stateful backing stack. It runs once and stays
// up across deploys:
//
//	docker compose -p nucleus-data -f docker-compose.data.yml up -d
//
// Volumes are pinned to the original nucleus_* names so an existing install's
// data is reused verbatim (no migration). Services attach to the external
// dataNet under their plain service names, which is how color servers reach them.
func generateDataCompose(p paths, apps, widgets []*Manifest) string {
	needsMongo, needsMinio, needsRedis := backingNeeds(p, apps, widgets)

	var out strings.Builder
	out.WriteString(`# GENERATED by 'nucleus generate' (infra/tool) — do not edit manually
# Always-on shared data stack (mongo/redis/minio). Volumes are pinned to the
# original nucleus_* names so existing data is reused. Bring up with:
#   docker compose -p nucleus-data -f docker-compose.data.yml up -d
name: nucleus-data

services:
`)

	if needsMongo {
		out.WriteString(`  mongo:
    image: ${MONGO_IMAGE:-mongo:7}
    restart: unless-stopped
`)
		out.WriteString(labelsBlock("database", "", nil))
		out.WriteString(`    networks:
      - data
    volumes:
      - mongo_data:/data/db
    healthcheck:
      test: ["CMD-SHELL", "mongosh --eval 'db.adminCommand({ping:1})' --quiet 2>/dev/null || mongo --eval 'db.adminCommand({ping:1})' --quiet"]
      interval: 5s
      timeout: 5s
      retries: 10
      start_period: 10s

`)
	}

	if needsRedis {
		out.WriteString(`  redis:
    image: redis:7-alpine
    restart: unless-stopped
`)
		out.WriteString(labelsBlock("cache", "", nil))
		out.WriteString(`    command: ["redis-server", "--appendonly", "no", "--save", ""]
    networks:
      - data
    volumes:
      - redis_data:/data
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 5s
      timeout: 3s
      retries: 10
      start_period: 5s

`)
	}

	if needsMinio {
		out.WriteString(`  minio:
    image: minio/minio:latest
    command: server /data --console-address ":9001"
    restart: unless-stopped
`)
		out.WriteString(labelsBlock("object-store", "", nil))
		out.WriteString(`    networks:
      - data
    ports:
      - "9000:9000"
      - "9001:9001"
    environment:
      MINIO_ROOT_USER: ${MINIO_ACCESS_KEY:-nucleusadmin}
      MINIO_ROOT_PASSWORD: ${MINIO_SECRET_KEY:-nucleuschangeme}
      MINIO_SERVER_URL: ${MINIO_PUBLIC_URL:-http://localhost}
    volumes:
      - minio_data:/data
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:9000/minio/health/live"]
      interval: 5s
      timeout: 5s
      retries: 10
      start_period: 15s

`)
	}

	out.WriteString("networks:\n  data:\n    name: " + dataNet + "\n    external: true\n")

	out.WriteString("\nvolumes:\n")
	if needsMongo {
		out.WriteString("  mongo_data:\n    name: nucleus_mongo_data\n")
	}
	if needsMinio {
		out.WriteString("  minio_data:\n    name: nucleus_minio_data\n")
	}
	if needsRedis {
		out.WriteString("  redis_data:\n    name: nucleus_redis_data\n")
	}
	return out.String()
}

// ── Stack compose (blue/green color template) ────────────────────────────────

// generateStackCompose emits the color-swappable app stack: registry,
// plugin-runtime, every app/auth/widget server, and a per-color web nginx. It is
// parameterized by ${NUCLEUS_COLOR} (project suffix, network alias, snapshot
// dir) and ${NUCLEUS_WEB_PORT} (debug host port), so ONE file runs both colors:
//
//	NUCLEUS_COLOR=green NUCLEUS_WEB_PORT=8082 \
//	  docker compose -p nucleus-green -f docker-compose.stack.yml up -d --build
//
// 'web' serves this color's static snapshot (../.stacks/<color>/srv) and proxies
// /api/* to this color's servers; the edge proxy forwards production traffic to
// the web-<color> alias. Upload volumes are pinned to their original nucleus_*
// names and shared across colors (user uploads persist across deploys).
func generateStackCompose(p paths, apps, widgets []*Manifest) string {
	for _, m := range widgets {
		m.role = "widget-server"
	}
	all := append(append(append([]*Manifest{}, apps...), widgets...), coreServices(p)...)

	var withServers []*Manifest
	for _, m := range all {
		if m.Server != nil {
			withServers = append(withServers, m)
		}
	}

	// web nginx static mounts: hub + per-route app dist, from this color's snapshot.
	// Snapshots live under infra/.stacks (see snapshot_dist), and compose resolves
	// relative bind mounts against the compose file's dir (infra/), so these are
	// "./.stacks/…" — NOT "../" (that would point one level above infra/).
	webVols := []string{
		"      - ./.stacks/${NUCLEUS_COLOR}/srv/hub:/srv/hub:ro",
		"      - ./.stacks/${NUCLEUS_COLOR}/srv/static:/srv/static:ro",
		"      - ../state:/srv/state:ro", // maintenance.json flag (infra/maintenance)
	}
	for _, m := range all {
		if m.Route == "" {
			continue
		}
		dir := strings.TrimPrefix(m.Route, "/")
		webVols = append(webVols, fmt.Sprintf("      - ./.stacks/${NUCLEUS_COLOR}/srv/%s:/srv/%s:ro", dir, dir))
	}
	webVols = append(webVols, "      - ./nginx/stack:/etc/nginx/conf.d:ro")

	var webDepends []string
	for _, m := range withServers {
		webDepends = append(webDepends, fmt.Sprintf("      %s:\n        condition: service_healthy", m.Server.Service))
	}

	// Shared upload named volumes, pinned to the original nucleus_* names.
	var uploadVols []string
	seenVol := map[string]bool{}
	for _, m := range withServers {
		for _, v := range m.Server.NamedVolumes {
			name := strings.Split(v, ":")[0]
			if !seenVol[name] {
				seenVol[name] = true
				uploadVols = append(uploadVols, name)
			}
		}
	}

	var serverBlocks []string
	for _, m := range withServers {
		serverBlocks = append(serverBlocks, stackServerBlock(p, m))
	}

	var out strings.Builder
	out.WriteString(`# GENERATED by 'nucleus generate' (infra/tool) — do not edit manually
# Color-swappable app stack (blue/green). Run one color at a time with a distinct
# project name and debug port, e.g.:
#   NUCLEUS_COLOR=green NUCLEUS_WEB_PORT=8082 \
#     docker compose -p nucleus-green -f docker-compose.stack.yml up -d --build
name: nucleus-${NUCLEUS_COLOR}

services:
  web:
    image: nginx:alpine
    restart: unless-stopped
`)
	out.WriteString(labelsBlock("proxy", "", nil))
	out.WriteString(`    ports:
      - "${NUCLEUS_WEB_PORT}:80"
    volumes:
`)
	out.WriteString(strings.Join(webVols, "\n"))
	out.WriteString(`
    depends_on:
`)
	out.WriteString(strings.Join(webDepends, "\n"))
	out.WriteString(`
    networks:
      internal:
      data:
      edge:
        aliases:
          - web-${NUCLEUS_COLOR}
    healthcheck:
      test: ["CMD", "wget", "-q", "-O", "/dev/null", "http://localhost/"]
      interval: 5s
      timeout: 5s
      retries: 10
      start_period: 5s

  registry:
    build:
      context: ./registry
    restart: unless-stopped
`)
	out.WriteString(labelsBlock("registry", "", nil))
	out.WriteString(`    environment:
      PORT: 4000
      APPS_DIR: /apps
      WIDGETS_DIR: /widgets
      NUCLEUS_MANIFEST: /nucleus.json
    volumes:
      - ../apps:/apps:ro
      - ../widgets:/widgets:ro
      - ./nucleus.json:/nucleus.json:ro
    networks:
      - internal

  plugin-runtime:
    build:
      context: ../plugin-runtime
    restart: unless-stopped
`)
	out.WriteString(labelsBlock("plugin-runtime", "", nil))
	out.WriteString(`    environment:
      PORT: 4100
      PLUGINS_DIR: /plugins
      NUCLEUS_MANIFEST: /nucleus.json
    volumes:
      - ../plugins:/plugins:ro
      - ./nucleus.json:/nucleus.json:ro
    networks:
      - internal

`)
	out.WriteString(strings.Join(serverBlocks, "\n"))
	out.WriteString("\n")

	// Networks: internal (intra-color DNS: web ↔ servers), plus the two shared
	// external networks (data → mongo/redis/minio, edge → the edge proxy).
	out.WriteString("networks:\n")
	out.WriteString("  internal:\n    driver: bridge\n")
	out.WriteString("  data:\n    name: " + dataNet + "\n    external: true\n")
	out.WriteString("  edge:\n    name: " + edgeNet + "\n    external: true\n")

	if len(uploadVols) > 0 {
		out.WriteString("\nvolumes:\n")
		for _, v := range uploadVols {
			out.WriteString(fmt.Sprintf("  %s:\n    name: nucleus_%s\n", v, v))
		}
	}

	return out.String()
}

// ── Edge compose (always-on front proxy) ─────────────────────────────────────

// generateEdgeCompose emits the always-on edge proxy: one nginx binding 80/443,
// terminating TLS, and forwarding to the active color over edgeNet. Bring up with:
//
//	docker compose -p nucleus-edge -f docker-compose.edge.yml up -d
//
// The nginx/edge dir is mounted whole (not a single file) so a rewritten
// active.inc is a new inode a running nginx sees on `nginx -s reload`.
func generateEdgeCompose(p paths) string {
	var out strings.Builder
	out.WriteString(`# GENERATED by 'nucleus generate' (infra/tool) — do not edit manually
# Always-on edge proxy: TLS on 80/443, forwards to the active color (web-<color>).
name: nucleus-edge

services:
  nginx:
    image: nginx:alpine
    restart: unless-stopped
`)
	out.WriteString(labelsBlock("edge", "", nil))
	out.WriteString(`    ports:
      - "80:80"
      - "443:443"
    volumes:
      - /etc/ssl/certs/nucleus.crt:/etc/ssl/certs/nucleus.crt:ro
      - /etc/ssl/private/nucleus.key:/etc/ssl/private/nucleus.key:ro
      - ./nginx/edge:/etc/nginx/conf.d:ro
    networks:
      - edge

networks:
  edge:
    name: ` + edgeNet + `
    external: true
`)
	return out.String()
}

// ── Compose override ─────────────────────────────────────────────────────────

type hubLib struct {
	id  string
	rel string
}

// findHubLibraries finds apps with a client/ dir but no client/vite.config.js —
// hub libraries whose client/ is mounted into the hub container so the @<id>
// vite alias resolves.
func findHubLibraries(p paths) ([]hubLib, error) {
	if !exists(p.apps) {
		return nil, nil
	}
	entries, err := os.ReadDir(p.apps)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	var libs []hubLib
	for _, name := range names {
		appDir := filepath.Join(p.apps, name)
		fi, err := os.Stat(appDir)
		if err != nil || !fi.IsDir() {
			continue
		}
		clientDir := filepath.Join(appDir, "client")
		if isIgnored(appDir) || !exists(clientDir) || exists(filepath.Join(clientDir, "vite.config.js")) {
			continue
		}
		libs = append(libs, hubLib{id: name, rel: "../apps/" + name + "/client"})
	}
	return libs, nil
}

func generateOverride(p paths, apps, widgets []*Manifest, hubLibs []hubLib) string {
	var includes []string
	for _, m := range append(append([]*Manifest{}, apps...), widgets...) {
		if exists(filepath.Join(m.dir, "docker-compose.app.yml")) {
			rel := relFromRoot(p, m.dir)
			includes = append(includes, fmt.Sprintf("  - path: %s/docker-compose.app.yml", rel))
		}
	}

	var hubVolumes []string
	for _, l := range hubLibs {
		hubVolumes = append(hubVolumes, fmt.Sprintf("      - %s:/app/%s:ro", l.rel, l.id))
	}

	parts := []string{"# GENERATED by 'nucleus generate' (infra/tool) — do not edit manually"}
	if len(includes) > 0 {
		parts = append(parts, "include:\n"+strings.Join(includes, "\n"))
	}
	if len(hubVolumes) > 0 {
		parts = append(parts, "services:\n  hub:\n    volumes:\n"+strings.Join(hubVolumes, "\n"))
	}
	return strings.Join(parts, "\n\n") + "\n"
}
