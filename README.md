# Nucleus — infra

Orchestration for the Nucleus stack: the Docker Compose definitions, nginx
config, the registry service, and the tooling that builds and deploys
everything. Apps and widgets live in sibling repos (`../apps/*`, `../widgets/*`)
and are auto-discovered from their manifests.

## Commands

All commands are run from the `infra/` directory (or by absolute path — they
resolve their own location).

| Command | What it does |
|---|---|
| `./dev` | Start the **dev** stack if it isn't already running (no-op if it is). |
| `./nucleus [--all] <compose args>` | Dev Compose passthrough — **app-scoped by default**. |
| `./production [--force] [-j N]` | Build all frontends and deploy the **prod** stack. |
| `./update [repo…]` | `git pull` across all (or named) Nucleus repos. |

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
