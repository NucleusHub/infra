// infra/lib/healthy.js — exit 0 iff every service in the piped
// `docker compose ps --format json` output is running and (if it declares a
// healthcheck) healthy. Handles both the NDJSON (one object per line) and the
// JSON-array shapes that different Compose versions emit. Empty input → 1 (the
// stack hasn't been created yet).
const fs = require('fs');

let raw = '';
try {
  raw = fs.readFileSync(0, 'utf8').trim();
} catch {
  process.exit(1);
}
if (!raw) process.exit(1);

let rows = [];
try {
  const parsed = JSON.parse(raw);
  rows = Array.isArray(parsed) ? parsed : [parsed];
} catch {
  for (const line of raw.split('\n')) {
    const l = line.trim();
    if (!l) continue;
    try {
      rows.push(JSON.parse(l));
    } catch {
      /* ignore non-JSON noise */
    }
  }
}
if (!rows.length) process.exit(1);

for (const r of rows) {
  const state = String(r.State || '').toLowerCase();
  const health = String(r.Health || '').toLowerCase();
  if (state !== 'running') process.exit(1);
  if (health && health !== 'healthy') process.exit(1);
}
process.exit(0);
