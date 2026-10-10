# Website snapshot export and GCS read mode: interface spec (DRAFT)

For the Go coder. Infra drafted the GCP side in this folder (`README.md`). This
file says what the program must do so that infra works. Nothing here is
implemented yet. Names and defaults are proposals; if you change one, change
`service.yaml`, `systemd/` and `README.md` with it.

```
 prod server (never exposed)                          GCP (us-central1)
 ┌──────────────────────────────────────┐   HTTPS    ┌──────────────────────────────────┐
 │ scout-web (-web, reads assetdb)       │  upload   │ GCS bucket (private)             │
 │   snapshot every SCOUT_WEB_REFRESH ───┼──────────►│   public/parts/rows/<h>.json.gz   │
 │   export: ≤ 1 upload/min, only the    │  SA key   │   public/parts/reports/<h>.json.gz│
 │   parts that changed                  │           │   public/parts/prices/<h>.json.gz │
 └──────────────────────────────────────┘           │   public/current.json (pointer)   │
                                                     └──────────────┬───────────────────┘
                                                                    │ objectViewer (runtime SA)
                                                    ┌──────────────▼───────────────┐
  visitors ── HTTPS ── (Firebase Hosting rewrite) ─►│ Cloud Run scout-web          │
                       __HOSTNAME__ (default        │ -web, SCOUT_WEB_SOURCE=gcs    │
                       scout-analytics.lylelabs.io) │ no DB, no node, no Telegram   │
                                                    └──────────────────────────────┘
```

## 1. Two new behaviours of `-web`

| | Export (prod server) | Read (Cloud Run) |
|---|---|---|
| Enabled by | `SCOUT_WEB_EXPORT=gcs` in the existing prod `-web` process | `SCOUT_WEB_SOURCE=gcs` (`db` = today's behaviour, the default) |
| Reads | `assetdb`, as today: **no extra queries** (it rides on the snapshot `-web` already reads) | GCS only |
| Writes | GCS objects in one bucket | nothing |
| Credentials | ADC: `GOOGLE_APPLICATION_CREDENTIALS` = SA key file (set in the systemd drop-in), or an X.509 WIF credential config (README step 4, option b) | ADC: Cloud Run metadata server (runtime SA). No key file. |

Why inside prod `-web` rather than a separate process: `assetdb` is **shared
with the main API**. A separate exporter would add its own snapshot reads every
15 s, a connection pool and a `LISTEN` connection on it. Riding on `-web` adds
none. A standalone mode (for example `-web-export`, same code, no HTTP listener)
is fine as a later option; it would cost those extra reads and connections.

### Environment variables (names and purpose; values never in git)

Both sides:

| Name | Purpose | Default |
|---|---|---|
| `SCOUT_WEB_GCS_BUCKET` | bucket name (no `gs://`) | required when either mode is on |
| `SCOUT_WEB_GCS_OBJECT` | name of the pointer object | `public/current.json` |

Export side only:

| Name | Purpose | Default |
|---|---|---|
| `SCOUT_WEB_EXPORT` | `gcs` turns the export on; `off` | `off` |
| `SCOUT_WEB_GCS_PREFIX` | prefix of the part objects (the lifecycle rule matches it) | `public/parts/` |
| `SCOUT_WEB_EXPORT_MIN_INTERVAL` | minimum time between two uploads; values below `1m` are raised to `1m` with a warning | `1m` |
| `SCOUT_WEB_EXPORT_HEARTBEAT` | rewrite the pointer (new `uploaded_at`, nothing else) when no pointer write happened for this long and the data is unchanged (3.6). Raised to `1m` if lower; must stay ≤ half of the readers' `SCOUT_WEB_STALE_AFTER` | `2m` |
| `SCOUT_WEB_EXPORT_PRICES` | `full` = the prices part holds every row; `delta` = a base plus the rows changed since it (2.3) | `full` |
| `SCOUT_WEB_EXPORT_URL_HOSTS` | comma list of hosts allowed in published report links (4.4) | empty = **no report links published** (warn once at start). Owner decision: `perceptor.info` |
| `GOOGLE_APPLICATION_CREDENTIALS` | path of the uploader credential (standard ADC variable) | set by the systemd drop-in |

Read side only:

| Name | Purpose | Default |
|---|---|---|
| `SCOUT_WEB_SOURCE` | `gcs` = read snapshots from GCS; `db` = today | `db` |
| `SCOUT_WEB_GCS_POLL` | minimum time between two pointer checks per instance (min `5s`) | `15s` |
| `SCOUT_WEB_EVENTS` | `off` disables `GET /api/events` (5.4) | `on` for `db`, `off` for `gcs` |
| `SCOUT_WEB_STALE_AFTER` | stale when the pointer's `uploaded_at` (prod's last pointer write, data change or heartbeat, 3.6) is older than this (health, summary) | `5m` |
| `PORT` | set by Cloud Run. In `gcs` mode, when `SCOUT_WEB_ADDR` is unset and `PORT` is set, listen on `:$PORT`. The image and `service.yaml` do **not** set `SCOUT_WEB_ADDR` | (Cloud Run: 8080) |
| `SCOUT_GMGN_URL` | as today | as today |

`SCOUT_WEB_REFRESH` and every database, Telegram and node variable are ignored
in read mode.

## 2. Objects and formats

The export is split by how often each part changes, so the server uploads, and
the readers download, only what changed:

| Part | Holds | Changes when | Expected gzip size (10k tokens, **estimate, measure on prod**) |
|---|---|---|---|
| `reports` | every report the rows name, after the scrub | a Perceptor or sAlpha report arrives | 0.5–1 MB (texts) |
| `rows` | every exported row **without** the four latest-price fields | a new call, a performance window, tracking status, a re-scan | 0.4–0.8 MB |
| `prices` | `call_id → latest_return, latest_price, latest_at, latest_trade_at` | the tracker's latest-price pass (≤ every minute) | full: 0.25–0.4 MB; delta: a few KB to ~100 KB |

```
gs://BUCKET/public/current.json                          pointer, overwritten on each upload
gs://BUCKET/public/parts/rows/<sha256>.json.gz           content-addressed, never changed*
gs://BUCKET/public/parts/reports/<sha256>.json.gz        content-addressed, never changed*
gs://BUCKET/public/parts/prices/<sha256>.json.gz         content-addressed, never changed*
gs://BUCKET/gated/...                                     reserved for a later logged-in version
```
\* except the keep-alive re-upload in 3.5, which writes the same content.

### 2.1 Hashes and determinism

- **`<sha256>` is the hash of the uncompressed JSON** of the part, not of the
  gzip bytes. Go's gzip output may change between Go versions, so a hash over
  gzip bytes would no longer match its name after a re-upload by a newer
  binary. The reader checks the hash **after** gunzip.
- The JSON must be deterministic: rows sorted by `call_id`, reports by `id`,
  prices by `call_id`; one struct per part with fixed field order; times in
  UTC RFC 3339 with milliseconds; no maps with non-sorted output (Go sorts map
  keys, but use slices). `snapshot_at` and every other upload-time value live
  **only in the pointer**, never in a part, so an unchanged part keeps its
  hash.
- Encoder: `json.Encoder` with `SetEscapeHTML(false)` (so the deny check in 4.3
  sees `&`, `<`, `>` as written, not as `\u0026`, `\u003c`, `\u003e`). The page never inserts
  these strings as HTML.

### 2.2 Pointer (`public/current.json`)

`Content-Type: application/json`, `Cache-Control: no-store`. Small (< 1 KB).

```json
{
  "format": 2,
  "content_sha256": "9a0d…41",
  "snapshot_at": "2026-10-09T12:00:15.123Z",
  "prices_at": "2026-10-09T11:59:58Z",
  "uploaded_at": "2026-10-09T12:00:16.002Z",
  "update_posts": 312,
  "parts": {
    "rows":    { "object": "public/parts/rows/3f1c…e9.json.gz",    "sha256": "3f1c…e9", "gz_size": 612034 },
    "reports": { "object": "public/parts/reports/77ab…10.json.gz", "sha256": "77ab…10", "gz_size": 803311 },
    "prices":  { "object": "public/parts/prices/c2d4…8f.json.gz",  "sha256": "c2d4…8f", "gz_size": 301877 }
  }
}
```

With `SCOUT_WEB_EXPORT_PRICES=delta`, `parts` has `prices_base` and
`prices_delta` instead of `prices` (2.3).

- `content_sha256`: sha256 of the text `rows:<sha>\nreports:<sha>\nprices:<sha>\n`
  (delta mode: `prices_base:<sha>\nprices_delta:<sha>\n` in place of the prices
  line) plus `update_posts:<n>\n`. It **excludes** `snapshot_at`,
  `uploaded_at` and `prices_at`. The exporter compares it at start with what it
  would upload ("skip if same", 3.2), and the reader uses it to skip a check
  quickly.
- `snapshot_at`: when prod read the database for the snapshot whose content
  this pointer names (`loadedAt` on prod). It does **not** move on a
  heartbeat (3.6). The read side shows this time, never its own download time.
- `uploaded_at`: when prod wrote this pointer, on a data change or on a
  heartbeat (3.6). Outside the hash. The read side uses it for staleness
  ("prod is alive and confirms this content"), never for the data's age.
  It compares prod's clock with Cloud Run's: **prod must run NTP**
  (`systemd-timesyncd` or `chrony`; `timedatectl` shows `System clock
  synchronized: yes`).
- `prices_at`: the newest `latest_at` among the rows (null if none).
- `gz_size`: size of the stored object; the reader rejects a larger object.
- Nothing else: no host names, IPs, paths, versions with host info, or env values.

### 2.3 Parts

Each part object: gzip of one JSON document. `Content-Type: application/gzip`,
**no `Content-Encoding` header** (GCS must never transcode it).
`Cache-Control: private, max-age=86400, immutable` (informational: the bucket
is private).

```json
// rows
{ "format": 2, "rows": [ { "call_id": 1, …every field of 4.1 except the four latest-price fields… } ] }
// reports
{ "format": 2, "reports": [ { "id": 123, "tool": "salpha", "at": "…", "verdict": "clean",
                              "label": "…", "summary": "…", "text": "…", "url": "https://…",
                              "truncated": false } ] }
// prices (full)
{ "format": 2, "prices": [ { "call_id": 1, "latest_return": 0.42, "latest_price": 0.0012,
                             "latest_at": "…", "latest_trade_at": "…" } ] }
```

**Delta prices (`SCOUT_WEB_EXPORT_PRICES=delta`, optional, phase 2).** No chain
of deltas:

- `prices_base`: a full prices part as above. A new base is written when the
  delta would exceed 25 % of the base's row count, every 60 minutes, and at
  start.
- `prices_delta`: `{ "format": 2, "base": "<sha of prices_base>", "prices": [ …rows whose
  latest-price fields differ from the base, sorted by call_id… ] }`, i.e.
  **cumulative since the base**, not since the last upload. A row whose
  latest-price fields went back to nil is listed with nulls.
- The reader keeps the base in memory, downloads a new base only when its hash
  changes, and applies the newest delta on top (a delta whose `base` is not the
  base in memory is an error: keep the old snapshot).
- `full` is the default until delta is implemented and measured. Delta cuts the
  prices volume by about 10× (README "Costs").

`format`: bump only on incompatible changes. A reader that sees an unknown
`format` keeps its current snapshot and logs once a minute.

Field names: snake_case, one Go struct per part with explicit `json` tags (for
example `webExportRow`, `webExportPrice`, `webExportReport`), **separate from
`ScoutWebRow`**, so a new field on `ScoutWebRow` is never published by accident.
The structs *are* the allowlist.

The read side merges `rows` and `prices` by `call_id` back into
`[]ScoutWebRow`, turns `reports` into the `map[int]*webReport` that
`readReportTexts` builds today (setting `declined = salphaDeclined(text)` as
the conversion at websnapshot.go ~271 does), and builds the snapshot with the
**same code** as today (`newWebSnapshot`; `webReportsFor` then sets each
row's `salphaDeclined`). Every
endpoint (`/api/summary`, `/api/calls`, `/api/call`, `/api/analytics`) then
works unchanged. Pre-rendered answers are not exported; they depend on the
query.

## 3. Export side (prod `-web`, `SCOUT_WEB_EXPORT=gcs`)

### 3.1 Inputs (what exactly is exported)

In `readSnapshot` (web.go): rows come from `s.readRows`, reports from
`s.readReportTexts`, then `s.fillTrades24h` fills `Trades24h`, and **then
`newWebSnapshot` changes the rows in place** (it sets `Trades24h` to nil
unless `TradesFinal`, clears `EntryPrice`/`HasPerf` for non-USD rows, hides a
rugged call's peak, and fills `anaHas`/`anaPerf`).

- Export the rows **as they are after `fillTrades24h` and before
  `newWebSnapshot`**. A shallow copy of the slice is **not** enough:
  `newWebSnapshot` also writes through pointers (`*r.LatestAt =
  r.LatestAt.UTC()`, the same for `LatestTradeAt` and `PerceptorTodayAt`,
  websnapshot.go ~549–578), which would race with the exporter goroutine.
  So **convert to the export DTO values on the refresh path**, right there in
  `readSnapshot` (plain values and freshly allocated pointers, nothing shared
  with `rows`; about 10k small structs, milliseconds). The read side then runs
  `newWebSnapshot` itself and gets the same result as prod.
- Reports: the `map[int]*webReport` after `readReportTexts` (texts already cut
  to `webReportMaxBytes`). These `*webReport` objects are **shared with the
  previous snapshot** (`readReportTexts` reuses them), so convert them to DTO
  values on the refresh path as well; never hand the pointers to the
  exporter goroutine.
- The read side **never** calls `readTrades`/`fillTrades24h` (no database);
  `Trades24h` comes from the export.

### 3.2 Trigger and rate

1. After `readSnapshot` stores a new snapshot, hand the DTO values of 3.1
   (rows, prices, reports, update post count, `loadedAt`) to the exporter
   **without blocking**: a one-slot "latest wins" channel (drop the older
   pending item). Only the DTO conversion runs on the refresh path; scrub,
   JSON, deny check, gzip and upload happen in the exporter goroutine, which
   owns the DTOs after the hand-off. The website never waits for GCS.
2. The exporter builds the three parts and their hashes (2.1). If
   `content_sha256` equals the last uploaded one: no upload.
3. At start, the exporter GETs the pointer once. If its `content_sha256` equals
   the first snapshot's, it adopts that pointer (and its generation) and
   uploads no new parts, **but runs the keep-alive (3.5) at once**: after a
   long prod outage the parts the pointer names may be close to, or past,
   their 30-day expiry. If a part is already gone, the keep-alive simply
   recreates it (the exporter has its content). After the keep-alive it
   **writes a heartbeat at once** (3.6), so the site does not stay stale
   after a prod restart with unchanged data.
4. Changed: upload when at least `SCOUT_WEB_EXPORT_MIN_INTERVAL` (1 min) has
   passed since the last successful upload; otherwise wait until then and
   upload the **latest** pending snapshot. Public data therefore changes at
   most once a minute; a new call appears within about a minute plus
   `SCOUT_WEB_REFRESH`.

### 3.3 Upload order (never a pointer to a missing object)

1. For each part whose hash differs from the part the last pointer names:
   gzip (default level), upload `PREFIX<part>/<sha>.json.gz` with precondition
   `ifGenerationMatch=0` (create only). `412 Precondition Failed` = an object
   with that name (so that content) is already there = success.
2. Only after **every** changed part succeeded: write the pointer. Use
   `ifGenerationMatch=<last pointer generation>` once known; on 412 another
   writer exists: log a clear warning ("another exporter writes this bucket"),
   re-read the pointer, go on.
3. Per-attempt timeouts: 60 s per part, 15 s for the pointer.

### 3.4 Failures

- Retry with exponential backoff and jitter: 10 s, 20 s, 40 s … capped at 5 min.
  Each retry uploads the **latest** pending snapshot, not the one that failed.
- A failed pointer write after good part uploads just leaves orphan objects;
  the lifecycle rule removes them.
- Logging: the first failure at once, then at most one line a minute (reuse
  `logLimiter`): `web export: upload failed (N tries): <err> — the public site
  still shows the data of 12:00:15`. On recovery: `web export: uploads work again`.
  Never log the key, tokens or signed URLs.
- Credentials missing or invalid at start: log once, keep `-web` serving, retry
  every 5 min. The export never makes `-web` exit or return errors to visitors.
- Deny check failed (4.3): **no upload at all** for that snapshot (no part, no
  pointer); log loudly (below).
- Shutdown: stop with `ctx`; do not hold up SIGTERM for an upload (give the one
  in flight at most a few seconds).
- Log one line per successful upload only when a part other than prices
  changed, or every 60th upload, to keep journald small:
  `web export: uploaded snapshot of 12:00:15 (rows, prices; 9,871 tokens, 0.9 MB) in 420ms`.

### 3.5 Bucket lifecycle and keep-alive

The bucket deletes part objects **30 days** after their creation
(`bucket-lifecycle.json`, prefix `public/parts/`). Owner choice, review item 7:

- **Keep-alive**: every **12 hours**, the exporter re-uploads **every part the
  current pointer names** (plain overwrite of the same name, no precondition),
  which resets their age. This matters most for `reports`, which may not
  change for weeks. Without it, a quiet period would leave the pointer naming
  deleted objects, and new Cloud Run instances could not start. The
  re-uploaded gzip bytes may differ (newer Go), the name and uncompressed hash
  do not (2.1).
- **Prod down**: no keep-alive. The public site keeps serving the last snapshot
  for **at least 30 days** (stale banner). After that, new instances cannot
  load it and the site is down until prod uploads again. 30 days instead of 7
  costs a little more storage (README "Costs").
- **Rejected**: a reader fallback to "the newest remaining parts" (needs
  `storage.objects.list`, which `objectViewer` already has). With split parts,
  the newest remaining `rows`, `reports` and `prices` may come from different
  snapshots; stitching them would publish a state that never existed (prices
  of one hour on rows of another day), and the hash chain in the pointer could
  no longer vouch for it. A dark site after 30 days of prod outage is the
  intended, visible failure; the uptime check (README step 8) alerts long before.

### 3.6 Pointer heartbeat (freshness when the data is quiet)

Without it, an unchanged `content_sha256` means no pointer write, so a quiet
period (no new call, no price change) would look exactly like a dead prod and
the site would report `stale` after `SCOUT_WEB_STALE_AFTER` although prod is
healthy.

- When no pointer write has happened for `SCOUT_WEB_EXPORT_HEARTBEAT` (default
  `2m`, at least `1m`, at most half of `SCOUT_WEB_STALE_AFTER`), the exporter
  rewrites the pointer with **the same** `content_sha256`, `parts`,
  `snapshot_at`, `prices_at` and `update_posts`, and a new `uploaded_at`. No
  part is uploaded. Same precondition as any pointer write
  (`ifGenerationMatch=<last generation>`, 3.3), same retries and log limits.
- **Only while the published content is current**: the heartbeat runs only if
  the newest snapshot the exporter received (after its scrub, deny check and
  hashes) has the **same** `content_sha256` as the published pointer. No
  heartbeat while an upload is pending or failing, and **none while the deny
  check refuses** (4.3): the site must turn stale then, so the uptime check
  fires. So the exporter builds and hashes every received snapshot (it does
  already, 3.2 step 2) and remembers whether the newest one was refused.
- **Only after a fresh database read**: at least one snapshot with that same
  `content_sha256` must have **arrived since the last pointer write**. The
  exporter only receives a snapshot when `readSnapshot` succeeds; if `assetdb`
  reads fail (web.go ~871, "could not refresh the snapshot"), nothing arrives,
  no heartbeat is written, and the site turns stale as it should. The
  heartbeat means "prod read the database again and found the same content",
  not merely "the exporter is alive". With a 15 s `SCOUT_WEB_REFRESH` and a
  2-minute heartbeat there are always several such snapshots when prod is
  healthy.
- At start, on the "adopt" path (3.2 step 3): a heartbeat at once (the first
  snapshot has just arrived, so the rule above holds).
- Cost: pointer writes stay at most one a minute (a data change and a
  heartbeat never both run within the 1-minute floor), so the Class A numbers
  in README "Costs" (1,440 pointer writes a day) do not change.
- Read side: when a check finds the same `content_sha256`, it keeps the
  snapshot (no download, no rebuild) and only moves its **confirmed-at** time
  forward to the pointer's `uploaded_at` (an atomic value next to the
  snapshot pointer, never moved backwards). It does **not** change the
  snapshot's `loadedAt`: that feeds the Analytics day cut-offs
  (websnapshot.go ~947), the cached answers and `X-Snapshot-At`. A check that
  loads new content sets confirmed-at to that pointer's `uploaded_at`.
- Staleness (health, summary) = now − confirmed-at > `SCOUT_WEB_STALE_AFTER`.
  It says "prod's exporter is alive and confirms this content"; whether the
  **tracker** is moving shows separately in `prices_at`.

## 4. Public field allowlist, scrub and deny check

### 4.1 Fields (owner decision 9 Oct 2026: everything the LAN site shows)

The public site shows **everything the LAN site shows**: returns, peaks,
drawdowns, rugged flag, tracking status, "good" holders and buyers, the full
Analytics page, Perceptor summary text, Perceptor report links and sAlpha text.
There are **no switches** per group any more: every field below is exported.
The DTO structs stay the allowlist; a field added to `ScoutWebRow` later is not
published until it is added to the DTO and to the approved list in the test (7).

| `ScoutWebRow` field(s) | Part |
|---|---|
| `CallID` | rows (and prices, as the key) |
| `MessageID`, `MessageDate`, `ChannelUsername` | rows (post link built on the read side by `postURL`, public channel only) |
| `ContractAddress`, `TokenName`, `TokenSymbol` | rows |
| `CalledAtMcap`, `PostMcap`, `PostPrice` | rows |
| `Holders`, `ProofElite`, `ProofGood` | rows |
| `LiveBuysEliteCount`, `LiveBuysEliteUSD`, `LiveBuysGoodCount`, `LiveBuysGoodUSD` | rows |
| `PriceUnit`, `EntryPrice`, `Tracked` | rows |
| `Perf`, `HasPerf` (returns, peaks, drawdowns per window), `Rugged` | rows |
| `TrackingStatus`, `CallCount`, `LastCallDate` | rows |
| `PerceptorVerd`, `PerceptorURL` (4.4), `PerceptorID` | rows |
| `PerceptorTodayVerd`, `PerceptorTodayURL` (4.4), `PerceptorTodayAt` | rows |
| `SAlphaID` | rows |
| `PostedDex`, `EntrySource`, `QuoteSym`, `NoData`, `VerdictAtCall`, `TradesFinal`, `Trades24h` | rows |
| `PreBuyVol60`, `PreSellVol60`, `PreSwaps60`, `PreChg60`, `PreVolUnit` | rows |
| `LatestReturn`, `LatestPrice`, `LatestAt`, `LatestTradeAt` | **prices** |
| unexported fields (`usd`, `verdict`, `mcapVals`, `anaHas`, …) | not exported; recomputed by `newWebSnapshot` |

| `webReport` field | Export | Note |
|---|---|---|
| `id`, `tool`, `at`, `verdict` | yes | |
| `label` | yes | scrubbed |
| `summary` (Perceptor `verdict_summary`) | yes | scrubbed |
| `text` (sAlpha `report_text`, already cut) | yes | scrubbed. May contain **public `@handles`** and public `t.me/<channel>` links: allowed, not scrubbed (only private links are, 4.2) |
| `truncated` | yes | |
| `url` | yes, if it passes 4.4 | |
| `declined` | no | recomputed on the read side from `text` (`salphaDeclined`), as the DB path does when it builds `webReport` |

**Never published** (not in any DTO, and caught by 4.3 if it leaks through a
text): the private delivery group's invite link and any link into it; anything
from `scout_deliveries` / `scout_delivery_investigations`; `SCOUT_NOTIFY_PEER`
and `SCOUT_NOTIFY_CHAT_ID` (in the forms of 4.2.1);
`request_text`, `bot_message_ids`, `details`, `error`, `external_id`,
`created_by`/`updated_by`; secrets (the values of `API_HASH`, `TG_PASSWORD`,
`PHONE`, `SCOUT_NOTIFY_BOT_TOKEN`, `SCOUT_PRICE_API_KEY`, the database
password); any environment value; server, database or node addresses (host
names, IPs, ports, paths, DSNs, RPC URLs, `SCOUT_WEB_ADDR`); log text.

### 4.2 Text scrub (every string of every part)

Apply the scrub to **every string field of every DTO** (walk them all), not
only report texts: `TokenName`, `TokenSymbol`, `QuoteSym`, `ChannelUsername`
and the rest come from token metadata or posts that anyone can write, and a
token named after a private invite link must be scrubbed, not stop the whole
export at the deny check (4.3). Fields that must keep their exact value
(`contract_address`, enum-like fields) never match the patterns in practice;
if one ever does, the scrub still wins (the field shows `[link removed]`).

Do **not** reuse `inviteLinkRe` (main.go): it is built to find a hash to join,
not to remove whole links in every encoding. Use one explicit,
case-insensitive RE2 pattern that removes the **whole URL**. Built from these
pieces (Go raw strings; this exact block was tested in a scratch program on
9 Oct 2026 against 24 strings: every link variant of the "scrub" test bullet
in 7 plus three negatives, and one serialised-JSON deny check):

```go
const (
	xDot   = `(?:\.|%2e|&#0*46;|&#x0*2e;|&period;)`
	xSlash = `(?:/|\\/|%2f|&#0*47;|&#x0*2f;|&sol;)` // also JSON's \/
	xPlus  = `(?:\+|%2b|&#0*43;|&#x0*2b;|&plus;)`
	xColon = `(?::|%3a|&#0*58;|&#x0*3a;|&colon;)`
	xRest  = `[^\s"'<>()\[\]{}]*` // rest of the URL
)

var webPrivateLinkRe = regexp.MustCompile(`(?i)` +
	// t.me / telegram.me / telegram.dog, with or without scheme and www,
	// followed by /+… /joinchat… /c/… /addlist/…
	`(?:(?:https?` + xColon + xSlash + xSlash + `)?(?:www` + xDot + `)?` +
	`(?:t` + xDot + `me|telegram` + xDot + `(?:me|dog))` + xSlash +
	`(?:` + xPlus + `|joinchat|c` + xSlash + `|addlist` + xSlash + `)` + xRest + `)` +
	// tg: links, with or without //
	`|(?:\btg` + xColon + `(?:` + xSlash + xSlash + `)?[a-z]` + xRest + `)`)
```

Covers: `t.me/+…`, `t.me/%2B…`, `t.me/&#43;…`, `t.me/joinchat/…`, `t.me/c/…`,
`t.me/addlist/…`, the same on `telegram.me` and `telegram.dog`, any letter
case, percent-encoded (`https%3A%2F%2Ft.me%2F%2B…`), HTML-entity-encoded
(`t&#46;me&#x2f;&plus;…`, `&sol;`), JSON-escaped slashes, and `tg:` /
`tg://` links. It does **not** touch public `t.me/<channel>` links or
`@handles`. It may over-match a non-Telegram host ending in `t.me` followed by
`/+`; that is acceptable (safer).

**Not covered** (accepted, noted only): a link broken up with whitespace or a
zero-width character (`t .me/+…`, a U+200B inside `t.me`), look-alike letters
(Cyrillic `т`, full-width `．` or `／`), or spelled out ("t dot me slash
plus …"). None of these is clickable; a reader would have to retype it.
Unicode NFKC folding (`golang.org/x/text/unicode/norm`) before matching would
catch the full-width forms if wanted later; not required now.

Scrub of one string `s`:

1. `s = webPrivateLinkRe.ReplaceAllString(s, "[link removed]")`.
2. Every pattern of the configured-value set (4.2.1), the digit-only ones
   included (strings are where they apply): replace each match with
   `[removed]`.
3. Second pass on a **normalised copy** `n, stable := normaliseForMatch(s)`
   (below). If `webPrivateLinkRe` or any pattern of 4.2.1 matches `n`, **or**
   `stable` is false (still changing after 3 rounds: more than triple
   encoding, treated as suspicious), replace the **whole field** with
   `[text removed: it contained a private link or a server detail]`.

The normalisation must be **lenient**. Do **not** use `url.QueryUnescape` or
`url.PathUnescape`: they fail on the whole string at the first malformed
escape, so a stray `%` anywhere ("up 50% t.me/%252B…") makes them return an
error, nothing is decoded, and a double-encoded link passes both the scrub and
the deny check (verified: `url.PathUnescape("up 50% t.me/%252Babc")` returns
`invalid URL escape "% t"`). `QueryUnescape` would also turn `+` into a space.

```go
// percentDecodeLenient decodes every valid %XX (two hex digits) and leaves
// everything else unchanged: a lone '%', "%4", "%zz", and '+' (never a
// space). It never fails.
func percentDecodeLenient(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if hi, ok := unhex(s[i+1]); ok {
				if lo, ok := unhex(s[i+2]); ok {
					b = append(b, hi<<4|lo)
					i += 2
					continue
				}
			}
		}
		b = append(b, s[i])
	}
	return string(b)
}

// unhex: '0'-'9', 'a'-'f', 'A'-'F' -> value, true; anything else -> 0, false.

// normaliseForMatch runs up to 3 rounds of html.UnescapeString followed by
// percentDecodeLenient, stopping early when a round changes nothing. stable
// is false when a 4th round would still change the string.
func normaliseForMatch(s string) (n string, stable bool) {
	for i := 0; i < 3; i++ {
		next := percentDecodeLenient(html.UnescapeString(s))
		if next == s {
			return s, true
		}
		s = next
	}
	return s, percentDecodeLenient(html.UnescapeString(s)) == s
}
```

The decoded copy may hold invalid UTF-8 (for example from `%FF`). That is not
an error: RE2 matches invalid bytes as U+FFFD, and the copy is only used for
matching. This block and the "lenient decoder" vectors of 7 were run in a
scratch program on 9 Oct 2026 (Go 1.21): all passed, and `url.PathUnescape`
on the first vector returned the error above. `handlePattern` (4.2.1) and its
handle vectors were run the same way. The invite-hash, digit-only and secret
vectors of 7 have **not** been run.

The page renders these strings as text (`textContent`), so unescaping is
only used for matching, never for output.

### 4.2.1 Configured values (notify target, secrets, server addresses)

Built once when the exporter starts, from the configuration prod `-web`
already loads (`loadConfig`, `loadPriceConfig`, `loadWebConfig`, the DSN of
`database.SetupDatabase`). Kept in memory only, never logged, never exported.
Each pattern carries the **name** of its variable, which is all a refusal log
line may name (4.3).

**`SCOUT_NOTIFY_PEER` and `SCOUT_NOTIFY_CHAT_ID`**, classified in the order
`resolveNotify` (main.go ~1352) parses the peer:

| Form | What is matched |
|---|---|
| empty, `me`, `self` | nothing |
| `inviteLinkRe` matches (`t.me/+HASH`, `t.me/joinchat/HASH`, `join?invite=HASH`, a bare `+HASH`) | the hash (`m[1]`, at least 8 characters), **case-sensitive** (Telegram hashes are), anywhere in a string. The link itself is already removed whole by `webPrivateLinkRe` |
| `strconv.ParseInt` succeeds: a raw id, a Bot-API `-100…` id, a basic-group `-…` id | **nothing extra.** A supergroup or channel id appears in a link only as `t.me/c/<id>/…`, which `webPrivateLinkRe` removes whole; a basic group has no link form. Never matched as a bare number, neither in the raw JSON nor inside strings: it could equal an mcap, an id or a holder count |
| starts with `@`, or matches `usernameRe` (4–32 characters) without `@` | the handle, case-insensitive, **only** as `@handle` or `t.me/handle` / `telegram.me/handle` / `telegram.dog/handle` (in the encodings of `xDot`/`xSlash`), **followed by a word boundary**: `@scout` must not hit `@scoutrobinhood`, or every post link of the source channel would be scrubbed. A `usernameRe` value without `@` could also be a one-word chat title; it is treated as a handle (the safer reading) |
| anything else: a chat title (`findDialog` by title) | **nothing.** A title is free text ("scout analytics") and scrubbing it would cut harmless words out of reports and token names; a title alone gives no access to the group. Documented gap |

```go
func handlePattern(h string) *regexp.Regexp { // h without '@'
	return regexp.MustCompile(`(?i)(?:@|(?:t` + xDot + `me|telegram` + xDot + `(?:me|dog))` + xSlash + `)` +
		regexp.QuoteMeta(h) + `\b`)
}
```

`SCOUT_NOTIFY_CHAT_ID` (Bot API: a numeric id or `@channelusername`) goes
through the same table.

**Secret values**, matched exactly and **case-sensitive**
(`regexp.QuoteMeta`), skipped when shorter than **8** characters (too likely to
occur in harmless text):

- `API_HASH`, `TG_PASSWORD`, `SCOUT_PRICE_API_KEY`;
- `SCOUT_NOTIFY_BOT_TOKEN`: the whole value and the part after its first `:`;
- the database password: from `SCOUT_DATABASE_URL`, or the one
  `database.SetupDatabase` uses (`DB_PASS`, or the content of the file named by
  `DB_SECRET_PATH`). The coder exposes it from the database package or parses
  the DSN; database **user** names are skipped (short and common);
- path segments of `SCOUT_RPC_URL` / `SCOUT_MAINNET_RPC_URL` of 16 or more
  characters (an API key in the URL path);
- `PHONE`: see the digit-only rule.

**Digit-only values** (`PHONE`, optionally with a leading `+`, and any other
value above that is all digits): matched only **inside string values**, as a
run not touching other digits (`(?:^|\D)\+?<digits>(?:\D|$)`), never in the
raw JSON bytes, where they could equal a number.

**Server addresses**, case-insensitive: the full value, the host, and
`host:port`, of `SCOUT_DATABASE_URL` (or the host `database.SetupDatabase`
connects to, and the instance name from `INSTANCE_SECRET_PATH`),
`SCOUT_RPC_URL`, `SCOUT_MAINNET_RPC_URL`, `SCOUT_MODEL_URL`,
`SCOUT_WEB_ADDR` and `SCOUT_PRICE_API_BASE`. Skipped: hosts shorter than 6
characters, `localhost`, loopback and unspecified addresses (`127.0.0.0/8`,
`::1`, `0.0.0.0`, `::`) and an empty host. So the usual `SCOUT_WEB_ADDR`
forms (`:8090`, `127.0.0.1:8090`) add nothing; a LAN or public address does.
`SCOUT_PRICE_API_BASE` defaults to the public `https://api.geckoterminal.com/api/v2`,
a host that report texts may well mention: when its host is one of a short
list of **known public API hosts** in the code (`api.geckoterminal.com`,
`api.coingecko.com`, `pro-api.coingecko.com`), nothing is matched for it (the
key is matched separately); any other host (for example a private proxy) is
matched with its full value.

### 4.3 Deny check after serialisation (last line of defence)

After the three parts are serialised (before gzip), the exporter scans them
and **refuses to upload anything** (no part, no pointer) if a match remains:

- Scan (a): the raw bytes of each part with `webPrivateLinkRe` and the
  patterns of 4.2.1 that are **not digit-only**, i.e. invite hashes, handles,
  secrets and server addresses (works because of `SetEscapeHTML(false)`, 2.1).
  None of these can match a JSON number: handles are word-bounded strings,
  and an invite hash or secret that is all digits counts as digit-only.
- Scan (b): unmarshal each part into `any`, walk every string, normalise it
  with `normaliseForMatch` (4.2, the lenient decoder, never
  `url.QueryUnescape`/`PathUnescape`), and match `webPrivateLinkRe` and
  **every** pattern of 4.2.1, the digit-only ones included (catches
  `\u002b`-style JSON escapes and encoded forms). A string whose
  normalisation is not stable after 3 rounds is a hit.
- Because 4.2 scrubs every string, a hit here means a bug (a field the scrub
  missed, a new encoding). On a hit: log at once, every time, at error level,
  **without** the matched text and **without any configured value**, naming
  only the part, the field, the row and the pattern's name:
  `web export: REFUSED to upload: part reports, field text of report 123
  matches a private-link pattern — the public site keeps the data of 12:00:15`
  (or `… matches SCOUT_PRICE_API_KEY`, `… matches the SCOUT_NOTIFY_PEER handle`).
  Keep exporting nothing until a snapshot passes; the public site goes stale
  rather than leak. Count refusals in the health of the export (log line every
  10 minutes while it lasts).

### 4.4 Report links (`perceptor_url`, `perceptor_today_url`, report `url`)

Publish only when: `https://`, no user info, host (lower case, exact match or a
subdomain of) one of `SCOUT_WEB_EXPORT_URL_HOSTS`, and not matched by
`webPrivateLinkRe`. Otherwise export `null`. Owner decision: `perceptor.info`
is allowed. Other hosts (for example sAlpha's, if it has links) are added after
the owner checks them with this **read-only** query (pgAdmin):

```sql
-- Report link hosts by tool (read-only)
SELECT t.code AS tool,
       lower(substring(i.report_url from '^[a-zA-Z]+://([^/:?#]+)')) AS host,
       count(*) AS n
FROM scout_investigations i
JOIN scout_investigation_tools t ON t.id = i.tool_id
WHERE i.report_url IS NOT NULL
GROUP BY 1, 2
ORDER BY 1, 3 DESC;

-- How many report texts, labels or summaries contain private Telegram links,
-- in any of the encodings the scrub handles (read-only)
SELECT t.code AS tool, count(*) AS n
FROM scout_investigations i
JOIN scout_investigation_tools t ON t.id = i.tool_id
WHERE concat_ws(' ', i.report_text, i.verdict_label, i.verdict_summary)
      ~* '(t|telegram)(\.|%2e|&#0*46;|&#x0*2e;|&period;)(me|dog)(/|\\/|%2f|&#0*47;|&#x0*2f;|&sol;)(\+|%2b|&#0*43;|&#x0*2b;|&plus;|joinchat|c(/|%2f|&sol;)|addlist)|\mtg(:|%3a|&#0*58;|&#x0*3a;|&colon;)'
GROUP BY 1;
```

## 5. Read side (Cloud Run, `SCOUT_WEB_SOURCE=gcs`)

### 5.1 Start

- Branch on `SCOUT_WEB_SOURCE=gcs` (with `-web`) **right after
  `flag.Parse()`** (main.go ~1607), before anything that needs a database or
  `.env`:
  - `loadConfig` (~1612; it requires `API_ID`);
  - `newScanner`, `openScoutStore`, `db.Migrate`, `s.registerTools`
    (the whole block ~1632–1652);
  - the database-only mode switch (~1655), and inside it `classifyPosts(ctx, s.db)`
    (~1708) and `runWeb(ctx, s.db, …)`;
  - `database.SetupDatabase`, the `LISTEN` connection.
  Call a new `runWebGCS(ctx, loadWebConfig(), gcsConfig)` instead. A missing
  `.env` is not an error in this mode.
- Start listening **at once** (unlike today, where the first snapshot is read
  before listening). Until the first snapshot is loaded, `/api/health` and the
  API answer 503 with `Retry-After` (the existing `snapshot()` path). Cloud
  Run's startup probe on `/api/health` keeps visitors away until then.
- Load the first snapshot with retries (1 s, 2 s, 4 s … max 30 s). Do not exit
  on failure: the startup probe (60 s) replaces an instance that never loads.

### 5.2 Checking for a new snapshot (request-based billing)

Cloud Run is configured with **request-based billing**: the instance gets CPU
only while it handles a request (and during start). Work started by a request
and left running after the answer is sent is throttled to almost nothing and
may take minutes or never finish. So **the whole check happens inside a
request**:

- **On request** (any path except `/api/health`): if the last finished check
  is older than `SCOUT_WEB_GCS_POLL` and none is in flight, start one
  (single-flight) with a context timeout of **10 s**. The request that started
  it **waits for the whole check** (pointer, downloads, hash checks, gunzip,
  decode, `newWebSnapshot`), bounded by those 10 s, and only then answers,
  from the new snapshot if it succeeded. Other requests that arrive meanwhile
  never wait; they answer from the snapshot in place.
- **Dead checks**: a check that started more than **30 s** ago and has not
  finished (for example it was left without CPU) counts as dead. The next
  request cancels its context and starts a new one. A dead check that wakes
  up later must not overwrite anything: swap a result in **only if its
  `snapshot_at` is newer** than the snapshot in place (compare-and-swap on the
  `atomic.Pointer`).
- **Background ticker**: every `SCOUT_WEB_GCS_POLL`, best effort only (useful
  with min instances or instance-based billing; harmless otherwise). Nothing
  relies on it.
- A check: GET the pointer (tiny). Same `content_sha256` as the snapshot in
  place: move confirmed-at forward to its `uploaded_at` (heartbeat, 3.6),
  done. Otherwise, for each part whose `sha256` differs from the part in
  memory: GET it, reject if larger than `gz_size` or 64 MB, gunzip with a cap
  (64 MB; the 512 MiB instance must hold two snapshots during the swap), check
  the **uncompressed** sha256 against the name, decode, check `format`. Then
  merge rows and prices, build the snapshot with `newWebSnapshot`/
  `webReportsFor`, set `loadedAt` to the pointer's `snapshot_at`, swap, and
  set confirmed-at to the pointer's `uploaded_at`.
  Keep the decoded parts (and in delta mode the base) in memory for the next
  check. Any error in any part: keep the **whole** old snapshot (never mix
  parts of two pointers), log at most once a minute.
- Object 404 (the lifecycle rule deleted it after 30 days without prod): keep
  the old snapshot; health reports stale.
- Never fall back to anything but the last good snapshot (3.5, "Rejected").

### 5.3 HTTP changes (gcs mode, unless noted)

- **`GET /api/health`** (new, **both modes**; a path not ending in `z`, because
  Cloud Run reserves some paths ending in `z`):
  - 503 `{"status":"starting"}` before the first snapshot;
  - 200 `{"status":"ok"|"stale","source":"gcs"|"db","snapshot_at":"…","confirmed_at":"…","age_seconds":12.3}`;
    `stale` when now − confirmed-at exceeds `SCOUT_WEB_STALE_AFTER` (3.6; in
    `db` mode confirmed-at is `loadedAt`). Still 200 when stale (a restart
    would not fix it). `Cache-Control: no-store`. No bucket name, error text
    or host details.
  - It does **not** trigger a check (5.2): it must stay fast for the 2 s
    startup probe. So its `status` is only as fresh as the **last finished
    check** of that instance, and on an otherwise idle instance it can say
    `ok` long after prod stopped. **Use `/api/summary` for alerting**, not
    this (README step 8).
- **`GET /api/summary`**: `updated_at` and `snapshot_age_seconds` come from the
  exporter's `snapshot_at` (the data's time). New fields: `prices_at`,
  `confirmed_at` (3.6), `price_refresh_seconds` (60), `source` (`"gcs"`),
  `live` (false when events are off), `stale` (bool, as in health, computed
  after the request's own check), `manual_refresh` (false in gcs mode).
  Like every path but health, it triggers the bounded check of 5.2, so an
  uptime check on it sees the current pointer (allow > 10 s). The encoder
  writes `"stale":false` with no space; the uptime check matches that text.
  If the summary answers 304 to an `If-None-Match`, its ETag must cover
  `confirmed_at` and `stale` too, or a heartbeat would never reach the page.
- **Banner** (frontend): a note at the top of the page, for example "Prices
  refresh about every minute. Data as of 12:00:15, checked 12:41:02." (data
  time from `updated_at`, check time from `confirmed_at`; they differ when the
  data has been quiet) plus a warning line when `stale` is true. In `db` mode
  the page can keep showing it or not (owner's choice).
- **`POST /api/refresh`** ("Refresh now"): **not registered in gcs mode**
  (answers 404 like any unknown `/api/` path). The page hides the button when
  `summary.manual_refresh` is false. The public site gains nothing from it
  (data changes at most once a minute) and it would be the only POST, so the
  `sameOrigin` / Host-header question behind a Firebase Hosting rewrite does
  not arise. `db` mode is unchanged.
- **`GET /api/events`** with `SCOUT_WEB_EVENTS=off`: answer **204 No Content**
  (`Cache-Control: no-store`) at once. The page must not open the stream when
  `summary.live` is false. Today it retries a refused stream every 120 s
  (`LIVE_RETRY_MS`) and polls every 30 s anyway (`REFRESH_MS`), so polling is
  the fallback that already exists.
- **Caching behind Firebase Hosting**: Firebase's CDN caches a Cloud Run answer
  only when it sends `Cache-Control: public` with `max-age`/`s-maxage`. The API
  sends `no-cache`, so nothing is cached, which is what we want. Static files
  may use `public, max-age=300` later if wanted (coder/owner decision).
- Optional: `Strict-Transport-Security: max-age=31536000` when the request came
  over HTTPS (`X-Forwarded-Proto: https`). Owner/coder decision.

### 5.4 Why no SSE on Cloud Run

- Every open stream is an in-flight request. With request-based billing the
  instance is billed as **active** for as long as any tab is open, and it
  never scales to zero. One tab open around the clock ≈ 2.6 M active
  vCPU-seconds a month, far past the free tier.
- Streams are cut at the request timeout (max 60 min; `service.yaml` sets 30 s;
  Firebase Hosting rewrites also cut at 60 s).
- Each instance has its own event hub. With several instances, a reconnect may
  land on an instance whose event ids differ, and the page gets `reload`
  (see DEPLOY.md 7.2).
- Polling (every 30 s, ETag/304) is cheap and already in the page.

## 6. Later: a gated version on the same service

Keep room now:

- Bucket prefixes `public/` and `gated/`. The exporter would write a second
  pointer/parts set under `gated/` with more fields. The public read path
  never reads `gated/`.
- The same Cloud Run service can serve gated endpoints (for example `/api/g/…`)
  that check a Firebase Auth / Identity Platform ID token in a Go middleware
  (Firebase Hosting is already in front), or sit behind IAP (needs a load
  balancer or IAP's Cloud Run integration; check its status then).
- The runtime SA already reads the whole bucket. To keep the public service
  from reading `gated/`, split the services or add an IAM condition on the
  object prefix then.

## 7. Tests the coder should add (no GCS, no database)

- An interface over the few GCS calls (get object, put object with optional
  `ifGenerationMatch`, returning generation and HTTP-like errors), a fake in
  memory, and table tests for:
  - upload order (pointer only after every changed part), only changed parts
    uploaded, 412 handling, backoff with "latest wins", the 1-minute floor;
  - `content_sha256` excludes `snapshot_at`; the start-up "skip if same";
  - the 12-hour keep-alive re-uploads **every** part the pointer names;
  - determinism: the same rows in a different order give the same hashes;
  - delta prices (when built): cumulative since base, new base rules, a delta
    with a foreign `base` is rejected;
  - the allowlist: a test that fails when a DTO struct gains a field that is
    not in an approved list;
  - **the scrub**, with at least: upper/lower/mixed case (`T.ME/+`,
    `TeLeGrAm.DoG`), `t.me/%2B…` and `%2b`, a fully percent-encoded URL
    (`https%3A%2F%2Ft.me%2F%2B…`), HTML entities (`&#43;`, `&#x2b;`, `&plus;`,
    `&#46;`, `&#x2F;`, `&sol;`), double encoding (`&amp;#43;`), JSON-escaped
    `\/`, `t.me/joinchat/…`, `t.me/c/…`, **`t.me/addlist/…`** on all three
    hosts, `tg://…` and `tg:resolve…`, the fake links of `notify_test.go`; and
    negatives that must stay: public `t.me/<channel>/<n>`, `@handles`,
    `https://perceptor.info/…`, `rating: ok`;
  - **the lenient decoder** (`percentDecodeLenient`, `normaliseForMatch`),
    each through the scrub **and** through deny scan (b) with the scrub
    bypassed. Must be removed / refused:
    `up 50% t.me/%252Babc12345` (double-encoded next to a stray `%`; the case
    `url.PathUnescape` misses), `100%%2B t.me/%2Babc12345`,
    `up 50% https%253A%252F%252Ft.me%252Fjoinchat%252Fabc12345`,
    `%zz t.me%2F%2Babc12345 %`, `t.me/&amp;#37;2Babc12345`,
    `a+b t.me/+abc12345` (normalises to itself: `+` kept), and
    `t.me/%25252525252Babc12345` (not stable after 3 rounds: whole field
    removed). Must stay untouched: `a+b` (normalises to `a+b`, never `a b`),
    `&amp;#37;2B` alone (normalises to `+`, no match), `up 50% today`,
    `ends with %`, `ends with %2`, `https://t.me/scoutrobinhood/123 50%`;
  - **notify peer forms** (4.2.1), for both `SCOUT_NOTIFY_PEER` and
    `SCOUT_NOTIFY_CHAT_ID`: `me`/`self`/empty add nothing; an invite link
    and a bare `+HASH` value remove the hash in a text (case-sensitive: the
    same hash in another case stays); `-1001234567890`, `1234567890` and
    `-4012345678` add no pattern (a row whose mcap or holder count equals
    those digits is exported unchanged and not refused), while
    `t.me/c/1234567890/5` is still removed; `@scoutalerts` and `scoutalerts`
    remove `@ScoutAlerts`, `https://t.me/scoutalerts/7` and
    `t%2Eme%2Fscoutalerts` but **not** `@scoutalertsbot`, `scoutalerts_x` or
    the plain word; a title such as `scout analytics` adds nothing;
  - **secrets and addresses** (4.2.1): a planted `API_HASH`,
    `SCOUT_PRICE_API_KEY`, bot token (whole and the part after `:`),
    database password, a LAN `SCOUT_WEB_ADDR` host, a private
    `SCOUT_PRICE_API_BASE` host are scrubbed; values under 8 characters,
    `:8090`, `127.0.0.1:8090` and the default geckoterminal base add
    nothing (a report mentioning `api.geckoterminal.com` is exported); a
    `PHONE` value matches inside a string but never a JSON number with the
    same digits; the refusal log line names the variable and never contains
    the value;
  - the scrub runs on **every string**: a private link in `token_name`,
    `token_symbol` or `quote_sym` is scrubbed and the export still uploads;
  - **the deny check** (scrub bypassed): a DTO with a private link, a peer
    hash or handle, a secret or a DB/RPC host planted in each part and in
    each encoding is refused (no part, no pointer uploaded); a clean export
    passes;
  - **the heartbeat** (3.6): with unchanged data, a pointer write with a new
    `uploaded_at` and nothing else changed every `SCOUT_WEB_EXPORT_HEARTBEAT`;
    none while the newest snapshot differs from the published one (pending,
    failing or **refused** upload); **none when no snapshot arrived since the
    last pointer write** (database reads failing); one at once on the
    start-up adopt path;
    reader: same hash moves confirmed-at forward (never back) and leaves
    `loadedAt` and the cached answers alone; `stale` follows confirmed-at;
  - the link host check;
  - reader: uncompressed hash mismatch, `gz_size` exceeded, unknown `format`,
    404 of a part, one bad part keeps the whole old snapshot;
  - the on-request check: single-flight, the starting request waits for the
    whole check (bounded at 10 s), a check older than 30 s is replaced, a late
    result with an older `snapshot_at` is not swapped in;
  - `/api/health` before and after the first snapshot; `/api/events` = 204 when
    off; `/api/refresh` = 404 in gcs mode; `summary.manual_refresh`.
- A round trip with **every** field: rows (as after `fillTrades24h`) → three
  parts → read side → `newWebSnapshot`: the same `/api/calls`, `/api/call` and
  `/api/analytics` answers as prod builds from the rows directly.
- A test that `newWebSnapshot` does not change the exported copy of the rows.
- `gcs` mode starts with no `.env`, no `API_ID` and no database (5.1).
- Library: `cloud.google.com/go/storage` is the usual choice (it adds
  dependencies and binary size); the JSON API over `golang.org/x/oauth2/google`
  is a smaller alternative. Either way: ADC only, no credentials in code.
