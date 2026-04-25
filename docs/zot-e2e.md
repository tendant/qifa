# Zot E2E Harness

This repo includes a Docker-based integration harness for real build, authenticated push, authenticated pull, SSH, and container startup checks using a local `zot` registry.

## What It Tests

- local Docker build through `qifa`
- authenticated push to `zot`
- SSH into a simulated deploy host
- authenticated remote Docker pull and container start
- real `kamal-proxy` startup and target switching
- web health check and app endpoint reachability through the proxy
- `qifa status`
- `qifa logs`
- `qifa app exec`
- worker redeploy and rollback flow

## Run It

```bash
make test-e2e
```

To keep the Docker environment up after the script exits:

```bash
KEEP_E2E_ENV=1 bash scripts/test-zot-e2e.sh
```

To run the same sequence used in CI:

```bash
make ci
```

## Services

- `zot` on `127.0.0.1:5001`
- registry hostname `zottest:5001`
- SSH deploy host on `127.0.0.1:2222`
- remote Docker daemon on `127.0.0.1:23750`
- deployed web app through `kamal-proxy` on `127.0.0.1:8080`

## Registry Auth

The harness uses a shared registry hostname, `zottest:5001`:

- on the host runner, `HOSTALIASES` maps `zottest` to `127.0.0.1`
- inside the Docker test network, Compose gives the `zot` service the `zottest` alias

This allows the same image reference to be used for both:

- local build and push
- remote pull on the deploy host

Authentication is exercised with a static `htpasswd` file mounted into `zot`, and `qifa` now injects Docker auth config for the local build/push path as well as the remote pull path.

## Important Limitation

The harness now validates repeated web deploys without publishing app containers directly on the host. The proxy routes to container IPs discovered from the remote Docker daemon, which is close to the production model but still assumes the proxy process can reach bridge-network container addresses on the host.

## CI

GitHub Actions runs:

- `make test`
- `make test-e2e`
