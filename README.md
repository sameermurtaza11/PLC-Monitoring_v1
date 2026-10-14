# PLC Monitoring — Version 01

Instrument → PLC → Modbus TCP → Go acquisition → PostgreSQL → Gin → HTMX → ECharts

## Layout

```
cmd/server/main.go          starts DB pool, acquisition manager, Gin server
internal/config/            env / .env settings (DB URL, HTTP port) — NOT PLC config
internal/store/             all SQL (config load, scan writes, dashboard, history, CRUD)
internal/plc/               Modbus: address parsing (1xxxx/3xxxx/4xxxx), block reads, TCP client
internal/acquisition/       Manager (one worker per PLC) + Worker (scan loop) + scaling
internal/web/               Gin handlers, templates/, static/ (htmx + echarts served locally)
migrations/001_v01_schema.sql
migrations/002_digital_inputs_and_cleanup.sql
sql/demo_multi_plc.sql      demo: 3 PLCs on 127.0.0.1/.2/.3 with AI + DI points
```

## Point types (derived from the address — Modbus convention)

| Address | Modbus type | Size | Function | Value in the app |
|---|---|---|---|---|
| 10001–19999 | Discrete Input (DI) | 1 bit | FC02 | 0 / 1, shown as state text (e.g. STOPPED / RUNNING) |
| 30001–39999 | Input Register (AI) | 16 bit | FC04 | raw 0–65535 scaled to pv_min…pv_max |
| 40001–49999 | Holding Register | 16 bit | FC03 | raw 0–65535 scaled to pv_min…pv_max |

`PV = pv_min + (raw / 65535) × (pv_max − pv_min)` — digital inputs are not scaled.

Digital inputs:
- `state0_text` / `state1_text` — what 0 and 1 mean (OFF/ON, CLOSED/OPEN, …)
- `alarm_state` — NULL = no alarm, 0 or 1 = alarm in that state
- History is stored on **every state change** (+ one row per minute), not every scan.
  Nothing is lost: each transition is stored with the timestamp of the scan that saw it.

## Database tables (after 002)

| Table | Purpose |
|---|---|
| `plc_metadata` | one row per PLC: name, IP, port, unit id, scan interval, timeout, enabled |
| `pv_metadata`  | one row per PV: `plc_id` + register, name, system, equipment, unit, scaling, history deadband, state texts (alarm limits and priorities are NOT here: see `alarm_metadata`) |
| `pv_latest`    | last value + quality per PV (dashboard reads only this) |
| `pv_history`   | timestamped samples, PK `(pv_id, ts)` |

Constraints: `UNIQUE(plc_id, register_address)`, `UNIQUE(plc_id, pv_name)`, `pv_max > pv_min`,
register must be 10001–19999, 30001–39999 or 40001–49999, `alarm_state IN (0,1)`.

Old prototype tables `pv_mapping`, `pv_data`, `raw_data` are removed by 002
(matching `pv_data` rows are first moved into `pv_history`).

## Setup / upgrade (Windows, VS Code terminal, inside D:\PLC_Monitoring)

```powershell
# 0. Backup of the old tables (002 drops them)
pg_dump -U postgres -d plc_monitoring -t pv_mapping -t pv_data -t raw_data -f legacy_backup.sql

# 1. Schema (both safe to re-run)
psql -U postgres -d plc_monitoring -f migrations/001_v01_schema.sql
psql -U postgres -d plc_monitoring -f migrations/002_digital_inputs_and_cleanup.sql

# 2. Demo: 3 PLCs with different IPs (AI + DI)
psql -U postgres -d plc_monitoring -f sql/demo_multi_plc.sql

# 3. Simulated PLCs 127.0.0.1 / .2 / .3 : 502
cd D:\plc_simulator
.\plc_service.ps1 start
cd D:\PLC_Monitoring

# 4. App
go test ./...
go run ./cmd/server          # → http://localhost:8080
```

`.\plc_service.ps1 status` / `stop` / `restart` manage all simulated PLCs.
Every 127.x.x.x address is local loopback on Windows, so each simulated PLC has its own IP
without extra network adapters. A real PLC just needs its real IP in `plc_metadata`.

Optional: `copy .env.example .env` and set `DATABASE_URL` if your DB needs a password.

## Acquisition behaviour

- One goroutine per enabled PLC; a failed PLC only affects its own PVs (`COMM_FAIL`).
- Reconnect with backoff 1 s → 2 s → … → 30 s.
- Adjacent points of the same type are read in one request
  (registers: ≤125, gaps ≤8; discrete inputs: ≤2000 bits, gaps ≤64).
- Modbus exception (e.g. illegal address) → `READ_FAIL`, connection kept.
- Each scan = one DB transaction (latest upsert + history insert + failures).
- DB down → history buffered in memory (≤ 200 000 samples per PLC), written on recovery.
- Analog `deadband > 0` → history only when value moves ≥ deadband (+ one row per minute).
- Saving in `/config` reloads acquisition immediately; direct SQL edits within `CONFIG_RELOAD` (10 s).
- Dashboard status: `No data`, `COMM_FAIL`, `READ_FAIL`, `STALE`, `HIGH`, `LOW`, `ALARM` (digital), `Normal`.

## Web UI

| Page | What it does |
|---|---|
| `/` Dashboard | latest value of every PV (DI as state text), refresh 2 s; **More** opens the PV detail |
| PV detail | metadata; analog: live gauge; digital: status lamp; history trend + table; compare up to 4 PVs |
| `/config` | add / edit / delete PLCs and PVs; the PV form switches between analog and digital fields by address |

### Trend rules
- Ranges 15m … 7d, zoom, refresh 10 s. >1000 samples → averaged buckets with min/max
  (for a DI, Avg = fraction of time ON).
- Digital PVs are drawn as step lines.
- Same unit → real values. Different units, or digital + analog → % of each PV's range
  (digital: 0 % = OFF, 100 % = ON). One y-axis only.

### Delete rules
- PLC with PVs → blocked. PV with history → blocked unless "Delete PV and its history" is confirmed.
  Disabling keeps the history.

## Login, roles and users

Every page needs a login (except `/login`, `/healthz` and the static files).

| Role | Dashboard, PV detail, history | Configuration (PLC / PV) | User management |
|---|---|---|---|
| operator | yes | no | no |
| supervisor | yes | yes | no |
| admin | yes | yes | yes |

Access is enforced on the server for every request, from one table: `internal/auth/rules.go`.
A route that is not listed there is closed to everyone except administrators, and a test
(`internal/web/routes_test.go`) fails if a route has no rule.

### First setup (there is no default account)

```powershell
$env:PGCLIENTENCODING = "UTF8"
psql -U postgres -d plc_monitoring -f migrations/003_users.sql      # creates users + sessions (safe to re-run)
go run ./cmd/adduser -user YOURNAME -role admin                      # asks for the password (8-128 chars)
go run ./cmd/server
```

`adduser` reads the password from the `PLC_USER_PASSWORD` environment variable or asks for it;
it is never a command-line argument. After that, administrators manage users in
**Configuration → User Management** (create, edit, delete; the last administrator can be neither
deleted nor demoted).

### Sessions and cookies (`.env`)

| Setting | Default | Meaning |
|---|---|---|
| `COOKIE_SECURE` | `false` | set `true` when served over HTTPS (keeps the cookie off plain HTTP) |
| `COOKIE_SAMESITE` | `lax` | `lax` or `strict` |
| `SESSION_HOURS` | `12` | how long a login lasts |

The cookie is always HTTP-only and holds only a random token; the server stores its hash. The
user's role is read from the database on every request, so a role change or a deleted account
applies immediately. Logging in always issues a new session identifier; logout destroys the
session on the server. Forms and HTMX requests carry a CSRF token.

### Known limitation: passwords are stored in plain text (Version 01)

`users.password` holds the password as typed, by decision, and administrators can see it in the
edit form. Anyone who can read the database or a backup can read every password. All reading and
writing goes through `internal/auth/password.go` (`StorePassword` / `VerifyPassword`), so moving to
hashed passwords later means changing that file and migrating the existing rows.
`last_online` is the time of the last successful login, not live presence.

### Tests

`go test ./...` runs the unit tests. The database test for user management is opt-in and only
runs on a database whose name contains `scratch` or `test` (it empties `users`):

```powershell
$env:PLC_TEST_DATABASE_URL = "postgres://postgres@localhost:5432/plc_users_scratch"
go test ./internal/store/ -run Integration -v
```

## Alarm settings and test alarms

- `migrations/005_move_alarm_columns.sql` moves the old per-PV alarm settings (`priority`, `alarm_low`, `alarm_high`,
  `alarm_state`) out of `pv_metadata`: they are copied to `alarm_metadata` as unapproved drafts, then the columns are dropped.
  `pv_metadata.deadband` stays: it is the *history* deadband used by the storage module. Back up first (`pg_dump`).
- `scripts/seed_test_alarms.sql` configures sample alarms for every PV of the demo plant so the alarm system can be tried
  against live data (stored as approved, approver "test-data seed"). Test data only — replace with rationalized alarms.
- Dashboard status and the PV gauge bands come from the alarm tables; the menu badge on **Alarms** shows how many alarms
  are waiting for an acknowledgement (unacknowledged, or returned to normal but not yet acknowledged).

## PV and alarm configuration in one form

The Configuration page edits a PV and its alarms together (`Add PV` / `Edit`): the PV fields first, then an **Enable Alarms**
switch. Analog PVs get four independent alarms (HIHI, HI, LO, LOLO), digital PVs one discrete alarm (you choose the *normal* state;
the other state is the alarm). The data stays in two tables and is written in **one transaction**; a failure in either part changes nothing.

- Editing updates the existing alarm definitions (one per PV and type, never duplicated). Switching an alarm (or all alarms) off keeps the
  definition and its history; abnormal occurrences are ended and recorded as "taken out of service". The alarm engine also does this for any
  alarm that stops being evaluated (disabled, approval withdrawn, PV deleted).
- New and changed alarms are drafts until an administrator approves them (Alarm Configuration, or the "Approve these alarms now" box for administrators).
- **Delete PV** shows a warning and needs an explicit confirmation. Alarm history, audit records, outstanding alarms and (unless you also delete
  them) stored values are never erased: a PV with any of these is **archived** (`pv_metadata.deleted_at`, migration `006_pv_archive.sql`) — switched off,
  hidden, its name and register free again — otherwise it is erased completely together with its alarm configurations.
