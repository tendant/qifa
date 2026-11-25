# Changelog

## Unreleased

### Added

- **`qifa doctor` checks the domain** on each host kamal-proxy serves
  `proxy.host` from, before the first deploy rather than after a certificate
  that never arrives. `dns <name>` warns when the name doesn't resolve, or
  resolves somewhere other than the host. `port 80`/`port 443` connect from
  this machine and fail when a firewall drops the port (no answer within
  3s), which would stop Let's Encrypt. `ports free` fails when something
  else on the host, such as a preinstalled nginx, already listens where
  kamal-proxy needs to.

### Changed

- When Docker is missing on a host, `qifa setup`, failed commands and
  `qifa init --host` now say that qifa needs Docker and doesn't install it,
  and point to Docker's install guide instead of `get.docker.com`.

## v0.7.0 — 2026-11-22

### Added

- **`qifa init` shapes the starter to the project.** With a `Dockerfile`
  next to it, the starter builds that on each host (`builder: per_target`,
  no registry), on the port its last `EXPOSE` names, instead of pulling
  nginx. `--host [user@]host[:port]` deploys there over SSH instead of to
  this machine (the user goes to `ssh.user`), and
  `--domain <name>` serves the app at that name over HTTPS through
  kamal-proxy. The service is named after the directory. init loads the
  config it is about to write and refuses flags that would not load.
- **[docs/vps.md](docs/vps.md): from a fresh server to HTTPS.**
  `make test-e2e-vps` runs it as written in CI, against a fresh Ubuntu 24.04
  server with Pebble issuing the certificate.
- **Release binaries carry build-provenance attestations**, checkable with
  `gh attestation verify <binary> --repo tendant/qifa`.

### Fixed

- `qifa doctor` failed a `builder: per_target` app's "registry pull" check,
  asking the registry for an image each host builds and nothing pulls. It now
  reports it as info, and doesn't call the image "must be pulled".
- **A build on each host (`builder: per_target`, or a remote builder) uploaded
  the whole context directory**, including files `.dockerignore` excludes,
  such as `.env` files with secrets, `.git` and `node_modules`. They landed in
  a temporary directory on every host each deploy. The upload now follows
  `.dockerignore` with docker build's own rules, so excluded files never leave
  this machine; the Dockerfile and `.dockerignore` are always sent. qifa's own
  `.qifa/` state directory is left out too.

## v0.6.0 — 2026-11-21

### Added

- **Prebuilt binaries and a one-line install.** Tagging a release publishes
  a static binary per platform (Linux and macOS, amd64 and arm64) with
  `SHA256SUMS` on a GitHub release, with notes taken from CHANGELOG.md.
  `curl -fsSL https://raw.githubusercontent.com/tendant/qifa/main/install.sh | sh`
  installs the right one, checked against its checksum, without Go or sudo.
  `QIFA_VERSION`, `QIFA_INSTALL_DIR` and `QIFA_DOWNLOAD_URL` (for a mirror)
  adjust it. `make dist` builds the same binaries locally.

- **Join existing docker networks: `networks:`** on `servers.<role>` and on
  accessories, typically a Compose project's (`myproject_default`), so an app
  reaches services Compose runs on the same host by name. The container stays
  on the estate network too, and a non-proxied role's `<service>-<role>` alias
  applies on every network it joins. qifa never creates these networks. A
  deploy or accessory boot checks for them before touching the running
  container, and `qifa doctor` reports them. Built-in networks (`bridge`,
  `host`, `none`) and the estate network are refused at load. Native mode
  rejects the key. See README, "Beside Docker Compose". The health check
  reads the address of a container on several networks correctly: it used to
  run them together.

- **`qifa help`**, `-h`/`--help`, and `qifa <command> --help` print usage
  with a line on what each command does. Before, all of them were errors.
  An `exec`'s own `--help` still goes to the command it runs.

### Changed

- **Deploys boot kamal-proxy where it is missing.** A Docker deploy whose
  `proxy:` block names a host now boots the proxy on a routed host that has
  none, as `qifa proxy boot` would, instead of failing with "run qifa proxy
  boot first". A running proxy is left alone. A role proxied only by being
  called `web`, with no `proxy:` host, still gets the error, so ports 80 and
  443 are never taken unasked. Native mode is unchanged: its proxy boot
  needs root.
- **`proxy_boot.hosts` defaults to the proxied roles' hosts**, for `qifa
  proxy boot`, `upgrade`, `logs` and the rest, and for `qifa doctor`. The
  `proxy_boot:` block is now needed only to change how the proxy runs.
  Doctor reports a missing proxy that the next deploy will boot as info, not
  as a failure.

- **`qifa init` writes a starter that deploys as written:** a public image on
  this machine, published on port 8765, with no SSH, registry or proxy.
  Building, env, a domain with HTTPS and remote hosts are commented blocks,
  each tested to load once uncommented. The old starter needed rewriting
  before anything worked, and it warned about a deprecated key on first
  load.

### Fixed

- **Health checks on macOS.** A container checked by its IP (a proxied role,
  or one with no published port) always failed on Docker Desktop, Colima or
  OrbStack. Docker runs in a VM there, and the host has no route to container
  addresses. On a host that is not Linux, the check now runs inside the
  docker network, from a throwaway container of the app's own image (already
  present, no pull), or of `busybox:1.37` (pulled once) when that image has
  no shell and no curl or wget. Linux hosts check from the host, unchanged.
  With this and the proxy boot above, the full proxy path works on a Mac.

- `qifa init` overwrote an existing `qifa.yaml` without asking. It now
  refuses unless given `--force`.

## v0.5.0 — 2026-11-14

### Added

- **GPUs for containers: `gpus:`** on `servers.<role>` and on accessories,
  passed to `docker run --gpus`: `all`, a count, or `device=0` /
  `device=0,1` / GPU UUIDs (a device list is kept in the double quotes
  docker's parsing needs). Validated at load; rejected in native mode. Needs
  the NVIDIA Container Toolkit on the host. See README, "GPUs".

- **Native deploys to macOS: `native.init: launchd`.** The app runs as a
  LaunchAgent of the deploy user (`~/Library/LaunchAgents/qifa.<service>.plist`)
  instead of a systemd unit, with no root anywhere: `qifa setup` runs as that
  user, deploys load the agent with `launchctl`. Releases, env, health checks,
  rollback, status, logs (`<dir>/log/`), `app exec` and installed programs work
  as on Linux; `hardening` and the proxy do not apply. A release's executables
  are checked to be macOS builds (Mach-O). See docs/native.md, "macOS hosts".
  `make test-e2e-launchd` runs it end to end on the Mac you are on.

- `qifa setup --print --host <host>` prints one host's setup script from a
  config with several hosts. Before, `--print` refused such configs.

### Fixed

- A deploy to a host with no proxy failed at `docker run` with "network kamal
  not found": every container joins the estate network, which only
  `qifa proxy boot` created. The deploy now creates it first (a no-op when it
  exists), for app containers and accessories alike.

- Native release scripts ran only with GNU tools (`mv -T`, `stat -c`,
  `sha256sum`, `chmod --`); they now pick the BSD equivalents on macOS.
  Linux hosts behave as before.

## v0.4.0 — 2026-10-31

### Added

- **Native: run a program already installed on the server.** With no
  `native.artifact` and an absolute `servers.<role>.cmd`, qifa ships nothing
  and supervises the program: unit, env, restarts, health check, logs,
  status, route. A deploy installs the env and restarts; a failed one
  restores the previous env. `qifa rollback` refuses, as there are no
  releases. See [docs/native.md](docs/native.md), "A program already
  installed on the server".
- **`native.workdir`**: the app's working directory, instead of the live
  release. `qifa setup` creates it if missing.
- **`native.hardening: false`** drops the unit's systemd sandboxing
  (`NoNewPrivileges`, `PrivateTmp`, `ProtectSystem=strict`,
  `ReadWritePaths`), for programs that write across their user's home or
  start privileged helpers. The default is unchanged (on), and existing
  units render byte for byte as before, so no `qifa setup` is needed.
- **`qifa setup --print`** (native) writes the root setup script for a host
  where qifa cannot become root (no root login, sudo with a password), to
  run there with `sudo sh`.

### Fixed

- A host known in `known_hosts` only by a key type Go does not prefer (often
  ed25519 alone) failed with `knownhosts: key mismatch` though `ssh` worked.
  qifa now offers the host key types known for the host, as OpenSSH does.
- `qifa remove --purge` deleted the `native.user` account even when it was
  an existing login user rather than one `qifa setup` created. It now deletes
  only an account whose home is the install directory.

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
