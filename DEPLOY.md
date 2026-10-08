# scoutanalytics: deploying and running it

How the project runs today and how to deploy a change safely. It covers the
topology, the three processes, a checklist (with rollback) for each kind of
change, draft systemd units, an optional nginx front, health checks and a short
look at cloud hosting.

Related: [README.md](README.md) (every setting and log line),
[HANDOFF.md](HANDOFF.md) ("Deploying", "Prod state", "Repo migration"),
the `ops/*/README.md` files (pgAdmin scripts).

> Secrets: `.env`, `scout.session.json` and `scoutanalytics_data/` stay on the
> server. They are never committed, copied into an image or pasted anywhere.
> This file lists variable **names** only.

## 1. Topology now

```
 prod server (a single Linux server)                             separate machine
 ┌──────────────────────────────────────────────────────────┐    ┌─────────────────────┐
 │ docker: offchainlabs/nitro-node:v3.11.2                   │    │ Ethereum Erigon      │
 │   Robinhood Chain full node, full log index               │    │ archive node         │
 │   --execution.rpc.log-history=0, host 8540 → 8547        │    │ (SCOUT_MAINNET_      │
 │                                                           │    │  RPC_URL)            │
 │ Postgres 17, database assetdb (SHARED with the main API)  │    └─────────▲───────────┘
 │                                                           │              │ eth_call /
 │ go run ./telegrambot/scoutanalytics -listen-only  ────────┼── Telegram   │ getLogs
 │ go run ./telegrambot/scoutanalytics -track  ──────────────┼──────────────┘
 │ go run ./telegrambot/scoutanalytics -web   (:8090 or SCOUT_WEB_ADDR)
 └──────────────────────────────────────────────────────────┘
 owner's Windows workstation: development, git push, SQL through pgAdmin
```

- Production runs the `main` branch; development is on `scout-call-model`.
- Nitro must keep `--execution.rpc.log-history=0`. Without the full log index,
  old block ranges are read one block at a time and the tracker slows to a crawl.
- **`assetdb` is shared with the main API.** Every scout process applies
  `scoutanalytics.sql` (only `scout_*` tables and views) at startup. The shared
  tables `assets`, `chains` and `asset_chains` are read and never altered.
- `scoutanalytics.sql` and `frontend/` are embedded in the binary (`go:embed`),
  so the binary is self-contained. Only `.env`, the session file and the state
  directory are read from the working directory.

## 2. Processes

All three run from the **repo root**. Their working directory must hold `.env`:
`loadConfig` reads `API_ID`/`API_HASH` in every mode, and
`database.SetupDatabase` loads `.env` from the working directory.
Note that `godotenv.Load` does not override variables that are already set, so
a value in the process environment (systemd `Environment=`) wins over `.env`.

Database variables for all three (from `.env`, through `database.SetupDatabase`):
`DB_USER`, `DB_PASS`, `DB_NAME_DEV`, `APP_ENV`, `GETH_HOST_PATH` (LOCAL_GETH)
or `HOST_SECRET_PATH` + `SSL_CERT_FILE_PATH`. Optional `SCOUT_DATABASE_URL`
overrides them (see 7.4); `SCOUT_DB=off` disables recording;
`SCOUT_DB_AUTO_MIGRATE` (default `true`) applies the schema at start.

### 2.1 Listener: `-listen-only`

| | |
|---|---|
| Command | `go run ./telegrambot/scoutanalytics -listen-only` |
| Needs | Telegram (`API_ID`, `API_HASH`, `PHONE`, `TG_PASSWORD` if 2FA; session `SCOUT_SESSION_FILE`, default `scout.session.json`), database, `SCOUT_STATE_DIR` (default `scoutanalytics_data`). It does not need the nodes. |
| Key vars | `SCOUT_SOURCE_CHANNEL` (scoutrobinhood), `SCOUT_NOTIFY_PEER` (default `me`), `SCOUT_DELIVER_LEVELS` (`clean,caution`), `SCOUT_TOOLS` (`perceptor,salpha`) and `SCOUT_TOOL_<CODE>_*`, `SCOUT_POLL_INTERVAL` (`20s`; with `0` the catch-up and requeue still run once at start, not retried), `SCOUT_CATCHUP_MAX` (`100`), `SCOUT_CATCHUP_MAX_AGE` (`24h`), `SCOUT_SCAN_GAP` (`3s`), `SCOUT_MODEL_URL` (off), `SCOUT_MODEL_TIMEOUT` (`5s`) |
| Writes | `scout_calls`, `scout_investigations`, `scout_deliveries`, …; queues new calls for tracking; sends `pg_notify('scout_events', …)` for each new real call and report |

**Restart cost: missed posts and waiting calls are picked up on start, within
limits.** Polling resumes from a saved cursor (the lower of the newest post in
`scout_calls` for the channel and `scoutanalytics_data/poll_cursor.json`) and
handles the posts that arrived while the listener was down (`catchup.go`,
README "How calls are picked up"):

- the newest `SCOUT_CATCHUP_MAX` missed posts (default `100`) that are at most
  `SCOUT_CATCHUP_MAX_AGE` old (default `24h`) are handled live: scanned and
  delivered as usual;
- older missed posts are stored only, like `-backfill` (tracked, no bot scans,
  no delivery);
- posts already in `scout_calls` are skipped.

The job queue is in memory. Calls still waiting in it at the stop (`queued`,
or `dropped` because it was full, with no completed report) are queued again
at the start, before the missed posts, with the same limits: beyond them they
are stored only (status `backfill`). Only calls posted within the requeue
window (`SCOUT_CATCHUP_MAX_AGE` + 48h, 72h with the defaults) are looked at;
older stuck rows are left as they are and logged once (see 3.3). Several
waiting calls of one token are scanned once (the newest; the older ones become
`duplicate`, or `backfill` with it). A call being scanned at the moment of the
stop can end as `failed` and is not retried; rerun it with `-post <id>` if
needed.

What a restart costs, then: missed posts and waiting calls beyond
`SCOUT_CATCHUP_MAX` or older than `SCOUT_CATCHUP_MAX_AGE` are stored but not
scanned or delivered, and the scans already queued start again from the
oldest. No manual `-backfill` or `-post` is needed after a restart. They are
still the tools for a post you want scanned and delivered although it was
stored only (`-post <id>`), or for history before the cursor (`-backfill`).
A long outage puts up to `SCOUT_CATCHUP_MAX` missed posts plus up to
`SCOUT_CATCHUP_MAX` requeued calls in front of the bots (Perceptor: one every
2m5s, so 100 calls take about 3.5 hours); lower it before a restart after a
long outage if new calls must not wait. If the database or Telegram is
unreachable at the start, nothing is skipped: the start is retried every poll
interval (with `SCOUT_POLL_INTERVAL=0` only at the next start). Keep
`scoutanalytics_data/` (with `poll_cursor.json` and `seen_cas.json`) when
moving the listener, or it falls back to the database's newest post. A stop or
start does not touch tracking.

**NEVER run two listeners on one Telegram session.** That includes a manual
`go run … -listen-only` while the systemd unit is up, and old and new repos
running at the same time during the repo move. Before you start one, check that
none is running:

```bash
pgrep -af 'scoutanalytics.*-listen-only' || echo "no listener running"
```

The first login (entering the login code) is interactive. Do it in a terminal
before any unit runs the listener.

**Log after a restart:** `recording to SQL via … → … (tables scout_calls, …)`,
`reports will be delivered to …`, `listen-only: the performance tracker is not
running in this process …`, `listening to @scoutrobinhood → … → deliver to …`, and from the polling
goroutine `requeue: N queued call(s) from before the restart (M scanned now, K
stored only)` (only when there were any), `catch-up: N post(s) since post X (M
handled live, K stored only)` (the gap; `; R already recorded, skipped` when
some were in the database already), then `polling @scoutrobinhood every 20s as
a backup (starting after post N, from …)`. The `requeue:`/`catch-up:`/`polling`
lines come from another goroutine than `listening to …`, so their order
relative to it varies from start to start; among themselves they are in this
order.
The catch-up's live posts and then new posts log one line each: `post 10002:
queued 0x…` or `post …: update for 0x… (not a call), skipping`. A `catch-up
after post X: …`, `catch-up: reading the resume point: …` or `requeue: …` error
line means that step did not run; it is retried every poll interval (with
`SCOUT_POLL_INTERVAL=0`: `… (not retried …)`, restart to try again) and
nothing was skipped. A `requeue: … (after the catch-up)` error is the second
requeue; polling runs meanwhile and each poll retries it until it works. A
`catch-up after post X: storing post N: …` error means a stored-only post could
not be written: the catch-up stopped there and is retried from post X.

### 2.2 Tracker: `-track`

| | |
|---|---|
| Command (prod) | `SCOUT_RPC_RPS=300 SCOUT_TRACK_WORKERS=12 SCOUT_RPC_PARALLEL=4 SCOUT_RPC_MAX_INFLIGHT=48 go run ./telegrambot/scoutanalytics -track` |
| Needs | database, Nitro (`SCOUT_RPC_URL`, default `http://localhost:8540`), Erigon (`SCOUT_MAINNET_RPC_URL`). No Telegram connection, but `API_ID`/`API_HASH` must be in `.env`. |
| Key vars (README defaults) | `SCOUT_RPC_RPS` (0 = unlimited; prod 300), `SCOUT_TRACK_WORKERS` (8; prod 12), `SCOUT_RPC_PARALLEL` (8; prod 4), `SCOUT_RPC_MAX_INFLIGHT` (64; prod 48), `SCOUT_RPC_LOG_CHUNK` (200000; prod unset), `SCOUT_TRACK_INTERVAL` (`1m`), `SCOUT_LATEST_REFRESH` (`on`; prod on), `SCOUT_LATEST_REFRESH_RECENT` (`15m`), `SCOUT_LATEST_REFRESH_OLD` (`24h`), `SCOUT_LATEST_BATCH` (200), `SCOUT_RUG_LIQ_USD` (500, set in `.env`), `SCOUT_CHAINLINK_FEEDS` (in `.env`; 10–33 stock feeds, plus `asset_chains`), `SCOUT_MAINNET_CHAINLINK_FEEDS`, `SCOUT_MAINNET_RPC_RPS` (0), `SCOUT_STABLES` (USDG), `SCOUT_PRICE_LOOKBACK_BLOCKS` (8640000), `SCOUT_DISCOVERY_BLOCKS` (18000), `SCOUT_PONS_FACTORY`, `SCOUT_PONS_HOOK`, `SCOUT_RPC_LOG_CACHE` (300000), `SCOUT_PERF_HORIZONS` (`1h,1d,3d,7d,30d`), `SCOUT_PRICE_SOURCE` (`onchain`) |

**Restart cost:** Ctrl+C (SIGINT) and `systemctl stop` (SIGTERM) are both safe.
The tracker stops handing out calls, and calls in progress keep the pieces they
had stored and resume from there. The in-memory caches are lost: discovered
pools, the swap-log cache, USD pool-search progress, and USD prices per asset
and hour. The first cycles after a restart therefore put more load on the node.
Restart it whenever needed, but not in a loop.

**Log after a restart:** `performance tracking on: … via eth_getLogs ranges of
up to 200000 blocks, …; 12 call(s) at a time`, `chainlink feeds: N on Robinhood
Chain and M on Ethereum mainnet from the asset database; in use …`, then every
cycle (every `SCOUT_TRACK_INTERVAL`, also when idle) a status line:
`tracking: processed N call(s), K in progress — pending …, tracking …, done …,
repeat …; …` or `tracking: idle — …; next check in …`, and per latest-price pass
`latest prices: N refreshed (M changed) in …, … RPC requests, W waiting`.
**If no `tracking:` line appears for more than a minute, the tracker is stuck.**

### 2.3 Website: `-web`

| | |
|---|---|
| Command | `go run ./telegrambot/scoutanalytics -web` |
| Needs | database only. `API_ID`/`API_HASH` must still be in `.env`. Uses one extra connection outside its pool for `LISTEN scout_events`. |
| Key vars | `SCOUT_WEB_ADDR` (`:8090`; in `.env` on prod; use `127.0.0.1:8090` behind a proxy), `SCOUT_WEB_REFRESH` (`15s`), `SCOUT_WEB_DIR` (empty = page built into the binary), `SCOUT_GMGN_URL` |

**Restart cost:** the page is unavailable for a few seconds while the first
snapshot is read. Open pages fall back to their 30-second refresh, and their
event streams reconnect and get `reload`. If the first snapshot read fails, the
process exits and says why.

**Log after a restart:** `website on http://… (…; read-only, no login; data read
again every 15s, …)`, then `web: snapshot 9,871 tokens in 180ms`, repeated only
when the token count changes or a read is slow. Problems show as
`web: could not refresh the snapshot: …` or
`web: live updates: cannot listen for database events: … — retrying`.

## 3. Deploy checklist per kind of change

### 3.0 Common steps

On the PC: commit and push `scout-call-model`, merge to `main` with a PR (the
owner does this). On the server:

```bash
cd /srv/geth-analytics                     # EDIT: the server's repo root
git rev-parse HEAD > /tmp/scout-prev-commit   # note what runs now
git checkout main && git pull
git log --oneline "$(cat /tmp/scout-prev-commit)"..HEAD -- telegrambot/scoutanalytics   # what changed
```

**Today (`go run`):** the restart itself compiles the new code. Rollback is
`git checkout <previous commit>` followed by the same restarts. Return to
`main` once a fix is merged.

**With a built binary (section 4):**

```bash
go build -o /opt/scoutanalytics/scoutanalytics.new ./telegrambot/scoutanalytics
cp -p /opt/scoutanalytics/scoutanalytics /opt/scoutanalytics/scoutanalytics.prev
mv /opt/scoutanalytics/scoutanalytics.new /opt/scoutanalytics/scoutanalytics
# then restart only the units the change needs (below)
```

Rollback for every kind of change:
`mv /opt/scoutanalytics/scoutanalytics.prev /opt/scoutanalytics/scoutanalytics`,
then restart the same units. A unit picks up the new binary only when it
restarts, so the units you did not restart keep running the old code until then.

### 3.1 Website only (`frontend/`, `web.go`, `websnapshot.go`, `events.go` web side)

1. Rebuild. The page is embedded, so even a CSS change needs a new binary,
   unless `-web` runs with `SCOUT_WEB_DIR`.
2. Restart `-web` only. The listener and tracker are unaffected.
3. Check: the `website on …` and `web: snapshot …` lines, `curl /api/summary`
   (section 6), and the page in a browser at 1280 px and 390 px.
4. Rollback: the previous binary or commit, then restart `-web`.

### 3.2 Tracker (`tracker*.go`, `onchain*.go`, `prices.go`, `feeds_db.go`, …)

1. **Before deploying, check for a full re-track:**
   ```bash
   git diff "$(cat /tmp/scout-prev-commit)"..HEAD -- telegrambot/scoutanalytics | grep -n onchainStateVersion
   ```
   Today it is `const onchainStateVersion = 2`. A higher number makes the tracker
   re-track **every** call from scratch, which means hours to days of node load.
   Do not deploy that without the owner's explicit go-ahead.
2. With the new code, run `-price-check` on 2–3 affected tokens **before**
   restarting. It is read-only and exits. Run it from the repo root so `.env`
   is found: today
   `go run ./telegrambot/scoutanalytics -price-check 0xTokenCA -price-at 6h`;
   with the binary flow
   `/opt/scoutanalytics/scoutanalytics.new -price-check 0xTokenCA -price-at 6h`
   (before the `mv`).
3. Restart `-track` (Ctrl+C or `systemctl restart scout-track`; it waits for
   the calls in progress).
4. Watch for the startup lines, two or three `tracking:` status lines, and one
   `latest prices:` line (section 2.2). Rising `error` counts or repeated
   `eth_getLogs … refused / timed out` lines mean stop and look.
5. Optional: run `-price-check` again on the same tokens after the restart.
6. Rollback: the previous binary or commit, then restart `-track`. Rows written
   by the new code stay as they are. If they need repair, that is a pgAdmin
   script for the owner, never an automatic reset.

### 3.3 Listener or NOTIFY (`main.go` listener paths, `postkind.go`, `callmeta.go`, the `pg_notify` payload, the `LISTEN` side in `events.go`)

1. Restart `-web` first, so the receiving side understands the new payload. It
   reads the list once after connecting, so nothing is lost.
2. Note the listener's last `post …` id. Stop it, confirm it has exited
   (`pgrep -af … -listen-only` prints nothing), then start it again. Never
   start the new one before the old one has exited.
3. Check: the listener startup lines (2.1): the `catch-up: … since post X`
   line should name the noted id (or a lower one) and covers the gap; a
   `requeue: …` line appears if calls were still waiting to be scanned. On the next call, `post …: queued …`
   in the listener log and the row on the page within about a second.
   Optionally run `LISTEN scout_events;` in a psql session to see the payloads.

   **First deploy of the requeue** (and any start): the requeue only looks at
   calls posted within its window, `SCOUT_CATCHUP_MAX_AGE` + 48h (72h with the
   defaults; 24h + 48h when `SCOUT_CATCHUP_MAX_AGE=0`). Inside the window, the
   newest `SCOUT_CATCHUP_MAX` waiting calls no older than
   `SCOUT_CATCHUP_MAX_AGE` are scanned and the rest become `backfill` (their
   tokens freed in `seen_cas.json`), or `duplicate` when the token has a
   report. Rows older than the window are never rewritten; when there are
   any, one line per process says so:
   `requeue: N queued or dropped call(s) posted before <time> (older than 72h0m0s) left as they are; DEPLOY.md 3.3 has the SQL to fix them by hand`.
   To look at them (read-only; adjust the interval if `SCOUT_CATCHUP_MAX_AGE`
   is not 24h):

   ```sql
   SELECT c.id, c.message_id, c.message_date, c.contract_address, c.status
   FROM scout_calls c
   WHERE c.channel_username = 'scoutrobinhood'
     AND c.status IN ('queued', 'dropped')
     AND c.message_date < now() - interval '72 hours'
     AND NOT EXISTS (SELECT 1 FROM scout_investigations i
                     WHERE i.call_id = c.id AND i.status = 'completed')
   ORDER BY c.message_id;
   ```

   If they should be marked stored only (what the requeue does with calls too
   old to scan), the owner can run, after checking the count matches:

   ```sql
   BEGIN;
   UPDATE scout_calls c
   SET status = 'backfill', updated_by = 'scoutanalytics', updated_at = now()
   WHERE c.channel_username = 'scoutrobinhood'
     AND c.status IN ('queued', 'dropped')
     AND c.message_date < now() - interval '72 hours'
     AND NOT EXISTS (SELECT 1 FROM scout_investigations i
                     WHERE i.call_id = c.id AND i.status = 'completed');
   -- the row count must match the SELECT above; otherwise ROLLBACK;
   COMMIT;
   ```

   The SQL does not touch `seen_cas.json`: those tokens stay marked as
   investigated, so a later call of one of them is recorded as `duplicate`
   (the requeue would have freed them). To scan one of them, use `-post <id>`.
4. Rollback: the previous binary or commit; restart the listener (stop it
   first), then `-web`.

### 3.4 Schema (`scoutanalytics.sql`)

`scoutanalytics.sql` runs at **every** start of **every** mode. Every statement
must be safe to repeat, and views are dropped and recreated (dependents first).
Two lines of work migrating the same schema caused both production startup
failures so far, so **one line of work at a time**. **`assetdb` is shared with
the main API**: the script may touch only `scout_*` objects, and a long lock
there is felt only by scout processes, but the database is the API's.

1. Stop all three processes: `-track` (Ctrl+C), the listener, `-web`.
2. Start **one** process with the new code, `-web`, and wait for
   `website on …`. It has no Telegram or node traffic, but it does write: it
   applies the schema and classifies unclassified `post_kind` rows. A migration error is fatal (`database migrate: …`)
   and the process exits. Fix it before starting anything else.
3. Start `-track`, then the listener.
4. Never run old and new binaries side by side. Each recreates its own view
   definitions at start, and they would undo each other.
5. Rollback is **forward-only**. Added columns and tables stay. The old binary
   recreates its own views at start, which works only if it does not depend on
   anything the new script dropped or renamed. Check that before deploying.
   Destructive changes (DROP COLUMN, type changes) need the owner's sign-off
   and a backup (`pg_dump -n public -t 'scout_*'`) first.

### 3.5 Ops scripts (`ops/<date>-<name>/`, pgAdmin A/B/C/D)

Follow that folder's `README.md`. The routine:

1. Deploy the code the scripts depend on, and restart `-track` (and the
   listener) as that README says.
2. **A** (read-only): run it in pgAdmin and paste the result grid to the PM.
   Wait for the go-ahead.
3. **Stop every tracking process before B.** With `-listen-only` that is
   `-track` only (the listener runs no tracker); a listener started *without*
   `-listen-only` must be stopped too. Wait until it has exited.
4. **B** (writes, one transaction): set `expected_count` on the line marked
   `<<< EDIT` and nowhere else. On a mismatch B changes nothing; run `ROLLBACK;`
   and ask the PM.
5. Copy `reset_at` from B's grid exactly (microseconds and time zone).
6. Start `-track` again.
7. **C** (read-only): paste `reset_at` on its `<<< EDIT` line. `race_victims`
   must be 0; only if it is not, run **D** once with the same `reset_at`, then C
   again. Re-run C later to watch `waiting` fall to 0.

Rollback: B is one transaction, so a failed B changes nothing. A committed B is
not undone. Re-tracked calls simply get their results again from the tracker.

## 4. systemd units (DRAFTS, not installed)

> **Draft for the owner to review.** Nothing here has been installed or tested
> on the server. Check paths, the user, and the Postgres unit name
> (`systemctl list-units 'postgresql*'`; on Ubuntu the cluster unit is often
> `postgresql@17-main.service`), then verify with
> `systemd-analyze verify /etc/systemd/system/scout-*.service`.

Build (from the repo root, as in 3.0):

```bash
sudo install -d -o scout -g scout /opt/scoutanalytics       # EDIT: user
go build -o /opt/scoutanalytics/scoutanalytics ./telegrambot/scoutanalytics
```

Notes for all three:

- `WorkingDirectory` = the repo root, because `.env`, `scout.session.json` and
  `scoutanalytics_data/` are read there, and `database.SetupDatabase` needs
  `.env` in the working directory.
- **Do not use `.env` as an `EnvironmentFile`.** systemd parses quotes and
  inline `# comments` differently (e.g. `SCOUT_DELIVER_LEVELS=clean,caution #
  …` would keep the comment). The program loads `.env` itself. Use
  `EnvironmentFile` only for small, non-secret, per-process files such as
  `/etc/scoutanalytics/track.env`. These values win over `.env`.
- Logs go to journald: `journalctl -u scout-track -f`,
  `journalctl -u scout-listener --since "10 min ago"`.
- SIGTERM is handled like Ctrl+C (`signal.NotifyContext`).
- `Wants=`/`After=` docker and Postgres, never `Requires=`, so a docker or
  Postgres restart does not stop the scout units. If Postgres runs in docker
  rather than as a system service, drop the `postgresql.service` entries and
  order every unit on `docker.service`.

`/etc/scoutanalytics/track.env` (non-secret):

```ini
SCOUT_RPC_RPS=300
SCOUT_TRACK_WORKERS=12
SCOUT_RPC_PARALLEL=4
SCOUT_RPC_MAX_INFLIGHT=48
SCOUT_LATEST_REFRESH=on
```

`/etc/systemd/system/scout-listener.service`:

```ini
# DRAFT - review before installing. Never run a manual listener while this is active.
[Unit]
Description=scoutanalytics listener (-listen-only)
Wants=network-online.target postgresql.service
After=network-online.target postgresql.service
# A crash loop stops after 3 tries in 30 minutes (no repeated Telegram logins).
StartLimitIntervalSec=1800
StartLimitBurst=3

[Service]
Type=simple
User=scout
Group=scout
WorkingDirectory=/srv/geth-analytics
# flock: a second copy (e.g. a manual run with the same wrapper) refuses to start.
ExecStart=/usr/bin/flock -n /run/lock/scout-listener.lock /opt/scoutanalytics/scoutanalytics -listen-only
Restart=on-failure
RestartSec=120
TimeoutStopSec=60
StandardInput=null
NoNewPrivileges=yes
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
```

The listener's guards against a second session are:

- systemd never starts a second copy of one unit.
- `Restart=on-failure` with a long `RestartSec` and a tight start limit. A bad
  session (one that needs a login code) fails without a TTY instead of
  prompting, and stops after 3 tries. Re-login in a terminal with the unit
  stopped.
- The `flock` wrapper. Manual runs should use it too:
  `flock -n /run/lock/scout-listener.lock ./scoutanalytics -listen-only`.

When you switch from the terminal process to the unit: Ctrl+C the terminal
listener, check that `pgrep -af listen-only` prints nothing, then
`sudo systemctl enable --now scout-listener`.

`/etc/systemd/system/scout-track.service`:

```ini
# DRAFT - review before installing.
[Unit]
Description=scoutanalytics tracker (-track)
Wants=network-online.target docker.service postgresql.service
After=network-online.target docker.service postgresql.service
StartLimitIntervalSec=1800
StartLimitBurst=5

[Service]
Type=simple
User=scout
Group=scout
WorkingDirectory=/srv/geth-analytics
EnvironmentFile=/etc/scoutanalytics/track.env
ExecStart=/opt/scoutanalytics/scoutanalytics -track
Restart=on-failure
RestartSec=60
# Calls in progress stop and keep their stored pieces; give them time before SIGKILL.
TimeoutStopSec=300
NoNewPrivileges=yes
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
```

`/etc/systemd/system/scout-web.service`:

```ini
# DRAFT - review before installing.
[Unit]
Description=scoutanalytics website (-web)
Wants=network-online.target postgresql.service
After=network-online.target postgresql.service
StartLimitIntervalSec=600
StartLimitBurst=10

[Service]
Type=simple
User=scout
Group=scout
WorkingDirectory=/srv/geth-analytics
# Enable ONLY once nginx (section 5) is in front: binds to this machine only and
# wins over SCOUT_WEB_ADDR in .env. Enabled now, it would cut off http://<server IP>:8090.
#Environment=SCOUT_WEB_ADDR=127.0.0.1:8090
ExecStart=/opt/scoutanalytics/scoutanalytics -web
# Also covers a database that is not ready at boot (the first snapshot read fails -> exit).
Restart=on-failure
RestartSec=10
TimeoutStopSec=30
NoNewPrivileges=yes
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
```

Files the `scout` user must read: `.env` (mode 600). It must read and write
`scout.session.json` (600) and `scoutanalytics_data/`. If the owner keeps
running under his own account instead, set `User=` to it.

Daily use:

```bash
sudo systemctl restart scout-web           # website change
sudo systemctl restart scout-track         # tracker change (waits for calls in progress)
sudo systemctl stop scout-listener                       # listener change: stop first,
pgrep -af listen-only || sudo systemctl start scout-listener   # start only if none is left
systemctl status scout-track scout-web scout-listener
```

## 5. Reverse proxy (optional): nginx for `-web`

The proxy brings HTTPS, which browsers require for desktop notifications on
anything but `localhost`, and HTTP/2, which lifts the six-connections limit
that SSE streams hit. Bind `-web` to `127.0.0.1:8090` behind it (see the unit
above).

```nginx
# /etc/nginx/sites-available/scout  (DRAFT)
server {
    listen 443 ssl http2;                 # nginx 1.18 syntax
    server_name scout.example.com;        # EDIT
    ssl_certificate     /etc/ssl/scout/fullchain.pem;   # EDIT
    ssl_certificate_key /etc/ssl/scout/privkey.pem;     # EDIT

    # Same-origin check of POST /api/refresh and GET /api/events compares
    # Origin with Host: pass Host exactly as the browser sent it (with port).
    proxy_set_header Host $http_host;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;

    location = /api/events {              # Server-Sent Events
        proxy_pass http://127.0.0.1:8090;
        proxy_http_version 1.1;
        proxy_set_header Connection "";
        proxy_set_header Host $http_host;
        proxy_buffering off;              # the app also sends X-Accel-Buffering: no
        proxy_cache off;
        proxy_read_timeout 120s;          # must stay above the 25 s keep-alive
    }

    location / {
        proxy_pass http://127.0.0.1:8090;
        proxy_http_version 1.1;
    }
}
server { listen 80; server_name scout.example.com; return 301 https://$host$request_uri; }
```

- Use `$http_host`, not `$host`. `$host` drops a non-default port, and
  `/api/refresh` and `/api/events` then answer 403. The check (`sameOrigin` in
  `web.go`) compares only the host and port of `Origin` with `Host`, not the
  scheme, so HTTPS in front of plain HTTP works.
- Certificate: a server that is not reachable from the internet often cannot pass the Let's Encrypt
  HTTP-01 check. Use DNS-01 or a locally trusted certificate. Test with
  `sudo nginx -t` before `systemctl reload nginx`.
- The site has no login. Restrict it with `allow`/`deny`, basic auth or the
  firewall if it should not be public.

## 6. Health checks

| What | How | Healthy |
|---|---|---|
| Website up and fresh | `curl -s http://127.0.0.1:8090/api/summary` | HTTP 200, and `snapshot_age_seconds` below about 60 (with `SCOUT_WEB_REFRESH=15s`). It keeps answering 200 from a stale snapshot while the database is down, so **check the age, not only the status**. 503 only before the first snapshot. |
| Live updates | `curl -N http://127.0.0.1:8090/api/events` | `retry: 5000` and `: connected` at once, then `: ping` every 25 s. |
| Tracker alive | `journalctl -u scout-track --since "3 min ago" \| grep 'tracking:'` (or the terminal) | at least one `tracking:` line per `SCOUT_TRACK_INTERVAL` (1 min), also when idle |
| Latest prices | the same log, `grep 'latest prices:'` | `waiting` falls over time; failures are counted on the line |
| Listener alive | its log; in pgAdmin, read-only: `SELECT max(message_date) FROM scout_calls;` | a `post …` line for each new channel post; `max(message_date)` follows the channel |
| Nitro head advancing | run twice, a few seconds apart: `curl -s -X POST -H 'Content-Type: application/json' --data '{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}' http://localhost:8540` | the number grows by about 10 per second |
| Erigon reachable | the same `eth_blockNumber` against `$SCOUT_MAINNET_RPC_URL` (from the environment; do not paste it) | grows by about 1 per 12 s |
| Postgres connections | `SELECT count(*) FROM pg_stat_activity WHERE datname = 'assetdb';` | well below `max_connections` (shared with the main API) |

## 7. Future: cloud and scaling (cloud-neutral)

### 7.1 Where the cost is

The **nodes are the dominant cost and the hardest part to move**. The Nitro full
node with a full log index needs a large, fast NVMe disk, and the Erigon mainnet
archive needs several TB. Both need a long initial sync. The tracker sends up to
300 requests per second, mostly `eth_getLogs`, to Nitro, so **the tracker
belongs next to the Nitro node**. A hosted RPC provider with request limits and
per-request pricing does not fit that load.

- **Option A: the app in the cloud, nodes on the prod server.** In practice `-web` (and
  optionally the listener) go to a cloud VM; `-track` stays on the prod server with Nitro.
  This needs a private tunnel (WireGuard or similar) from the cloud to Postgres
  (and to the nodes, if anything moves). Cost: one small VM, plus managed
  Postgres if the database moves too.
- **Option B: everything in the cloud.** A VM for the three processes, managed
  Postgres, and two node VMs with large SSDs. The node VMs and their disks are
  most of the bill.
- **Moving Postgres moves the main API's database (`assetdb` is shared).**
  Splitting only the scout tables out would break the `asset_chains` feed join.
  The tracker then falls back to the feeds in `SCOUT_CHAINLINK_FEEDS` only.

Rough monthly ranges, on-demand, one region, before discounts. **These are
estimates not checked against live prices; confirm them in the calculators**
([GCP](https://cloud.google.com/products/calculator),
[AWS](https://calculator.aws/),
[Azure](https://azure.microsoft.com/en-us/pricing/calculator/)):

| Item | Size | Rough $/month |
|---|---|---|
| App VM (3 processes) | 4 vCPU, 16 GB, 50 GB SSD | 100 – 160 |
| Managed Postgres | 2 vCPU, 8 GB, 100–200 GB SSD, no HA | 120 – 250 (HA about ×2) |
| **Nitro full node** (expensive) | 8 vCPU, 32 GB, 2–4 TB SSD | 400 – 1,000 |
| **Erigon mainnet archive** (expensive) | 8–16 vCPU, 32–64 GB, 3–4 TB SSD | 500 – 1,300 |
| Egress, backups, snapshots | | 20 – 100 |

Option A: about $100–400 a month. Option B: about $1,100–2,800 a month, of which
the nodes are roughly 80 %. The prod server costs power and hardware only.

### 7.2 Live updates with several `-web` instances

Today the listener sends Postgres `NOTIFY scout_events` (a tiny signal) and each
`-web` `LISTEN`s, re-reads the database and sends SSE. With several instances
the owner plans **GCP Pub/Sub** (already used by the main API):

- **One snapshot builder** (a `-web` role or its own mode) reads the database,
  diffs the snapshot, and publishes the snapshot version plus the changed rows
  (or "reload") to a Pub/Sub topic. The database is read once, not once per
  instance.
- The listener keeps its tiny signal (NOTIFY to the builder, or a publish
  straight to Pub/Sub). The `-web` instances subscribe (one subscription each,
  or a fan-out) and serve the SSE stream and the API from memory.
- **SSE ids must come from the builder.** Today they are `<process>-<n>` per
  process, so a reconnect to another instance would get `reload`. The
  alternative is sticky sessions at the load balancer.
- `POST /api/refresh` must reach the builder, which does the single
  rate-limited read, and the result is published like any other change.
- Pub/Sub is GCP-specific. AWS SNS+SQS or Azure Service Bus would be the
  equivalents if the cloud choice changes. Cost at this volume is near zero
  ([Pub/Sub pricing](https://cloud.google.com/pubsub/pricing)).

### 7.3 Repo move to `kfukue/scoutanalytics`

Planned and on hold. See [HANDOFF.md, section 5](HANDOFF.md#5-repo-migration-on-hold-until-the-pending-tasks-are-done).
For deployment it changes:

- the build command (`go build -o … .` at the new root);
- `WorkingDirectory` (the new checkout, with `.env`, `scout.session.json` and
  `scoutanalytics_data/` copied over);
- the cut-over itself: stop the old listener **before** starting the new one.
  Rollback is restarting the old one.

### 7.4 Database connections: `SCOUT_DATABASE_URL` vs `database.SetupDatabase`

- With `SCOUT_DATABASE_URL` the pool is hard-coded to `MaxConns = 4`
  (`NewScoutStore` in `scout_models.data.go`). That is too few for 12 tracker
  workers, and it needs raising (code change) before prod switches to it.
- The default path (`database.SetupDatabase`) sets no `MaxConns`. pgxpool's
  default is `max(4, NumCPU)` per process. It also
  fatally requires `.env` in the working directory.
- Connection budget on the **shared** `assetdb`: about 3 processes × the pool
  size, plus 1 `LISTEN` connection for `-web`, plus the main API and pgAdmin.
  Keep the total below Postgres `max_connections`, which matters most on a
  small managed Postgres tier.
