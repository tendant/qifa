# Zot E2E Harness

This repo includes a Docker-based integration harness for real build, push, pull, SSH, and container startup checks using a local `zot` registry.

## What It Tests

- local Docker build through `godeploy`
- push to `zot`
- SSH into a simulated deploy host
- remote Docker pull and container start
- web health check and app endpoint reachability
- `godeploy status`
- `godeploy logs`
- `godeploy app exec`
- worker redeploy and rollback flow

## What It Does Not Test

- a real `kamal-proxy` binary
- true zero-downtime web traffic switching

The deploy host image uses a lightweight `kamal-proxy` shim because the current project reuses the proxy command interface but does not yet ship the real proxy as part of this repo.

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
- deployed web app on `127.0.0.1:3000`

## Important Limitation

The web smoke test covers a single live deployment. Repeated web deploys are still constrained by the current deployer implementation binding a fixed published port (`3000:3000`) for each new container. The worker path is used for rollback coverage because it avoids that fixed-port conflict.
