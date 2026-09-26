#!/usr/bin/env node
import { execFileSync } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const HERE = path.dirname(fileURLToPath(import.meta.url))
const REPO = path.resolve(HERE, '..', '..')
const CORE = path.join(REPO, 'core')
const CORE_REAL = fs.realpathSync(CORE)
const BRANCH = 'feat/standalone'

const appId = process.argv[2]
const target = (process.argv[3] || 'pwa').toLowerCase()
if (!appId) fail('usage: package-standalone.mjs <appId> [pwa|android|ios]')
if (!['pwa', 'android', 'ios'].includes(target)) fail(`unknown target "${target}"`)

const APP = path.join(REPO, 'apps', appId)
const CLIENT = path.join(APP, 'client')
const SRC = path.join(CLIENT, 'src')
const OUT_CORE = path.join(SRC, 'core')
if (!fs.existsSync(CLIENT)) fail(`no client at ${CLIENT}`)

const manifest = JSON.parse(fs.readFileSync(path.join(APP, 'nucleus.app.json'), 'utf8'))
const APP_NAME = manifest.name || appId
const BUNDLE_ID = manifest.native?.appId || `app.nucleus.${appId}`

function fail(msg) { console.error(`✗ ${msg}`); process.exit(1) }
function log(msg) { console.log(msg) }
function run(cmd, args, cwd, env) {
  log(`  $ ${cmd} ${args.join(' ')}`)
  execFileSync(cmd, args, { cwd, stdio: 'inherit', env: env || process.env })
}

function tmdbKey() {
  if (process.env.VITE_TMDB_API_KEY) return process.env.VITE_TMDB_API_KEY
  try {
    const env = fs.readFileSync(path.join(REPO, 'infra', '.env'), 'utf8')
    const m = env.match(/^VITE_TMDB_API_KEY=(.*)$/m)
    if (m) return m[1].trim()
  } catch {}
  return ''
}
function git(args) {
  return execFileSync('git', args, { cwd: APP, encoding: 'utf8' }).trim()
}

log(`\n▸ ${appId} → standalone (${target})`)
log('▸ branch')
const cur = git(['rev-parse', '--abbrev-ref', 'HEAD'])
if (cur !== BRANCH) {
  git(['checkout', '-B', BRANCH])
  log(`  on ${BRANCH}`)
} else {
  log(`  already on ${BRANCH}`)
}

log('▸ inline core')

const DENY = [/^auth\//, /^AppSidebar\.vue$/, /^useRegistry\.js$/, /^usePlugins\.js$/]
const OVERRIDES = { 'useI18n.js': i18nOverride() }

const visited = new Set()
const copied = new Set()

function resolveFile(base) {
  const tries = ['', '.js', '.ts', '.vue', '.json', '.mjs', '/index.js']
  for (const ext of tries) {
    const p = base + ext
    if (fs.existsSync(p) && fs.statSync(p).isFile()) return p
  }
  return null
}

function specifiers(code, isCss) {
  const specs = new Set()
  const add = (re) => { let m; while ((m = re.exec(code))) specs.add(m[1]) }
  if (isCss) {
    add(/@import\s+["']([^"']+)["']/g)
  } else {
    add(/\bfrom\s+["']([^"']+)["']/g)
    add(/\bimport\s+["']([^"']+)["']/g)
    add(/\bimport\(\s*["']([^"']+)["']\)/g)
  }
  return [...specs]
}

function copyCore(realPath) {
  const rel = path.relative(CORE_REAL, realPath)
  if (DENY.some((re) => re.test(rel))) {
    fail(`core file "${rel}" is on the drop list but is still imported — fix the src edits first`)
  }
  if (copied.has(rel)) return
  copied.add(rel)
  const dest = path.join(OUT_CORE, rel)
  fs.mkdirSync(path.dirname(dest), { recursive: true })
  const override = OVERRIDES[rel]
  if (override != null) {
    fs.writeFileSync(dest, override)
    scan(realPath, override)
  } else {
    fs.copyFileSync(realPath, dest)
    if (/\.(js|ts|mjs|vue|css)$/.test(rel)) scan(realPath, fs.readFileSync(realPath, 'utf8'))
  }
}

function scan(absFile, code) {
  if (visited.has(absFile)) return
  visited.add(absFile)
  const isCss = absFile.endsWith('.css')
  for (const spec of specifiers(code ?? fs.readFileSync(absFile, 'utf8'), isCss)) {
    const clean = spec.split('?')[0]
    let resolved
    if (clean.startsWith('@core/')) {
      resolved = resolveFile(path.join(CORE, clean.slice('@core/'.length)))
    } else if (clean.startsWith('@/')) {
      resolved = resolveFile(path.join(SRC, clean.slice(2)))
    } else if (clean.startsWith('.') || clean.startsWith('/')) {
      let abs = path.resolve(path.dirname(absFile), clean)
      // Prior runs may have rewritten specs into OUT_CORE; resolve them from the real core.
      if (abs === OUT_CORE || abs.startsWith(OUT_CORE + path.sep)) {
        abs = path.join(CORE, path.relative(OUT_CORE, abs))
      }
      resolved = resolveFile(abs)
    } else {
      continue
    }
    if (!resolved) continue
    const real = fs.realpathSync(resolved)
    if (real.startsWith(CORE_REAL + path.sep)) {
      copyCore(real)
    } else if (real.startsWith(fs.realpathSync(SRC) + path.sep)) {
      if (/\.(js|ts|mjs|vue|css)$/.test(real)) scan(real, fs.readFileSync(real, 'utf8'))
    }
  }
}

fs.rmSync(OUT_CORE, { recursive: true, force: true })
for (const entry of ['main.js', 'assets/main.css']) {
  scan(path.join(SRC, entry), fs.readFileSync(path.join(SRC, entry), 'utf8'))
}
log(`  inlined ${copied.size} core files → src/core`)

log('▸ rewire build')
const cssPath = path.join(SRC, 'assets', 'main.css')
let css = fs.readFileSync(cssPath, 'utf8')
css = css.replaceAll('../../core/', '../core/')
fs.writeFileSync(cssPath, css)

fs.writeFileSync(path.join(CLIENT, 'vite.config.js'), viteConfig())

for (const link of ['core', 'widgets', 'plugins']) {
  const p = path.join(CLIENT, link)
  try {
    if (fs.lstatSync(p).isSymbolicLink()) { fs.unlinkSync(p); log(`  removed symlink ${link}`) }
  } catch {}
}

log('▸ assets + capacitor')
const pub = path.join(CLIENT, 'public')
fs.mkdirSync(pub, { recursive: true })
const iconSvg = path.join(APP, 'icon.svg')
if (fs.existsSync(iconSvg)) fs.copyFileSync(iconSvg, path.join(pub, 'icon.svg'))

fs.writeFileSync(path.join(CLIENT, 'capacitor.config.json'), JSON.stringify({
  appId: BUNDLE_ID,
  appName: APP_NAME,
  webDir: 'dist',
}, null, 2) + '\n')

log('▸ install deps')
run('npm', ['install', '--no-audit', '--no-fund', '--save', 'dexie@^4'], CLIENT)
run('npm', ['install', '--no-audit', '--no-fund', '--save-dev', 'vite-plugin-pwa@^1'], CLIENT)
if (target !== 'pwa') {
  run('npm', ['install', '--no-audit', '--no-fund', '--save',
    '@capacitor/core@^7', '@capacitor/cli@^7', `@capacitor/${target}@^7`], CLIENT)
}

log('▸ build web')
const KEY = tmdbKey()
if (KEY) {
  fs.writeFileSync(path.join(CLIENT, '.env.local'), `VITE_TMDB_API_KEY=${KEY}\n`)
  log('  wrote client/.env.local (gitignored) — TMDb key loaded by dev + build')
} else {
  log('  ⚠ VITE_TMDB_API_KEY not found (env or infra/.env) — TMDb search/autofill will be disabled')
}
run('npm', ['run', 'build'], CLIENT)

if (target === 'pwa') {
  log(`\n✓ PWA built → apps/${appId}/client/dist`)
  log(`  preview:  npx vite preview --outDir dist   (from apps/${appId}/client)`)
  process.exit(0)
}

const platDir = path.join(CLIENT, target)
if (!fs.existsSync(platDir)) run('npx', ['cap', 'add', target], CLIENT)
run('npx', ['cap', 'sync', target], CLIENT)

if (target === 'android') {
  try {
    run('./gradlew', ['assembleDebug'], platDir)
    log(`\n✓ Android APK → apps/${appId}/client/android/app/build/outputs/apk/debug/`)
  } catch {
    log('\n⚠ Gradle build failed — needs the Android SDK + JDK. Project is scaffolded/synced;')
    log(`  open apps/${appId}/client/android in Android Studio to build/run.`)
  }
} else if (target === 'ios') {
  if (process.platform !== 'darwin') {
    log('\n⚠ iOS builds require macOS + Xcode. Project is scaffolded/synced.')
    log(`  Build it later on a Mac: open apps/${appId}/client/ios/App/App.xcworkspace`)
  } else {
    run('npx', ['cap', 'open', 'ios'], CLIENT)
  }
}

function viteConfig() {
  return `import { fileURLToPath, URL } from 'node:url'
import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import tailwindcss from '@tailwindcss/vite'
import svgLoader from 'vite-svg-loader'
import { VitePWA } from 'vite-plugin-pwa'

// Generated by infra/native/package-standalone.mjs for the standalone build:
// base '/', @core inlined at ./src/core, PWA + offline caching. Data is local
// (Dexie) so offline is inherent; the SW also caches TMDb posters/metadata.
export default defineConfig({
  base: '/',
  plugins: [
    vue(),
    tailwindcss(),
    svgLoader({
      defaultImport: 'url',
      svgo: true,
      svgoConfig: {
        plugins: [{ name: 'preset-default', params: { overrides: { removeViewBox: false, convertColors: false } } }],
      },
    }),
    VitePWA({
      registerType: 'autoUpdate',
      includeAssets: ['icon.svg'],
      manifest: {
        name: ${JSON.stringify(APP_NAME)},
        short_name: ${JSON.stringify(APP_NAME)},
        start_url: '/',
        display: 'standalone',
        background_color: '#0d0d1a',
        theme_color: '#0d0d1a',
        icons: [{ src: '/icon.svg', sizes: 'any', type: 'image/svg+xml', purpose: 'any maskable' }],
      },
      workbox: {
        globPatterns: ['**/*.{js,css,html,svg,png,woff2}'],
        runtimeCaching: [
          {
            urlPattern: /^https:\\/\\/image\\.tmdb\\.org\\/.*/,
            handler: 'CacheFirst',
            options: { cacheName: 'tmdb-images', expiration: { maxEntries: 500, maxAgeSeconds: 2592000 } },
          },
          {
            urlPattern: /^https:\\/\\/api\\.themoviedb\\.org\\/.*/,
            handler: 'StaleWhileRevalidate',
            options: { cacheName: 'tmdb-api' },
          },
        ],
      },
    }),
  ],
  css: {
    transformer: 'lightningcss',
    lightningcss: {
      // Concrete targets so Lightning CSS vendor-prefixes backdrop-filter etc.
      targets: {
        safari: (15 << 16) | (4 << 8),
        ios_saf: (15 << 16) | (4 << 8),
        firefox: 103 << 16,
        chrome: 90 << 16,
        edge: 90 << 16,
      },
    },
  },
  build: { cssMinify: 'lightningcss' },
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
      '@core': fileURLToPath(new URL('./src/core', import.meta.url)),
    },
  },
})
`
}

function i18nOverride() {
  return `import { ref } from 'vue'
import coreFallbackEn from './locales/en-US.json'

// Standalone build (generated): static single-language i18n — no localization
// plugin, no network, no auth. registerFallback (called from main.js) composes
// the catalog from the bundled core base + the app's locale file; t() reads it.
const FALLBACK = 'en-US'
function getCookie(name) {
  const m = document.cookie.match(new RegExp('(?:^|; )' + name + '=([^;]*)'))
  return m ? decodeURIComponent(m[1]) : null
}
const locale = ref(getCookie('nucleus-locale') || FALLBACK)
const messages = ref({})
const ready = ref(true)
const active = ref(false)   // single-language: hide any language picker
let appFallback = {}
function applyFallback() { messages.value = { ...coreFallbackEn, ...appFallback } }
export function registerFallback(msgs) { appFallback = msgs || {}; applyFallback() }
applyFallback()
function interpolate(str, params) {
  if (!params) return str
  return str.replace(/\\{(\\w+)\\}/g, (_, k) => (k in params ? String(params[k]) : \`{\${k}}\`))
}
function t(key, params) { const msg = messages.value[key]; return msg == null ? key : interpolate(msg, params) }
export function initI18n() {}
export function useI18n() { return { t, locale, messages, ready, active, initI18n, setLocale() {}, registerFallback } }
export { t }
`
}
