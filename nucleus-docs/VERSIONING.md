# Nucleus Versioning

Nucleus follows [Semantic Versioning](https://semver.org): `MAJOR.MINOR.PATCH`.

The project is in the **0.x development phase** — breaking changes are expected
until `1.0.0`, which will mark the first production-ready, stable release.

## Supported version format

```
0.1.0
0.9.0-beta.2
1.0.0-rc.1
1.0.0
1.1.0
2.0.0
```

Prerelease identifiers (SemVer suffixes only — never `Beta 1.0.0`):

| Channel | Suffix        | Example          |
| ------- | ------------- | ---------------- |
| Alpha   | `-alpha.N`    | `1.0.0-alpha.1`  |
| Beta    | `-beta.N`     | `1.0.0-beta.3`   |
| RC      | `-rc.N`       | `1.0.0-rc.2`     |
| Stable  | *(none)*      | `1.0.0`          |

The release channel is **derived automatically** from the suffix — see
`core/version.js` (`channelOf`, `channelLabel`).

## The three versions

Nucleus tracks three independent versions:

### 1. Nucleus platform version

The platform itself — Core, the Docker generator, SDK, APIs, manifest format,
and installer. Single source of truth, tracked in the **infra** repo:

```
infra/nucleus.json  →  { "version": "0.1.0", "manifestVersion": 1 }
```

Served to clients at `GET /api/registry/nucleus` (the registry mounts it
read-only). Shown in the Admin **Overview**.

### 2. Module version

Every **installable module** — apps, widgets, and future modules (themes,
language packs, scanner plugins, …) — carries its own independent SemVer
version in its manifest:

```json
{
  "id": "echo",
  "version": "0.8.1-beta.2",
  ...
}
```

Each module bumps its version on its own schedule.

### 3. Manifest version

An integer that lets the **manifest schema** evolve independently of module
versions:

```json
{ "manifestVersion": 1 }
```

## Compatibility

Every module manifest declares which platform versions it supports:

```json
{
  "compatibility": {
    "nucleus": ">=0.1.0 <1.0.0"
  }
}
```

This is groundwork for install/update-time compatibility checks. The range
grammar understood today (`core/version.js` → `satisfies`) is a space-separated
AND list of `>=`, `>`, `<=`, `<`, `=` comparators — enough for `">=0.8.0 <1.0.0"`.

## Displaying versions

Always display versions **v-prefixed**: `v0.8.0`, `v0.9.0-beta.2`, `v1.0.0`.
Use the shared helpers rather than formatting by hand:

- `core/version.js` — `parseVersion`, `formatVersion`, `channelOf`,
  `channelLabel`, `compareVersions`, `satisfies`.
- `core/VersionBadge.vue` — renders `vX.Y.Z` plus an auto-derived channel pill
  (Alpha / Beta / RC; stable shows no pill by default).

Surfaces today:

- **Admin → Overview** — Nucleus platform version + channel.
- **Admin → Apps / Widgets** — per-module version + channel badge.
- **Pulse** — widget versions appear only in the widget library (while
  customizing), never on the normal dashboard.

## Bumping versions

Each installable thing lives in its own git repo, so **the repo you push maps
directly to the version you bump**:

| Push target        | Bump                                          |
| ------------------ | --------------------------------------------- |
| `apps/<app>`       | that app's `version` (`nucleus.app.json`)     |
| `widgets`          | the changed widget(s)' `version`              |
| `core` / `hub` / `infra` | the platform version (`infra/nucleus.json`) |

While in 0.x:

- **patch** (`0.1.0 → 0.1.1`) — bug fixes, no behavior change
- **minor** (`0.1.0 → 0.2.0`) — new features **and** breaking changes
- **prerelease** — `-alpha.N` / `-beta.N` / `-rc.N` when working toward a release

Use the helper instead of hand-editing (it keeps the file byte-identical apart
from the version and enforces the SemVer rules):

```
nucleus bump <module> <part> [channel]
  part:    major | minor | patch | pre | release
  channel: alpha | beta | rc   (optional)

nucleus bump echo minor            0.1.3        -> 0.2.0
nucleus bump echo minor beta       0.1.3        -> 0.2.0-beta.1
nucleus bump echo pre              0.2.0-beta.1 -> 0.2.0-beta.2
nucleus bump echo pre rc           0.2.0-beta.2 -> 0.2.0-rc.1   (promote channel)
nucleus bump echo release          0.2.0-rc.1   -> 0.2.0
nucleus bump nucleus minor         0.1.0        -> 0.2.0        (platform)
```

**Workflow:** decide on the bump *before pushing a repo*, run `nucleus bump`,
then commit — the bump edits a manifest file, so it has to be committed or it
never reaches the remote. Preferred order:

1. `nucleus bump <module> …` **before** committing, so the version change rides
   in the same commit as the work; or
2. if the work is already committed, make a follow-up commit for the bump (or
   amend) **before** pushing.

Never push with an uncommitted version bump left in the working tree.

## Validation

The Docker build (`infra/tool`) **warns** (never fails) when a module manifest
is missing a `version`, `manifestVersion`, or `compatibility.nucleus`, or when
the version isn't valid SemVer. This keeps builds unblocked while the ecosystem
adopts versioning.

## Future-proofing

The pieces above are intentionally modular so later features can build on them
without a redesign: update checking, dependency resolution, compatibility
validation, rollbacks, database migrations, release channels, and automatic
updates.
