# Changelog

## v0.3.0 — 2026-10-31

### Added

- **Docker and native apps behind one proxy on the same server.** A native
  config can set `proxy_boot.mode: container` to route through the server's
  Docker kamal-proxy, reached via the Docker network's gateway, instead of a
  native one. The native proxy stays the default. See
  [docs/native.md](docs/native.md), "One proxy for both".

## v0.2.1 — 2026-10-31

### Fixed

- A relative `-c` path failed for commands that load the config twice
  (`proxy`, `remove`, `cert renew`) with `chdir to config directory:
  .../deploy/deploy`. That broke the `qifa -c deploy/qifa.production.yaml …`
  pattern from [docs/deploying.md](docs/deploying.md). The path is now
  resolved once, against the shell's directory.

## v0.2.0 — 2026-10-31

### Added

- **Native deploys, no Docker on the server.** A config with a `native:` block
  ships a binary or directory as a systemd service: immutable releases, a
  `current` link, automatic rollback when a release fails its health check,
  `status`, `logs` (every host), `app exec`, `prune`, `remove`. One-time
  `qifa setup --ssh-user root` creates the account, directories, units and a
  narrow sudoers rule. See [docs/native.md](docs/native.md).
- **kamal-proxy as a systemd service** for native apps: HTTPS (`tls.source`
  `kamal`, `static` or `qifa`), domains and maintenance mode with no Docker.
  `qifa proxy boot --ssh-user root`.
- **`tls.source: qifa` on native servers**: lego runs as a pinned,
  checksum-verified binary in the deploy user's `~/.qifa/bin`.
- **Zero-downtime check in CI**: requests through kamal-proxy during deploy,
  rollback and same-version redeploy must all succeed.

### Changed

- **Docker and native mode share commands**: `qifa setup`, `qifa remove
  [--purge]`, `status` (one per-host layout), `logs`, `prune`, `sync`,
  `--ssh-user`. `qifa native setup|remove` still work and say they were
  renamed.
- **Docker and native mode share config keys.** The old keys still load, with
  a warning naming the new one; setting both is an error:

  | Setting | Key | Replaced |
  | --- | --- | --- |
  | Health check | `healthcheck` (top level) | `proxy.healthcheck`, `native.healthcheck` |
  | App port | `servers.<role>.app_port` | `native.port` |
  | Versions kept | `prune.retain` | `prune.retain_containers`, `native.retain` |
  | Command | `servers.<role>.cmd` (string or list) | `servers.<role>.exec`, `native.exec` |

  `healthcheck.timeout` bounds one attempt; `healthcheck.deploy_timeout` the
  whole wait, defaulting to five attempts (`5 × (timeout + interval)`), which
  is what Docker mode always allowed.
- **Redeploying the live version is zero-downtime** on proxied roles: the
  replacement runs beside it as `<service>-<role>-<version>-r` until the
  proxy switches.
- **kamal-proxy defaults to v0.10.2** (was v0.9.2). Routes saved by v0.9.2
  load in v0.10.2.
- **Go 1.27.1** is required to build.

### Upgrading from v0.1.x

1. Install v0.2.1 everywhere you deploy from (see README, Install). Configs
   written for v0.2 (top-level `healthcheck:`) are rejected by older qifa.
2. Optionally move old keys to the new ones; `qifa config` prints the loaded
   config with the new keys, and each old key prints a warning until moved.
3. Upgrade running proxies one host at a time with `qifa proxy upgrade`. It
   pulls the new image before replacing the container, and recreates it
   from the `proxy_boot` of the config you run it from, so run it from a
   config whose `proxy_boot` matches how the proxy was booted.

## v0.1.0 — 2026-09-06

First tagged release: Docker deploys over SSH with kamal-proxy for
zero-downtime traffic switching.
