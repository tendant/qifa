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
- **zero downtime** through `kamal-proxy`: one client sends requests back to
  back while the web role is deployed from `zd1` to `zd2`, rolled back to
  `zd1`, and redeployed at `zd1`. Every request must succeed, there must be at least 20 of them, and
  the responses must show the version change happening during the run. Each
  response carries the version that served it (`APP_VERSION`), which is how
  the check tells a switch under load from one before or after it

## Run It

```bash
make test-e2e
```

To keep the Docker environment up after the script exits:

```bash
KEEP_E2E_ENV=1 bash scripts/test-zot-e2e.sh
```

If a default host port is taken on your machine, move it (the registry stays
`zottest:5001` inside the test network):

```bash
QIFA_E2E_ZOT_PORT=25001 QIFA_E2E_HTTP_PORT=28080 \
QIFA_E2E_SSH_PORT=22222 QIFA_E2E_DOCKER_PORT=33750 make test-e2e
```

To run the same sequence used in CI:

```bash
make ci
```

## Services

- `zot` on `127.0.0.1:5001` (`QIFA_E2E_ZOT_PORT`)
- registry hostname `zottest:5001`
- SSH deploy host on `127.0.0.1:2222` (`QIFA_E2E_SSH_PORT`)
- remote Docker daemon on `127.0.0.1:23750` (`QIFA_E2E_DOCKER_PORT`)
- deployed web app through `kamal-proxy` on `127.0.0.1:8080` (`QIFA_E2E_HTTP_PORT`)

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

## Zero downtime: what is and is not covered

The check covers a proxied role moving between two versions on deploy and on
rollback, and a redeploy of the version already live. That last one used to
replace the container in place (one 502 in a few hundred requests); its
replacement now runs beside it as `<service>-<role>-<version>-r` until the
proxy switches, and the next redeploy of that version alternates back.

Not covered, because they are not zero-downtime by design: roles without the
proxy and roles with `extra_ports` stop the old container first, since a
published port or the role's network alias cannot be shared.

## CI

GitHub Actions runs:

- `make test`
- `make test-e2e`
