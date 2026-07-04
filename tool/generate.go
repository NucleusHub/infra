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
		{filepath.Join(p.infra, "nginx", "nginx.conf"), withHost(generateNginx(p, apps, widgets)), "→ nginx/nginx.conf"},
		{filepath.Join(p.infra, "docker-compose.override.yml"), generateOverride(p, apps, widgets, hubLibs), "→ docker-compose.override.yml"},
		{filepath.Join(p.infra, "nginx", "nginx.prod.conf"), withHost(generateProdNginx(p, apps, widgets)), "→ nginx/nginx.prod.conf"},
		{filepath.Join(p.infra, "docker-compose.prod.yml"), generateProdCompose(p, apps, widgets), "→ docker-compose.prod.yml"},
	}
	for _, w := range writes {
		if err := os.WriteFile(w.path, []byte(w.content), 0o644); err != nil {
			return err
		}
		fmt.Println(w.log)
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

// ── Prod Nginx ───────────────────────────────────────────────────────────────

func generateProdNginx(p paths, apps, widgets []*Manifest) string {
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

func prodServerBlock(p paths, m *Manifest) string {
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

	var dependsLines []string
	for _, d := range s.Depends {
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
	b.WriteString("    healthcheck:\n")
	b.WriteString(fmt.Sprintf("      test: [\"CMD\", \"node\", \"-e\", \"require('http').get('%s',r=>process.exit(r.statusCode<500?0:1)).on('error',()=>process.exit(1))\"]\n", healthURL))
	b.WriteString("      interval: 5s\n")
	b.WriteString("      timeout: 5s\n")
	b.WriteString("      retries: 10\n")
	b.WriteString(fmt.Sprintf("      start_period: %s\n", startPeriod))
	return b.String()
}

func generateProdCompose(p paths, apps, widgets []*Manifest) string {
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

	dependsOn := func(svc string) bool {
		for _, m := range withServers {
			for _, d := range m.Server.Depends {
				if d == svc {
					return true
				}
			}
		}
		return false
	}
	needsMongo := dependsOn("mongo")
	needsMinio := dependsOn("minio")
	needsRedis := dependsOn("redis")

	// Nginx read-only mounts for each standalone app's pre-built dist.
	nginxDistVols := []string{
		"      - ../hub/dist:/srv/hub:ro",
		"      - ../hub/public:/srv/static:ro",
		"      - ../state:/srv/state:ro", // maintenance.json flag (infra/maintenance)
	}
	for _, m := range all {
		if m.Route == "" {
			continue
		}
		rel := relFromRoot(p, m.dir)
		dir := strings.TrimPrefix(m.Route, "/")
		nginxDistVols = append(nginxDistVols, fmt.Sprintf("      - %s/client/dist:/srv/%s:ro", rel, dir))
	}

	nginxDependsLines := []string{"      registry:\n        condition: service_started"}
	for _, m := range withServers {
		nginxDependsLines = append(nginxDependsLines, fmt.Sprintf("      %s:\n        condition: service_healthy", m.Server.Service))
	}

	// Named volumes — insertion-ordered set (mongo, minio, redis, then declared).
	var namedVols []string
	seenVol := map[string]bool{}
	addVol := func(v string) {
		if !seenVol[v] {
			seenVol[v] = true
			namedVols = append(namedVols, v)
		}
	}
	if needsMongo {
		addVol("mongo_data")
	}
	if needsMinio {
		addVol("minio_data")
	}
	if needsRedis {
		addVol("redis_data")
	}
	for _, m := range withServers {
		for _, v := range m.Server.NamedVolumes {
			addVol(strings.Split(v, ":")[0])
		}
	}

	var serverBlocks []string
	for _, m := range withServers {
		serverBlocks = append(serverBlocks, prodServerBlock(p, m))
	}

	var out strings.Builder
	out.WriteString(`# GENERATED by 'nucleus generate' (infra/tool) — do not edit manually
# Production stack: nginx serves pre-built static files, servers run node index.js
name: nucleus

services:
  nginx:
    image: nginx:alpine
    restart: unless-stopped
`)
	out.WriteString(labelsBlock("proxy", "", nil))
	out.WriteString(`    ports:
      - "80:80"
      - "443:443"
    volumes:
`)
	out.WriteString(strings.Join(nginxDistVols, "\n"))
	out.WriteString(`
      - /etc/ssl/certs/nucleus.crt:/etc/ssl/certs/nucleus.crt:ro
      - /etc/ssl/private/nucleus.key:/etc/ssl/private/nucleus.key:ro
      - ./nginx/nginx.prod.conf:/etc/nginx/conf.d/default.conf:ro
    depends_on:
`)
	out.WriteString(strings.Join(nginxDependsLines, "\n"))
	out.WriteString(`

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
    volumes:
      - ../apps:/apps:ro
      - ../widgets:/widgets:ro

`)
	out.WriteString(strings.Join(serverBlocks, "\n"))
	out.WriteString("\n")

	if needsMongo {
		out.WriteString(`  mongo:
    image: ${MONGO_IMAGE:-mongo:7}
    restart: unless-stopped
`)
		out.WriteString(labelsBlock("database", "", nil))
		out.WriteString(`    volumes:
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
		out.WriteString(`    ports:
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

	if len(namedVols) > 0 {
		out.WriteString("volumes:\n")
		for _, v := range namedVols {
			out.WriteString(fmt.Sprintf("  %s:\n", v))
		}
	}

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
