---
name: scout-infra
description: Rules for scout analytics deployment and operations drafts (containers, process definitions, health checks, CI, infrastructure-as-code); never changes real cloud resources.
---

# Scout analytics: infrastructure

You draft files; the owner applies them. GCP is chosen for the public
read-only site `scout-analytics.lylelabs.io` (Cloud Run; see `deploy/gcp/`,
including `INTERFACE.md`, once that branch is merged). The other parts stay on
the Ubuntu prod server. Keep other drafts provider-neutral unless the task
picks one.

## Scope

- Dockerfiles ([best practices](https://docs.docker.com/build/building/best-practices/)):
  multi-stage, pinned base images, non-root user, no secrets in layers.
- Process definitions, as systemd units
  ([systemd.service](https://www.freedesktop.org/software/systemd/man/latest/systemd.service.html))
  or compose files, for:
  - the Go binary with `-listen-only`, `-track` and `-web`;
  - the Python scoring service (`ml/serve.py`, run with uvicorn).
- Environment-variable inventory: names and purpose only, taken from
  `README.md` and `ml/README.md`. Never values.
- Health checks (e.g. `GET /health` of the scoring service) and where logs go.
- Deploy and rollback steps in `DEPLOY.md`.
- Later: Terraform (or the chosen IaC) and CI.

## Project facts

- It depends on a Robinhood Chain Nitro node (needs
  `--execution.rpc.log-history=0` and a large NVMe disk;
  [Arbitrum node docs](https://docs.arbitrum.io/run-arbitrum-node/run-full-node))
  and an Ethereum Erigon archive node ([Erigon docs](https://docs.erigon.tech/)).
  These are the dominant cost and the hardest parts to move.
- Postgres `assetdb` is shared with the main API; say so whenever a proposal
  affects it.
- The Telegram session file (`scout.session.json`) is a secret, like `.env`.

## Infra rules

- Never run commands that create, change or delete real cloud resources or
  spend money: `terraform apply`, `gcloud`/`aws`/`az` create, deploy or delete
  commands, `kubectl apply` against a real cluster, `docker push`.
- Allowed: `terraform fmt`, `terraform validate`, `terraform plan` without
  credentials, `docker build`, linters (e.g. hadolint, shellcheck).
- Never handle, print or commit credentials. Secrets come from a secret
  manager or the environment, never from files in git.

## Reporting cost

- When proposing cloud resources, give an estimated monthly cost with its
  source (the provider's pricing page or calculator:
  [GCP](https://cloud.google.com/products/calculator),
  [AWS](https://calculator.aws/),
  [Azure](https://azure.microsoft.com/en-us/pricing/calculator/)).
