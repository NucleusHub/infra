# Nucleus — infra

Orchestration for the Nucleus stack: the Docker Compose definitions, nginx
config, the registry service, and the tooling that builds and deploys
everything. Apps and widgets live in sibling repos (`../apps/*`, `../widgets/*`)
and are auto-discovered from their manifests.

## Modules

The minimal working setup is **core + infra + hub**. Everything else is an
optional sibling that can be added or removed at any time — `./nucleus up
--build` (or `./production`) picks up the change:

| Module | Where | What infra does when it's present |
|---|---|---|
| Apps | `../apps/<id>` (`nucleus.app.json`) | Includes its `docker-compose.app.yml`, routes it in nginx, builds its client. A `client/` without a `vite.config.js` is a **hub library**, linked at `hub/libs/<id>` and bundled into the hub (Pulse is the reference). |
| Widgets | `../widgets` (`package.json` + `<id>/nucleus.widget.json`) | Mounts it into the hub and registry, builds the workspace. |
| Plugins | `../plugins/<id>` (`nucleus.plugin.json`) | Mounts it into the hub and auth-server; see `../plugins/README.md` for the client and server extension points. |
| Plugin runtime | `../plugin-runtime` | Runs the `/api/plugins` discovery service. Without it nginx answers `{"plugins":[]}`. |

A module counts as installed only when it has that marker file, so an empty
directory (e.g. one Docker recreated for an app's bind mount) is ignored.

### Installing and removing modules — `./modules`

`./modules` reads the catalog from the Nucleus marketplace and shows every app,
plugin and widget it offers next to what's installed. The code itself is still
cloned from the GitHub org infra was cloned from:

```bash
./modules                          # list installed + available
./modules install shelf spotify    # install (with dependencies), then apply
./modules remove spotify           # remove (with what it bundles), then apply
./modules import home.nucleus.json # install a build from the site's /create, apply its look
./modules appearance [reset]       # show (or drop) the imported look
./modules apply [--dev]            # just rebuild the stack from the checkout
./modules ui                       # the same in a small local web UI
```

- **Apply** runs `./production` (blue/green, no downtime) by default; `--dev`
  uses `./nucleus up -d --build --remove-orphans` instead, `--no-apply` only
  changes the checkout. The UI offers the same three choices.
- **What gets installed.** An app is its own repo, cloned to `apps/<repo>`. A
  plugin or widget is one folder of the `plugins` / `widgets` repo, added as a
  sparse checkout so each installs on its own. Dependencies come from the
  manifests — a widget's `dependsOn`, a plugin's `dependencies.{apps,plugins}`,
  the widget that shares an app's id (its dashboard widget), a collection's
  `locked` entries (`widgets/core`) — and plugins bring the plugin runtime.
- **Safety.** A removal is refused while something installed still needs it,
  and whenever the folder has uncommitted or unpushed work; ignored files that
  would go with it (e.g. a `.env`) are listed first. `--force` (or the
  *Remove anyway* checkbox in the UI, shown when a removal is blocked) removes
  it regardless and discards that work. Modules that aren't in any
  repo are shown as *Local* and never touched. Removing an app deletes its code,
  not its data in MongoDB/MinIO.
- **The catalog** comes from the marketplace's internal feed
  (`/api/v1/internal/items`), which carries each part's manifest, repo and
  folder, read out of the org's repos by the marketplace seed
  (`npm run seed:sync` in nucleus-web). It needs an API token:
  `NUCLEUS_MARKETPLACE_TOKEN` in `infra/.env` (or the environment) — create one
  on the marketplace server with
  `docker compose exec api npm run token -- create <name>`. The marketplace is
  `https://nucleus-home.dev` unless `NUCLEUS_MARKETPLACE_URL` says otherwise.
  A part that is in the org but not (yet) in the marketplace is not offered —
  re-run the seed after adding one.
- **Cloning.** The org's repos are private, so installing needs git access:
  `NUCLEUS_GITHUB_TOKEN` in `infra/.env` (or the environment), else
  `gh auth token`, else whatever git already uses for github.com (an ssh
  checkout clones over ssh). The org can be overridden with
  `NUCLEUS_GITHUB_ORG`.
- **Importing a build.** The configurator on the Nucleus site (`/create`)
  downloads a `*.nucleus.json`; `./modules import <file>` (or *Import build* /
  drag-and-drop in the UI) installs every app, plugin and widget it lists with
  their dependencies. Core and Hub are always there and skipped; parts the
  marketplace doesn't offer are listed and skipped; per-package settings and
  Smart aren't applied. Its appearance (theme, accent, radius, glass,
  transparency, type scale, motion, wallpaper) is written to
  `../state/appearance.json`, which nginx serves as `/appearance.json` and
  `core/useAppearance.js` applies at runtime by overriding the Tailwind theme
  variables — no rebuild needed, a reload shows it. `--no-appearance` keeps the
  current look; `./modules appearance reset` goes back to stock. A theme picked
  in the app still wins over the build's default.
- **The UI** listens on `127.0.0.1:7777` (`--addr` to change, `--no-open` to not
  launch a browser) and prints a link with a one-time token; its API refuses
  requests without it. On a remote server, reach it over an SSH tunnel
  (`ssh -L 7777:127.0.0.1:7777 server`) rather than binding it publicly.

## Commands

All commands are run from the `infra/` directory (or by absolute path — they
resolve their own location).

| Command | What it does |
|---|---|
| `./dev` | Start the **dev** stack if it isn't already running (no-op if it is). |
| `./nucleus [--all] <compose args>` | Dev Compose passthrough — **app-scoped by default**. |
| `./production [--force] [-j N]` | Build all frontends and deploy the **prod** stack. |
| `./update [repo…]` | `git pull` across all (or named) Nucleus repos. |
| `./modules [install\|remove\|import\|appearance\|apply\|ui] …` | Install / remove apps, plugins and widgets from the GitHub org, then apply — see *Modules*. |
| `./mail <subject> [to]` | Send an email (body on stdin) through the mail relay — see *`./mail`*. |

### `./dev`

Brings up the dev stack (`docker-compose.yml` + the generated
`docker-compose.override.yml`) with `docker compose up -d`, unless its
containers are already running. Picks the right MongoDB image for the CPU,
regenerates configs from manifests, and prints the local link
(`http://localhost/`).

### `./nucleus`

Thin wrapper over `docker compose` for the dev stack. It regenerates configs,
reloads nginx, then runs your command. **By default it only touches app
services** — the stateful backing services (`mongo`, `minio`, `redis`) are never
implicitly recreated or removed, so you can bounce apps without dropping the
database, cache or object store.

```sh
./nucleus up -d              # (re)create app services only
./nucleus up -d --build      # rebuild + recreate app images (not mongo/minio/redis)
./nucleus down               # tear down app services only
./nucleus logs -f hub        # a named service is always respected as-is
./nucleus restart mongo      # name a backing service explicitly to target it
./nucleus --all up -d        # operate on the WHOLE stack, backing services included
```

`--all` may appear anywhere in the arguments.

### `./production`

Builds every Vue frontend (the hub + each `apps/*/client` with a
`vite.config.js` + the `widgets` pnpm workspace) and brings up the production
stack (nginx serving pre-built static assets, servers running `node index.js`).

- Incremental and parallel: each unit is only rebuilt when its source
  fingerprint changes; `npm`/`pnpm install` is skipped unless the lockfile
  changed. Fingerprints cache to `.build-cache/`.
- `--force` / `-f` rebuilds every unit; `-j N` caps parallel build units.

Deployment is **blue/green** and zero-downtime: `production` builds the inactive
color alongside the running one, health-checks it, then flips the edge proxy to
it in a single graceful reload. See `BLUEGREEN.md` for the full flow.

```sh
./bluegreen-init        # ONE-TIME migration off the old single-project stack
./production            # incremental build + health-gated switch to the other color
./production --force    # ignore the cache, rebuild everything
./rollback              # one command back to the previous color
```

### `./mail`

Outgoing mail goes through a Postfix relay (`mail`, in the shared data stack)
that forwards to Resend's SMTP. Set it up in `infra/.env`:

```sh
RESEND_API_KEY=re_...                          # Resend → API Keys (sending access is enough)
MAIL_FROM=Nucleus <alerts@nucleus-home.dev>    # must be on a domain verified in Resend
MAIL_TO=you@example.com                        # default recipient
```

The relay is generated only once `RESEND_API_KEY` is set; the next
`./production` starts it. Then:

```sh
echo "Disk at 91%" | ./mail "nucleus: disk almost full"
./mail "Deploy done" someone@else.com < report.txt
```

Containers on `nucleus-data-net` can send through `mail:587` without
credentials (it accepts only private networks), and Postfix queues and retries
when Resend is unreachable.

## The infra tool (`tool/`)

`dev`, `production`, and `nucleus` are thin shims around a single Go binary
(`tool/`) with subcommands `generate`, `build`, and `dev`. `ensure-tool`
compiles it on demand in a `golang:1.23-alpine` container — **no host Go
toolchain is required** — caching the binary by source hash. The compiled
binary (`tool/nucleus`) is git-ignored.

`nucleus generate` regenerates the nginx + compose configs from the app/widget
manifests: `nginx/conf.d/default.conf` and `docker-compose.override.yml` (dev),
plus the blue/green production set — `nginx/stack/default.conf` (per-color web),
`nginx/edge/default.conf` (edge proxy), and `docker-compose.{data,stack,edge}.yml`.
It runs automatically inside `dev`, `nucleus`, and `production`; you rarely call
it directly. (The edge's `nginx/edge/active.inc`, which records the live color,
is owned by the deploy/switch helpers and is only seeded, never overwritten.)

## Configuration

Copy `.env.example` to `.env` and fill in your values.

| Variable | Description |
|---|---|
| `NUCLEUS_HOST` | Public hostname. Single source of truth for the nginx `server_name`, the prod link, Spotify OAuth URLs, and each Vite dev server's `allowedHosts`. Defaults to `nucleus.olm-altair.ts.net`. Dev access is via `localhost` regardless. |
| `MONGODB_URI` | MongoDB connection string |
| `VITE_TMDB_API_KEY` | TMDb API key for Watchlist metadata — free at [themoviedb.org](https://www.themoviedb.org/settings/api) |
| `SPOTIFY_*` | Spotify app credentials + OAuth redirect overrides (default to `https://$NUCLEUS_HOST/...`) |
| `MINIO_*` | MinIO object-store credentials and public URL (Orbit) |

`MONGO_IMAGE` is set automatically by `dev`/`nucleus` based on CPU AVX support
(MongoDB 5+ needs AVX; older CPUs fall back to `mongo:4.4`).
