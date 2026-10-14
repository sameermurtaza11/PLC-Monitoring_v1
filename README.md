# PLC Monitoring

Instrument → PLC → Modbus TCP → Go acquisition → in-process sample bus → storage · live data · alarm engine → Gin + HTMX web UI (PostgreSQL)

A Go web application that reads PLCs over Modbus TCP, stores the values in PostgreSQL, shows them live and runs a basic
alarm-management system designed along ISA-18.2 principles (rationalized alarm definitions, acknowledgement, shelving,
event history, monitoring statistics).

## Architecture

The process is split into independent modules that talk through Go channels (no Redis, no database polling between modules).

```
PLC ─► acquisition ──► sample bus ──► storage     PostgreSQL (pv_latest, pv_history, failures)
        (one worker      (bounded       ├─► live data   latest value per PV in memory ─► SSE ─► browsers
         per PLC)         queue per     └─► alarms      alarm engine ─► active_alarms / alarm_history
                          consumer)
```

- **Acquisition never waits** for the database, the alarm engine or a browser. Every sample (value, quality, timestamp,
  sequence number) is published on the bus; each consumer has its own bounded queue and an explicit overload policy, and
  every drop is counted and logged.
- **Storage** batches writes, buffers up to 500 000 samples while the database is down, retries with backoff and
  reports any loss. History rules (deadband, one row per minute) are unchanged.
- **Live data** keeps the latest sample of every PV in memory and pushes changes to the browser with Server-Sent Events
  (`/events`); `pv_latest` is only read once at start-up.
- **Alarms** evaluate samples directly: limits, deadband, on/off delays on the sample timestamps; bad data neither
  activates nor clears an alarm. Occurrences and their history rows are written in one transaction. One engine runs per
  database (advisory lock).
- `GET /status/pipeline` (administrators) shows queue sizes, drops, scan overruns and database health.

## Layout

```
cmd/server/            starts the modules and the web server (graceful shutdown)
cmd/adduser/           creates a login account (first administrator)
internal/config/       env / .env settings (database URL, HTTP port, cookies, shelving limit)
internal/plc/          Modbus: address parsing, block reads, TCP client
internal/acquisition/  manager + one worker per PLC; publishes samples
internal/bus/          sample bus: per-consumer bounded queues, drop policies, statistics
internal/storage/      database writer: batching, retry, buffering
internal/live/         latest values in memory
internal/alarms/       alarm engine, alarm list view, configuration validation
internal/auth/         users, sessions, roles, the route access table (rules.go)
internal/store/        all SQL
internal/web/          Gin handlers, templates/, static/ (htmx + echarts served locally)
migrations/            001 … 006 (see below)
scripts/               seed_test_alarms.sql (sample alarms for the demo plant)
sql/demo_multi_plc.sql demo: 3 PLCs on 127.0.0.1/.2/.3 with analog + digital points
docs/                  alarm philosophy decisions
```

## Point types (derived from the address — Modbus convention)

| Address | Modbus type | Size | Function | Value in the app |
|---|---|---|---|---|
| 10001–19999 | Discrete Input (DI) | 1 bit | FC02 | 0 / 1, shown as state text (e.g. STOPPED / RUNNING) |
| 30001–39999 | Input Register (AI) | 16 bit | FC04 | raw 0–65535 scaled to pv_min…pv_max |
| 40001–49999 | Holding Register | 16 bit | FC03 | raw 0–65535 scaled to pv_min…pv_max |

`PV = pv_min + (raw / 65535) × (pv_max − pv_min)` — digital inputs are not scaled. A digital PV stores a history row on
every state change (+ one per minute).

## Database

| Table | Purpose |
|---|---|
| `plc_metadata` | one row per PLC: name, IP, port, unit id, scan interval, timeout, enabled |
| `pv_metadata` | one row per PV: PLC + register, name, system, equipment, description, unit, scaling, **history** deadband, state texts, `deleted_at` (archived PVs) |
| `pv_latest` / `pv_history` | last value + quality per PV / timestamped samples |
| `alarm_metadata` | one approved alarm definition per PV and type (HIHI, HI, LO, LOLO, DISCRETE) incl. rationalization |
| `active_alarms` | one row per outstanding occurrence (UNACK, ACKED, RTNUN, SHLVD) |
| `alarm_history` | append-only event log (activated, acknowledged, returned to normal, shelved, unshelved) |
| `alarm_audit_log` | append-only audit of configuration changes, approvals, deletions and refused actions |
| `users` / `sessions` | logins |

The alarm state rules are enforced by the database itself (allowed transitions only, a history row in the same transaction,
append-only history and audit).

### Migrations (safe to re-run; apply in order)

| File | What |
|---|---|
| `001_v01_schema.sql`, `002_digital_inputs_and_cleanup.sql` | base schema, digital inputs |
| `003_users.sql` | users and sessions |
| `004_alarms.sql` | alarm tables, triggers, audit log |
| `005_move_alarm_columns.sql` | moves the old per-PV alarm settings out of `pv_metadata` into `alarm_metadata` (as drafts); `pv_metadata.deadband` stays (history deadband) |
| `006_pv_archive.sql` | `pv_metadata.deleted_at`; name/register must be unique only among PVs that are not archived |

## Setup (Windows, inside the project folder)

```powershell
$env:PGCLIENTENCODING = "UTF8"
psql -U postgres -d plc_monitoring -f migrations/001_v01_schema.sql
psql -U postgres -d plc_monitoring -f migrations/002_digital_inputs_and_cleanup.sql
psql -U postgres -d plc_monitoring -f migrations/003_users.sql
psql -U postgres -d plc_monitoring -f migrations/004_alarms.sql
psql -U postgres -d plc_monitoring -f migrations/005_move_alarm_columns.sql
psql -U postgres -d plc_monitoring -f migrations/006_pv_archive.sql
psql -U postgres -d plc_monitoring -f sql/demo_multi_plc.sql                # demo PLCs and PVs
psql -U postgres -d plc_monitoring -f scripts/seed_test_alarms.sql          # optional: sample alarms (test data)

go run ./cmd/adduser -user YOURNAME -role admin                              # first administrator (asks for the password)
go test ./...
go run ./cmd/server                                                          # http://localhost:8080
```

Back up first (`pg_dump`) before running 005 or 006 on a database with data. A PLC simulator for the demo
(`plc_simulator.py`, `plc_service.ps1`) lives outside this repository. A real PLC only needs its real IP in `plc_metadata`.

### Settings (`.env` or environment; see `.env.example`)

| Setting | Default | Meaning |
|---|---|---|
| `DATABASE_URL` | `postgres://postgres@localhost:5432/plc_monitoring` | PostgreSQL |
| `HTTP_ADDR` | `:8080` | listen address |
| `CONFIG_RELOAD` | `10s` | how often PLC/PV configuration is re-read |
| `ACQUISITION` | `on` | `off` = web UI only |
| `COOKIE_SECURE` / `COOKIE_SAMESITE` / `SESSION_HOURS` | `false` / `lax` / `12` | login cookie |
| `ALARM_MAX_SHELVE` | none | longest time an alarm may be shelved (e.g. `8h`); the user always chooses a finite time |

## Web UI

| Page | What it does |
|---|---|
| `/` Dashboard | live table: System, PLC, Equipment, PV, Description, value with unit, Status, Last Updated; search; **More** opens the PV detail |
| PV detail | live value (gauge or lamp), identification, scaling, PLC/communication, **all alarms of the PV with their state**, recent alarm events, trend and history, compare up to 4 PVs |
| `/alarms` Alarms | outstanding alarms (live), filters (priority, type, state, class, PLC, search), pagination, **Acknowledge** and **More** (definition, rationalization, status, response time, event history, shelving) |
| `/alarms/history` | alarm event log (filters, pagination) and monitoring statistics (activations, peak rate, time to acknowledge, most frequent alarms, standing alarms) |
| `/config` | PLCs, PVs with their alarms in one form, alarm configuration and approval, users |
| every page | menu badge with the number of alarms waiting for acknowledgement, and the **alarm ticker** at the bottom (active alarms, oldest first, pauses under the pointer) |

Dashboard **Status** is `No data`, `COMM_FAIL`, `READ_FAIL`, `STALE`, the type of the PV's active alarm (`HIHI`, `HI`, `LO`, `LOLO`,
`ALARM`; shelved ones marked) or `Normal`. With several active alarms: not shelved first, then HIHI/LOLO, then priority,
then unacknowledged, then the newest.

### Alarm management (ISA-18.2 oriented)

- States: `UNACK`, `ACKED`, `RTNUN` (returned to normal, unacknowledged), `SHLVD`; a completed occurrence leaves `active_alarms`
  and stays in `alarm_history`. Priorities Minor / Major / Critical; classes PROCESS, EQUIPMENT_PROTECTION, ENVIRONMENTAL,
  SAFETY_CRITICAL.
- An alarm is evaluated only when it is **approved** by an administrator, enabled and consistent with its PV. Changing an approved
  alarm's limit, delays, deadband, priority, class or instructions withdraws the approval.
- Everyone logged in acknowledges. Supervisors and administrators shelve (reason and a finite duration are required; shelving
  never hides the process condition, and the alarm returns automatically when the time ends). Refused attempts are audited.
- Alarm history stores the setpoint and priority that applied at the time and the process value at the moment of each action.

### PV and alarm configuration in one form

The PV form has the PV fields first, then an **Enable Alarms** switch. Analog PVs get four independent alarms (HIHI, HI, LO, LOLO),
digital PVs one discrete alarm (you choose the *normal* state; the other state is the alarm). PV and alarms are written in
**one transaction**.

- Editing updates the existing definitions (never duplicates). Switching an alarm off keeps the definition and its history; its abnormal
  occurrences are ended and recorded as "taken out of service". The engine does the same for any alarm that stops being evaluated.
- New and changed alarms are drafts until an administrator approves them (Alarm Configuration, or "Approve these alarms now" in the form).
- **Delete PV** warns that its alarm configurations are deleted too and needs explicit confirmation. A PV with alarm history, outstanding
  alarms or (unless you also delete them) stored values is **archived** (hidden, not acquired, name and register reusable); otherwise it is
  erased completely. Alarm history and the audit log are never erased.

### Sample alarms

`scripts/seed_test_alarms.sql` configures realistic sample alarms for every PV of the demo plant (HI/HIHI near 80/90 % of the range,
LO/LOLO near 20/10 %, deadband ≈ 2 %, delays that ride through noise). They are stored as approved with approver "test-data seed"
and are **test data**: replace them with rationalized alarms before real use.

## Login, roles and users

Every page needs a login (except `/login`, `/healthz` and static files). Access is decided for every request from one table,
`internal/auth/rules.go`; a route that is not listed is closed to everyone except administrators, and a test fails if a route has no rule.

| Role | Dashboard, alarms, history | Acknowledge | Shelve | PV / PLC / alarm configuration | Approve alarms, users |
|---|---|---|---|---|---|
| operator | yes | yes | no | no | no |
| supervisor | yes | yes | yes | yes | no |
| admin | yes | yes | yes | yes | yes |

There is no default account: create the first administrator with `go run ./cmd/adduser -user NAME -role admin` (password from
`PLC_USER_PASSWORD` or typed, never a command-line argument). The session cookie is HTTP-only and holds a random token (only its hash
is stored); the role is read from the database on every request; forms and HTMX requests carry a CSRF token.

**Known limitation:** passwords are stored in plain text (`users.password`), by decision for this version. All access goes through
`internal/auth/password.go`, so moving to hashed passwords means changing that file and migrating the rows.

## Tests

`go test ./...` runs the unit tests (bus, storage, acquisition with a fake PLC, alarm engine, validation, web templates and streams).
The database integration tests are opt-in and only run on a database whose name contains `scratch` or `test`; the alarm tests need a
**fresh** database each (history cannot be cleaned): apply migrations 001–006 and load `plc_metadata` / `pv_metadata`, then

```powershell
$env:PLC_TEST_DATABASE_URL = "postgres://postgres@localhost:5432/plc_xxx_scratch"
go test ./internal/store -run TestIntegrationAlarmLifecycle -v      # also: …AlarmSchema, …AlarmConfiguration, …PVWithAlarms
```
