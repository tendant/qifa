# Contributing

Go 1.27.1 or newer builds qifa (`make build`, `make install`).

## Tests

```bash
make test             # unit tests (go test ./...)
make test-e2e         # Docker deploys, end to end
make test-e2e-native  # native (systemd) deploys, end to end
make test-e2e-mixed   # Docker and native apps on one server
make test-e2e-launchd # native deploys to this Mac, under launchd (macOS only)
```

The end-to-end suites need Docker on your machine, only to provide
throwaway "servers" as containers; each cleans up after itself. CI
(`.github/workflows/ci.yml`) runs all four on every push.

### `test-e2e`: Docker deploys

[`test/e2e/docker-e2e.sh`](test/e2e/docker-e2e.sh) starts, with
[`test/e2e/docker-compose.yml`](test/e2e/docker-compose.yml), a local
[zot](https://zotregistry.dev) registry with a password, and a deploy host
running sshd and its own Docker daemon. Against them qifa:

- builds images locally and pushes them to the registry, authenticated
- deploys over SSH: the host pulls, authenticated, and starts the containers
- boots kamal-proxy and switches it between versions; health checks and the
  app answer through the proxy
- runs `status`, `logs`, `app exec`, and a worker redeploy and rollback
- checks **zero downtime**: a client sends requests back to back while the
  web role deploys `zd1` → `zd2`, rolls back to `zd1`, and redeploys `zd1`.
  Every request must succeed (at least 20), and the responses, which carry
  the version that served them, must show the switch happening during the run

The registry is `zottest:5001` both on your machine (via `HOSTALIASES`) and
inside the test network (a Compose alias), so one image reference works for
the push and the pull.

Host ports, each moved to the next free one when something on your machine
listens on it or another container has it published (the script says so):

| Port | Default | Override |
| --- | --- | --- |
| zot | 5001 | `QIFA_E2E_ZOT_PORT` |
| deploy host SSH | 2222 | `QIFA_E2E_SSH_PORT` |
| deploy host Docker | 23750 | `QIFA_E2E_DOCKER_PORT` |
| the app through kamal-proxy | 8080 | `QIFA_E2E_HTTP_PORT` |

`KEEP_E2E_ENV=1 test/e2e/docker-e2e.sh` leaves the environment up after the
run.

Not covered, because they are not zero-downtime by design: roles without the
proxy and roles with `extra_ports` stop the old container first, since a
published port or the role's network alias cannot be shared. The proxy routes
to container IPs on the host's bridge network, as in production.

### `test-e2e-native`: native deploys

[`test/e2e/native-e2e.sh`](test/e2e/native-e2e.sh) boots a privileged
container running systemd, sshd and sudo, and no Docker
([`test/e2e/native-host`](test/e2e/native-host)), and deploys
[`test/e2e/native-app`](test/e2e/native-app) to it: setup, the native
kamal-proxy, deploys, a crashing release rolled back, rollback, `app exec`,
status, reconcile, prune, certificates from a local ACME server (Pebble, for
kamal-proxy and for `tls.source: qifa`), a program installed on the server
(no artifact, `workdir`, `hardening: false`, `setup --print`), and remove.
SSH port 2299 (`QIFA_NATIVE_E2E_PORT`).

### `test-e2e-launchd`: native deploys on macOS

[`test/e2e/launchd-e2e.sh`](test/e2e/launchd-e2e.sh) needs no Docker: it
deploys [`test/e2e/native-app`](test/e2e/native-app) to the Mac it runs on
(`hosts: [local]`, `native.init: launchd`) as a real LaunchAgent in your login
session: setup without root, deploys, the env file byte for byte, a crashing
release rolled back, launchd restarting a killed app, restart/stop/start,
logs, `app exec`, status, rollback, and remove. Everything lives in a temp
directory and the agent is booted out at the end. Port 38085
(`QIFA_LAUNCHD_E2E_PORT`).

### `test-e2e-mixed`: one proxy for Docker and native

[`test/e2e/mixed-e2e.sh`](test/e2e/mixed-e2e.sh) boots a host with systemd
and Docker ([`test/e2e/mixed-host`](test/e2e/mixed-host)), runs a Docker app
behind the Docker kamal-proxy, and a native app routed through the same proxy
(`proxy_boot.mode: container`): deploy, rollback and maintenance of one leave
the other alone. SSH port 2399 (`QIFA_MIXED_E2E_PORT`).
