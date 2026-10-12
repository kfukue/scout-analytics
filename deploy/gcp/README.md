# Public website on GCP: owner runbook (DRAFT)

Status: **draft, nothing created.** No command here has been run. Every
`gcloud`/`firebase` command that creates or changes something is for the owner
to run, in order, after reading the cost note. Placeholders are in CAPITALS or
`__LIKE_THIS__` (in files rendered with `sed`).

Design (owner decisions, 9 Oct 2026):

- The **prod server** (Postgres + Robinhood Chain node, never exposed) runs
  `-web` as today. A new export inside it uploads the website snapshot to a
  **private GCS bucket**, split into three parts by how often they change
  (reports, rows, prices), at most once a minute and only the parts that
  changed.
- A **Cloud Run** service in **`us-central1`**, **min instances 0** (cold start
  accepted), runs the same program in a new read mode
  (`SCOUT_WEB_SOURCE=gcs`): no database, no node, no Telegram. It reads the
  snapshot from the bucket and serves the site to anyone.
- The site is reached at **`__HOSTNAME__`**, default **`scout-analytics.lylelabs.io`**
  (owner decision, 9 Oct 2026), through a **Firebase Hosting**
  rewrite to Cloud Run (step 10).
- The public site shows **everything the LAN site shows** (`INTERFACE.md`
  4.1), except the private delivery group's invite link and links into it,
  secrets, and server/DB/node addresses. Two layers enforce that: a scrub of
  every exported string and a deny check on the final export that refuses to upload.
- **Live updates (SSE) are off** on Cloud Run and **"Refresh now" is not
  offered** there. The page polls every 30 s (already built in). Reasons in
  `INTERFACE.md` 5.3 and 5.4.
- `assetdb` (**shared with the main API**) gets **no extra load**: the export
  reuses the snapshot that prod `-web` already reads.

| File | What it is |
|---|---|
| `INTERFACE.md` | Spec for the Go coder: export and read modes, split format, allowlist, scrub, deny check, health, banner |
| `Dockerfile`, `Dockerfile.dockerignore` | Image: static Go build, distroless, non-root. The ignore file sits next to the Dockerfile (BuildKit), because the context is the repo root. A `context-list` stage lists the build context (dry check) |
| `.gcloudignore` | What `gcloud builds submit` may upload (allowlist) |
| `cloudbuild.yaml` | Cloud Build: build and push the image |
| `service.yaml` | Cloud Run service |
| `bucket-lifecycle.json` | Deletes part objects 30 days after creation |
| `cloudbuild-staging-lifecycle.json` | Optional: deletes Cloud Build's uploaded source tarballs after 7 days |
| `uploader-role.yaml` | Custom role for the prod uploader (create + overwrite + get in one bucket) |
| `ar-cleanup-policy.json` | Keeps Artifact Registry under its free 0.5 GB |
| `org-policy/*.yaml` | Project-level org-policy exceptions (step 0, step 4 option a) |
| `firebase/firebase.json`, `firebase/.firebaserc`, `firebase/public/` | Firebase Hosting rewrite to Cloud Run (step 10). `public/` stays empty |
| `systemd/scout-web-gcp-export.conf`, `systemd/web-export.env.example` | Prod: turn on the export in `scout-web.service` |
| `github-actions/deploy-web.yml` | Optional CI draft (keyless, WIF). Inactive where it is |

Prerequisite: the Go side of `INTERFACE.md` must be merged and running on prod
before steps 5–8. Steps 0–4 and 6 can be done earlier.

Line endings: `.gitattributes` has `deploy/gcp/** text eol=lf`, so every file
here (the unit drop-in and env file, `Dockerfile`, `.gcloudignore`, the YAML
and JSON files) stays LF on a Windows checkout. Copy the systemd files to the
server from git, not through an editor that rewrites line endings.

---

## Placeholders

```bash
export PROJECT_ID=your-project-id          # EDIT
export PROJECT_NUMBER=123456789012         # EDIT: gcloud projects describe $PROJECT_ID --format='value(projectNumber)'
export REGION=us-central1                  # owner decision
export BUCKET=${PROJECT_ID}-scout-web      # globally unique; EDIT if taken
export AR_REPO=scout
export TAG=$(git rev-parse --short HEAD)   # image tag = commit
export RUN_SA=scout-web-run@${PROJECT_ID}.iam.gserviceaccount.com
export EXPORT_SA=scout-web-export@${PROJECT_ID}.iam.gserviceaccount.com
export SITE_HOST=scout-analytics.lylelabs.io   # __HOSTNAME__; owner decision
                                           # (not HOSTNAME: bash already sets that)
export ORG_ID=000000000000                 # EDIT: gcloud projects get-ancestors $PROJECT_ID
gcloud config set project "$PROJECT_ID"
```

**Region `us-central1`** (owner decision). The Cloud Storage free tier applies
only in `us-east1`, `us-west1` and `us-central1`
([free features](https://cloud.google.com/free/docs/free-cloud-features)). It
is a Tier 1 Cloud Run region ([pricing](https://cloud.google.com/run/pricing))
and a region Firebase Hosting can rewrite to. Bucket and service are in the
same region, so transfer between them is free.

Render helper used below (writes `*.rendered.*` files, which the
`/deploy/gcp/**/*.rendered.*` line this draft adds to `.gitignore` keeps out
of git; see step 4):

```bash
render() { sed -e "s/__PROJECT_ID__/${PROJECT_ID}/g" -e "s/__REGION__/${REGION}/g" \
               -e "s/__BUCKET__/${BUCKET}/g" -e "s/__TAG__/${TAG}/g" \
               -e "s/__HOSTNAME__/${SITE_HOST}/g" "$1" > "$2"
           grep -n '__[A-Z_]*__' "$2" && echo "UNFILLED placeholders in $2" || echo "ok: $2"; }
```

---

## Step 0. Pre-checks (read-only, free)

The project belongs to the organisation **Lyle Labs LLC**. Organisations
created on or after 3 May 2024 get "secure by default" org policies, among
them **service-account key creation disabled** and **domain-restricted
sharing** ([restricting service accounts](https://cloud.google.com/resource-manager/docs/organization-policy/restricting-service-accounts)).
Check what is in force for this project:

```bash
gcloud billing projects describe "$PROJECT_ID"            # billingEnabled: true
gcloud projects get-ancestors "$PROJECT_ID"               # shows ORG_ID
for c in iam.disableServiceAccountKeyCreation \
         iam.managed.disableServiceAccountKeyCreation \
         iam.serviceAccountKeyExpiryHours \
         iam.allowedPolicyMemberDomains \
         iam.managed.allowedPolicyMembers \
         run.managed.requireInvokerIam \
         gcp.resourceLocations; do
  echo "== $c"; gcloud org-policies describe "$c" --project="$PROJECT_ID" --effective 2>&1 | head -20
done
gcloud org-policies list --project="$PROJECT_ID"          # everything set on or above the project
```

What each one means here:

| Constraint | If enforced | Fix |
|---|---|---|
| `iam.disableServiceAccountKeyCreation` (legacy) or `iam.managed.disableServiceAccountKeyCreation` (managed) | step 4's key cannot be created | step 4, option (a) (chosen) or (b) (alternative) |
| `iam.serviceAccountKeyExpiryHours` | keys expire after the set time | rotate within it (step 4) |
| `iam.allowedPolicyMemberDomains` / `iam.managed.allowedPolicyMembers` (domain-restricted sharing) | an `allUsers` binding on Cloud Run is rejected | `service.yaml` uses the "invoker IAM check off" switch instead, which Google recommends for this case ([managing access](https://cloud.google.com/run/docs/securing/managing-access)) |
| `run.managed.requireInvokerIam` | the "invoker IAM check off" switch is **rejected**. With domain-restricted sharing also on, **neither** public path works | project exception `org-policy/run.managed.requireInvokerIam.yaml` (commands below this table). Needed: Firebase Hosting reaches Cloud Run as an anonymous caller, so the service must be public |
| `gcp.resourceLocations` | `us-central1` may be refused | allow `us-central1` for the project |

- Turning the invoker check off needs `run.services.setIamPolicy` on the
  service. The owner has it as project owner; a CI deployer would not (step 9
  deploys images only).
- Exact output of `gcloud org-policies describe … --effective` for managed
  constraints is **unverified**; if a command errors, check IAM & Admin →
  Organization policies in the console, filtered to this project.

**Apply an exception only for a constraint step 0 showed as enforced.** A
project-level `enforce: false` is an explicit project setting, and it wins
over what the organisation sets above it. Set on a constraint the organisation
does not enforce yet, it changes nothing today but **pins this project as
exempt** if the organisation enforces that constraint later, silently. So:
not enforced now → no file, nothing to undo.

**If `run.managed.requireInvokerIam` is enforced** (needed before step 7; needs
`roles/orgpolicy.policyAdmin` on the organisation, see step 4 option a for how
to grant it):

```bash
render deploy/gcp/org-policy/run.managed.requireInvokerIam.yaml \
       deploy/gcp/org-policy/run.managed.requireInvokerIam.rendered.yaml
gcloud org-policies set-policy deploy/gcp/org-policy/run.managed.requireInvokerIam.rendered.yaml
gcloud org-policies describe run.managed.requireInvokerIam --project="$PROJECT_ID" --effective
```

Propagation can take a few minutes; step 7 fails on the invoker switch until
it has. Undo: `gcloud org-policies delete run.managed.requireInvokerIam
--project="$PROJECT_ID"`. Whether a managed constraint takes the same
`spec.rules: [{enforce: false}]` form is **unverified**; if `set-policy`
refuses it, follow the constraint's page in the console.

**Budget alert (recommended, free).** It alerts and does not cap spend:

```bash
gcloud services enable billingbudgets.googleapis.com
gcloud billing budgets create --billing-account=BILLING_ACCOUNT_ID \
  --display-name="scout-web" --budget-amount=10USD \
  --filter-projects="projects/${PROJECT_ID}" \
  --threshold-rule=percent=0.5 --threshold-rule=percent=0.9 --threshold-rule=percent=1.0
```

## Step 1. Enable APIs

```bash
gcloud services enable run.googleapis.com artifactregistry.googleapis.com \
  cloudbuild.googleapis.com storage.googleapis.com iam.googleapis.com \
  iamcredentials.googleapis.com orgpolicy.googleapis.com \
  firebase.googleapis.com firebasehosting.googleapis.com
```

Creates nothing billable. Enabling is free.

## Step 2. Snapshot bucket (private)

```bash
gcloud storage buckets create "gs://${BUCKET}" --location="$REGION" \
  --default-storage-class=STANDARD --uniform-bucket-level-access \
  --public-access-prevention --soft-delete-duration=0
gcloud storage buckets update "gs://${BUCKET}" --lifecycle-file=deploy/gcp/bucket-lifecycle.json
gcloud storage buckets describe "gs://${BUCKET}"
```

- One regional Standard bucket with uniform bucket-level access (no ACLs) and
  public access prevention. **Nobody but the two service accounts can read or
  write it.** Visitors never touch the bucket.
- **No soft delete** (the default keeps deleted objects 7 days, billed) and **no
  versioning**: parts are content-addressed and never changed.
- Lifecycle (review item 7, owner choice): objects under `public/parts/` and
  `gated/parts/` are deleted **30 days** after creation (the rule runs
  asynchronously; expect up to about a day more). The pointer
  `public/current.json` is never deleted. While the exporter runs, it
  re-uploads every part the pointer names every 12 h, so the live parts never
  expire (`INTERFACE.md` 3.5). **If prod is down, the public site keeps serving
  the last snapshot for at least 30 days** (stale banner, `/api/summary` says
  `stale`). After that, new Cloud Run instances cannot load it and the site is
  down until prod uploads again. A reader fallback to "the newest remaining
  parts" was rejected: it would stitch parts of different times (reasons in
  `INTERFACE.md` 3.5).
- Object names: `public/parts/{rows,reports,prices}/<sha256>.json.gz` and the
  pointer `public/current.json` (`INTERFACE.md` 2).
- Flag names `--public-access-prevention` and `--soft-delete-duration=0` are
  from memory: **verify with `gcloud storage buckets create --help`**.
- Cost: see "Costs" (storage ≈ the last 30 days of uploads; about
  $0–0.70/month).

## Step 3. Runtime service account for Cloud Run (read-only, one bucket)

```bash
gcloud iam service-accounts create scout-web-run \
  --display-name="scout-web Cloud Run runtime (reads snapshots)"
gcloud storage buckets add-iam-policy-binding "gs://${BUCKET}" \
  --member="serviceAccount:${RUN_SA}" --role=roles/storage.objectViewer
```

No project-level roles. Cloud Run collects stdout/stderr logs without any role.
`objectViewer` also allows listing, which the reader does not need; a custom
role with `storage.objects.get` only is stricter (optional). Free.

## Step 4. Uploader identity for the prod server (write, one bucket)

```bash
gcloud iam service-accounts create scout-web-export \
  --display-name="scout-web export from prod (writes snapshots)"
gcloud iam roles create scoutSnapshotWriter --project="$PROJECT_ID" \
  --file=deploy/gcp/uploader-role.yaml
gcloud storage buckets add-iam-policy-binding "gs://${BUCKET}" \
  --member="serviceAccount:${EXPORT_SA}" \
  --role="projects/${PROJECT_ID}/roles/scoutSnapshotWriter" \
  --condition="expression=resource.name.startsWith(\"projects/_/buckets/${BUCKET}/objects/public/\"),title=public-prefix-only"
```

**Why a custom role:** the exporter overwrites the pointer every upload (and
the current parts every 12 h), and overwriting needs `storage.objects.delete`
as well as `create`. `roles/storage.objectCreator` "does not give permission
to view, delete, or overwrite objects"
([IAM roles for Cloud Storage](https://cloud.google.com/storage/docs/access-control/iam-roles)).
`get` reads the pointer at start ("skip if same", `INTERFACE.md` 3.2).
Predefined fallback: `roles/storage.objectUser`.

How the prod server proves it is `scout-web-export`: the org policy (step 0)
most likely blocks key creation. Two options; the owner chose (a) on 9 Oct 2026.

### Option (a), chosen (owner decision, 9 Oct 2026): project-level exception, then a key

You are the org admin, but **Organization Administrator does not, as far as
I know, include `orgpolicy.policy.set`** (that is in Organization Policy
Administrator, `roles/orgpolicy.policyAdmin`; **verify** under IAM → Roles).
Grant it to yourself on the organisation first if needed:

```bash
gcloud organizations add-iam-policy-binding "$ORG_ID" \
  --member="user:OWNER_EMAIL" --role=roles/orgpolicy.policyAdmin
```

Then set the exception **for this project only**, and **only for the
constraints step 0 showed as enforced**: an `enforce: false` on one the
organisation does not enforce yet would pin this project as exempt from a
later org-wide enforcement (step 0). Run only the matching pair of lines:

```bash
render deploy/gcp/org-policy/iam.disableServiceAccountKeyCreation.yaml \
       deploy/gcp/org-policy/iam.disableServiceAccountKeyCreation.rendered.yaml
render deploy/gcp/org-policy/iam.managed.disableServiceAccountKeyCreation.yaml \
       deploy/gcp/org-policy/iam.managed.disableServiceAccountKeyCreation.rendered.yaml
gcloud org-policies set-policy deploy/gcp/org-policy/iam.disableServiceAccountKeyCreation.rendered.yaml
gcloud org-policies set-policy deploy/gcp/org-policy/iam.managed.disableServiceAccountKeyCreation.rendered.yaml
gcloud org-policies describe iam.disableServiceAccountKeyCreation --project="$PROJECT_ID" --effective
gcloud org-policies describe iam.managed.disableServiceAccountKeyCreation --project="$PROJECT_ID" --effective
```

Each file is `name: projects/PROJECT_ID/policies/<constraint>` with
`spec.rules: [{enforce: false}]`. Propagation can take a few minutes.

**Undo** (back to the organisation's enforcement):
`gcloud org-policies delete iam.disableServiceAccountKeyCreation --project="$PROJECT_ID"`
(and the same for `iam.managed.…`). Use `delete`, not `reset`: `reset` applies
Google's constraint default, which is "not enforced".

Optional hardening: create the key, then undo the exception. The constraint
blocks **creating** keys; an existing key keeps working (**verify** in the
constraint's description). Each rotation then needs the exception again for a
few minutes.

**Key file** (a secret, like `.env`). Create it on your workstation outside
any repo, in a directory only you can read, copy it to the server without
`/tmp`, and delete every copy but the installed one.

Workstation (Git Bash, WSL or Linux):

```bash
umask 077
mkdir -p ~/.scout-keys
gcloud iam service-accounts keys create ~/.scout-keys/gcp-export.json --iam-account="$EXPORT_SA"
# copy into the admin's home on the server, created 0600 there (umask in the remote shell):
ssh PROD_HOST 'umask 077 && mkdir -p ~/.scout-keys && cat > ~/.scout-keys/gcp-export.json' \
  < ~/.scout-keys/gcp-export.json
shred -u ~/.scout-keys/gcp-export.json 2>/dev/null || rm -f ~/.scout-keys/gcp-export.json
```

On the prod server (as the admin):

```bash
sudo install -d -m 0750 -o root -g scout /etc/scoutanalytics
sudo install -m 0640 -o root -g scout ~/.scout-keys/gcp-export.json /etc/scoutanalytics/gcp-export.json
shred -u ~/.scout-keys/gcp-export.json
sudo ls -l /etc/scoutanalytics/          # -rw-r----- root scout gcp-export.json
```

- One-step alternative, **only if the admin has passwordless sudo** on the
  server (stdin carries the key, so sudo cannot prompt):
  `ssh PROD_HOST 'sudo install -m 0640 -o root -g scout /dev/stdin /etc/scoutanalytics/gcp-export.json' < ~/.scout-keys/gcp-export.json`
- **Windows without Git Bash/WSL**: `umask` does not exist. Create the key in
  `%USERPROFILE%\.scout-keys\` (your profile is readable only by you and
  Administrators by default; check with `icacls`), copy it with the built-in
  OpenSSH as above (`type file | ssh PROD_HOST "umask 077 && cat > …"`), then
  `Remove-Item`. `shred` is not available; deleting is the practical limit on
  an SSD.
- Ownership **root:scout, 0640**: the service user (`scout`) can read the key
  but not rewrite it. If the units run under another account, use its group.
- Rotate (for example every 90 days, or within `iam.serviceAccountKeyExpiryHours`
  if set): create a new key, install it the same way, `sudo systemctl restart
  scout-web`, then `gcloud iam service-accounts keys delete KEY_ID
  --iam-account="$EXPORT_SA"`. List: `gcloud iam service-accounts keys list
  --iam-account="$EXPORT_SA"`.
- If it leaks, someone can overwrite the public site's data (not read the
  database). Disable the key at once
  (`gcloud iam service-accounts keys disable KEY_ID --iam-account="$EXPORT_SA"`).
- Free.

### Option (b), alternative (not chosen): keyless, Workload Identity Federation with X.509

No key exists anywhere; the org policy stays fully enforced. The server holds a
client certificate from **your own CA** and exchanges it over mTLS for
short-lived Google tokens
([WIF with X.509](https://cloud.google.com/iam/docs/workload-identity-federation-with-x509-certificates)).
Outline (flag names **unverified**; follow the linked page):

1. A small CA (for example `step-ca`, or `openssl` for a root plus one
   intermediate), kept off the prod server. Issue the server a client
   certificate with CN `scout-prod-web`, short validity (for example 30 days)
   and automatic renewal on the server (a systemd timer).
2. A workload identity pool (`scout-prod`) and an X.509 provider with a trust
   store of your root/intermediate certificates
   (`gcloud iam workload-identity-pools providers create-x509 …
   --trust-store-config-path=…`), attribute mapping
   `google.subject=assertion.subject.dn.cn`.
3. Grant the federated principal
   `principal://iam.googleapis.com/projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/scout-prod/subject/scout-prod-web`
   the custom role on the bucket (same binding as above, with that member), or
   `roles/iam.workloadIdentityUser` on `$EXPORT_SA` to impersonate it.
4. Generate a credential configuration
   (`gcloud iam workload-identity-pools create-cred-config … --credential-cert-path=…
   --credential-cert-private-key-path=…`) and install that JSON (it holds no
   secret, only paths; the private key file is the secret: root:scout 0640) at
   `/etc/scoutanalytics/gcp-export.json`. The drop-in is unchanged.
5. **Check with the Go coder** that the Go auth library version in use
   supports X.509 credential configurations (**unverified**).

Cost: free. Effort: a CA and renewal to run. Worth it if the owner prefers not
to open any exception.

**`.gitignore` additions** (this draft **adds** these lines to the repo's
`.gitignore`; review them with the rest of the draft). `.env`, `.env.*`,
`*.session.json` and `*.pem` were already covered:

```gitignore
# GCP deploy (never commit keys or rendered files)
gcp-*.json
*-key.json
*.p12
/deploy/gcp/**/*.rendered.*
/deploy/gcp/firebase/.firebase/
```

## Step 5. Turn on the export on prod

Needs the Go export mode (`INTERFACE.md` section 3) deployed on prod.

```bash
# on the prod server, in a checkout of the repo
sudo install -m 0640 -o root -g scout deploy/gcp/systemd/web-export.env.example /etc/scoutanalytics/web-export.env
sudoedit /etc/scoutanalytics/web-export.env          # BUCKET; URL hosts default perceptor.info
sudo install -d /etc/systemd/system/scout-web.service.d
sudo install -m 0644 deploy/gcp/systemd/scout-web-gcp-export.conf /etc/systemd/system/scout-web.service.d/gcp-export.conf
sudo systemctl daemon-reload && sudo systemctl restart scout-web
journalctl -u scout-web --since "3 min ago" | grep 'web export'
```

From the workstation:

```bash
gcloud storage cat "gs://${BUCKET}/public/current.json"
gcloud storage ls -l "gs://${BUCKET}/public/parts/**" | tail -5
```

- A `web export: REFUSED to upload` line means the deny check found a private
  link, a notify-target handle or invite hash, a configured secret value or a
  server address in the export (`INTERFACE.md` 4.2.1, 4.3); the line names
  the variable, never the value. Nothing is uploaded (not even the heartbeat)
  until it is fixed; the public site goes stale instead of leaking.
- Freshness: with unchanged data the exporter still rewrites the pointer
  every 2 minutes (`SCOUT_WEB_EXPORT_HEARTBEAT`, `INTERFACE.md` 3.6), so a
  quiet but healthy prod never shows as stale. Each heartbeat needs a fresh,
  successful `assetdb` read with the same content since the last pointer
  write, so a prod that cannot read the database turns stale too. The age is prod's clock against
  Google's: check `timedatectl` on prod shows `System clock synchronized: yes`
  (NTP via `systemd-timesyncd` or `chrony`).
- Upstream from the server: about **4–40 GB a month** depending on how often
  the rows part changes and on full vs. delta prices (table in "Costs").
  Upload is ingress to GCP, which is free.
- `assetdb` (shared with the main API): no extra reads (the export reuses
  `-web`'s snapshot).

## Step 6. Artifact Registry and the image

```bash
gcloud artifacts repositories create "$AR_REPO" --repository-format=docker \
  --location="$REGION" --description="scout-analytics images"
gcloud artifacts repositories set-cleanup-policies "$AR_REPO" --location="$REGION" \
  --policy=deploy/gcp/ar-cleanup-policy.json
```

Build **from a clean clone** of `kfukue/scout-analytics`, never from the prod
working directory, which holds `.env`, `scout.session.json` and
`scoutanalytics_data/`. The stripped static binary measures 51.5 MB (local
cross-compile of the current code); the image is about 53 MB uncompressed. The
cleanup policy keeps the 5 newest and deletes the rest after 30 days, so
storage stays under the free 0.5 GB ($0.10/GB-month beyond it,
[Artifact Registry pricing](https://cloud.google.com/artifact-registry/pricing)).
The `set-cleanup-policies` flags (including whether it defaults to a dry run)
and the `olderThan` format are **unverified**; check `--help`.

**Before the first upload: dry check what leaves the machine** (local only,
nothing uploaded):

```bash
git clone https://github.com/kfukue/scout-analytics.git /path/to/scratch/scout-clean
cd /path/to/scratch/scout-clean
# (1) what `gcloud builds submit` would upload. `gcloud meta list-files-for-upload`
#     reads ./.gcloudignore and, as far as I know, has no --ignore-file flag
#     (unverified: check --help), so copy the file into this throwaway clone:
cp deploy/gcp/.gcloudignore ./.gcloudignore
gcloud meta list-files-for-upload . | sort
rm ./.gcloudignore
# (2) what the Docker build context holds after deploy/gcp/Dockerfile.dockerignore:
DOCKER_BUILDKIT=1 docker build -f deploy/gcp/Dockerfile --target=context-list \
  --progress=plain --no-cache .
```

Expect only `go.mod`, `go.sum`, top-level `*.go` (no `_test.go`), `internal/`,
`frontend/`, `scoutanalytics.sql` (plus, for (1), `deploy/gcp/Dockerfile`,
`Dockerfile.dockerignore`, `cloudbuild.yaml`). No `.env*`, `*.json`,
`*.session.json`, `scoutanalytics_data/`, `ml/`, `.git/`. `gcloud builds
submit --ignore-file` is a documented flag of `builds submit`; (1) checks the
same rules through the default file name.

**Option A: Cloud Build (no local Docker needed).**

```bash
gcloud builds submit --config=deploy/gcp/cloudbuild.yaml \
  --ignore-file=deploy/gcp/.gcloudignore \
  --substitutions="_REGION=${REGION},_REPO=${AR_REPO},_TAG=${TAG}" .
```

- `.gcloudignore` decides what is uploaded to Cloud Build's **staging bucket**
  `gs://${PROJECT_ID}_cloudbuild` (created automatically on the first build,
  multi-region US, kept forever by default; it holds the source tarballs, about
  1 MB each). Optional cleanup:
  `gcloud storage buckets update "gs://${PROJECT_ID}_cloudbuild" --lifecycle-file=deploy/gcp/cloudbuild-staging-lifecycle.json`.
- Cost: about 2–4 build-minutes per build on `e2-standard-2`, **free** within
  2,500 build-minutes a month, then $0.006/min
  ([Cloud Build pricing](https://cloud.google.com/build/pricing)). In new
  projects the build may run as the Compute Engine default service account; if
  the push is denied, grant it `roles/artifactregistry.writer` on the repo
  (**unverified** which account your project uses; the build log says).

**Option B: local Docker.**

```bash
gcloud auth configure-docker "${REGION}-docker.pkg.dev"
DOCKER_BUILDKIT=1 docker build -f deploy/gcp/Dockerfile \
  -t "${REGION}-docker.pkg.dev/${PROJECT_ID}/${AR_REPO}/scout-web:${TAG}" .
docker push "${REGION}-docker.pkg.dev/${PROJECT_ID}/${AR_REPO}/scout-web:${TAG}"
```

Free apart from storage. Pin the base images by digest before the first real
push (see the comment at the top of `Dockerfile`).

## Step 7. Cloud Run service

Run step 5 first. A new revision only goes live once `/api/health` answers 200,
which needs a snapshot in the bucket.

```bash
render deploy/gcp/service.yaml deploy/gcp/service.rendered.yaml
gcloud run services replace deploy/gcp/service.rendered.yaml --region="$REGION"
```

- Creates the service `scout-web`: public (invoker IAM check off; needs step 0's
  `run.managed.requireInvokerIam` check), ingress all, request-based billing,
  **min 0** / max 3 instances, concurrency 80, 1 vCPU, 512 MiB, 30 s timeout,
  HTTP startup probe on `/api/health`, runtime SA `scout-web-run`. No
  `SCOUT_WEB_ADDR`: the program listens on Cloud Run's `$PORT` (8080).
- **Step 7b, only if you remove the `invoker-iam-disabled` annotation**
  (`services replace` sets no IAM; blocked by domain-restricted sharing):
  `gcloud run services add-iam-policy-binding scout-web --region="$REGION" --member=allUsers --role=roles/run.invoker`
- The same with flags instead of YAML (alternative):

```bash
gcloud run deploy scout-web --region="$REGION" \
  --image="${REGION}-docker.pkg.dev/${PROJECT_ID}/${AR_REPO}/scout-web:${TAG}" \
  --service-account="$RUN_SA" --no-invoker-iam-check --ingress=all \
  --min-instances=0 --max-instances=3 --concurrency=80 --cpu=1 --memory=512Mi \
  --timeout=30s --cpu-throttling --cpu-boost --port=8080 \
  --set-env-vars="SCOUT_WEB_SOURCE=gcs,SCOUT_WEB_GCS_BUCKET=${BUCKET},SCOUT_WEB_GCS_OBJECT=public/current.json,SCOUT_WEB_GCS_POLL=15s,SCOUT_WEB_EVENTS=off" \
  --startup-probe="httpGet.path=/api/health,httpGet.port=8080,periodSeconds=2,timeoutSeconds=2,failureThreshold=30"
```

**Min instances 0** (owner decision): the first visit after an idle period
waits for a cold start (container start + first snapshot download + build,
**estimated 1–3 s**, not measured). Freshness does not depend on it: with
request-based billing even a warm idle instance gets no CPU, so the check
happens inside the next request (`INTERFACE.md` 5.2). Min 1 would cost about
$9.86/month (1 vCPU × 2,628,000 s × $0.0000025 idle + 0.5 GiB × 2,628,000 s ×
$0.0000025; [Cloud Run pricing](https://cloud.google.com/run/pricing)); switch
with `gcloud run services update scout-web --region="$REGION" --min-instances=1`.

## Step 8. Verify

```bash
URL=$(gcloud run services describe scout-web --region="$REGION" --format='value(status.url)')
curl -fsS "$URL/api/health"                         # {"status":"ok",...}
curl -s  "$URL/api/summary" | head -c 400; echo     # updated_at = the export time; manual_refresh false
curl -sI "$URL/" | grep -i -E 'content-security-policy|x-frame-options'
curl -s -o /dev/null -w '%{http_code}\n' "$URL/api/events"                  # 204 (events off)
curl -s -o /dev/null -w '%{http_code}\n' -X POST "$URL/api/refresh"         # 404 (not offered)
# the bucket must NOT be public: expect 401 or 403
curl -s -o /dev/null -w '%{http_code}\n' "https://storage.googleapis.com/${BUCKET}/public/current.json"
# no private link in what the site serves (spot check of one list page)
curl -s "$URL/api/calls?per=200" | grep -i -E -c 't\.me/(\+|joinchat|c/|addlist)|telegram\.(me|dog)/(\+|joinchat|c/|addlist)|tg:' # 0
# logs (Cloud Run stdout -> Cloud Logging)
gcloud logging read 'resource.type="cloud_run_revision" AND resource.labels.service_name="scout-web"' \
  --limit=50 --freshness=1h --format='value(timestamp,textPayload)'
```

Then open the URL in a browser. Check the banner ("Prices refresh about every
minute. Data as of …"), the list with returns/peaks/drawdowns, the Analytics
page, a row's detail with Perceptor summary and sAlpha text, that report links
go to `perceptor.info`, that there is no "Refresh now" button, and that no
link points into the private delivery group. The `grep` spot check covers one
page of 200 tokens (`webMaxPer`); the deny check on the exporter covers all.

**Where logs go:** Cloud Run → Cloud Logging (request log and the program's
lines). The prod exporter → journald (`journalctl -u scout-web | grep 'web export'`).
Cloud Logging's free allotment (50 GiB/project/month, **from memory,
unverified**) is far above this site's volume.

**Health / alerting:** the startup probe gates each new instance on
`/api/health`. **Alert on `/api/summary`, not on `/api/health`**: health
never triggers a snapshot check (it must stay fast for the 2 s probe), so on
an idle instance it reports the state of the last check, possibly long ago.
`/api/summary` runs the bounded check (up to 10 s) before it answers
(`INTERFACE.md` 5.2, 5.3). A Cloud Monitoring uptime check:

- URL `https://${SITE_HOST}/api/summary`, GET, every 5 minutes, **timeout
  20 s** (more than the 10 s check plus a cold start);
- content match "contains" `"stale":false` (exact bytes the Go encoder writes,
  no space). Matching on a JSON path instead is **unverified** in the uptime
  check options; the text match is enough.

It catches "down", "stale" (prod or its export stopped: the pointer heartbeat
of `INTERFACE.md` 3.6 keeps a healthy but quiet prod fresh) and a stuck deny
check or a prod that cannot read `assetdb` (no heartbeat in either case),
long before the 30-day lifecycle
matters. Uptime checks probe from several regions (at least 3), so every
5 minutes means about 26,000–52,000 requests a month; the probes keep an
instance mostly warm, so roughly 0.2–1.5 s billed each, about 5,000–80,000
vCPU-s (estimate). That is inside the 180,000 free vCPU-s and 2 million free
requests, but a real share of them at low traffic; a 15-minute period cuts it
by three. The uptime check's own pricing and free quota are **unverified**;
check before adding it ([Cloud Monitoring pricing](https://cloud.google.com/stackdriver/pricing)).

## Step 9 (optional). GitHub Actions with Workload Identity Federation

No keys in GitHub: GitHub's OIDC token is exchanged for short-lived
credentials. The condition pins the repository and its owner by **numeric id**
(names can be re-registered after a rename or deletion), the branch, and the
GitHub environment `production` (which the workflow uses; add required
reviewers to it in GitHub).

```bash
# numeric ids (read-only):
gh api repos/kfukue/scout-analytics --jq '.id, .owner.id'
export GH_REPO_ID=REPO_ID GH_OWNER_ID=OWNER_ID          # EDIT from the output above

gcloud iam service-accounts create scout-web-deployer --display-name="GitHub deploy of scout-web"
export DEPLOYER_SA=scout-web-deployer@${PROJECT_ID}.iam.gserviceaccount.com
gcloud iam workload-identity-pools create github --location=global --display-name="GitHub Actions"
gcloud iam workload-identity-pools providers create-oidc scout-analytics \
  --location=global --workload-identity-pool=github \
  --issuer-uri="https://token.actions.githubusercontent.com" \
  --attribute-mapping="google.subject=assertion.sub,attribute.repository=assertion.repository,attribute.repository_id=assertion.repository_id,attribute.repository_owner_id=assertion.repository_owner_id,attribute.ref=assertion.ref,attribute.environment=assertion.environment" \
  --attribute-condition="assertion.repository_owner_id=='${GH_OWNER_ID}' && assertion.repository_id=='${GH_REPO_ID}' && assertion.ref=='refs/heads/main' && assertion.environment=='production'"
gcloud iam service-accounts add-iam-policy-binding "$DEPLOYER_SA" --role=roles/iam.workloadIdentityUser \
  --member="principalSet://iam.googleapis.com/projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/github/attribute.repository_id/${GH_REPO_ID}"
gcloud artifacts repositories add-iam-policy-binding "$AR_REPO" --location="$REGION" \
  --member="serviceAccount:${DEPLOYER_SA}" --role=roles/artifactregistry.writer
gcloud run services add-iam-policy-binding scout-web --region="$REGION" \
  --member="serviceAccount:${DEPLOYER_SA}" --role=roles/run.developer
gcloud iam service-accounts add-iam-policy-binding "$RUN_SA" \
  --member="serviceAccount:${DEPLOYER_SA}" --role=roles/iam.serviceAccountUser
```

Then set the repository variables listed in `github-actions/deploy-web.yml`
and copy it to `.github/workflows/`. The ids are compared as strings (GitHub
sends them as strings in the token). A job without `environment: production`
has no `environment` claim and is refused. Whether `run.developer` on the
service alone is enough to deploy a revision is **unverified**; if the deploy
is denied, grant it on the project. Cost: WIF is free; builds on GitHub's
runners cost nothing on GCP.

## Step 10. Custom domain via Firebase Hosting (owner decision)

Hostname **`__HOSTNAME__`**, default **`scout-analytics.lylelabs.io`** (owner decision,
9 Oct 2026). Firebase Hosting (GA) puts a managed certificate and
Google's CDN in front of Cloud Run and rewrites every path to the service.

1. **Billing plan.** Hosting rewrites to Cloud Run need a project with billing,
   i.e. the Firebase **Blaze** plan. A GCP project with a billing account
   should appear as Blaze in the Firebase console (**verify** there). Blaze
   keeps Hosting's no-cost quota: 10 GB storage, 360 MB/day transfer; beyond
   it $0.15/GB transfer ([Firebase pricing](https://firebase.google.com/pricing)).
2. **Add Firebase to the existing project** (free; needs the Firebase CLI,
   `npm i -g firebase-tools`, and `firebase login` by the owner):

   ```bash
   firebase projects:addfirebase "$PROJECT_ID"
   ```

3. **Deploy the rewrite** from `deploy/gcp/firebase/` (it holds `firebase.json`
   with `"**" → run: scout-web, us-central1`, and an empty `public/`):

   ```bash
   cd deploy/gcp/firebase
   # --project overrides the __PROJECT_ID__ placeholder in .firebaserc
   firebase deploy --only hosting --project "$PROJECT_ID"
   curl -fsS "https://${PROJECT_ID}.web.app/api/health"
   ```

   - **Never run `firebase init hosting` in this directory.** It writes
     `public/index.html` and `public/404.html`; static files win over
     rewrites, so `index.html` would replace the site's start page.
   - Whether `firebase deploy` accepts an empty `public/` (only a dotfile,
     which is ignored) is **unverified**; if it refuses, add a harmless
     `public/robots.txt` (it would then be served by Firebase, not Cloud Run).
   - The service must stay publicly invocable (step 0 / step 7): Hosting calls
     Cloud Run without credentials.
4. **Connect the domain** in the Firebase console: Hosting → Add custom domain
   → `__HOSTNAME__`. At the DNS provider of `lylelabs.io`, add **exactly the
   records the console shows** (typically a TXT record for ownership and/or an
   ACME challenge, and an A record for `scout-analytics` pointing to Firebase's
   address). Remove any existing A, AAAA or CNAME for `scout-analytics` first.
   The certificate can take up to 24 h. Only the `scout-analytics` label
   changes; nothing else under `lylelabs.io` is touched.
5. **Verify:**

   ```bash
   curl -fsS "https://${SITE_HOST}/api/health"
   curl -s -o /dev/null -w '%{http_code}\n' -X POST "https://${SITE_HOST}/api/refresh"   # 404
   curl -sI "https://${SITE_HOST}/" | grep -i -E 'content-security-policy|strict-transport'
   ```

Notes:

- **Host header / `sameOrigin`.** Behind a Hosting rewrite, what Cloud Run sees
  as `Host` is **unverified** (it may be the `run.app` name, not
  `__HOSTNAME__`). The only check that uses it is `sameOrigin` on
  `POST /api/refresh`, and the public site does **not** register that path
  (`INTERFACE.md` 5.3), so nothing breaks. Keep any future POST in mind.
- **Caching**: Hosting's CDN caches a Cloud Run answer only with
  `Cache-Control: public` and `max-age`/`s-maxage`; the API sends `no-cache`,
  so the 30 s polls always reach Cloud Run (fresh data).
- **Timeout**: Hosting cuts Cloud Run requests at 60 s; the site has none that
  long (SSE is off).
- The `run.app` URL and `PROJECT_ID.web.app` stay reachable too. Restricting
  Cloud Run ingress to Hosting only is not possible (Hosting is not a load
  balancer); a load balancer would be needed for that.
- Alternatives considered: Cloud Run domain mapping (free, but **Preview**,
  "not production-ready"), a global external Application Load Balancer
  (≈ $18/month, needed later for Cloud Armor or IAP).

## Rollback

- **Bad image or config**: route traffic back to the previous revision.

  ```bash
  gcloud run revisions list --service=scout-web --region="$REGION"
  gcloud run services update-traffic scout-web --region="$REGION" --to-revisions=PREVIOUS_REVISION=100
  ```

  Revisions are kept and cost nothing while they serve no traffic.
- **Bad data published** (for example a link that should not be public):
  1. Stop the export on prod: `sudo rm /etc/systemd/system/scout-web.service.d/gcp-export.conf && sudo systemctl daemon-reload && sudo systemctl restart scout-web`.
  2. **Take the site offline at once**: `gcloud run services update scout-web --region="$REGION" --invoker-iam-check` (it then needs authentication, Firebase Hosting gets 403).
  3. Delete the bad objects: `gcloud storage rm "gs://${BUCKET}/public/**"`.
     Running instances **still hold the bad snapshot in memory** (a missing
     object keeps the old snapshot, `INTERFACE.md` 5.2), so deleting is not
     enough on its own; step 6 replaces them.
  4. Fix the exporter (scrub/deny patterns), deploy the fix on prod, and
     re-enable the export (step 5's drop-in, `systemctl restart scout-web`).
  5. **Confirm a clean pointer exists** before anything else:
     `journalctl -u scout-web --since "5 min ago" | grep 'web export'` shows
     `uploaded snapshot of …` and no `REFUSED`;
     `gcloud storage cat "gs://${BUCKET}/public/current.json"` shows a new
     `uploaded_at`.
  6. Replace every instance: deploy a new revision (any change forces one,
     for example `gcloud run services update scout-web --region="$REGION"
     --update-env-vars=SCOUT_WEB_RESTART=$(date +%s)`), then send **all**
     traffic to it, because a traffic split pinned earlier (for example by
     the "Bad image or config" rollback above) would give the new revision
     0 % and leave the old instances serving:
     `gcloud run services update-traffic scout-web --region="$REGION" --to-latest`.
     Check `gcloud run revisions list --service=scout-web --region="$REGION"`
     shows the new revision at 100 %. The old revision's instances then stop,
     and the new ones load only the clean pointer. (`SCOUT_WEB_RESTART` is
     ignored by the program and stays on the service until the next
     `services replace`.)
  7. Spot-check the closed service with your own identity token:
     ```bash
     URL=$(gcloud run services describe scout-web --region="$REGION" --format='value(status.url)')
     curl -s -H "Authorization: Bearer $(gcloud auth print-identity-token)" "$URL/api/summary" | head -c 400; echo
     curl -s -H "Authorization: Bearer $(gcloud auth print-identity-token)" "$URL/api/calls?per=200" \
       | grep -i -E -c 't\.me/(\+|joinchat|c/|addlist)|telegram\.(me|dog)/(\+|joinchat|c/|addlist)|tg:'   # 0
     ```
     plus a check for the specific text that leaked.
  8. **Only then re-open**: `gcloud run services update scout-web --region="$REGION" --no-invoker-iam-check`.
- **Firebase Hosting**: `firebase hosting:rollback` (or the console's release
  history) restores the previous Hosting release; removing the custom domain
  in the console detaches `__HOSTNAME__`.
- **Prod export broken or prod down**: nothing to do on GCP for up to 30 days.
  The site keeps serving the last snapshot; the banner shows its age and
  `/api/summary` says `"stale":true` (the uptime check alerts). After about 30 days the lifecycle rule deletes
  the parts and the site goes down until prod uploads again (step 2).
- **Old images**: the Artifact Registry cleanup policy keeps the 5 newest images.
  A revision whose image was deleted cannot serve again, so roll back only to
  recent revisions, or tag an image you want to keep and exclude it from the
  policy.

## Teardown (stops all charges)

```bash
firebase hosting:disable --project "$PROJECT_ID"         # and remove the custom domain in the console
gcloud run services delete scout-web --region="$REGION"
gcloud artifacts repositories delete "$AR_REPO" --location="$REGION"
gcloud storage rm --recursive "gs://${BUCKET}"          # deletes the bucket and every object
gcloud storage rm --recursive "gs://${PROJECT_ID}_cloudbuild"   # Cloud Build staging bucket
gcloud iam service-accounts keys list --iam-account="$EXPORT_SA"   # then delete each key
gcloud iam service-accounts delete "$EXPORT_SA"
gcloud iam service-accounts delete "$RUN_SA"
gcloud iam roles delete scoutSnapshotWriter --project="$PROJECT_ID"
gcloud org-policies delete iam.disableServiceAccountKeyCreation --project="$PROJECT_ID"          # if set
gcloud org-policies delete iam.managed.disableServiceAccountKeyCreation --project="$PROJECT_ID"  # if set
gcloud org-policies delete run.managed.requireInvokerIam --project="$PROJECT_ID"                # if set
# if step 9 was done:
gcloud iam workload-identity-pools delete github --location=global
gcloud iam service-accounts delete "scout-web-deployer@${PROJECT_ID}.iam.gserviceaccount.com"
```

On prod: remove the drop-in (Rollback, step 1), then
`sudo shred -u /etc/scoutanalytics/gcp-export.json /etc/scoutanalytics/web-export.env`.
Remove the `scout-analytics` DNS records at the `lylelabs.io` DNS provider.

---

## Costs

Prices checked on 9 Oct 2026 on the providers' pages, us-central1, list prices
in USD, no discounts, except where marked. Recheck in the
[GCP calculator](https://cloud.google.com/products/calculator).

| Item | Price | Free each month | Source |
|---|---|---|---|
| Cloud Run, request-based: CPU | $0.000024 /vCPU-s active; $0.0000025 idle (min instances) | 180,000 vCPU-s | [Cloud Run pricing](https://cloud.google.com/run/pricing) |
| Cloud Run, request-based: memory | $0.0000025 /GiB-s | 360,000 GiB-s | same |
| Cloud Run requests | $0.40 /million | 2 million | same |
| Cloud Run egress (to Firebase Hosting / internet) | assumed $0.12 /GiB, worst case (**whether Cloud Run → Firebase Hosting is billed as internet egress is unverified**) | 1 GiB in North America | same, [Cloud Storage pricing](https://cloud.google.com/storage/pricing) |
| Firebase Hosting transfer | $0.15 /GB | 360 MB/day (≈ 10.8 GB/month) | [Firebase pricing](https://firebase.google.com/pricing) |
| GCS Class A (writes) | $0.005 /1,000 | 5,000 (US regions) | [Cloud Storage pricing](https://cloud.google.com/storage/pricing), [free features](https://cloud.google.com/free/docs/free-cloud-features) |
| GCS Class B (reads) | $0.0004 /1,000 | 50,000 (US regions) | same |
| GCS Standard storage, regional | ≈ $0.02 /GB-month (**from memory; the page loads the regional table dynamically**) | 5 GB-months (US regions) | same |
| GCS multi-region US (Cloud Build staging bucket) | ≈ $0.026 /GB-month (**from memory**) | none | same |
| Artifact Registry storage | $0.10 /GiB-month | 0.5 GB | [AR pricing](https://cloud.google.com/artifact-registry/pricing) |
| Cloud Build | $0.006 /build-minute (e2-standard-2) | 2,500 build-minutes | [Cloud Build pricing](https://cloud.google.com/build/pricing) |

### Upload volume (split by change rate)

Assumptions (**estimates; measure on prod** once the exporter logs part sizes):
10k tokens; gzip sizes reports 0.75 MB, rows 0.6 MB, full prices 0.3 MB; the
prices part changes every minute (the tracker's latest-price pass); reports
change ~30 times a day; the **rows** part changes on every new call,
performance window, tracking-status change or re-scan, which is the big
unknown: **100 to 1,440 uploads a day** (low / high). Delta prices: an hourly
0.3 MB base plus a cumulative delta averaging ~30 KB (the tracker logs about
"180 refreshed (12 changed)" per pass).

| Scheme | Uploads/day (rows; prices; reports; pointer) | Upstream from prod, per month | Class A writes/month (parts + pointer + keep-alive) | GCS storage held (30-day lifecycle) |
|---|---|---|---|---|
| Before (one 1.5 MB file per minute) | — | ≈ 45–90 GB | ≈ 87,600 | (7-day) ≈ 16 GB |
| **Split, full prices, low** | 100; 1,440; 30; 1,440 | ≈ 13 + 1.8 + 0.7 ≈ **15.5 GB** | ≈ 90,000 | ≈ 15.5 GB |
| **Split, full prices, high** | 1,440; 1,440; 30; 1,440 | ≈ 13 + 26 + 0.7 ≈ **40 GB** | ≈ 131,000 | ≈ 40 GB |
| Split, delta prices, low | 100; 1,440 + 24 bases; 30; 1,440 | ≈ 1.5 + 1.8 + 0.7 ≈ **4 GB** | ≈ 91,000 | ≈ 4 GB |
| Split, delta prices, high | 1,440; 1,440 + 24; 30; 1,440 | ≈ 1.5 + 26 + 0.7 ≈ **28 GB** | ≈ 132,000 | ≈ 28 GB |

GCS cost of the export (same at any traffic):

| Scheme | Writes | Storage | **Export total** |
|---|---|---|---|
| Full prices, low | (90k − 5k) × $0.005/1k ≈ $0.43 | (15.5 − 5) × $0.02 ≈ $0.21 | **≈ $0.64** |
| Full prices, high | ≈ $0.63 | (40 − 5) × $0.02 ≈ $0.70 | **≈ $1.33** |
| Delta prices, low | ≈ $0.43 | free | **≈ $0.43** |
| Delta prices, high | ≈ $0.64 | (28 − 5) × $0.02 ≈ $0.46 | **≈ $1.10** |

If prod measurements show the rows part changing most minutes, the next step is
to move the performance fields (`Perf`, `HasPerf`, `TrackingStatus`) into a
fourth part, or to give rows-only changes a 5-minute floor (owner question 5).

### Monthly total by traffic

Assumptions (guesses, not measured): one page view ≈ 20 requests (8 on load,
2 per 30 s poll over a 3-minute visit) and ≈ 100 KB transferred; ≈ 0.1 s billed
per request, no overlap (pessimistic), plus ≈ 1.5 s of snapshot checks per
visit (now inside requests, 10 s cap) and a ≈ 3 s cold start for most visits at
low traffic. Reader GCS reads: ≈ 15 Class B operations per visit (pointer +
changed parts, ~6 checks). Min instances 0, request-based billing, 1 vCPU,
512 MiB, Firebase Hosting in front, no load balancer. Export at "full prices",
low–high.

| Monthly page views | Cloud Run CPU/memory | Cloud Run requests | Cloud Run egress (worst case) | Firebase transfer | GCS reads | GCS export | Build staging, AR, Build | **Total** |
|---|---|---|---|---|---|---|---|---|
| ~1,000 | ~6,500 vCPU-s: free | free | ~0.1 GB: free | ~0.1 GB: free | ~15k: free | $0.64–1.33 | < $0.01 | **≈ $0.65–1.35** |
| ~10,000 | ~45,000 vCPU-s: free | free | ~1 GB: $0–0.12 | ~1 GB: free | ~150k: $0.04 | $0.64–1.33 | < $0.01 | **≈ $0.70–1.50** |
| ~100,000 | ~350,000 vCPU-s pessimistic: ≈ $4.10 (likely $1–3 with overlap); memory free | ≈ $0–0.40 | ~10 GB: ≈ $1.10 | ~10 GB: ≈ $0–0.15 | ~1.5M: ≈ $0.58 | $0.64–1.33 | < $0.01 | **≈ $3.5–8** |

Add-ons: min instances 1 ≈ +$10/month; instance-based billing with min 1 ≈
+$45/month; load balancer ≈ +$18/month. Cloud Build staging bucket: ~1 MB per
build in multi-region US (not in the free tier), ≈ $0.003/month for 100
builds, or nothing with the 7-day lifecycle. A tab left open around the clock
polls about 175,000 requests a month (inside the free tier); with SSE it would
cost about $60–70/month in active time instead (`INTERFACE.md` 5.4). The nodes
and the prod server are unchanged and not part of this bill.

## Open questions for the owner

Decided on 9 Oct 2026: publish everything the LAN site shows; min instances 0;
`us-central1`; Firebase Hosting; split upload; org policy option (a);
hostname `scout-analytics.lylelabs.io`.

1. **Who manages the `lylelabs.io` DNS?** Needed for step 10 (records for the
   `scout-analytics` label only).
2. Should new calls and reports skip the 1-minute upload floor? Default: no.
3. After the key exists: undo the org-policy exception (re-open it for each
   rotation) or leave it in place for this project?
4. Delta prices (`INTERFACE.md` 2.3): build it in phase 2 after measuring?
5. If the rows part changes most minutes on prod: split performance into its
   own part, or a 5-minute floor for rows-only changes?
6. Is the prod server's upstream fine with 15–40 GB a month (4–28 GB with
   delta prices)?
7. Live updates on the prod site (`-web` on the server) stay as they are.
   Should the banner also show there?
