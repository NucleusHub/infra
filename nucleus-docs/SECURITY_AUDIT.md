# Nucleus — Security & Bug Audit

**Date:** 2026-06-14
**Scope:** Full monorepo, with primary focus on **Orbit** (`apps/orbit`) and the shared auth/infra it depends on (`core/auth-server`, `infra/`). Other app servers (watchlist, goal-calendar, pulse, spotify, sysinfo, admin) were given a lighter pass.

This audit reviewed source only (no live testing). Severities reflect impact assuming the stack is reachable on a home/LAN network; some drop to "hardening" if the host is fully isolated.

---

## Executive summary

The most important finding is that **Orbit's entire access-control model is enforced only in the application layer, while the actual file bytes sit in a world-readable MinIO bucket exposed directly through nginx.** "Private" files, password-protected files, and password-protected folders are all effectively public to anyone who learns (or guesses) the object URL — no authentication, password, or group membership is checked when the object is fetched. The password features are, in their current form, advisory UI gating rather than real protection.

Secondary themes: a committed weak default `JWT_SECRET` that allows full auth/admin forgery if the env var is ever unset, MinIO exposed on the host with default credentials, no upload size limits, a globally-scoped login rate limiter, and orphaned storage on folder deletion.

| # | Severity | Issue | Location |
|---|----------|-------|----------|
| 1 | **Critical** | Public MinIO bucket → all files readable without auth; password protection bypassable | `apps/orbit/server/routes/files.js:70-82`, `infra/nginx/nginx.prod.conf` |
| 2 | **Critical** | Weak hardcoded `JWT_SECRET` fallback → token/admin forgery | `core/auth-server/.../auth.js:4`, all servers |
| 3 | **High** | MinIO S3 API + console published on host with default creds | `infra/docker-compose*.yml` |
| 4 | **High** | Regex injection / ReDoS in Orbit file search (unescaped) | `apps/orbit/server/routes/files.js:100` |
| 5 | **High** | NoSQL operator injection in watchlist filter | `apps/watchlist/server/routes/watchlist.js:10-13` |
| 6 | **Medium** | No upload size/type limit (disk-exhaustion DoS) | `apps/orbit/server/routes/files.js:61` |
| 7 | **Medium** | Recursive folder delete orphans MinIO objects (leak + stay public) | `apps/orbit/server/routes/folders.js:256-262` |
| 8 | **Medium** | Login rate limiter keyed on proxy IP → global, `trust proxy` unset | `core/auth-server/routes/index.js:28-37,189` |
| 9 | **Medium** | Spotify control endpoints unauthenticated + open redirect | `widgets/spotify/server/routes/spotify.js` |
| 10 | **Medium** | CORS `origin:true` + `credentials:true` everywhere | all `index.js` |
| 11 | **Low** | Auth cookie not `Secure`; served over plain HTTP | `core/auth-server/routes/index.js:14-19` |
| 12 | **Low** | `v-html` renders registry-supplied SVG | `core/AppIcon.vue:15` |
| 13 | **Low** | Misc input validation gaps (rename, toggle-date, stored URLs) | various |
| 14 | **Low** | Hardcoded migration PIN `1339`; first-admin bootstrap race | `core/auth-server/utils/migration.js:35` |

---

## Critical

### 1. Orbit files are world-readable; password protection is bypassable

`ensureBucket()` applies a bucket policy granting `s3:GetObject` to `Principal: { AWS: ["*"] }` over the entire bucket:

```js
// apps/orbit/server/routes/files.js:70-82
Statement: [{ Effect: 'Allow', Principal: { AWS: ['*'] }, Action: ['s3:GetObject'],
             Resource: [`arn:aws:s3:::${BUCKET}/*`] }]
```

nginx then proxies `/orbit-uploads` straight to MinIO with `Access-Control-Allow-Origin: *` and **no authentication** (`infra/nginx/nginx.prod.conf`, `nginx.conf`). The file URL is a static path: `fileUrl()` returns `/${BUCKET}/${objectKey}` (`files.js:49-51`).

Consequences:
- **Any unauthenticated person who has a file URL can download it.** URLs are handed out in API responses, can be copied/shared/logged/cached, and the only secret protecting them is the random UUID in the key.
- **File "password protection" provides no real confidentiality.** `serializeFile` merely omits the `url` field for protected files (`files.js:53-59`), and `/files/:id/unlock` returns that same permanent public URL after a password check (`files.js:238-250`). Anyone who saw the URL before a password was set — or who guesses the key — bypasses the password entirely. The password also never re-encrypts or moves the object.
- **Folder password protection is likewise advisory.** `/folders/browse` gates *listing* on `x-folder-password` (`folders.js:91-96`), but the files it would list are served from the same public path regardless.
- **Cross-tenant exposure at the storage layer.** Personal-file keys are `uploads/<profileId>/<uuid>` and group keys `uploads/group/<groupId>/<uuid>` (`files.js:152`). profileIds/groupIds are returned by the API, so the only entropy is the per-object UUID; the app-layer `canView`/`canEdit` checks (which are themselves correct) are simply not in the data path.

**Remediation:** Do not make the bucket public. Remove the `PutBucketPolicy` public grant and stop proxying `/orbit-uploads` directly. Serve downloads through the authenticated Orbit server (stream from S3 after `canView` + password checks), or issue short-lived presigned URLs per request (the memory note references a "two-S3-client presigned URL pattern" — that pattern should replace the static public path). For real password protection, gate the download in the app and consider that the password must be required on every fetch, not once.

---

### 2. Weak hardcoded `JWT_SECRET` fallback → full auth/admin forgery

Every service falls back to a secret that is committed to the repo:

```js
const secret = () => process.env.JWT_SECRET || 'nucleus-jwt-secret'
```

Found in `core/auth-server/middleware/auth.js:4`, `core/auth-server/routes/index.js:13`, `apps/orbit/server/middleware/auth.js:3`, and the watchlist/goal-calendar/pulse auth middlewares. The compose files also default it: `JWT_SECRET: ${JWT_SECRET:-nucleus-jwt-secret}`.

If `JWT_SECRET` is ever unset (it defaults silently — nothing fails), **anyone can mint a valid `nucleus_token` with `{ profileId, role: 'admin' }`** and impersonate any user, including admin, across every app. Because the value is public, the only thing standing between an attacker and full compromise is whether the operator remembered to set the env var.

**Remediation:** Fail closed — refuse to start if `JWT_SECRET` is unset (`if (!process.env.JWT_SECRET) throw`). Remove the literal from source and from the compose defaults. Rotate the secret (invalidates existing tokens, which is desirable).

---

## High

### 3. MinIO S3 API and console published on the host with default credentials

Both `infra/docker-compose.yml` and `docker-compose.prod.yml` publish MinIO directly:

```yaml
minio:
  ports: ["9000:9000", "9001:9001"]
  environment:
    MINIO_ROOT_USER: ${MINIO_ACCESS_KEY:-nucleusadmin}
    MINIO_ROOT_PASSWORD: ${MINIO_SECRET_KEY:-nucleuschangeme}
```

Port 9000 is the raw S3 API and 9001 is the admin console, both reachable from the host network. Combined with the default credentials `nucleusadmin` / `nucleuschangeme` (committed in `.env.example`), anyone on the LAN can log into the MinIO console and read/write/delete all objects. Even without creds, the public bucket policy (finding #1) means listing/reading objects via 9000 needs no auth.

**Remediation:** Don't publish 9000/9001 to the host — MinIO only needs to be reachable from the `orbit-server` container on the internal network. If the console is needed, bind it to localhost only. Force non-default credentials (fail if defaults are in use).

### 4. Regex injection / ReDoS in Orbit file listing search

`apps/orbit/server/routes/files.js:100`:

```js
if (search) query.filename = { $regex: search, $options: 'i' }
```

The `search` query param is passed **unescaped** into a Mongo `$regex`. A user can supply a catastrophic-backtracking pattern (e.g. `(a+)+$`) to pin the DB/CPU (ReDoS), or use regex metacharacters to alter matching. Note the sibling endpoint `/folders/search` does this correctly — it escapes with `q.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')` (`folders.js:129`). The list endpoint should match.

**Remediation:** Escape the input the same way `folders.js` does, or use an anchored `$text`/exact match. Optionally cap input length.

### 5. NoSQL operator injection in watchlist filter

`apps/watchlist/server/routes/watchlist.js:10-13` reads `status`/`type` from `req.query` and assigns them directly into the Mongo filter. Express's default `qs` parser turns `?status[$ne]=x` into an object, so these become query operators (`$ne`, `$gt`, `$regex`, …). Queries are still scoped by `profileId`, so this is filter-bypass/data-disclosure within the user's own items rather than cross-tenant — but the values should be validated as strings.

**Remediation:** `if (typeof status === 'string') filter.status = status` (same for `type`); reject non-string. This pattern should be audited anywhere `req.query`/`req.body` flows into a Mongo query.

---

## Medium

### 6. No upload size or type limit (disk-exhaustion DoS)

`apps/orbit/server/routes/files.js:61` configures multer with only a `dest` and **no `limits`**, and nginx sets `client_max_body_size 0` for `/api/orbit` (unlimited). Any authenticated user can upload arbitrarily large or numerous files, filling `/tmp/orbit-uploads` and the MinIO volume. There is also no MIME/type allowlist. (For contrast, the watchlist uploader does enforce a 5 MB limit and `image/*` filter — Orbit should too.)

**Remediation:** Add `multer({ dest, limits: { fileSize: <cap>, files: <n> } })`, set a sane `client_max_body_size`, and reject unexpected content types.

### 7. Recursive folder delete orphans MinIO objects (and they stay public)

`deleteFolderRecursive` (`apps/orbit/server/routes/folders.js:256-262`) deletes `File` documents via `File.deleteMany(...)` and removes subfolders, but **never issues a `DeleteObjectCommand`**. Only the single-file `DELETE /files/:id` route (`files.js:252-262`) and the group teardown route delete S3 objects. So deleting any folder leaves all contained objects in MinIO forever — a storage leak, and because of finding #1 those orphaned objects remain publicly downloadable even though the user believes they deleted them.

**Remediation:** Collect object keys during the recursive walk and delete them from S3 (batch `DeleteObjects`) as part of the folder deletion.

### 8. Login rate limiter is effectively global; `trust proxy` not set

The brute-force guard keys on `req.ip` (`core/auth-server/routes/index.js:189`, limiter at `:28-37`). Express's `trust proxy` is **not** configured anywhere, and all traffic arrives via the nginx container, so `req.ip` is the proxy's address for every request. Result:
- The "10 attempts / 15 min" budget is shared across **all** users → a single attacker (or even normal load) can lock out everyone (availability), and per-attacker isolation doesn't exist.
- The keyspace is small: PINs are 4 hex chars (`PIN_RE = /^[0-9A-F]{4}$/`, `:49`) = 65,536 combinations; bcrypt slows guessing but the limiter doesn't meaningfully bound a determined attacker who can reset by waiting.

**Remediation:** Set `app.set('trust proxy', 1)` (and have nginx pass `X-Forwarded-For`), then key the limiter per real client IP and/or per `profileId`. Consider a longer PIN for sensitive profiles.

### 9. Spotify control endpoints unauthenticated + open redirect

`widgets/spotify/server` uses open CORS and no `requireAuth`; OAuth tokens live in a single module-level store, so any unauthenticated caller (or any web page) can drive `/play`, `/pause`, `/next`, `/seek` and read `/now-playing` for whoever linked last. The OAuth callback also derives its post-login redirect from the attacker-controllable `Referer`/`state` and `res.redirect`s to it with no allowlist (`spotify.js:67,108-110`) — an open redirect.

**Remediation:** Require auth on control routes; validate the redirect target against an allowlist.

### 10. CORS `origin: true` with `credentials: true` across services

`apps/orbit/server/index.js:13`, `core/auth-server/index.js:12`, and the watchlist/goal-calendar/pulse servers all reflect any `Origin` while allowing credentials. This is unsafe in general (any site gets a valid CORS grant for credentialed requests). The practical CSRF impact is reduced because the auth cookie is `SameSite=Strict` (`core/auth-server/routes/index.js:16`), but the config should still be an explicit allowlist rather than a reflector.

**Remediation:** Replace `origin: true` with an allowlist of the known front-end origins.

---

## Low / Hardening

### 11. Auth cookie is not `Secure`; app served over plain HTTP
`COOKIE` (`core/auth-server/routes/index.js:14-19`) sets `httpOnly` + `sameSite:'strict'` but **no `secure` flag**, and nginx serves both HTTP and HTTPS (`listen 80 default_server; listen 443 ssl;`). On a plain-HTTP access path the token can be sniffed on the network. Set `secure: true` and redirect all traffic to HTTPS.

### 12. `v-html` renders registry-supplied SVG
`core/AppIcon.vue:15` renders `iconSvg` via `v-html`. The registry (`infra/registry/index.js`) inlines each app/widget's `icon.svg` verbatim. Source is repo-controlled today, so risk is low, but any path that lets an SVG with embedded script reach this sink becomes stored XSS. Sanitize the SVG or render via `<img>`/`<use>`.

### 13. Input-validation gaps
- `PATCH /files/:id/rename` and `/folders/:id/rename` assign `req.body.filename`/`name` with no validation (`files.js:194`, `folders.js:188`) — empty/oversized/non-string values either 500 or pollute data. Trim, length-cap, and require a string.
- `goal-calendar` `toggle-date` pushes an unvalidated `req.body.date` into `completedDates` (`apps/goal-calendar/server/routes/goals.js:42-47`).
- Watchlist stores `posterUrl`/`watchLink`/`streamingLogo` as arbitrary strings (`models/WatchlistItem.js`) — validate scheme to avoid `javascript:`/`data:` URLs if rendered as links.

### 14. Hardcoded migration PIN and bootstrap race
- `core/auth-server/utils/migration.js:35` creates the `Honzyk` admin with PIN `1339` (bcrypt-hashed but a known constant) on the legacy-migration path. Force a reset on first login.
- `POST /profiles` bootstrap (`routes/index.js:86-108`) allows unauthenticated creation of the first admin when `adminCount === 0`; two concurrent requests could both pass the check and create two admins. Minor; guard with a unique constraint or atomic check.

### Reviewed — no issue found
- Orbit `canView`/`canEdit`/`childOwnership`/`loadEditable` logic is sound; move/rename/delete/password are correctly owner-gated and group↔personal moves are blocked. The IDOR surface is the storage layer (finding #1), not these checks.
- `folders.js` move correctly prevents cycles (self + descendant walk) and `/folders/search` correctly escapes its regex.
- watchlist/goals/pulse CRUD is consistently scoped by `profileId`/`userId` (no cross-tenant IDOR). sysinfo takes no user input (no command injection). watchlist upload enforces 5 MB + image-only (though that upload route lacks `requireAuth` — add it). admin client gates UI on role but relies — correctly — on server-side enforcement.

---

## Edge cases & runtime bugs

These are non-obvious behaviors that cause crashes, data corruption, hangs, or stale-access under specific sequences — distinct from the vulnerabilities above.

### Data integrity / corruption

- **E1 — Folder-cycle race hangs requests permanently (and can crash the server).** `PATCH /folders/:id/move` guards against cycles by walking the target's ancestors (`folders.js:221-227`), but the read and the `folder.save()` are separated by `await`s. Two concurrent moves — *A under B* and *B under A* — can each pass the check against pre-move state, then both commit, creating a parent cycle. Once a cycle exists:
  - `buildBreadcrumbs()` (`folders.js:50-60`) loops `while (current)` with **no depth guard** → the request hangs/spins forever, and every future `browse` of that subtree does too.
  - `deleteFolderRecursive()` (`folders.js:256-262`) recurses children with no guard → infinite recursion / stack overflow.
  Note `files.js`'s `pathOf` *does* have a `guard++ < 50`, so the guard was simply omitted in two places. **Fix:** add a depth cap to both walkers, and ideally make the move check + save atomic (or re-validate after save).

- **E2 — Duplicate group-root folders (race).** `ensureGroupRoot()` does find-then-create with no unique index (`scope.js:54-66`). Two members of the same group hitting `/folders/browse` at the top level simultaneously both see `null` and both `create` → two "Group - {name}" roots, and the UI shows the group twice thereafter. **Fix:** unique index on `{ groupId, isGroupRoot }` (partial) and upsert.

- **E3 — File deleted from DB but not from storage on partial failure.** `DELETE /files/:id` deletes the Mongo doc *first* (`files.js:256`) then the S3 object (`:257`). If the S3 call throws (MinIO down), the doc is already gone, the object is orphaned (and stays publicly reachable per finding #1), and the user gets a 500 as if nothing happened. **Fix:** delete the object first, or tolerate S3 failure after recording the orphan for cleanup.

- **E4 — Orphaned object when `File.create` fails after upload.** In `/upload`, the object is `PutObject`'d to S3 *before* the Mongo doc is created (`files.js:155-170`). If `File.create` throws (validation, DB blip), the `finally` only unlinks the tmp file — the S3 object leaks with no DB record. **Fix:** create the doc first or roll back the object on failure.

### Stale access after profile/group changes

- **E5 — Deleted or demoted users keep access for up to 30 days.** `requireAuth` in Orbit/watchlist/goal-calendar/pulse only verifies the JWT signature; it never checks the profile still exists or its current role (confirmed: token-only). The token lives 30 days. So a **deleted** profile can still read/write its Orbit files, and a **demoted** admin keeps `user` access, until expiry. Worse: **Orbit's `requireAdmin` reads `role` from the token, not the DB** (`groups.js:11-14`) — unlike auth-server's `requireAdmin`, which re-checks. A demoted admin can therefore still call the group-teardown endpoint (delete/transfer shared files) until their token expires. **Fix:** re-validate profile existence/role against the DB in `requireAuth` (or shorten token TTL + add a revocation check), and make Orbit's admin check DB-backed.

- **E6 — Self-demotion / deleting the last admin reopens unauthenticated bootstrap.** `POST /profiles` allows creating an admin with no auth when `adminCount === 0` (`routes/index.js:86-108`). Nothing prevents an admin from demoting themselves (`PATCH /profiles/:id` with `role:'user'`) or deleting the last non-guest admin (`DELETE /profiles/:id` only blocks Guest). If admin count hits 0, **anyone can then create a fresh admin without authenticating.** **Fix:** refuse to demote/delete the last remaining admin.

- **E7 — Removing a member, or toggling `sharedOrbit` off, orphans data.** A user removed from a group loses `canView` on files they uploaded there (the groupId leaves their `gset`) — they can no longer see or manage their own group files, though the files remain for other members. Toggling `sharedOrbit` off stops `ensureGroupRoot` from surfacing the root (`folders.js:111`), so all that group's files/folders still exist (and stay public in MinIO) but become unreachable in the UI with no cleanup. **Fix:** decide and implement an explicit policy (reassign on removal; warn/teardown on disabling sharedOrbit).

### Auth / login edge cases

- **E8 — PIN-less profiles = open login.** PIN is optional on profile creation. Any profile without a PIN (including an **admin** created without one) can be logged into by anyone — and `/profiles` advertises `hasPin` per profile, so an attacker knows exactly which to pick. PIN space is also tiny (4 hex = 65,536, case-insensitive). **Fix:** require a PIN for admin/non-guest profiles; consider longer PINs.

- **E9 — Legitimate users get locked out (global limiter).** Because the rate limiter keys on the proxy IP (finding #8) and counts *every* attempt including successful ones before reset, 10 logins from anyone in 15 minutes locks out the whole household. Per-IP/per-profile keying (after fixing `trust proxy`) resolves this.

### Input handling → 500s instead of clean errors

- **E10 — Invalid `ObjectId` anywhere → 500 + error leak.** Orbit passes `req.params.id` / `req.body.folderId` / `parentId` straight into `findById` (`files.js:95,147,211,240`; `folders.js:53,89,175,219`). A malformed id (`"abc"`) throws a Mongoose `CastError`, caught by the generic handler that returns `500 { error: err.message }` — leaking internal detail and using the wrong status. Auth-server validates with `isValidId` first; Orbit does not. **Fix:** validate ids up front (400) and return generic messages.

- **E11 — Missing required body fields → 500.** `POST /folders` with no `name`, `POST /files/upload` with no file (`req.file` undefined → `path.extname(req.file.originalname)` TypeError at `files.js:151`), and `PATCH .../rename` with no/empty value all surface as 500s rather than 400s. **Fix:** validate inputs before touching the model.

### Other

- **E12 — `GET /files?folderId=` bypasses the folder password.** The client navigates via `/folders/browse`, which enforces `x-folder-password` (`folders.js:91-96`), but the separate `GET /files` list endpoint (`files.js:87-106`) performs only `canView` — it never checks the parent folder's `passwordHash`. Anyone can list a protected folder's files (with their public URLs) by calling `/files?folderId=<id>` directly. Also, folder passwords don't cascade: navigating straight to an *unprotected child* of a protected folder skips the parent's password entirely. (Both are moot for confidentiality once finding #1 is fixed, but the inconsistency is real.)

- **E13 — 30-day cookie + non-`Secure` over HTTP** compounds E5: a token sniffed once on the plain-HTTP path is usable for a month.

## Recommended priority order
1. **#1 + #3** — stop serving files publicly and stop exposing MinIO; this is the core data-confidentiality breach.
2. **#2** — make `JWT_SECRET` mandatory and rotate it.
3. **#4, #5** — fix the injection sinks.
4. **#6, #7, #8** — upload limits, delete orphaning, rate-limiter scoping.
5. Remaining hardening (#9–#14).
