# Zot E2E Harness

This repo includes a Docker-based integration harness for real build, push, pull, SSH, and container startup checks using a local `zot` registry.

## What It Tests

- local Docker build through `godeploy`
- push to `zot`
- SSH into a simulated deploy host
- remote Docker pull and container start
- real `kamal-proxy` startup and target switching
- web health check and app endpoint reachability through the proxy
- `godeploy status`
- `godeploy logs`
- `godeploy app exec`
- worker redeploy and rollback flow

## Run It

```bash
chmod +x scripts/test-zot-e2e.sh
scripts/test-zot-e2e.sh
```

To keep the Docker environment up after the script exits:

```bash
KEEP_E2E_ENV=1 scripts/test-zot-e2e.sh
```

## Services

- `zot` on `127.0.0.1:5001`
- SSH deploy host on `127.0.0.1:2222`
- remote Docker daemon on `127.0.0.1:23750`
- deployed web app through `kamal-proxy` on `127.0.0.1:8080`

## Important Limitation

The harness now validates repeated web deploys without publishing app containers directly on the host. The proxy routes to container IPs discovered from the remote Docker daemon, which is close to the production model but still assumes the proxy process can reach bridge-network container addresses on the host.
