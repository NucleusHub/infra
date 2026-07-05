# Nucleus Ecosystem — Bug & Edge-Case Report

_Generated 2026-06-22. Audit of all backends (5 Node servers + Anchor/Go + shared auth), Vue clients, widgets, and infra. Findings verified against source; the highest-impact claims were re-checked at the cited lines._

This complements `SECURITY_AUDIT.md` — it focuses on **bugs and edge cases** (which happen to include security defects), with file:line and a one-line fix each.

---

## Cross-cutting themes (read first)

1. **Public MinIO bucket undermines Orbit's entire authz/password model.** `ensureBucket` grants `s3:GetObject` to `Principal:*`, so any object is fetchable by URL with no auth. Per-file passwords, folder locks, and `/save` authorization are all cosmetic for *read* access. This is the root cause behind several Orbit "High" findings below.
2. **Default JWT secret (`'nucleus-jwt-secret'`) in every Node service.** `process.env.JWT_SECRET || 'nucleus-jwt-secret'` in auth-server, orbit, goal-calendar, watchlist, pulse — and `JWT_SECRET` also defaults in `infra/docker-compose*.yml`. Any deployment that doesn't set the env var signs/verifies tokens with a repo-known secret → forgeable admin tokens. **Fix once, everywhere: fail fast at boot if `JWT_SECRET` is unset.**
3. **Mass-assignment via `req.body` spread / direct assignment** recurs across goal-calendar, watchlist, pulse, orbit, and echo — letting clients overwrite `profileId`/ownership or write internal fields. Whitelist updatable fields per route.
4. **No rate limiting at the edge** (nginx has no `limit_req`), and the auth-server's in-memory limiter is per-process and proxy-blind (`req.ip` without `trust proxy`). Brute-force protection is effectively absent.
5. **Read-then-write races without transactions** (last-admin demotion, DM dedupe, folder-move cycle check, first-run setup) — each can produce an inconsistent state under concurrency.

---

## Fix status (2026-06-22)

The four **Critical** findings were addressed as follows:

- ✅ **Echo `chat:join` / typing no-membership** — `apps/echo/server/realtime/socket.js`: added an `isMember()` check gating `chat:join`, `typing:start`, `typing:stop`.
- ✅ **Auth-server PIN-less admin + stale-token role** — `core/auth-server/routes/index.js`: POST `/profiles` now re-checks the actor's role from the DB (not the JWT claim); admins require a PIN at creation and promotion; an admin's PIN can't be cleared; login refuses any legacy PIN-less admin. (PIN-less *regular* profiles remain intentional — picker login.)
- ✅ **Spotify unauthenticated proxy** — `widgets/spotify/server/`: added a zero-dependency `requireAuth` (HS256 verify of `nucleus_token`) gating all data/control routes; `/login` & `/callback` stay open (SameSite=strict cookie can't ride Spotify's cross-site redirect). `JWT_SECRET` wired into the Spotify compose env. _Token store is still process-global (single-account) — per-profile scoping is a feature, not a bug fix._
- ⏳ **Orbit public bucket** — DEFERRED pending an approach decision (see note). It's architectural (no presign infra exists today; the client uses raw `/orbit-uploads/<key>` paths in img/video/download/fetch), can't be verified without running the stack, and risks breaking all file serving. Not shipped blind.

---

## core/auth-server (shared auth — highest blast radius)

- **[Critical] PIN-less profiles log in with zero credentials + stale-token admin checks.** `routes/index.js:321` (`if (profile.pin)` — no PIN means no check) and `:96-108` (admin gated on the raw JWT `role` claim, not DB). A PIN-less admin = unauthenticated takeover; a demoted admin keeps power for 30 days. → Require a PIN for all non-guest profiles; re-check role from DB via `requireAdmin`.
- **[High] Login limiter is per-process, unbounded, and proxy-blind.** `routes/index.js:25-42,309-312`; `req.ip` with no `trust proxy`. Behind nginx all clients share one IP → either bypassable or shared-IP lockout; the `Map` never evicts (slow leak). → Set `trust proxy`, back the limiter with Redis + key eviction.
- **[Medium] Last-admin demotion race.** `routes/index.js:164-172` — count-then-update with no transaction; two concurrent demotions can drop admin count to zero. → Atomic conditional update / transaction.
- **[Medium] Profile delete teardown is non-atomic.** `routes/index.js:281-304` — group `$pull` + `UserOverride.deleteMany` + `deleteOne` with no session; partial failure strips membership but leaves the profile. → Wrap in a Mongo transaction.
- **[Medium] Legacy migration is non-idempotent and bootstraps a hardcoded PIN `1339`.** `utils/migration.js:17-46`; no unique index on `_migrations.name`, concurrent startups can double-run, and it seeds a well-known admin PIN. → Unique index + insert marker first; PIN from env.
- **[Medium] Guest profile gets a full 30-day session with no credentials.** `routes/index.js:308-353` — no `isGuest` handling; escalates if a guest is ever promoted to admin. → Handle guest login explicitly; forbid promoting guests.
- **[Medium] `colorFromName` can yield `undefined` color → create throws.** `models/Profile.js:9-13` — `Math.abs(INT_MIN) % 12` is negative, `COLORS[-8]` is undefined for a `required` field. → Use `>>> 0` instead of `& 0xffffffff` + `Math.abs`.
- **[Low] CORS reflects any origin with credentials.** `index.js:12` (`origin:true, credentials:true`). Mitigated by `sameSite:'strict'` cookie, but defense-in-depth gap. → Allow-list origins.
- **[Low] One-time/temp PINs stored in plaintext (`pinTempPlain`) + 4-hex-char space.** `routes/index.js:122,131,219-222`, `models/Profile.js:25`. Brute-forceable and recoverable from a DB dump. → Don't persist plaintext; widen PIN space.
- **[Low] `/logout` requires auth**, so an expired token can't clear its own cookie. `routes/index.js:408-411`. → Drop `requireAuth` from logout.

## apps/orbit (file manager)

- **[Critical] "Password-protected" / private files are publicly downloadable.** `routes/files.js:102-114` (public bucket policy) + `:56-62` (only `url` withheld for protected files). Object URL is `/${BUCKET}/${objectKey}`, directly fetchable by anyone, bypassing `/unlock` and folder `/verify` entirely. → Serve objects through an authenticated route or scoped presigned URLs; remove the `Principal:*` grant.
- **[High] Protected files still leak `objectKey`** in their serialized JSON. `routes/files.js:60` — the client can reconstruct the public URL itself. → Omit `objectKey` (and URL-deriving fields) for protected items.
- **[High] `/files/:id/save` copies any file by ID with no grant check.** `routes/files.js:251-291` — intentional (documented) but lets any authenticated user clone any other user's/group's file by enumerating ObjectIds. → Require a signed share grant before copying.
- **[High] Regex/ReDoS injection in file search.** `routes/files.js:132` — raw `search` into `{ $regex }` unescaped (folders.js:142 escapes correctly). → Escape input like folders.js does.
- **[Medium] Move endpoints transfer ownership / cross-scope with only `canView` on the destination.** `routes/files.js:315-339`, `routes/folders.js:221-262` — a group member can privatize shared files or re-home items into any group they can merely view. → Require `canEdit` on destination; preserve `ownerId` across group boundaries.
- **[Medium] Folder-move cycle check is racy/non-atomic.** `routes/folders.js:234-256` — concurrent "A→B / B→A" moves can both pass and create a cycle. → Transaction + atomic re-validation, or materialized path.
- **[Medium] Recursive delete/rescope and `buildBreadcrumbs` have no cycle/depth guard.** `routes/folders.js:18-25,63-73,297-308` — a cycle (or very deep tree) → infinite recursion / stack overflow; breadcrumbs also leak out-of-scope ancestor names. → Visited-set + max-depth; scope ancestor lookups.
- **[Medium] Rename/create assign `req.body` fields with no validation.** `routes/files.js:307`, `routes/folders.js:163-178,201` — empty/object/oversized names → 500 or poisoned MIME logic. → Validate string/trim/length.
- **[Medium] Content-type spoofed via filename; stored HTML/SVG served same-origin = stored XSS.** `routes/files.js:202-209`, `utils/mime.js:71-77`. → Force `Content-Disposition: attachment` / sanitize SVG/HTML, or separate origin.
- **[Low] Group/profile teardown trusts raw id params in queries (admin-only).** `routes/groups.js:21-66`, `routes/profiles.js:21-37`. → Cast/validate ObjectIds.
- **[Low] Write into a password-protected folder doesn't re-check the password.** `routes/files.js:197-204`, `routes/folders.js:104` — upload/create/move only check `canView`. → Require the folder password on writes too (or document as view-only UX).

## apps/echo (real-time messaging)

- **[Critical] `chat:join` socket event has no membership check.** `realtime/socket.js:84` — `socket.on('chat:join', ({chatId}) => socket.join(...))`. Any authenticated user can join any chat room from the console and receive all live messages/typing. → `await Chat.exists({ _id: chatId, members: userId })` before join.
- **[High] Message payload is unvalidated** (no required field, no length cap). `services/messageService.js:25-42` — empty or multi-MB messages persisted and fanned out. → Validate per-type, cap length.
- **[High] `typing:start/stop` publish to arbitrary chats without membership check.** `realtime/socket.js:70-75` — spoof typing indicators (server-trusted `userId`) into any chat. → Gate on membership.
- **[Medium] DM dedupe race creates duplicate 1:1 chats.** `routes/chats.js:60-65` — findOne→create non-atomic, no unique index on the member pair. → `findOneAndUpdate(...{upsert:true})` on a sorted member key + unique index.
- **[Medium] `/api/echo/registry` endpoints are unauthenticated.** `routes/registry.js` (no `requireAuth`; **confirmed**) — leaks full app inventory/versions/actions. → `router.use(requireAuth)`.
- **[Medium] Removed group member isn't force-disconnected from the room.** `routes/chats.js:109-126` — keeps receiving live events until reload. → `io.in(userRoom(target)).socketsLeave(chatRoom(chatId))`.
- **[Medium] Unvalidated `chatId` → unhandled CastError 500.** `routes/messages.js:25-28`, `services/messageService.js:73-77`. → `isValidObjectId` guard + try/catch.
- **[Medium] `before` pagination cursor unvalidated** → `new Date('garbage')` silent wrong page. `services/messageService.js:75`. → Reject `isNaN(getTime())`.
- **[Medium] Unread counter double-counts (Redis `incrUnread` + client `unread:bump`)** can desync across tabs/reloads. `services/messageService.js:49-53`, `socket.js:113-117`. → Carry authoritative count in the bump or read from Redis on load.
- **[Low] Chat create accepts arbitrary/unvalidated `members` IDs and client `kind`.** `routes/chats.js:55-68`. → Validate each member is a real Profile.
- **[Low] `presence:update` broadcast to every socket** (`io.emit`, socket.js:123) — presence leak across unrelated users. → Fan out only to shared-chat users.
- **[Low] Fire-and-forget `broadcastUpsert` races `systemNotice` ordering.** `routes/chats.js:66,83-84,102-104`. → Await publish; order membership before the notice.
- _Verified safe:_ renderers use text interpolation/`linkify`, no `v-html` on message content.

## apps/goal-calendar / watchlist / pulse

**goal-calendar**
- **[High] Mass-assignment on PATCH `/:id`** — whole `req.body` into `findOneAndUpdate`; can set `profileId` to transfer a goal to another account. `routes/goals.js:44-48`. → Whitelist fields.
- **[Medium] `toggle-date` pushes unvalidated `date` into `completedDates`** (objects/`$gt`/garbage). `routes/goals.js:56-65`. → Validate `/^\d{4}-\d{2}-\d{2}$/`.
- **[Medium] DST off-by-one in custom day/week recurrence** — raw ms modulo across DST. `client/src/views/CalendarView.vue:123,125`. → `Math.round(diff/MS_DAY) % n`.
- **[Low] `custom` repeat with `customInterval` 0/null** → `% 0` = NaN, goal never shows. `models/Goal.js:10`. → Require `customInterval >= 1`.
- **[Low] Cross-app watchlist PATCH unguarded** in `toggleCompletion` (rejects whole promise if item deleted). `CalendarView.vue:269-275`. → try/catch (best-effort).

**watchlist**
- **[High] Mass-assignment on PATCH** can overwrite `profileId`. `routes/watchlist.js:60-63`. → Whitelist.
- **[Medium] `/api/upload` has NO auth** — mounted on the bare app outside the protected router (**confirmed**); unauthenticated 5 MB image uploads, publicly served from `/uploads`. `server/index.js:40-43`. → Apply `requireAuth`.
- **[Medium] TMDB API key shipped in client bundle** (`VITE_TMDB_API_KEY` as `?api_key=`). `client/src/api/tmdb.js:2,7`. → Proxy through the server.
- **[Low] `runRefresh` iterates a live-mutating array; `refreshTotal` captured once.** `WatchlistView.vue:184,243`. → Snapshot first.
- **[Low] `rating`/`year` can persist NaN** from non-numeric input. `ItemFormModal.vue:174-178`. → `Number.isFinite` guard.

**pulse**
- **[High] Dashboard `PUT` overwrites `widgets` with zero validation** (no `Array.isArray`, Mixed `config` = unbounded JSON). `routes/dashboard.js:33-42`. → Validate array + cap size.
- **[Low] Dashboard fetch doesn't check `res.ok`** — a 401/500 body assigned to state → `undefined.find` crashes. `client/composables/useDashboard.js:107-108`. → Throw on `!res.ok`.

## apps/anchor (Go — backups/restore)

- **[High] Partial restore leaves MinIO objects and Mongo metadata inconsistent.** `backups.go:401-418` — MinIO volume is wiped/re-extracted *before* the Mongo restore; if `restoreMongo` fails, only MinIO is restarted (no rollback) → Orbit split-brain (dangling refs/orphans). → Restore Mongo first or snapshot MinIO before wiping; swap only when both halves succeed.
- **[Medium] First-run `/setup` is racy.** `handlers.go:54-77`, `auth.go:39-42` — `NeedsSetup()` check and `SetPassword` aren't atomic; a racing request can overwrite the operator's password. → Hold the store mutex across check-and-set.
- **[Medium] Confirm tokens aren't bound to a session and are consumed on mismatch.** `confirm.go:39-51` — a leaked token is replayable within 60s by anyone; `Check` deletes before validating. → Bind token to session id; delete only on successful match.
- **[Medium] Stale `.partial` dir can be mixed into a new bundle.** `backups.go:225-255` — `MkdirAll` is idempotent over a crashed run's dir. → `RemoveAll` before `MkdirAll`; reject existing dest.
- **[Low] Rename-failure path leaks the `.partial` dir** (no cleanup). `backups.go:251-253`. → `os.RemoveAll(localWork)` on that branch.
- **[Low] SSE events goroutine holds an untimed Docker stream** per disconnected client. `handlers.go:199-209`, `docker.go:196-212`. → Close body on `ctx.Done()`.
- **[Low] `json.Decode` errors ignored** on create/restore/confirm endpoints. `backups.go:205,334`, `actions.go:76`. → Return 400 on malformed JSON.
- **[Low] `extractBackup` silently drops symlink/hardlink entries** — safe vs zip-slip, but silent data loss for unexpected archive shapes. `backups.go:554-556`. → Error or log on skip.
- _Verified safe:_ no shell/command injection (arg slices), restore target regex-gated against traversal, `mongorestore --nsInclude` correctly scopes to orbit collections, argon2id with constant-time compare, EXEC=0 honored.

## infra / widgets / hub

- **[Critical] Spotify proxy is unauthenticated + single global token store.** `nginx/nginx.conf:47-53` (no auth on `/api/spotify`) + `widgets/spotify/server/routes/spotify.js:5-9` — after the owner logs in once, any visitor can read/control their real Spotify account. → Gate `/api/spotify` behind `nucleus_token`; scope token per profile.
- **[High] Spotify OAuth callback has no `state`/CSRF validation and trusts `state` for redirect.** `spotify.js:66-77,106-110` — login-CSRF + open redirect. → Random server-stored state, verify, allowlist redirect.
- **[High] `sysinfo` widget runs `privileged: true` + `pid: host` + host root mounted.** `widgets/sysinfo/docker-compose.app.yml` — any RCE in this Node service = host root. → Drop `privileged`; `cap_drop: [ALL]`.
- **[High] Weak/defaulted secrets baked into compose** (`JWT_SECRET=nucleus-jwt-secret`, MinIO `nucleusadmin`/`nucleuschangeme`). `infra/docker-compose.yml:74,135-136`, `docker-compose.prod.yml`. → `${JWT_SECRET:?set me}`.
- **[Medium] MinIO API+console ports (9000/9001) published to the host.** `docker-compose*.yml` — reachable on LAN with default creds, bypassing nginx. → Keep internal / bind `127.0.0.1`.
- **[Medium] Open CORS (`*`) on `/orbit-uploads` and the registry.** `nginx.conf:32-39`, `infra/registry/index.js:43-46`. → Restrict to Nucleus origin.
- **[Medium] No nginx rate limiting** on `/api/auth`, `/api/spotify`, registry. `nginx.conf:110-116`. → Add `limit_req_zone` + `limit_req`.
- **[Medium] Default stack serves the hub via Vite dev server** (source maps/HMR exposed). `docker-compose.yml:23-42`. → Enforce prod compose for any exposed deployment.
- **[Medium] `position_ms` interpolated unvalidated into the Spotify upstream URL** (query injection). `spotify.js:161-166`. → Parse as a finite non-negative number.
- **[Medium] Echo widget renders registry `iconSvg` via `v-html`.** `widgets/echo/Widget.vue:259` — a malicious `icon.svg` dropped into `apps/`/`widgets/` = stored XSS. → Sanitize SVG or render via `<img>`.
- **[Low] Misc:** `create-app.js:146` writes literal `${JWT_SECRET:-…}` into JSON manifest; degraded-mode page links Anchor install to `example.com` (`nginx.conf:209`); Spotify album-art `:src` and Orbit widget deep-link IDs are unencoded; hub `allowedHosts` defaults to one hardcoded Tailscale host (`hub/vite.config.js:21`) → IP/localhost access 503s.
- _Verified safe:_ Echo `linkify` only matches `https?://` with `rel="noopener"`; sysinfo `/proc` parsing takes no request input; registry uses fixed dirs (no traversal).

---

## Suggested priority order

1. **JWT default secret** (one fix, all Node apps + compose) — forged-admin risk everywhere.
2. **Orbit public bucket** — collapses Orbit's whole authz/password model; also enables stored XSS.
3. **Spotify unauthenticated proxy** + **Echo `chat:join`/`typing` no-membership** — live data exposure/control with trivial triggers.
4. **PIN-less/guest credential-free login** + stale-token admin checks in auth-server.
5. **Mass-assignment sweep** (`profileId`/ownership) across goal-calendar, watchlist, pulse, orbit.
6. Concurrency/transaction fixes (last-admin, DM dedupe, folder-move, anchor partial-restore, setup race).
