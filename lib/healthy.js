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
    } catch {}
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
