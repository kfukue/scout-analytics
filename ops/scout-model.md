# Scoring service on the prod server (`scout-model.service`)

> **Draft for the owner. Nothing here has been installed or tested on the
> server.** Not a pgAdmin folder: nothing here touches the database.

`ml/serve.py` answers `GET /health` and `POST /score` on `127.0.0.1:8601`. The
Go listener calls it only when `SCOUT_MODEL_URL` is set (off by default). A
service that is down or slower than `SCOUT_MODEL_TIMEOUT` (default `10s`; the
pre-call trades, the feature row and the scoring request share that one budget)
never drops a delivery; the call is delivered without the model line. Scores
never filter deliveries.

The service needs no secrets and never sees `.env`, `scout.session.json` or
`scoutanalytics_data/` (the unit hides them). Prod runs it with
`SCOUT_MODEL_LINE=all` (owner decision 12 Oct 2026; see "Delivery line mode").

Paths below: the deployed checkout (with `.env`, the same `$REPO` as in
`ml/RUNBOOK.md`) is `/path/to/scout-analytics` (`$REPO`), training lives in
`~/scout-ml` (venv `~/scout-ml/venv`, models `~/scout-ml/models/<version>/`
and `~/scout-ml/models/LATEST`). Run everything as the account that runs
`ml/run_training.sh`, not as root (except `sudo`), in one shell so that
`$REPO` and `$ML` stay set.

```bash
REPO=/path/to/scout-analytics  # <<< EDIT: absolute path of the deployed checkout (no ~ in quotes, no trailing /)
ML=~/scout-ml
```

## 1. Check before installing

```bash
ls -ld "$ML/models" "$ML/models/LATEST"      # owned by you (umask 077: only you can read them)
cat "$ML/models/LATEST"                       # the version that will be served
"$ML/venv/bin/python" -c 'import fastapi, uvicorn, lightgbm; print("ok")'
# same ml/ code as the model was trained with? (empty output = same)
git -C "$REPO" diff --stat HEAD "$(git -C "$ML/src" rev-parse HEAD)" -- ml/scout_ml ml/serve.py ml/requirements.txt
```

If the diff is not empty, the deployed checkout's `ml/` differs from the code
that trained the model (training runs from `$ML/src`, origin/main at train
time). Pull `main` in `$REPO` first (DEPLOY.md 3.0) and tell the product
manager. If `ml/requirements.txt` changed, also run
`"$ML/venv/bin/python" -m pip install -r "$REPO/ml/requirements.txt"` before
starting or restarting the service. (If `$ML/src` was already removed by the
RUNBOOK cleanup, the `rev-parse` fails. Use the commit that `run_training.sh`
printed (`ml/ is up to date (<commit>)`) in place of the `$(...)` part, or
skip this check and tell the product manager.)

`SCOUT_MODEL_LINE` needs the `ml/` code from `main` at c31d859 or later
(`grep -c SCOUT_MODEL_LINE "$REPO/ml/serve.py"` prints more than `0`). An
older `serve.py` ignores the variable.

## 2. Install (not enabled, not started)

```bash
case "$REPO" in /*) test -f "$REPO/ml/serve.py" && echo "REPO ok: $REPO" || echo "no ml/serve.py in $REPO";; *) echo "REPO must be an absolute path: '$REPO'";; esac
cd "$REPO"
sed -e "s|__REPO__|$REPO|g" -e "s/__USER__/$(id -un)/g" ops/scout-model.service | sudo tee /etc/systemd/system/scout-model.service >/dev/null
grep -n '^User=' /etc/systemd/system/scout-model.service   # must be your account, not root
grep -n '^WorkingDirectory=' /etc/systemd/system/scout-model.service   # must be $REPO/ml
grep -n '__USER__\|__REPO__' /etc/systemd/system/scout-model.service   # must print nothing
sudo systemd-analyze verify /etc/systemd/system/scout-model.service
sudo systemctl daemon-reload
```

Do not go on unless the first line prints `REPO ok`. Run the `sed` line in
your own shell, not inside `sudo -i` or `sudo bash`: there `$(id -un)` is
`root` (the unit would run as root with paths under `/home/root`) and `$REPO`
is not set. Only `tee` needs `sudo`.

`__USER__` sits in `User=` and in the venv and model paths (`SCOUT_MODEL_DIR`,
`ExecStart`); `__REPO__` stands for the checkout in `WorkingDirectory`,
`ExecStart` (`--app-dir`) and `InaccessiblePaths`. There is no `Group=` line:
systemd uses your account's primary group. If your home is not `/home/<user>`,
fix the `scout-ml` paths in the installed file. The header comment lines that
mention `__USER__` and `__REPO__` also get your user name and checkout path;
they read oddly afterwards and can be ignored.

## 3. Start

```bash
sudo systemctl enable --now scout-model
systemctl status scout-model
systemctl show scout-model -p Environment     # includes SCOUT_MODEL_LINE=all and OMP_NUM_THREADS=2 (no secrets in this unit)
curl -s http://127.0.0.1:8601/health
# {"ok":true,"model_version":"20261008-1852"}
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8601/health   # 200
```

A sample score with a made-up row (no real call; nulls and missing columns are
fine). `/score` only computes; it never writes to the database:

```bash
curl -s -X POST -H 'Content-Type: application/json' \
  --data '{"row":{"mcap_usd":50000,"liq_usd":20000,"tax_buy_pct":0}}' \
  http://127.0.0.1:8601/score
# {"model_version":"...","line":"Model ... · 1d (not validated): runner ..%, collapse ..% · 3d: runner ..%, collapse ..% · 7d: runner ..%, collapse ..%","buckets":[...]}
```

With `all`, `line` names every bucket that has a model, by horizon; the ones
that did not pass their gates carry `(not validated)` right after the horizon
(which ones do depends on the version's `report.md`). There is no `30d` yet:
training skips the long bucket until enough calls have a matured 30-day
outcome (not before an export from about late December 2026). Once it has a
model it is shown as `30d: up ..%, collapse ..%` (or
`30d (not validated): up ..%, …`): "up" instead of "runner".

`/health` 503 means the process runs but finds no usable model (wrong
`SCOUT_MODEL_DIR`, missing `LATEST` or version directory, or permissions);
systemd does not restart for that. The error text names the directory.

If `systemctl start` fails or the service keeps restarting, see the journal
(Logs below). After 5 failed starts within 10 minutes systemd stops trying and
refuses further starts. Fix the cause, then:

```bash
sudo systemctl reset-failed scout-model && sudo systemctl start scout-model
```

Optional: `systemd-analyze security scout-model` (lower is better).

## 4. Switch the listener on and off

On: add this line to `$REPO/.env` with an editor (never `cat` the file):

```
SCOUT_MODEL_URL=http://127.0.0.1:8601
```

Check without showing values: `grep -c '^SCOUT_MODEL_URL=' "$REPO/.env"`
prints `1`. `SCOUT_MODEL_TIMEOUT` can stay unset (default `10s`). Then
restart the listener so it reads `.env` again: Ctrl+C the terminal listener
(or `sudo systemctl stop scout-listener` once that unit is installed), check
that `pgrep -af listen-only` prints nothing, and start it again the same way
(DEPLOY.md 4, "Daily use"). Its log then shows a `score <CA>: Model ...` line
per new call, with the pre-call time (`(precall 3.2s)`); `no score within
SCOUT_MODEL_TIMEOUT` lines mean the budget ran out. Scores are written to
`scout_call_predictions` in `assetdb`, the database shared with the main API
(one row per bucket per new call).

Off: delete the line (the count prints `0`), restart the listener the same
way. The service may keep running; `sudo systemctl disable --now scout-model`
stops it.

`SCOUT_MODEL_LINE` belongs to the service (the unit), not to the listener's
`.env`: the listener only appends the `line` it gets back.

## Delivery line mode (`SCOUT_MODEL_LINE`)

The unit sets `Environment=SCOUT_MODEL_LINE=all`. It only changes the `line`
that the listener appends to a delivery; scores, `passed` and what is stored in
`scout_call_predictions` are the same in both modes, and scores never decide
whether a call is delivered.

- `all` (prod): every bucket with a model, by horizon (`1d`, `3d`, `7d`,
  `30d`); a bucket that did not pass its gates is marked `(not validated)`.
  The line is empty only when no bucket has a model.
- `passed` (the default when unset): only buckets that passed their gates.
  When none passed, the line is empty and the delivery has no model line.

Any other value counts as `passed` and logs one warning in the journal.

serve.py reads the variable on each request, but a running process does not
see a changed unit, so a switch takes a restart of this service only. To go
back to `passed`:

```bash
sudoedit /etc/systemd/system/scout-model.service   # SCOUT_MODEL_LINE=all -> passed (or delete the line)
sudo systemctl daemon-reload
sudo systemctl restart scout-model
systemctl show scout-model -p Environment          # check the value
curl -s http://127.0.0.1:8601/health               # 200
```

No retrain, no `LATEST` change and no listener restart. The next delivery uses
the new mode. A delivery that falls in the few seconds of the restart is sent
without the model line. If you change `ops/scout-model.service` in the repo
too, install it again as in step 2 instead.

## Logs

```bash
journalctl -u scout-model -f
journalctl -u scout-model --since "1 hour ago"
```

uvicorn logs one access line per request (`POST /score 200`); request bodies
are not logged. The listener logs its side (`score ...`, timeouts, HTTP
errors).

`Start request repeated too quickly` / `start-limit-hit` in the journal or in
`systemctl status`: after 5 failed starts within 10 minutes systemd refuses to
start the service. Fix the cause (the lines before it say why), then
`sudo systemctl reset-failed scout-model && sudo systemctl start scout-model`.

## After a retrain

`run_training.sh` writes a new `$ML/models/<version>/` and then `LATEST`. The
service reads `LATEST` on every request, so the next request uses the new
version; no restart. Check with `curl -s http://127.0.0.1:8601/health`.

Restart (`sudo systemctl restart scout-model`) only when the `ml/` code in
`$REPO` changed (after pulling `main`): Python loads it once at start. Run the
diff from step 1 after each retrain.

Compare the new `report.md` with the previous one. With `all`, a bucket that
failed its gates is still shown on deliveries, marked `(not validated)`, so a
failed retrain does not have to be rolled back to keep it off. If the new
version is worse than the previous one, roll back as below.

## Rollback

Model (return to a previous version): write its name into `LATEST` (a new
training run overwrites it again).

```bash
ls "$ML/models"                                  # version directories
printf '%s\n' 20261008-1852 > "$ML/models/LATEST"   # EDIT: the version to serve
curl -s http://127.0.0.1:8601/health             # shows that version
```

Delivery line: back to `passed` as in "Delivery line mode".

Service: switch the listener off (step 4), then

```bash
sudo systemctl disable --now scout-model
sudo rm /etc/systemd/system/scout-model.service
sudo systemctl daemon-reload
```
