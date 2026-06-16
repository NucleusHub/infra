#!/usr/bin/env node
import { readFileSync, writeFileSync, readdirSync, existsSync } from 'fs'
import { join, resolve } from 'path'
import { fileURLToPath } from 'url'

const INFRA = fileURLToPath(new URL('.', import.meta.url))
const ROOT = resolve(INFRA, '..')
const APPS_DIR = join(ROOT, 'apps')
const WIDGETS_DIR = join(ROOT, 'widgets')

// Core services are ALWAYS present — they're not modular/discoverable apps, so
// they live in core/ and are hardcoded here (nginx route + prod container)
// instead of carrying a manifest under apps/. (Dev containers live in the base
// docker-compose.yml.) The registry never sees these, so they don't appear as
// manageable apps in the admin console.
const CORE_SERVICES = [
  {
    _dir: join(ROOT, 'core', 'auth-server'),
    nginx: { routes: [{ path: '/api/auth', upstream: 'auth-server:3005' }] },
    server: {
      service: 'auth-server',
      context: '../core/auth-server',
      port: 3005,
      healthEndpoint: '/api/auth/health',
      env: {
        MONGODB_URI: 'mongodb://mongo:27017/nucleus',
        JWT_SECRET: '${JWT_SECRET:-nucleus-jwt-secret}',
      },
      depends: ['mongo'],
    },
  },
]

function readManifests(baseDir, filename) {
  if (!existsSync(baseDir)) return []
  return readdirSync(baseDir, { withFileTypes: true })
    .filter(e => e.isDirectory())
    .flatMap(dir => {
      const path = join(baseDir, dir.name, filename)
      if (!existsSync(path)) return []
      try {
        return [{ ...JSON.parse(readFileSync(path, 'utf8')), _dir: join(baseDir, dir.name) }]
      } catch {
        console.warn(`  Warning: failed to parse ${path}`)
        return []
      }
    })
}

// ── Nginx ──────────────────────────────────────────────────────────────────

function nginxLocation(path, upstream, { websocket = false, cors = false, maxBodySize = null } = {}) {
  const ws = websocket ? `
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";` : ''

  // CORS: handle OPTIONS preflight then add Allow-Origin to all proxied responses.
  const corsBlock = cors ? `
        if ($request_method = 'OPTIONS') {
            add_header 'Access-Control-Allow-Origin' '*' always;
            add_header 'Access-Control-Allow-Methods' 'GET, PUT, DELETE, HEAD, OPTIONS' always;
            add_header 'Access-Control-Allow-Headers' '*' always;
            add_header 'Access-Control-Max-Age' '3000' always;
            add_header 'Content-Length' '0' always;
            return 204;
        }
        add_header 'Access-Control-Allow-Origin' '*' always;` : ''

  // maxBodySize: remove nginx body size limit and stream request body to upstream.
  // Set to "0" for unlimited (e.g. file upload endpoints).
  const sizeBlock = maxBodySize !== null ? `
        client_max_body_size ${maxBodySize};
        proxy_request_buffering off;` : ''

  // Use a variable so nginx resolves the upstream per-request (via Docker DNS)
  // rather than at startup — prevents boot failure when a service isn't up yet.
  return `
    location ${path} {${corsBlock}${sizeBlock}
        set $upstream ${upstream};
        proxy_pass http://$upstream;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;${ws}
    }`
}

function generateNginx(apps, widgets) {
  const routes = [...apps, ...widgets, ...CORE_SERVICES]
    .flatMap(m => m.nginx?.routes ?? [])
    .sort((a, b) => b.path.length - a.path.length) // longest path first for readability

  const blocks = routes.map(r => nginxLocation(r.path, r.upstream, r))

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
${blocks.join('\n')}

    location ~* ^(/watchlist)?/favicon\\.ico$ {
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
    }
}
`
}

// ── Prod Nginx ─────────────────────────────────────────────────────────────

function generateProdNginx(apps, widgets) {
  const all = [...apps, ...widgets, ...CORE_SERVICES]

  // Routes whose path === manifest.route are Vite dev-server routes; replace with static serving.
  // All other routes (API proxies, minio, etc.) stay as reverse proxies.
  const allRoutes = all.flatMap(m =>
    (m.nginx?.routes ?? []).map(r => ({ ...r, _manifestRoute: m.route ?? null }))
  )

  const apiRoutes = allRoutes
    .filter(r => !r._manifestRoute || r.path !== r._manifestRoute)
    .sort((a, b) => b.path.length - a.path.length)

  const spaRoutes = allRoutes
    .filter(r => r._manifestRoute && r.path === r._manifestRoute)
    .sort((a, b) => b.path.length - a.path.length)

  const apiBlocks = apiRoutes.map(r => nginxLocation(r.path, r.upstream, r))

  const spaBlocks = spaRoutes.map(({ path: p }) => {
    const dir = p.replace(/^\//, '')
    return `
    location ${p} {
        root /srv;
        try_files $uri $uri/ /${dir}/index.html;
    }`
  })

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
${apiBlocks.join('\n')}
${spaBlocks.join('\n')}

    location ~* ^(/watchlist)?/favicon\\.ico$ {
        root /srv/static;
        try_files /favicon.ico =404;
    }

    # Hub SPA — catch-all (pre-built static files in prod)
    location / {
        root /srv/hub;
        try_files $uri $uri/ /index.html;
    }
}
`
}

// ── Prod Compose ───────────────────────────────────────────────────────────

function prodServerBlock(m) {
  const s = m.server
  const relDir = '../' + m._dir.slice(ROOT.length + 1).replace(/\\/g, '/')
  const buildContext = s.context ?? `${relDir}/server`
  const healthUrl = `http://localhost:${s.port}${s.healthEndpoint}`
  const startPeriod = s.startPeriod ?? '10s'

  const envLines = [
    `      PORT: ${s.port}`,
    ...Object.entries(s.env ?? {}).map(([k, v]) => `      ${k}: ${v}`),
  ].join('\n')

  const volumeLines = (s.namedVolumes ?? [])
    .map(v => `      - ${v}`)
    .join('\n')

  const dependsLines = (s.depends ?? [])
    .map(d => `      ${d}:\n        condition: service_healthy`)
    .join('\n')

  let out = `  ${s.service}:\n`
  out += `    build:\n      context: ${buildContext}\n`
  out += `    restart: unless-stopped\n`
  out += `    command: ["node", "index.js"]\n`
  out += `    environment:\n${envLines}\n`
  if (volumeLines) out += `    volumes:\n${volumeLines}\n`
  if (dependsLines) out += `    depends_on:\n${dependsLines}\n`
  out += `    healthcheck:\n`
  out += `      test: ["CMD", "node", "-e", "require('http').get('${healthUrl}',r=>process.exit(r.statusCode<500?0:1)).on('error',()=>process.exit(1))"]\n`
  out += `      interval: 5s\n`
  out += `      timeout: 5s\n`
  out += `      retries: 10\n`
  out += `      start_period: ${startPeriod}\n`

  return out
}

function generateProdCompose(apps, widgets) {
  const all = [...apps, ...widgets, ...CORE_SERVICES]
  const withServers = all.filter(m => m.server)

  const needsMongo = withServers.some(m => (m.server.depends ?? []).includes('mongo'))
  const needsMinio = withServers.some(m => (m.server.depends ?? []).includes('minio'))
  const needsRedis = withServers.some(m => (m.server.depends ?? []).includes('redis'))

  // Nginx gets read-only mounts for each standalone app's pre-built dist
  const standaloneApps = all.filter(m => m.route)
  const nginxDistVols = [
    `      - ../hub/dist:/srv/hub:ro`,
    `      - ../hub/public:/srv/static:ro`,
    ...standaloneApps.map(m => {
      const rel = '../' + m._dir.slice(ROOT.length + 1).replace(/\\/g, '/')
      const dir = m.route.replace(/^\//, '')
      return `      - ${rel}/client/dist:/srv/${dir}:ro`
    }),
  ]

  const serverServiceNames = withServers.map(m => m.server.service)
  const nginxDependsLines = [
    `      registry:\n        condition: service_started`,
    ...serverServiceNames.map(n => `      ${n}:\n        condition: service_healthy`),
  ].join('\n')

  // Collect named volumes declared by server manifests
  const namedVols = new Set()
  if (needsMongo) namedVols.add('mongo_data')
  if (needsMinio) namedVols.add('minio_data')
  if (needsRedis) namedVols.add('redis_data')
  withServers.forEach(m => {
    ;(m.server.namedVolumes ?? []).forEach(v => namedVols.add(v.split(':')[0]))
  })

  const serverBlocks = withServers.map(m => prodServerBlock(m)).join('\n')

  let out = `# GENERATED by infra/generate.js — do not edit manually
# Production stack: nginx serves pre-built static files, servers run node index.js
name: nucleus

services:
  nginx:
    image: nginx:alpine
    restart: unless-stopped
    ports:
      - "80:80"
      - "443:443"
    volumes:
${nginxDistVols.join('\n')}
      - /etc/ssl/certs/nucleus.crt:/etc/ssl/certs/nucleus.crt:ro
      - /etc/ssl/private/nucleus.key:/etc/ssl/private/nucleus.key:ro
      - ./nginx/nginx.prod.conf:/etc/nginx/conf.d/default.conf:ro
    depends_on:
${nginxDependsLines}

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

${serverBlocks}
`

  if (needsMongo) {
    out += `  mongo:
    image: \${MONGO_IMAGE:-mongo:7}
    restart: unless-stopped
    volumes:
      - mongo_data:/data/db
    healthcheck:
      test: ["CMD-SHELL", "mongosh --eval 'db.adminCommand({ping:1})' --quiet 2>/dev/null || mongo --eval 'db.adminCommand({ping:1})' --quiet"]
      interval: 5s
      timeout: 5s
      retries: 10
      start_period: 10s

`
  }

  if (needsRedis) {
    out += `  redis:
    image: redis:7-alpine
    restart: unless-stopped
    command: ["redis-server", "--appendonly", "no", "--save", ""]
    volumes:
      - redis_data:/data
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 5s
      timeout: 3s
      retries: 10
      start_period: 5s

`
  }

  if (needsMinio) {
    out += `  minio:
    image: minio/minio:latest
    command: server /data --console-address ":9001"
    restart: unless-stopped
    ports:
      - "9000:9000"
      - "9001:9001"
    environment:
      MINIO_ROOT_USER: \${MINIO_ACCESS_KEY:-nucleusadmin}
      MINIO_ROOT_PASSWORD: \${MINIO_SECRET_KEY:-nucleuschangeme}
      MINIO_SERVER_URL: \${MINIO_PUBLIC_URL:-http://localhost}
    volumes:
      - minio_data:/data
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:9000/minio/health/live"]
      interval: 5s
      timeout: 5s
      retries: 10
      start_period: 15s

`
  }

  if (namedVols.size > 0) {
    out += `volumes:\n`
    namedVols.forEach(v => { out += `  ${v}:\n` })
  }

  return out
}

// ── Compose override ───────────────────────────────────────────────────────

// Apps with a client/ dir but no client/vite.config.js are hub libraries
// (standalone apps always have vite.config.js; library-only apps don't).
// Their client/ is mounted into the hub container so the @<id> vite alias resolves.
function findHubLibraries() {
  if (!existsSync(APPS_DIR)) return []
  return readdirSync(APPS_DIR, { withFileTypes: true })
    .filter(e => {
      const clientDir = join(APPS_DIR, e.name, 'client')
      return e.isDirectory()
        && existsSync(clientDir)
        && !existsSync(join(clientDir, 'vite.config.js'))
    })
    .map(e => ({ id: e.name, rel: `../apps/${e.name}/client` }))
}

function generateOverride(apps, widgets, hubLibs) {
  const includes = [...apps, ...widgets]
    .filter(m => existsSync(join(m._dir, 'docker-compose.app.yml')))
    .map(m => {
      const rel = '../' + m._dir.slice(ROOT.length + 1).replace(/\\/g, '/')
      return `  - path: ${rel}/docker-compose.app.yml`
    })

  const hubVolumes = hubLibs.map(l => `      - ${l.rel}:/app/${l.id}:ro`)

  const parts = ['# GENERATED by infra/generate.js — do not edit manually']

  if (includes.length > 0) {
    parts.push(`include:\n${includes.join('\n')}`)
  }

  if (hubVolumes.length > 0) {
    parts.push(`services:\n  hub:\n    volumes:\n${hubVolumes.join('\n')}`)
  }

  return parts.join('\n\n') + '\n'
}

// ── Validation ───────────────────────────────────────────────────────────────

// Backing services generate.js can auto-provision (see generateProdCompose).
// Anything an app/widget `depends` on must be one of these or another declared
// server, otherwise compose would reference a service that's never generated.
const PROVISIONED_SERVICES = ['mongo', 'redis', 'minio']

// Fail the build loudly on the few manifest mistakes that would otherwise emit a
// broken nginx config or an invalid compose file — duplicate location blocks,
// duplicate service keys, or a depends_on pointing at a service that never gets
// generated. A clear error here beats a cryptic `docker compose up` failure and
// keeps "drop a new app in and rebuild" honest.
function validate(apps, widgets) {
  const all = [...apps, ...widgets, ...CORE_SERVICES]
  const errors = []
  const warnings = []
  const label = m => m.id ?? m.server?.service ?? m._dir

  // Duplicate nginx paths → duplicate location {} blocks (nginx won't load).
  const pathOwners = {}
  for (const m of all)
    for (const r of (m.nginx?.routes ?? []))
      (pathOwners[r.path] ??= []).push(label(m))
  for (const [path, owners] of Object.entries(pathOwners))
    if (owners.length > 1) errors.push(`duplicate nginx route "${path}" claimed by: ${owners.join(', ')}`)

  // Duplicate service names → duplicate compose service keys (invalid compose).
  const svcOwners = {}
  for (const m of all)
    if (m.server?.service) (svcOwners[m.server.service] ??= []).push(label(m))
  for (const [svc, owners] of Object.entries(svcOwners))
    if (owners.length > 1) errors.push(`duplicate server service "${svc}" declared by: ${owners.join(', ')}`)

  // depends_on must resolve to a provisioned service or another declared one.
  const known = new Set([...PROVISIONED_SERVICES, ...Object.keys(svcOwners)])
  for (const m of all)
    for (const dep of (m.server?.depends ?? []))
      if (!known.has(dep))
        errors.push(`${label(m)} depends on unknown service "${dep}" — not another app's server and not auto-provisioned (${PROVISIONED_SERVICES.join('/')})`)

  // An app with a route but no matching location won't be served as an SPA in prod.
  for (const m of [...apps, ...widgets]) {
    if (!m.route) continue
    if (!(m.nginx?.routes ?? []).some(r => r.path === m.route))
      warnings.push(`${label(m)} sets route "${m.route}" but no nginx route has that path — it won't be served as an SPA in prod`)
  }

  warnings.forEach(w => console.warn(`  ⚠ ${w}`))
  if (errors.length) {
    console.error('\n✖ Manifest validation failed — fix these and rebuild:')
    errors.forEach(e => console.error(`  - ${e}`))
    process.exit(1)
  }
}

// ── Main ───────────────────────────────────────────────────────────────────

// Core services own their service name and routes. Drop any discovered
// app/widget that collides — e.g. a stale apps/auth left over from before
// auth moved into core/ — so we never emit duplicate compose keys / nginx
// locations.
const coreServiceNames = new Set(CORE_SERVICES.map(c => c.server?.service).filter(Boolean))
const dropCoreCollisions = list => list.filter(m => {
  if (coreServiceNames.has(m.server?.service)) {
    console.warn(`  Skipping ${m.id ?? m._dir}: '${m.server.service}' is provided by core, not apps/widgets`)
    return false
  }
  return true
})

const apps = dropCoreCollisions(readManifests(APPS_DIR, 'nucleus.app.json'))
const widgets = dropCoreCollisions(readManifests(WIDGETS_DIR, 'nucleus.widget.json'))
const hubLibs = findHubLibraries()

console.log(`Apps:    ${apps.length ? apps.map(a => a.id).join(', ') : 'none'}`)
console.log(`Widgets: ${widgets.length ? widgets.map(w => w.id).join(', ') : 'none'}`)
console.log(`Hub libs: ${hubLibs.length ? hubLibs.map(l => l.id).join(', ') : 'none'}`)

validate(apps, widgets)

writeFileSync(join(INFRA, 'nginx', 'nginx.conf'), generateNginx(apps, widgets))
console.log('→ nginx/nginx.conf')

writeFileSync(join(INFRA, 'docker-compose.override.yml'), generateOverride(apps, widgets, hubLibs))
console.log('→ docker-compose.override.yml')

writeFileSync(join(INFRA, 'nginx', 'nginx.prod.conf'), generateProdNginx(apps, widgets))
console.log('→ nginx/nginx.prod.conf')

writeFileSync(join(INFRA, 'docker-compose.prod.yml'), generateProdCompose(apps, widgets))
console.log('→ docker-compose.prod.yml')
