import express from 'express'
import { readFileSync, readdirSync } from 'fs'
import { join } from 'path'

const app = express()
const PORT = process.env.PORT || 4000
const APPS_DIR = process.env.APPS_DIR || '/apps'
const WIDGETS_DIR = process.env.WIDGETS_DIR || '/widgets'

function readManifests(baseDir, filename) {
  try {
    return readdirSync(baseDir, { withFileTypes: true })
      .filter(e => e.isDirectory())
      .flatMap(dir => {
        const manifestPath = join(baseDir, dir.name, filename)
        try {
          const manifest = JSON.parse(readFileSync(manifestPath, 'utf8'))
          // Inline the app/widget's own icon.svg so clients can render it
          // themeably (currentColor) without a per-app build dependency.
          const iconFile = typeof manifest.icon === 'string' && manifest.icon.endsWith('.svg')
            ? manifest.icon
            : 'icon.svg'
          try {
            manifest.iconSvg = readFileSync(join(baseDir, dir.name, iconFile), 'utf8')
          } catch { /* no icon shipped */ }
          return [manifest]
        } catch {
          return []
        }
      })
  } catch {
    return []
  }
}

app.use((_, res, next) => {
  res.setHeader('Access-Control-Allow-Origin', '*')
  next()
})

app.get('/api/registry/apps', (_, res) => {
  res.json(readManifests(APPS_DIR, 'nucleus.app.json'))
})

app.get('/api/registry/widgets', (_, res) => {
  res.json(readManifests(WIDGETS_DIR, 'nucleus.widget.json'))
})

app.get('/health', (_, res) => res.json({ ok: true }))

app.listen(PORT, () => console.log(`Registry listening on :${PORT}`))
