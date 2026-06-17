#!/usr/bin/env node
// Scaffolds a new Nucleus app under apps/<id>/ that is plug-and-play out of the
// box: a manifest the registry + infra tool auto-discover, a Vite client wired
// to the shared @core components (BackgroundBlobs shader, AuthGuard, AppHeader,
// liquid-glass), an optional Express server with auth + health, an optional Echo
// integration, and its own git repo. Drop the result in, rebuild Docker, done.
//
//   node infra/create-app.js <id> [--name "Display Name"] [--description "..."]
//                                  [--no-server] [--echo] [--no-git]
//
// Ports (client dev + server) are auto-allocated to the next free slot so a new
// app never collides with an existing one.
import { readFileSync, writeFileSync, readdirSync, existsSync, mkdirSync, symlinkSync } from 'fs'
import { join, resolve } from 'path'
import { fileURLToPath } from 'url'
import { execFileSync } from 'child_process'

const INFRA = fileURLToPath(new URL('.', import.meta.url))
const ROOT = resolve(INFRA, '..')
const APPS_DIR = join(ROOT, 'apps')
const WIDGETS_DIR = join(ROOT, 'widgets')

// ── Args ─────────────────────────────────────────────────────────────────────

const argv = process.argv.slice(2)
const VALUE_OPTS = new Set(['name', 'description'])

// Parse into positionals + options in one pass; --name/--description take a value.
const positionals = []
const options = {}
for (let i = 0; i < argv.length; i++) {
  const a = argv[i]
  if (a.startsWith('--')) {
    const key = a.slice(2)
    if (VALUE_OPTS.has(key)) options[key] = argv[++i]
    else options[key] = true
  } else {
    positionals.push(a)
  }
}
const opt = name => options[name] ?? null
const has = name => options[name] === true

const id = positionals[0]

function die(msg) { console.error(`✖ ${msg}`); process.exit(1) }

if (!id) die('usage: node infra/create-app.js <id> [--name "..."] [--description "..."] [--no-server] [--echo] [--no-git]')
if (!/^[a-z][a-z0-9-]*$/.test(id)) die(`invalid id "${id}" — use lowercase kebab-case (e.g. my-app)`)

const appDir = join(APPS_DIR, id)
if (existsSync(appDir)) die(`apps/${id} already exists`)
if (existsSync(join(WIDGETS_DIR, id))) die(`a widget named "${id}" already exists — ids share a namespace`)

const withServer = !has('no-server')
const withEcho = has('echo')
const withGit = !has('no-git')
const name = opt('name') || id.split('-').map(s => s[0].toUpperCase() + s.slice(1)).join(' ')
const description = opt('description') || `${name} — a Nucleus app`
const route = `/${id}`
const apiPrefix = `/api/${id}`
const pascal = id.split('-').map(s => s[0].toUpperCase() + s.slice(1)).join('')
const serviceClient = `${id}-client`
const serviceServer = `${id}-server`

// ── Port allocation ───────────────────────────────────────────────────────────

// Scan existing manifests for ports in use; seed with reserved infra ports so we
// never hand out one that collides with a core service.
function usedPorts() {
  const server = new Set([3005, 4000]) // auth-server, registry
  const client = new Set()
  for (const [base, file] of [[APPS_DIR, 'nucleus.app.json'], [WIDGETS_DIR, 'nucleus.widget.json']]) {
    if (!existsSync(base)) continue
    for (const e of readdirSync(base, { withFileTypes: true })) {
      if (!e.isDirectory()) continue
      const mf = join(base, e.name, file)
      if (!existsSync(mf)) continue
      let m
      try { m = JSON.parse(readFileSync(mf, 'utf8')) } catch { continue }
      if (m.server?.port) server.add(Number(m.server.port))
      for (const r of (m.nginx?.routes ?? [])) {
        const p = Number(String(r.upstream ?? '').split(':')[1])
        if (p) client.add(p)
      }
    }
  }
  return { server, client }
}
const nextFree = (set, start) => { let p = start; while (set.has(p)) p++; return p }
const { server: usedServer, client: usedClient } = usedPorts()
const clientPort = nextFree(usedClient, 5180)
const serverPort = withServer ? nextFree(usedServer, 3010) : null

// ── File templates ─────────────────────────────────────────────────────────────

const files = {}

files['.gitignore'] = `node_modules/
dist/
.vite/
.env
.env.local
.env.*.local
*.log
.DS_Store
Thumbs.db
.idea/
.vscode/
`

files['README.md'] = `# ${name}

A Nucleus app. Auto-discovered via \`nucleus.app.json\` — drop this repo into
\`apps/${id}/\`, run \`infra/build\`, and it appears in the hub and is served at
\`${route}\`.

- **Client** — Vite + Vue, dev port \`${clientPort}\`, uses shared \`@core\` components
  (\`BackgroundBlobs\` shader background, \`AuthGuard\`, \`AppHeader\`, liquid-glass).
${withServer ? `- **Server** — Express + Mongo, port \`${serverPort}\`, API under \`${apiPrefix}\`.\n` : ''}${withEcho ? `- **Echo** — ships an integration under \`echo/\` (message type \`${id}.item\`).\n` : ''}
The \`client/core\` symlink points at the monorepo's \`core/\` so \`@core/*\` resolves.
`

// Manifest — discovered by the infra tool (infra/tool) and the registry service.
const manifest = {
  id,
  name,
  description,
  route,
  icon: 'icon.svg',
  hub: { showInSidebar: true, showOnDashboard: true },
  nginx: {
    routes: [
      { path: route, upstream: `${serviceClient}:${clientPort}`, websocket: true },
      ...(withServer ? [{ path: apiPrefix, upstream: `${serviceServer}:${serverPort}` }] : []),
    ],
  },
  ...(withServer ? {
    server: {
      service: serviceServer,
      port: serverPort,
      healthEndpoint: `${apiPrefix}/health`,
      depends: ['mongo'],
      env: {
        MONGODB_URI: 'mongodb://mongo:27017/nucleus',
        JWT_SECRET: '${JWT_SECRET:-nucleus-jwt-secret}',
      },
    },
  } : {}),
}
files['nucleus.app.json'] = JSON.stringify(manifest, null, 2) + '\n'

files['icon.svg'] = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round">
  <rect x="3" y="3" width="18" height="18" rx="4" />
  <path d="M8 12h8M12 8v8" />
</svg>
`

// Dev compose — base docker-compose.yml includes this via the generated override.
const composeServer = withServer ? `  ${serviceServer}:
    build:
      context: ./server
    restart: unless-stopped
    labels:
      nucleus.managed: "true"
      nucleus.stack: "nucleus"
      nucleus.role: "app-server"
      nucleus.app: "${id}"
    environment:
      MONGODB_URI: mongodb://mongo:27017/nucleus
      JWT_SECRET: \${JWT_SECRET:-nucleus-jwt-secret}
      PORT: ${serverPort}
    volumes:
      - ./server:/app
      - ${id.replace(/-/g, '_')}_server_modules:/app/node_modules
    depends_on:
      mongo:
        condition: service_healthy
    healthcheck:
      test: ["CMD", "node", "-e", "require('http').get('http://localhost:${serverPort}${apiPrefix}/health',r=>process.exit(r.statusCode<500?0:1)).on('error',()=>process.exit(1))"]
      interval: 5s
      timeout: 5s
      retries: 10
      start_period: 10s

` : ''
files['docker-compose.app.yml'] = `services:
${composeServer}  ${serviceClient}:
    build:
      context: ./client
    restart: unless-stopped
    labels:
      nucleus.managed: "true"
      nucleus.stack: "nucleus"
      nucleus.role: "app-client"
      nucleus.app: "${id}"
    environment:
      API_TARGET: http://${serviceServer}:${serverPort ?? ''}
      NUCLEUS_HOST: \${NUCLEUS_HOST:-nucleus.olm-altair.ts.net}
    volumes:
      - ./client/src:/app/src
      - ./client/index.html:/app/index.html
      - ./client/vite.config.js:/app/vite.config.js
      # core/ is mounted so the @core alias resolves inside the dev container
      # (mirrors the committed client/core symlink used by host builds).
      - ../../core:/app/core:ro
${withServer ? `    depends_on:\n      ${serviceServer}:\n        condition: service_healthy\n` : ''}${withServer ? `\nvolumes:\n  ${id.replace(/-/g, '_')}_server_modules:\n` : ''}`

// ── Client ───────────────────────────────────────────────────────────────────

files['client/package.json'] = JSON.stringify({
  name: `nucleus-${id}-client`,
  version: '1.0.0',
  private: true,
  type: 'module',
  scripts: { dev: 'vite', build: 'vite build' },
  dependencies: {
    '@zaosoula/liquid-glass-vue': '^1.1.2',
    tailwindcss: '^4.3.0',
    vue: '^3.5.32',
    'vue-router': '^5.1.0',
  },
  devDependencies: {
    '@tailwindcss/vite': '^4.3.0',
    '@vitejs/plugin-vue': '^6.0.6',
    vite: '^8.0.8',
    'vite-plugin-vue-devtools': '^8.1.1',
  },
  engines: { node: '^20.19.0 || >=22.12.0' },
}, null, 2) + '\n'

files['client/Dockerfile'] = `FROM node:24-alpine
WORKDIR /app
COPY package*.json ./
RUN npm install
COPY . .
EXPOSE ${clientPort}
CMD ["npm", "run", "dev"]
`

files['client/.dockerignore'] = `node_modules
dist
core
`

const proxyBlock = withServer ? `
    proxy: {
      '${apiPrefix}': {
        target: process.env.API_TARGET || 'http://localhost:${serverPort}',
        changeOrigin: true,
      },
    },` : ''
files['client/vite.config.js'] = `import { fileURLToPath, URL } from 'node:url'
import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import vueDevTools from 'vite-plugin-vue-devtools'
import tailwindcss from '@tailwindcss/vite'

export default defineConfig(({ mode }) => ({
  base: '${route}/',
  plugins: [vue(), mode !== 'production' && vueDevTools(), tailwindcss()].filter(Boolean),
  resolve: {
    preserveSymlinks: true,
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
      '@core': fileURLToPath(new URL('./core', import.meta.url)),
    },
  },
  server: {
    host: '0.0.0.0',
    port: ${clientPort},${proxyBlock}
    allowedHosts: [process.env.NUCLEUS_HOST || 'nucleus.olm-altair.ts.net'],
  },
}))
`

files['client/index.html'] = `<!DOCTYPE html>
<html lang="">
  <head>
    <meta charset="UTF-8">
    <script>(function(){function gc(n){var m=document.cookie.match(new RegExp('(?:^|; )'+n+'=([^;]*)'));return m?decodeURIComponent(m[1]):null}var t=gc('nucleus-theme')||'system';document.documentElement.classList.toggle('dark',t==='dark'||(t==='system'&&matchMedia('(prefers-color-scheme: dark)').matches))})()</script>
    <link rel="icon" href="./core/favicon.ico">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>${name} — Nucleus</title>
  </head>
  <body>
    <div id="app"></div>
    <script type="module" src="/src/main.js"></script>
  </body>
</html>
`

files['client/src/main.js'] = `import './assets/main.css'
import { createApp } from 'vue'
import App from './App.vue'
import router from './router/index.js'

createApp(App).use(router).mount('#app')
`

files['client/src/assets/main.css'] = `@import "tailwindcss";
@custom-variant dark (&:where(.dark, .dark *));

html { background-color: rgb(248 250 252); } /* slate-50 */
html.dark { background-color: rgb(2 6 23); }  /* slate-950 */
`

files['client/src/App.vue'] = `<script setup>
// Shared shader background + auth gate from @core (resolves via the client/core
// symlink). Every Nucleus app wraps its routes the same way.
import BackgroundBlobs from '@core/BackgroundBlobs.vue'
import AuthGuard from '@core/auth/AuthGuard.vue'
</script>

<template>
  <BackgroundBlobs />
  <AuthGuard>
    <RouterView />
  </AuthGuard>
</template>
`

files['client/src/router/index.js'] = `import { createRouter, createWebHistory } from 'vue-router'
import HomeView from '@/views/HomeView.vue'

export default createRouter({
  history: createWebHistory('${route}/'),
  routes: [
    { path: '/', component: HomeView },
  ],
})
`

files['client/src/views/HomeView.vue'] = `<script setup>
import AppHeader from '@core/AppHeader.vue'
</script>

<template>
  <AppHeader>
    <span class="font-semibold text-slate-900 dark:text-white">${name}</span>
  </AppHeader>

  <main class="min-h-screen flex items-center justify-center p-6">
    <!-- Glass card (CSS backdrop-blur). For the WebGL displacement glass, import
         { LiquidGlass } from '@zaosoula/liquid-glass-vue/components' — see the hub. -->
    <div class="max-w-md w-full rounded-3xl border border-white/40 dark:border-white/10 bg-white/40 dark:bg-white/5 backdrop-blur-xl shadow-xl p-8 text-center">
      <h1 class="text-2xl font-bold text-slate-900 dark:text-white">${name}</h1>
      <p class="mt-3 text-slate-600 dark:text-slate-300">
        Your new Nucleus app is live. Edit
        <code class="px-1 rounded bg-black/10 dark:bg-white/10">src/views/HomeView.vue</code>
        to begin.
      </p>
    </div>
  </main>
</template>
`

// ── Server ───────────────────────────────────────────────────────────────────

if (withServer) {
  files['server/package.json'] = JSON.stringify({
    name: `nucleus-${id}-server`,
    version: '1.0.0',
    private: true,
    type: 'module',
    scripts: { start: 'node index.js', dev: 'node --watch index.js' },
    dependencies: {
      'cookie-parser': '^1.4.6',
      cors: '^2.8.5',
      dotenv: '^17.4.2',
      express: '^5.2.1',
      jsonwebtoken: '^9.0.2',
      mongoose: '^9.6.3',
    },
    engines: { node: '^20.19.0 || >=22.12.0' },
  }, null, 2) + '\n'

  files['server/.dockerignore'] = `node_modules
dist
.env
`

  files['server/Dockerfile'] = `FROM node:24-alpine
WORKDIR /app
COPY package*.json ./
RUN npm install
COPY . .
EXPOSE ${serverPort}
CMD ["sh", "-c", "npm install && node --watch index.js"]
`

  files['server/index.js'] = `import 'dotenv/config'
import express from 'express'
import cors from 'cors'
import cookieParser from 'cookie-parser'
import mongoose from 'mongoose'
import mainRoutes from './routes/main.js'

const app = express()
const PORT = process.env.PORT || ${serverPort}

app.use(cors({ origin: true, credentials: true }))
app.use(express.json())
app.use(cookieParser())

app.get('${apiPrefix}/health', (_, res) => res.json({ ok: true }))
app.use('${apiPrefix}', mainRoutes)

mongoose
  .connect(process.env.MONGODB_URI)
  .then(() => {
    console.log('[${id}] connected to MongoDB')
    app.listen(PORT, () => console.log('[${id}] server on port ' + PORT))
  })
  .catch((err) => {
    console.error('[${id}] MongoDB connection error:', err)
    process.exit(1)
  })
`

  // Verifies the nucleus_token cookie issued by the auth-server; populates req.profile.
  files['server/middleware/auth.js'] = `import jwt from 'jsonwebtoken'

const secret = () => process.env.JWT_SECRET || 'nucleus-jwt-secret'

export function requireAuth(req, res, next) {
  const token = req.cookies?.nucleus_token
  if (!token) return res.status(401).json({ error: 'Unauthenticated' })
  try {
    req.profile = jwt.verify(token, secret())
    next()
  } catch {
    res.status(401).json({ error: 'Invalid or expired token' })
  }
}
`

  files['server/routes/main.js'] = `import { Router } from 'express'
import { requireAuth } from '../middleware/auth.js'

const router = Router()
router.use(requireAuth)

// Example authenticated endpoint, scoped to the signed-in profile.
// req.profile.profileId is set by requireAuth from the auth cookie.
router.get('/', (req, res) => {
  res.json({ app: '${id}', profileId: req.profile.profileId })
})

export default router
`
}

// ── Echo integration (optional) ────────────────────────────────────────────────

if (withEcho) {
  files['echo/manifest.echo.json'] = JSON.stringify({
    app: id,
    label: name,
    version: '1.0.0',
    message_types: [`${id}.item`],
    composer_actions: [
      { id: `share_${id.replace(/-/g, '_')}`, label: `Share ${name}`, icon: 'M12 4v16m8-8H4' },
    ],
  }, null, 2) + '\n'

  files['echo/integration.echo.js'] = `import ${pascal}Card from './${pascal}Card.vue'

// Auto-discovered by Echo's client (import.meta.glob over apps/*/echo) and its
// server (registry/manifestLoader.js over apps/*/echo/manifest.echo.json).
export default {
  app: '${id}',
  // message type -> renderer component
  renderers: {
    '${id}.item': ${pascal}Card,
  },
  // composer action handler. Either { source } (Echo's generic picker) or
  // { picker, toMessage } (your own picker component). This generic example
  // lists items the user can share into a chat.
  composerActions: {
    share_${id.replace(/-/g, '_')}: {
      source: {
        title: '${name}',
        layout: 'list',
        // Return [{ id, title, subtitle? }]; map each picked row to a message.
        fetch: async () => [],
        map: (item) => ({ type: '${id}.item', payload: item }),
      },
    },
  },
}
`

  files[`echo/${pascal}Card.vue`] = `<script setup>
// Renders a '${id}.item' message inside an Echo chat bubble.
defineProps({ message: { type: Object, required: true } })
</script>

<template>
  <div class="rounded-xl border border-white/40 dark:border-white/10 bg-white/40 dark:bg-white/5 backdrop-blur px-3 py-2">
    <span class="text-sm text-slate-900 dark:text-white">{{ message.payload?.title || '${name} item' }}</span>
  </div>
</template>
`
}

// ── Write everything ───────────────────────────────────────────────────────────

for (const [rel, content] of Object.entries(files)) {
  const full = join(appDir, rel)
  mkdirSync(join(full, '..'), { recursive: true })
  writeFileSync(full, content)
}

// Committed relative symlink so @core/* resolves on a fresh clone and host
// builds, without waiting for infra/build to create it. From apps/<id>/client/
// the monorepo core/ is three levels up.
symlinkSync('../../../core', join(appDir, 'client', 'core'))

// ── Git ─────────────────────────────────────────────────────────────────────

if (withGit) {
  try {
    execFileSync('git', ['init', '-q', '-b', 'main'], { cwd: appDir })
    execFileSync('git', ['add', '-A'], { cwd: appDir })
    execFileSync('git', ['commit', '-q', '-m', `chore: scaffold ${id} app`], { cwd: appDir })
  } catch (err) {
    console.warn(`  ⚠ git init/commit skipped: ${err.message.split('\n')[0]}`)
  }
}

// ── Done ─────────────────────────────────────────────────────────────────────

console.log(`✓ Created apps/${id}`)
console.log(`    route        ${route}`)
console.log(`    client port  ${clientPort}` + (withServer ? `\n    server port  ${serverPort}  (API ${apiPrefix})` : '  (client-only)'))
console.log(`    echo         ${withEcho ? `yes (type ${id}.item)` : 'no'}`)
console.log(`    git          ${withGit ? 'initialised (branch main, 1 commit)' : 'skipped'}`)
console.log(`\nNext:`)
console.log(`  • Add a GitHub remote if you want it backed up:`)
console.log(`      git -C apps/${id} remote add origin <url> && git -C apps/${id} push -u origin main`)
console.log(`  • Rebuild the stack so it goes live:  infra/build`)
console.log(`    (regenerates nginx + compose via the infra tool, which validates manifests)`)
