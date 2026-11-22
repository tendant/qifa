# Contributing

Go 1.27.1 or newer builds qifa (`make build`, `make install`).

## Tests

```bash
make test             # unit tests (go test ./...)
make test-install     # install.sh against freshly built dist/ binaries
make test-e2e         # Docker deploys, end to end
make test-e2e-native  # native (systemd) deploys, end to end
make test-e2e-mixed   # Docker and native apps on one server
make test-e2e-launchd # native deploys to this Mac, under launchd (macOS only)
make test-e2e-vps     # docs/vps.md as written: fresh Ubuntu server to HTTPS
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
- boots kamal-proxy from the first deploy, with no `qifa proxy boot`, and
  switches it between versions; health checks and the app answer through it
- runs `status`, `logs`, `app exec`, and a worker redeploy and rollback
- joins the worker to a network it doesn't own (`shop_default`, standing in
  for a Compose project's): the deploy refuses while the network is missing,
  then the worker and a container on that network reach each other by name
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

### `test-e2e-vps`: the fresh-server guide

[`test/e2e/vps-e2e.sh`](test/e2e/vps-e2e.sh) runs
[docs/vps.md](docs/vps.md) step by step against a container that stands in
for a new VPS: Ubuntu 24.04 with systemd and sshd, root logging in with a
key, and no Docker. Each guide command is printed as `$ …` and run as the
guide writes it, with the test's address for the server: Docker from
get.docker.com, the deploy user, `qifa init --host --domain`, `setup`,
`doctor`, `deploy`, then a second deploy. It checks the app answers at
`https://app.test` with a certificate issued by
[Pebble](https://github.com/letsencrypt/pebble), and that `http://`
redirects to it.

Only the plumbing differs from a real server:

- **kamal-proxy** is a build whose staging ACME URL points at Pebble, so the
  config adds `proxy_boot.image` and `tls.staging`.
- **DNS:** Pebble's DNS server points `app.test` at the box, and the box's
  Docker uses it so kamal-proxy finds `pebble.test`.

It doesn't cover real DNS, cloud firewalls or Let's Encrypt itself. It needs
the internet, because the box installs Docker and pulls nginx.

### `test-e2e-mixed`: one proxy for Docker and native

[`test/e2e/mixed-e2e.sh`](test/e2e/mixed-e2e.sh) boots a host with systemd
and Docker ([`test/e2e/mixed-host`](test/e2e/mixed-host)), runs a Docker app
behind the Docker kamal-proxy, and a native app routed through the same proxy
(`proxy_boot.mode: container`): deploy, rollback and maintenance of one leave
the other alone. SSH port 2399 (`QIFA_MIXED_E2E_PORT`).

## Releasing

1. Rename CHANGELOG.md's `## Unreleased` section to `## vX.Y.Z — <date>`, and
   update the version README.md names (Install, Status).
2. Commit that to main and let CI pass.
3. Tag it and push the tag: `git tag vX.Y.Z && git push origin vX.Y.Z`.

The tag runs [`release.yml`](.github/workflows/release.yml). It tests, runs
`make dist` (a static binary per platform, `qifa-<os>-<arch>`, and
`SHA256SUMS`), and publishes a GitHub release whose notes are the tag's
CHANGELOG section. A tag with no CHANGELOG section fails before anything is
published. A tag with a suffix (`v0.6.0-rc1`) becomes a prerelease, which
`install.sh` skips unless asked for it by `QIFA_VERSION`. `make dist` runs the
same build locally.
