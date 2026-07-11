# Plugin infrastructure

Nucleus can **discover** plugins and expose their metadata. This is
infrastructure only — nothing plugin-related is executed yet.

> **In scope (this PR):** a plugin directory, a manifest format, a discovery +
> validation runtime, a centralized registry, an Admin page, and prepared
> (empty) extension-point architecture.
>
> **Out of scope (future PRs):** executing plugin code, lifecycle hooks,
> dependency resolution, enable/disable, install/update, hot load/unload,
> automatic integration plugins, premium plugins. The architecture below is
> shaped so these land without a major refactor.

## Pieces

| Piece | Location | Role |
|-------|----------|------|
| Plugin directory | `/plugins/<id>/` | Where plugins live. One dir per plugin, each with a `nucleus.plugin.json`. |
| Manifest format | `nucleus.plugin.json` | Declares a plugin's identity, target, deps and permissions. See below. |
| Runtime | `infra/plugin-runtime/runtime.js` | Discovers plugins, validates manifests, builds registry entries. The only code that touches disk. |
| Registry | `infra/plugin-runtime/registry.js` | Single source of truth: installed plugins, manifest, discovery state, dependency metadata. |
| Service | `infra/plugin-runtime/index.js` | HTTP surface (`/api/plugins`). Mirrors the apps/widgets registry service. |
| Extension points | `infra/plugin-runtime/extensions.js` | **Prepared, empty.** Defines the scope taxonomy + a read-only catalogue; no points registered. |
| Client accessor | `core/usePlugins.js` | Fetches `/api/plugins`. |
| Admin page | `apps/admin/client/src/views/PluginsView.vue` | Lists discovered plugins + metadata. |

## Manifest schema — `nucleus.plugin.json`

```ts
{
  id: string            // required, kebab-case slug
  name: string          // required
  description?: string

  version: string       // required, SemVer
  apiVersion: string    // required, plugin API targeted (SemVer)

  target: string | string[]  // required: "core", an app id, or an array
  crossApp: boolean           // required: true when target spans >1 app

  author?: string

  dependencies?: {
    apps?: Record<string, string>      // appId → version range
    plugins?: Record<string, string>   // pluginId → version range
  }

  permissions?: string[]
}
```

Validation (`infra/plugin-runtime/manifest.js`) is structural only. It never
resolves dependencies or decides whether a plugin should run.

### Kinds of plugin (all supported by `target` + `crossApp`)

- **Core plugin** — `target: "core"`.
- **App plugin** — `target: "<appId>"`, `crossApp: false`.
- **Cross-app plugin** — `target: ["<appId>", …]`, `crossApp: true`.

## Discovery & state

The runtime scans `/plugins`, and for each directory that ships a manifest it
builds a registry entry. Each entry carries a discovery-level `state`:

- `discovered` — manifest valid and API-compatible.
- `incompatible` — valid manifest, but `apiVersion` major ≠ runtime's
  `PLUGIN_API_VERSION` (still listed, never run).
- `invalid` — manifest missing/unparseable or failed validation (`errors[]`
  explains why).

There is **no** `enabled`/`disabled` state — enable/disable is future work.

## API

Served by the `plugin-runtime` container, proxied by nginx at `/api/plugins`.

| Route | Returns |
|-------|---------|
| `GET /api/plugins` | `{ apiVersion, nucleus, plugins: Entry[] }` |
| `GET /api/plugins/dependencies` | `{ dependencies: [{ id, dependencies }] }` (declared only) |
| `GET /api/plugins/:id` | a single `Entry`, or 404 |

## Versioning

`PLUGIN_API_VERSION` (`infra/plugin-runtime/constants.js`) is the plugin contract
version, tracked separately from the Nucleus platform version
(`infra/nucleus.json`). Bump its MAJOR on a breaking change to the manifest shape
or extension-point surface.
