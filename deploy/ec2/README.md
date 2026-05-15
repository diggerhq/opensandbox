# Personal dev host on EC2

Spin up a personal end-to-end OpenComputer dev environment on a single EC2
bare-metal instance. Use this when you need to test a feature that touches the
**control plane, the worker, and real QEMU/KVM VMs** — anything where the
in-process combined-mode targets in `Makefile` (`run`, `run-pg`,
`run-pg-workos`) won't exercise enough of the system.

For control-plane-only or UI-only iteration, prefer the local Makefile tiers
first — they're cheaper and faster.

## What you get

- One bare-metal EC2 instance running both server and worker as systemd services
- Real QEMU VMs (KVM-accelerated, so requires bare metal — Nitro doesn't expose
  VMX to non-`.metal` instance types)
- Local Postgres + Redis on the host
- Vite dev server on your laptop, proxying to the dev host
- WorkOS staging login working in the browser
- ~$1.50/hr while running; `stop` between sessions to keep state and avoid charges

## Architecture

```
laptop                                    EC2 c5d.metal (us-east-2)
──────                                    ────────────────────────
Vite :3000  ───── /api, /auth, /webhooks ───►  opensandbox-server :8080
browser                                        │
                                               ├── Postgres :5432 (local)
                                               ├── Redis :6379 (local)
                                               └── opensandbox-worker
                                                      │
                                                      └── QEMU VMs (KVM)
                                                              │
                                                              └── osb-agent (vsock)
```

Server reaches Axiom (logs) and your S3 bucket (checkpoints) over the public
internet.

## Prerequisites

| Need | Verify |
|---|---|
| AWS account access (IAM user, region us-east-2) | `aws sts get-caller-identity` |
| AWS CLI profile with EC2/S3 perms | `aws configure list` |
| Local SSH private key | `ls ~/.ssh/<your-key>` |
| WorkOS staging project access | API key + client ID from team |
| Axiom workspace access | ingest token + query token from team |

## One-time setup

### 1. Register your SSH key with EC2

If your local key isn't already a key pair in EC2 us-east-2:

```bash
ssh-keygen -y -f ~/.ssh/<your-key> > /tmp/pubkey
aws ec2 import-key-pair --region us-east-2 \
  --key-name opensandbox-<your-name> \
  --public-key-material fileb:///tmp/pubkey
rm /tmp/pubkey
```

### 2. Create a personal S3 bucket for checkpoints

```bash
BUCKET=opensandbox-<your-name>-dev
aws s3api create-bucket --bucket "$BUCKET" --region us-east-2 \
  --create-bucket-configuration LocationConstraint=us-east-2
aws s3api put-public-access-block --bucket "$BUCKET" \
  --public-access-block-configuration \
  "BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true"
```

### 3. Get Axiom tokens

From the team Axiom workspace (or personal, free tier): generate an **ingest
token** and a **read/query token**, both scoped to the dataset. The default
dataset name is `oc-sandbox-logs` — override `AXIOM_DATASET` only if you want
isolation from other developers.

> ⚠️ Rotating any `AXIOM_*` value (ingest token, query token, dataset)
> requires a **server restart** to take effect — the values are read from
> the environment once at `config.Load` and frozen for the process lifetime.
> Workers spawned by a server with a stale `cfg.AxiomIngestToken` silently
> bake an empty value into their cloud-init. Watch for the
> `WARNING: AXIOM_INGEST_TOKEN empty` line in `journalctl -u
> opensandbox-server` after any rotation.

### 4. Get WorkOS staging keys + register the redirect URI

From the team WorkOS staging project, grab the API key (`sk_test_...`) and
client ID (`client_...`).

In the **WorkOS dashboard** → Redirects, add:

```
http://localhost:3000/auth/callback
```

WorkOS rejects callbacks not on the allowlist. The redirect URI must point at
the Vite host (not the EC2 IP) so the session cookie is set on the same origin
the browser is using.

### 5. Create your dev env file

Save to `~/.opensandbox-dev.env` (mode 600, outside the repo):

```bash
# Pull AXIOM_INGEST_TOKEN, AXIOM_DATASET etc from repo .env if present
REPO=~/path/to/opencomputer
set -a
[ -f "$REPO/.env" ] && . "$REPO/.env"
set +a

# AWS + SSH
export AWS_PROFILE=default
export KEY_NAME=opensandbox-<your-name>
export SSH_KEY=~/.ssh/<your-key>

# S3 (your bucket + your local AWS creds)
export S3_BUCKET=opensandbox-<your-name>-dev
export S3_ACCESS_KEY_ID=$(aws configure get aws_access_key_id --profile default)
export S3_SECRET_ACCESS_KEY=$(aws configure get aws_secret_access_key --profile default)

# Axiom — INGEST/DATASET come from $REPO/.env; QUERY token must be set explicitly
export AXIOM_QUERY_TOKEN=xaat-...

# WorkOS staging
export WORKOS_API_KEY=sk_test_...
export WORKOS_CLIENT_ID=client_...
export WORKOS_REDIRECT_URI=http://localhost:3000/auth/callback

# Dev host (set after first `create`; updated on each spot recreate)
export DEV_IP=

# Vite proxy target — read by web/vite.config.ts
export OC_API_TARGET="http://$DEV_IP:8080"

# API key for SDK/curl (server hashes this for the DB seed)
export API_KEY=test-dev-key
```

```bash
chmod 600 ~/.opensandbox-dev.env
```

### 6. Launch the host

```bash
source ~/.opensandbox-dev.env
cd <repo>
./deploy/ec2/deploy-qemu-dev.sh create     # ~5–10 min, prints the public IP
```

Update `DEV_IP=` in `~/.opensandbox-dev.env` to the printed IP, then re-source.

### 7. First deploy

```bash
source ~/.opensandbox-dev.env
./deploy/ec2/deploy-qemu-dev.sh deploy     # ~10–15 min on first run (rootfs build dominates)
```

## Daily workflow

```bash
source ~/.opensandbox-dev.env

# Backend changes
./deploy/ec2/deploy-qemu-dev.sh deploy     # rsync + rebuild + restart, ~30–60s

# UI iteration (separate terminal, also source the env)
cd web && npm install && npm run dev       # http://localhost:3000
```

`deploy` is `rsync --delete` from your local tree, so each run is a fresh
snapshot. If a parallel agent is editing files, finish their changes before
deploying, or use a worktree pinned to `main` to deploy stable code while you
keep editing.

## UI auth flow (so you can debug if it hangs)

1. Browser at `localhost:3000` clicks Sign in → hits `/auth/login`
2. Vite proxies to EC2 server → 302 to `workos.com/...`
3. WorkOS auths → redirects to `WORKOS_REDIRECT_URI` (`localhost:3000/auth/callback`)
4. Vite proxies callback to EC2 → server processes code, sets session cookie on
   `localhost:3000`
5. Server's `FrontendURL` auto-detects to `localhost:3000` (because `web/dist`
   isn't on the host) and final-redirects you to the dashboard

Step 3 fails with "redirect URI mismatch" → step 4 of the WorkOS dashboard
setup wasn't done.

## Lifecycle

```bash
./deploy/ec2/deploy-qemu-dev.sh ssh        # shell into the host
./deploy/ec2/deploy-qemu-dev.sh status     # health check + service status
./deploy/ec2/deploy-qemu-dev.sh stop       # stop instance, preserve disk, no compute charges
./deploy/ec2/deploy-qemu-dev.sh start      # bring it back, same disk
./deploy/ec2/deploy-qemu-dev.sh destroy    # tear down completely
```

`stop` is the right default between sessions. `destroy` only when starting
fresh — it forces a full rootfs rebuild on next `deploy`.

## Smoke tests

```bash
# 1. Server health
curl -sf http://$DEV_IP:8080/health

# 2. Create a sandbox (control plane + worker + QEMU)
SBX=$(curl -s -X POST http://$DEV_IP:8080/api/sandboxes \
  -H 'Content-Type: application/json' -H "X-API-Key: $API_KEY" \
  -d '{"templateID":"default"}' | jq -r .sandboxID)

# 3. Exec inside the VM (proves agent + vsock)
curl -s -X POST http://$DEV_IP:8080/api/sandboxes/$SBX/exec \
  -H 'Content-Type: application/json' -H "X-API-Key: $API_KEY" \
  -d '{"cmd":"uname -a && date"}'

# 4. Stream sandbox session logs (tests Axiom wiring)
curl -N http://$DEV_IP:8080/api/sandboxes/$SBX/logs -H "X-API-Key: $API_KEY"
```

## Troubleshooting

**`/auth/login` returns 404.** WorkOS routes register only when
`WORKOS_API_KEY` is non-empty (`internal/api/router.go`). Verify
`server.env` on the host has the keys (`ssh ... 'sudo cat /etc/opensandbox/server.env | grep WORKOS'`),
then `sudo systemctl restart opensandbox-server`. Most often: you ran `deploy`
without sourcing the env file first.

**WorkOS sign-in shows "redirect URI mismatch".** Add
`http://localhost:3000/auth/callback` to the WorkOS dashboard Redirects list
under your project. The URI must exactly match `WORKOS_REDIRECT_URI`.

**Sandbox creation returns `{"error":"missing API key"}`.** `$API_KEY` isn't
exported in your shell. `source ~/.opensandbox-dev.env`.

**`deploy` fails with `S3_ACCESS_KEY_ID: unbound variable`.** The script uses
`set -u`. Set both `S3_ACCESS_KEY_ID` and `S3_SECRET_ACCESS_KEY` (the dev env
file does this from your AWS profile).

**S3 operations 403.** Your AWS IAM principal doesn't have access to the
bucket — set `S3_BUCKET` to your personal one, not someone else's.

**Build fails with `cannot use orgID … as string value`** or similar mid-edit
errors. The parallel agent's working tree was caught mid-edit by rsync. Either
run `go build ./cmd/server ./cmd/worker ./cmd/agent` locally first, or deploy
from a worktree pinned to `main`.

**`./deploy-qemu-dev.sh ssh` says "Cannot reach via SSH".** Spot instance was
reclaimed. `create` again (the state file at `.qemu-dev-state-us-east-2` will
be reset).

## Lighter alternatives in `Makefile`

If you don't need real VMs, these are faster (no EC2):

- `make run` — combined mode, in-memory, no Postgres
- `make run-pg` — combined mode + local Postgres + Redis (`make infra-up` first)
- `make run-pg-workos` — same plus WorkOS browser login

Worker logic runs in-process in combined mode but sandbox VM lifecycle isn't
exercised — anything touching `internal/qemu/` should use the dev host.

## Files in this directory

- `deploy-qemu-dev.sh` — main lifecycle script (`create | deploy | ssh | status | stop | start | destroy`)
- `setup-azure-host.sh` (in `../azure/`) — host provisioning, reused for AWS
- `build-rootfs-docker.sh` — builds the guest rootfs ext4 image
- `setup-dev-env.sh`, `setup-secrets.sh` — env file installers run remotely
- `opensandbox-server.service`, `opensandbox-worker.service` — systemd units
- `setup-instance.sh`, `setup-single-host.sh`, `deploy-server.sh`,
  `deploy-worker.sh`, `deploy-aws-dev.sh` — older paths, currently unused by
  `deploy-qemu-dev.sh` workflow
