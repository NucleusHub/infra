package main

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

//go:embed fallback.html
var anchorFallbackHTML string

const defaultHost = "nucleus.olm-altair.ts.net"

const (
	dataNet = "nucleus-data-net"
	edgeNet = "nucleus-edge-net"
)

var dataServices = map[string]bool{"mongo": true, "redis": true, "minio": true}

// Floor keeps a slow server "starting" when all servers boot at once, so it doesn't abort the deploy.
const stackStartPeriodFloor = "120s"

func maxSeconds(v, floor string) string {
	sec := func(s string) int {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "s"))
		if err != nil || !strings.HasSuffix(s, "s") {
			return -1
		}
		return n
	}
	if sec(v) > sec(floor) {
		return v
	}
	return floor
}

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

type modules struct {
	plugins       bool
	pluginIDs     []string
	pluginRuntime bool
	widgets       bool
}

// Docker recreates missing bind-mount sources as empty dirs, so require real content.
func optionalModules(p paths) modules {
	manifests, _ := filepath.Glob(filepath.Join(p.root, "plugins", "*", "nucleus.plugin.json"))
	var ids []string
	for _, m := range manifests {
		ids = append(ids, filepath.Base(filepath.Dir(m)))
	}
	sort.Strings(ids)
	return modules{
		plugins:       len(ids) > 0,
		pluginIDs:     ids,
		pluginRuntime: exists(filepath.Join(p.root, "plugin-runtime", "Dockerfile")),
		widgets:       exists(filepath.Join(p.widgets, "package.json")),
	}
}

func pluginsLocation(p paths) string {
	if !optionalModules(p).pluginRuntime {
		return `
    location /api/plugins {
        # Plugin runtime not installed — report an empty plugin registry.
        default_type application/json;
        return 200 '{"plugins":[]}';
    }`
	}
	return `
    location /api/plugins {
        # Plugin runtime — discovery/metadata only. Variable + resolver so nginx
        # re-resolves its IP per request (survives container restarts).
        set $plugins_upstream plugin-runtime:4100;
        proxy_pass http://$plugins_upstream;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
    }`
}

func runGenerate(p paths) error {
	apps, widgets, err := discover(p)
	if err != nil {
		return err
	}
	hubLibs, err := findHubLibraries(p)
	if err != nil {
		return err
	}
	syncHubLibLinks(p, hubLibs)

	fmt.Printf("Apps:    %s\n", joinIDsOr(apps, "none"))
	fmt.Printf("Widgets: %s\n", joinIDsOr(widgets, "none"))
	fmt.Printf("Hub libs: %s\n", joinLibsOr(hubLibs, "none"))

	validate(p, apps, widgets)

	host := nucleusHost(p)
	withHost := func(s string) string { return strings.ReplaceAll(s, defaultHost, host) }

	writes := []struct {
		path, content, log string
	}{
		// Mount config dirs, not files: a single-file bind mount pins the old inode across reloads.
		{filepath.Join(p.infra, "nginx", "conf.d", "default.conf"), withHost(generateNginx(p, apps, widgets)), "→ nginx/conf.d/default.conf"},
		{filepath.Join(p.infra, "docker-compose.override.yml"), generateOverride(p, apps, widgets, hubLibs), "→ docker-compose.override.yml"},
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

	// Never clobber the live switch state on regenerate.
	activeInc := filepath.Join(p.infra, "nginx", "edge", "active.inc")
	if !exists(activeInc) {
		if err := os.WriteFile(activeInc, []byte("set $active web-blue;\n"), 0o644); err != nil {
			return err
		}
		fmt.Println("→ nginx/edge/active.inc (seeded: blue)")
	}
	return nil
}

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
` + pluginsLocation(p) + `

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
        # Variable + resolver so nginx re-resolves the hub per request — a
        # recreated hub container (new IP) is picked up without an nginx reload.
        set $hub_upstream hub:5174;
        proxy_pass http://$hub_upstream;
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
` + pluginsLocation(p) + `

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

// *.inc so the default conf.d/*.conf include doesn't load it at http scope.
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

func relFromRoot(p paths, dir string) string {
	rel := strings.TrimPrefix(dir, p.root+string(filepath.Separator))
	rel = filepath.ToSlash(rel)
	return "../" + rel
}

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
	startPeriod = maxSeconds(startPeriod, stackStartPeriodFloor)

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

func generateStackCompose(p paths, apps, widgets []*Manifest) string {
	mods := optionalModules(p)
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

	// Bind mounts resolve against infra/ (the compose file's dir): ./.stacks, not ../.
	webVols := []string{
		"      - ./.stacks/${NUCLEUS_COLOR}/srv/hub:/srv/hub:ro",
		"      - ./.stacks/${NUCLEUS_COLOR}/srv/static:/srv/static:ro",
		"      - ../state:/srv/state:ro",
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
      # 127.0.0.1, not localhost: in the alpine image localhost resolves to ::1
      # first, where nginx isn't listening (IPv4 only) — busybox wget would then
      # fail with "connection refused" and the web would be perma-unhealthy.
      test: ["CMD", "wget", "-q", "-O", "/dev/null", "http://127.0.0.1/"]
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
`)
	if mods.widgets {
		out.WriteString("      - ../widgets:/widgets:ro\n")
	}
	out.WriteString(`      - ./nucleus.json:/nucleus.json:ro
    networks:
      - internal

`)
	if mods.pluginRuntime {
		out.WriteString(pluginRuntimeService("    networks:\n      - internal\n", mods))
		out.WriteString("\n")
	}
	out.WriteString(strings.Join(serverBlocks, "\n"))
	out.WriteString("\n")

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

type hubLib struct {
	id  string
	rel string
}

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
		// An empty client skeleton recreated by Docker must not resurrect a removed hub library.
		if isIgnored(appDir) || !isApp(appDir) || !exists(clientDir) || exists(filepath.Join(clientDir, "vite.config.js")) {
			continue
		}
		libs = append(libs, hubLib{id: name, rel: "../apps/" + name + "/client"})
	}
	return libs, nil
}

func pluginRuntimeService(extra string, mods modules) string {
	var b strings.Builder
	b.WriteString("  plugin-runtime:\n    build:\n      context: ../plugin-runtime\n    restart: unless-stopped\n")
	b.WriteString(labelsBlock("plugin-runtime", "", nil))
	b.WriteString("    environment:\n      PORT: 4100\n      PLUGINS_DIR: /plugins\n      NUCLEUS_MANIFEST: /nucleus.json\n")
	b.WriteString("    volumes:\n")
	if mods.plugins {
		b.WriteString("      - ../plugins:/plugins:ro\n")
	}
	b.WriteString("      - ./nucleus.json:/nucleus.json:ro\n")
	b.WriteString(extra)
	return b.String()
}

func isApp(dir string) bool { return exists(filepath.Join(dir, "nucleus.app.json")) }

func syncHubLibLinks(p paths, libs []hubLib) {
	dir := filepath.Join(p.root, "hub", "libs")
	if !exists(filepath.Join(p.root, "hub")) {
		return
	}
	_ = os.MkdirAll(dir, 0o755)
	want := map[string]bool{}
	for _, l := range libs {
		want[l.id] = true
		forceSymlink(filepath.Join(p.apps, l.id, "client"), filepath.Join(dir, l.id))
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Type()&os.ModeSymlink != 0 && !want[e.Name()] {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

func generateOverride(p paths, apps, widgets []*Manifest, hubLibs []hubLib) string {
	mods := optionalModules(p)
	var includes []string
	for _, m := range append(append([]*Manifest{}, apps...), widgets...) {
		if exists(filepath.Join(m.dir, "docker-compose.app.yml")) {
			rel := relFromRoot(p, m.dir)
			includes = append(includes, fmt.Sprintf("  - path: %s/docker-compose.app.yml", rel))
		}
	}

	var hubVolumes []string
	for _, l := range hubLibs {
		hubVolumes = append(hubVolumes, fmt.Sprintf("      - %s:/app/libs/%s:ro", l.rel, l.id))
	}

	if mods.widgets {
		hubVolumes = append(hubVolumes, "      - ../widgets:/app/widgets:ro")
	}
	if mods.plugins {
		hubVolumes = append(hubVolumes, "      - ../plugins:/app/plugins:ro")
	}

	var services []string
	if len(hubVolumes) > 0 {
		services = append(services, "  hub:\n    volumes:\n"+strings.Join(hubVolumes, "\n")+"\n")
	}
	if mods.widgets {
		services = append(services, "  registry:\n    volumes:\n      - ../widgets:/widgets:ro\n")
	}
	if mods.plugins {
		// NUCLEUS_PLUGINS changes the config so `up` recreates it; node --watch misses new plugins.
		services = append(services, fmt.Sprintf("  auth-server:\n    environment:\n      NUCLEUS_PLUGINS: %q\n    volumes:\n      - ../plugins:/app/plugins:ro\n",
			strings.Join(mods.pluginIDs, ",")))
	}
	if mods.pluginRuntime {
		services = append(services, "  nginx:\n    depends_on:\n      - plugin-runtime\n")
		services = append(services, pluginRuntimeService("", mods))
	}

	parts := []string{"# GENERATED by 'nucleus generate' (infra/tool) — do not edit manually"}
	if len(includes) > 0 {
		parts = append(parts, "include:\n"+strings.Join(includes, "\n"))
	}
	if len(services) > 0 {
		parts = append(parts, "services:\n"+strings.TrimRight(strings.Join(services, "\n"), "\n"))
	}
	return strings.Join(parts, "\n\n") + "\n"
}
