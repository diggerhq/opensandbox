# EC2 Worker Deployment

## Software Stack

| Component | Version | Notes |
|-----------|---------|-------|
| Firecracker | latest | MicroVM hypervisor for sandbox isolation |
| Caddy | latest | Wildcard TLS for `*.workers.opencomputer.dev` (DNS-01 via Route53) |
| Redis | 7.0.15 | Local, used for sandbox state/routing |
| Go worker | custom | `/usr/local/bin/opensandbox-worker` |

## Architecture

```
Internet
  |
  v
Caddy (port 443) -- wildcard TLS for *.workers.opencomputer.dev (DNS-01 via Route53)
  |
  v
opensandbox-worker (port 8080) -- HTTP API + gRPC (9090)
  |
  v
Firecracker microVMs -- one per sandbox
  |
  v
Snapshot hibernate/wake -- pause to S3, resume on demand
```

## Scripts

- `setup-instance.sh` - Full instance setup from a fresh Ubuntu 24.04 AMI
- `deploy-worker.sh` - Build and deploy the worker binary (run from repo root)
- `opensandbox-worker.service` - Systemd unit template (fill in env vars)
- `caddy.service` - Caddy systemd unit file
- `Caddyfile` - Caddy configuration

## Quick Commands

```bash
# Deploy worker (from repo root on your Mac)
./deploy/ec2/deploy-worker.sh

# SSH into the instance
ssh -i ~/.ssh/opensandbox-worker.pem ubuntu@$WORKER_IP

# Check worker logs
sudo journalctl -u opensandbox-worker -f

# Check caddy logs
sudo journalctl -u caddy -f

# Restart worker
sudo systemctl restart opensandbox-worker
```
