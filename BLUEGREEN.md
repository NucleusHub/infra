# Blue/Green Deployment

Nucleus deploys with a **blue/green** strategy: the new version is built and
health-checked in a completely separate stack, and production traffic only moves
once that stack is confirmed healthy. Downtime is limited to a single graceful
reverse-proxy reload.

## Topology

```
Internet :80/:443
   └─ nucleus-edge        always-on nginx; TLS; forwards ALL traffic to the active color
         │  proxy_pass http://$active   ($active = web-blue | web-green)
   ┌─────┴───────────────────────────────┐
nucleus-blue                         nucleus-green      only ONE serves traffic
  web nginx (:8081 debug)              web nginx (:8082 debug)
  registry, plugin-runtime,            registry, plugin-runtime,
  auth + all app servers               auth + all app servers
   └────────────────┬────────────────────┘
              nucleus-data            always-on: mongo, redis, minio + volumes
```

- **Two color stacks** (`nucleus-blue`, `nucleus-green`) alternate every deploy.
  Only one serves traffic; the other is the deploy target. They run
  simultaneously during a deploy, so they use different project names, different
  debug host ports (8081/8082), and per-color network aliases (`web-blue` /
  `web-green`).
- **One shared data stack** (`nucleus-data`) holds the stateful services
  (mongo/redis/minio). You cannot run two copies on one volume, so both colors
  share this one stack over the `nucleus-data-net` network. Its volumes are
  pinned to the original `nucleus_*` names — existing data is reused as-is.
- **One edge proxy** (`nucleus-edge`) owns `:80/:443` + TLS and forwards
  everything to whichever color is active. Switching = rewrite one include
  (`nginx/edge/active.inc`) and `nginx -s reload` — no restart.

Everything is generated from the app/widget manifests by `nucleus generate`
(`docker-compose.{data,stack,edge}.yml`, `nginx/stack/default.conf`,
`nginx/edge/default.conf`). Do not edit those by hand.

## Commands

| Command | What it does |
|---|---|
| `infra/bluegreen-init` | **One-time** migration off the old single-project stack. The only step with (brief) downtime. |
| `infra/production` | Pull → build → start the inactive color → wait for health → switch traffic → stop the old color. |
| `infra/rollback` | Start the previous (still-intact) color → wait for health → switch back → stop the current one. |

## Deploy flow (`infra/production`)

```
Pulling latest changes...     # infra/update (git pull all repos)
Building images...            # nucleus build — frontends + regenerated configs
Starting Green...             # target color up on its own project + debug port
Waiting for health...         # every service healthy + an end-to-end auth probe
Switching traffic...          # rewrite edge active.inc + graceful reload
Stopping Blue...              # old color stopped (NOT removed — kept for rollback)
Deployment complete.
```

The live color keeps serving through the pull, the build, and the new color's
startup + health checks. If the new color never becomes healthy, traffic is
**not** switched: the old color keeps serving, a clear error is printed, and the
failed color is left running for debugging (`curl localhost:8082`, or
`docker compose -p nucleus-green logs`).

## Rollback

Because `production` only *stops* the old color (never `down`), rollback is
trivial and fast: `infra/rollback` starts it again, waits for health, flips the
edge back, and stops the bad color.

## Configuration

- `HEALTH_TIMEOUT` (env, default `180`) — seconds to wait for a color to go
  healthy before failing the deploy.
- Debug ports and network names are constants in `infra/lib/bluegreen.sh`.

## State

- `infra/.stacks/active` — the color currently serving traffic.
- `infra/.stacks/<color>/srv/` — that color's static snapshot (its own copy of
  the built frontends, so building one color never disturbs the other).
- `infra/nginx/edge/active.inc` — the edge's live upstream (`set $active web-<color>;`).

All three are runtime state, not generated config, and are git-ignored.
