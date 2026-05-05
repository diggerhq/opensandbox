# Manual integration tests

End-to-end verification scripts that need a deployed environment, real
sandboxes, and DB access. Not runnable in CI — they require the live
control plane + worker + Postgres on a target VM and an SSH key into
that VM.

Each script exits non-zero on failure so you can chain it into ad-hoc
deploy-verify pipelines if you want.

## Conventions

- Each script self-cleans on EXIT (via `trap`). It will best-effort
  destroy any sandboxes, secret stores, or other resources it created
  even on failure.
- All required env vars are checked up front and the script exits 2
  with a clear message if anything is missing.
- Scripts MUST use placeholder hosts (`example.com`, `httpbin.org`)
  rather than real customer or production endpoints.
- Output is colour-coded: yellow = section header, green = PASS,
  red = FAIL.

## Scripts

### `verify-billing-and-proxy-fixes.sh`

Verifies two related fixes that ship together:

1. **Bug A — billing scale event on fork** (`fix/worker-record-scale-event-on-fork`):
   forks-from-checkpoint must produce a `sandbox_scale_events` row so the
   usage-reporter can see them and credit/billable_events flow.

2. **Bug B — allowlist-only secret store** (`fix/secret-proxy-allowlist-only-stores`):
   a secret store carrying only an egress allowlist (no secret entries)
   must register a proxy session and enforce the allowlist (allowed host
   passes, disallowed host gets 403). Pre-fix both got `407 no_session`.

Required env:

```
OPENCOMPUTER_API_URL  e.g. http://10.0.0.5:8080
OPENCOMPUTER_API_KEY  e.g. opensandbox-dev
DEV_VM                public IP of the dev VM
DEV_KEY               path to SSH private key for the VM
```

Optional env (defaults shown):

```
DEV_USER=ubuntu
PG_USER=opensandbox
PG_DB=opensandbox
PG_PASS=opensandbox
OC=oc
```

Run:

```bash
chmod +x tests/manual/verify-billing-and-proxy-fixes.sh
tests/manual/verify-billing-and-proxy-fixes.sh
```
