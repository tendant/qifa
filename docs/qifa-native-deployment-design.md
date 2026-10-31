# Qifa Native Deployment — Design

**Status:** Implemented on branch `native-deploy` (phases 1–3). User guide: [native.md](native.md)  
**Scope:** Linux hosts, native application deployment without Docker  
**Principle:** Keep Qifa simple: build → SSH upload → activate release → systemd restart → health check → rollback on failure.

## 1. Goals

- Extend Qifa with a `native` deployment mode while preserving existing Docker behavior byte-for-byte.
- Deploy executable binaries or application directories to Linux servers over SSH.
- Use systemd for process supervision, restart and logs.
- Support repeatable releases, manual rollback and automatic rollback on failed deployment.
- Reuse existing Qifa configuration, SSH, lock, secrets, hooks, versioning and audit-log code.
- Keep the deployer stateless, as today: what is deployed is answered by the hosts, not by the machine running qifa.
- Avoid a resident Qifa agent, central database, artifact registry or large refactor.

## 2. Non-goals

- Zero-downtime deployment and blue-green switching: a deploy restarts the process. §16.4 records a design, which is not planned.
- TLS termination and domain routing in v1. The operator brings their own (Caddy, nginx) or binds the app port directly; §16 plans kamal-proxy for this.
- Non-Linux platforms or init systems other than systemd.
- Container image building for the native backend.
- Language-specific packaging or dependency installation.

Multi-host is **not** a non-goal: `servers.<role>.hosts` and `rollout.batch_size` already sequence hosts, and the native path reuses that loop per host. Each host is still an independent single-host deploy; there is no load balancing across them.

## 3. Architecture

```text
                         qifa CLI (internal/app)
              deploy / rollback / status / logs / restart ...
                            |
                  config has `native:` block?
                    /                     \
         internal/deploy (existing)    internal/native (new)
                    \                     /
       shared: internal/ssh, internal/lock, internal/hooks,
               internal/state, internal/secrets, version resolution
                                  |
                             Linux server
                                  |
                      releases/ + current link
                                  |
                               systemd
```

`deploy.Deployer` (`internal/deploy/deploy.go`) has no backend interface today. The seam is the CLI dispatch in `internal/app/app.go`: extract a small interface covering the commands both modes support, and construct either the Docker deployer or the native one.

```go
type Backend interface {
    Deploy(ctx) error
    Rollback(ctx, version string) error
    Stop(ctx) error
    Start(ctx) error
    Restart(ctx) error
    Remove(ctx) error
    Status(ctx, out) error
    Logs(ctx, lines int, follow bool, out) error
    Plan(ctx, out) error          // deploy --dry-run
    LockStatus(ctx, out) error
    LockRelease(ctx) error
    PinVersion(version string, allowDirty bool)
}
```

Docker-only commands (`proxy *`, `app maintenance|live|containers`, `sweep`, `prune`, `backup`, `restore`, `cert`) fail fast in native mode with a message naming the mode. The Docker deployer is not modified beyond satisfying the interface.

## 4. Configuration

The existing `builder:` block means a Docker build (`host`, `context`, `dockerfile`, `platform`, `repo`), `image` is required (`config.go`), and the loader rejects unknown keys. Native mode therefore gets its own top-level block rather than overloading `builder:` or adding a `deploy.type` switch. **The presence of `native:` selects native mode.**

```yaml
service: myapp

servers:
  web:
    hosts:
      - 10.0.0.11

native:
  build: make build                     # run on the deployer, in the config dir; optional
  artifact: dist                        # file or directory, relative to the config dir
  exec: [bin/myapp, --port, "8080"]     # argv; exec[0] resolved under current/; no shell
  user: myapp                           # service account (created by `qifa native setup`)
  arch: amd64                           # optional; verified against the artifact and host
  port: 8080                            # only needed for the HTTP health check
  healthcheck:
    path: /up
    interval: 2s
    timeout: 30s
  retain: 5
  restart: on-failure
  shared_dirs: [storage, log]           # symlinked from shared/ into each release

env:
  clear:
    APP_ENV: production
  secret_command: sops --decrypt secrets.production.enc.env

hooks:
  pre_build: ./scripts/pre_build.sh
  post_deploy: ./scripts/post_deploy.sh

ssh:
  user: deploy
```

A directory artifact's contents become the release root (`dist/bin/myapp` above runs as `bin/myapp`). A single-file artifact lands at the release root under its own name (`artifact: dist/myapp` runs as `exec: [myapp]`).

Validation in native mode:

- `native.artifact` and `native.exec` are required; `exec` must be a non-empty list and `exec[0]` a relative path (no `..`) unless absolute.
- Forbidden alongside `native:`: `image`, `source`, `pull_policy`, `registry`, `builder`, `proxy`, `proxy_boot`, `backup`, `restore`. `image` is no longer required in this mode.
- `servers.<role>.cmd`, `port`, `app_port`, `volumes`, `privileged`, `extra_ports` are container concepts and are rejected. A role may set `exec` to override `native.exec` (e.g. a worker role from the same artifact).
- `accessories` are rejected: they are containers, which a native config does not manage. Run them from a separate Docker config for the same host (see §15).
- `port` is required only if `healthcheck.path` is set. Without it, the check is `systemctl is-active` after a settle period (process liveness, not readiness).

Defaults:

| Property | Default |
| --- | --- |
| Install directory | `/opt/qifa/<service>` |
| Working directory | `/opt/qifa/<service>/current` |
| Unit name | `qifa-<service>.service` (`qifa-<service>-<role>.service` when there are several roles) |
| Env file | `/opt/qifa/<service>/etc/env` |
| Release retention | 5 |
| Restart policy | `on-failure` |
| Health-check timeout | 30s |
| Rollback on failed deploy | always |

## 5. Versions and release IDs

The release ID is the existing qifa version, from the same resolution logic the Docker path uses: `--version` / `QIFA_VERSION` if given, otherwise the short SHA of the config's checkout, and a dirty tree is refused unless `--allow-dirty` (which appends `-dirty`). This keeps the guarantee from `docs/deploying.md` that two people deploying the same commit ship the same thing, and it means `qifa rollback <version>` means the same thing in both modes.

Release IDs are validated as `[A-Za-z0-9._-]+` before use as a directory name. Redeploying an existing version reuses its directory only if the uploaded checksum matches; a mismatch is an error (the version names different bytes).

## 6. Target filesystem

```text
/opt/qifa/myapp/                      owner: deploy, mode 0755
├── releases/
│   ├── a1b2c3d/                      owner: deploy, mode 0755 (read-only to myapp)
│   │   ├── bin/myapp
│   │   ├── storage -> ../../shared/storage
│   │   ├── .qifa-checksum
│   │   └── .qifa-ok                  written after a successful health check
│   └── e4f5a6b/
├── current -> releases/e4f5a6b
├── etc/                              owner: deploy, mode 0700
│   ├── env                           mode 0600; read by systemd (root), not by myapp
│   └── qifa-myapp.service            the rendered unit, for drift detection only
└── shared/                           owner: myapp, never pruned
    └── storage/

/etc/systemd/system/qifa-myapp.service
```

- Releases are immutable after staging. They are owned by the deploy user and not writable by the service account.
- `shared/` holds persistent data and is never touched by pruning or `remove` (without `--purge`).
- **`.qifa-ok` is the known-good record.** It is written only after a release passes its health check. Rollback targets the newest release that has `.qifa-ok` and is not `current`. This is the minimal metadata needed to recover after an interrupted deploy, with no database.
- The deploy lock is the existing `internal/lock` (`/tmp/qifa-lock-<service>`, created atomically with `mkdir`), so `qifa lock status` and `qifa lock release` work unchanged in both modes.

## 7. Deployment lifecycle

1. **Validate** config; resolve the version (§5).
2. **pre_build hook**, then **build** by running `native.build` on the deployer, in the config directory.
3. **Check the artifact**: it must exist. For ELF executables, compare the architecture with `native.arch` / the host's `uname -m` and fail before upload on a mismatch. This catches the macOS-arm64-to-amd64 case (`exec format error`) that `builder.platform` handles in Docker mode.
4. **Acquire the lock** on every target host (`internal/lock`).
5. Per role, per host, in `rollout.batch_size` batches:
   1. **Stage**: upload a tar.gz of the artifact (a single file is packed too) to `releases/<id>.tmp/`, verify its sha256, extract, create the `shared_dirs` links, `chmod -R a+rX,go-w`, then `mv releases/<id>.tmp releases/<id>`. `ssh.Upload` writes with `umask 077`, so the explicit chmod is required or the service account cannot read its own release.
   2. **Render env** to `etc/env` (see §9). This needs no sudo: the file is the deploy user's, mode 0600, and systemd reads `EnvironmentFile` as root, so the service account never sees it.
   3. **Record the previous release** by reading the `current` link.
   4. **Activate**: `ln -sfn releases/<id> current.tmp && mv -T current.tmp current` (atomic rename on the same filesystem).
   5. **Restart** the unit (`sudo systemctl restart`). There is a short outage.
   6. **Verify**: `systemctl is-active`, then poll `http://127.0.0.1:<port><path>` with the existing `healthcheckCommand` until the timeout.
   7. **Commit**: `touch releases/<id>/.qifa-ok`, and append a `deployed` event to `.qifa/state.jsonl`.
   8. **On failure after activation**: point `current` back at the previous release, restart, verify, and return the original error together with any rollback error. Attach `journalctl -u <unit> -n 50` to the error, as `withContainerDiagnostics` does for Docker.
6. On a host failure, skip the remaining batches (same as Docker mode).
7. **post_deploy hook**; **prune** releases beyond `retain`, never removing `current` or the newest `.qifa-ok` release.
8. **Release the lock** (deferred, as in Docker mode).

A failed first deploy has no rollback target: the unit is left failed, the journal tail is reported, and the release directory is kept for inspection.

### Interrupted deploys

A link switch and a restart are not one atomic transaction. `qifa status` and the start of every deploy detect these states: `current` points at a release without `.qifa-ok`, the unit is not active, or `releases/*.tmp` directories exist. They are reported with the remedy (`qifa rollback`, or simply redeploy). Leftover `*.tmp` directories are removed at the start of the next deploy, under the lock.

## 8. systemd unit

```ini
[Unit]
Description=qifa: myapp (web)
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=0

[Service]
Type=exec
User=myapp
WorkingDirectory=/opt/qifa/myapp/current
ExecStart=/opt/qifa/myapp/current/bin/myapp --port 8080
EnvironmentFile=-/opt/qifa/myapp/etc/env
Restart=on-failure
RestartSec=3
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=/opt/qifa/myapp/shared

[Install]
WantedBy=multi-user.target
```

- `ExecStart` is rendered from the `exec` argv using systemd's quoting rules (each argument double-quoted with `\` and `"` escaped; `%` doubled to `%%`; `$` doubled to `$$`). It is never passed through a shell.
- Only `qifa native setup` installs or changes the unit. `deploy` re-renders it and compares it with the installed copy; on a difference it stops before touching any host and asks for `qifa native setup` to be rerun. The deploy user must never be able to write unit files: a unit can say `User=root`, so that permission would be root access by another name.
- `StartLimitIntervalSec=0` disables systemd's start rate limit. With the default (5 starts in 10s), a crashing release under `Restart=on-failure` spends the budget while qifa waits on its health check, and systemd then refuses to start the previous release qifa restores (`start-limit-hit`): the automatic rollback fails and nothing runs. Found by the e2e test. `RestartSec=3` still paces restarts, and the health check decides what is healthy.
- `WorkingDirectory` and `ExecStart` go through `current`, so the unit itself does not change between releases.

## 9. Environment and secrets

`secrets.Render` is reused to collect `env.clear`, `env.secret` and `env.secret_command`. Its output format is the format `docker --env-file` expects (values taken literally, only `\n` escaped). That format does **not** survive systemd's `EnvironmentFile=` parser, which strips quotes and interprets backslashes. A native renderer writes each value as `KEY="…"` with `\`, `"` and newlines escaped per systemd's rules, so any byte sequence round-trips. A unit test feeds adversarial values through `systemd-analyze`-equivalent parsing.

- `qifa env diff` in native mode compares the rendered env with `etc/env`, printing key names only, as today.
- Semantic difference from Docker mode: `systemctl restart` re-reads `EnvironmentFile`, whereas `qifa restart` of a container keeps its original env. Only `deploy` writes the env file, so in practice both modes apply new env only on deploy. `docs/deploying.md` should say this.

## 10. Privilege and provisioning

Two privilege tiers.

**One time, as a sudo-capable user: `qifa native setup`.** This command is idempotent and prints what it changes:

- creates the service account `native.user` (system user, no login shell);
- creates `/opt/qifa/<service>/{releases,shared,etc}` with the ownership shown in §6;
- writes the unit and runs `systemctl daemon-reload` and `systemctl enable`;
- installs `/etc/sudoers.d/qifa-<service>` granting the deploy user exactly:
  ```text
  deploy ALL=(root) NOPASSWD: /usr/bin/systemctl restart qifa-myapp.service, \
                              /usr/bin/systemctl start qifa-myapp.service,   \
                              /usr/bin/systemctl stop qifa-myapp.service
  ```
  One line per unit, with literal paths and **no wildcards**: a `*` in sudoers arguments also matches spaces and extra arguments, which would widen the grant. Setup writes the exact unit names for every role.
  The deploy user is added to the `systemd-journal` group, so `journalctl` and `systemctl status` need no sudo.

**Every deploy: the shared `deploy` user** from `docs/deploying.md`, with only the narrow sudo above. Root SSH is never required.

`internal/ssh` has no sudo support today (DESIGN.md says it does, but `ssh.go` has none). Phase 1 adds `Client.Sudo(ctx, host, argv...)`, which runs `sudo -n -- <quoted argv>`. With `-n`, a missing sudoers rule fails immediately with a clear error instead of hanging on a password prompt. Every deploy runs the same checks read-only on every host before changing any (preflight), and `qifa doctor` in native mode will report them: `sudo -n -l` for each required rule; that the deploy user's `systemctl` can reach systemd (as non-root it goes through D-Bus, so a host without `dbus` fails every health check: found by the e2e test, since minimal images omit it), `systemctl --version`, the unit's presence, directory ownership, and that `uname -m` matches `native.arch`.

**Alternative, documented but not the default:** systemd user units for the deploy user (`loginctl enable-linger deploy`, units in `~/.config/systemd/user`). This needs no sudo at all after setup, but the app runs as the deploy user rather than its own account, and `EnvironmentFile` must be readable by that user.

## 11. CLI mapping

| Command | Native behavior |
| --- | --- |
| `deploy [-n] [--version] [--allow-dirty]` | §7; `-n` prints build, upload, link switch and unit diff |
| `reconcile [-n]` | Converge if `current` ≠ version or the unit is inactive; report (not fix) unit drift |
| `rollback [version]` | Newest `.qifa-ok` release ≠ `current`, or the named version, which must exist on every host; switch, restart, verify |
| `stop` / `start` / `restart` | `sudo systemctl stop/start/restart <unit>` |
| `status` | Audit log history plus, per host: `current` version, unit state, health, available releases (`*` marks known-good) |
| `logs [-f] [-n N]` | `journalctl -u <unit> [-f] -n N` |
| `app exec <cmd>` | Not provided in v1: running as the service account would need a broad sudo rule. Use `ssh` |
| `remove` | Refuses, pointing at `native remove`: deleting units and sudoers needs root, which deploys never have |
| `native remove [--ssh-user root] [--purge [--yes]]` | As root: stop and disable the units, delete them and the sudoers rule. `--purge` also deletes `/opt/qifa/<service>` (releases and `shared/`) and the service account, after the operator types the service name (or `--yes`) |
| `env diff` | §9 |
| `lock status` / `lock release` | Unchanged |
| `native setup` | §10 (new) |
| `doctor` | Native checks (§10) instead of Docker and registry checks |
| `proxy *`, `app maintenance/live/containers`, `sweep`, `prune`, `backup`, `restore`, `cert` | Error: "not available with `native:`" |

Code layout: `internal/native/` with `native.go` (Backend), `release.go` (staging, link switch, retention, `.qifa-ok`), `systemd.go` (unit rendering, ExecStart quoting), `envfile.go` (systemd env rendering), `setup.go`. The shared pieces stay where they are: `ssh` (+ `Sudo`), `lock`, `hooks`, `state`, `secrets.Render`, plus version resolution, which moves from `internal/deploy` to a small `internal/version` so both modes import it.

## 12. Failure cases

| Failure | Expected result |
| --- | --- |
| Build fails, artifact missing, arch mismatch | Nothing on any host changes |
| Upload or checksum fails | `releases/<id>.tmp` left behind and cleaned next deploy; `current` untouched |
| Missing sudoers rule | `sudo -n` fails immediately; error names the rule; `doctor` would have caught it |
| systemd restart fails | Restore previous `current`, restart, verify; report the journal tail |
| Health check times out | Same as above |
| Rollback restart also fails | Report both errors; keep all release dirs; `current` points at previous |
| Command interrupted mid-switch | Next `status`/`deploy` reports a non-`.qifa-ok` current or inactive unit |
| Concurrent deploy | Second one fails immediately, naming the lock holder (existing behavior) |
| First deploy fails | Unit left failed; journal tail reported; no rollback target |
| Host fails in a multi-host rollout | Remaining batches skipped; already-deployed hosts keep the new release |

## 13. Testing

- Unit: config validation (native vs. Docker exclusivity, unknown keys), release-ID validation, ExecStart quoting, systemd env-file escaping round-trip, retention selection (never prunes `current` or newest `.qifa-ok`), ELF architecture detection.
- Integration, extending `internal/deploy/integration_test.go`'s harness with a systemd-enabled SSH container (e.g. `jrei/systemd-ubuntu` with sshd): first deploy, second deploy, failing health check → auto-rollback, manual rollback, rollback by version, restart, logs, interrupted deploy (kill qifa after link switch), concurrent deploy lock, `setup` idempotency, a missing sudo rule.
- The `local` host transport makes a no-SSH smoke test possible on a Linux dev box.
- Regression: the existing Docker test suite passes unchanged.

## 14. Implementation phases

**Phase 1: deploy end to end (done).** Config block and validation; `internal/version` extraction; `ssh.Sudo`; `internal/native` release staging, link switch, systemd unit and env file; `qifa native setup`; Backend interface and dispatch in `internal/app`; `deploy` with health check and auto-rollback (rollback-on-failure is in phase 1 because a deploy without it is not safe to ship).

**Phase 2: operations (done).** `rollback [version]`, `status`, `logs`, `stop/start/restart`, `native remove`, `env diff`, `reconcile`, `doctor` native checks, retention pruning, interrupted-deploy detection. Details as built:

- Rollback without a version picks the release whose `.qifa-ok` is newest and is not live: the most recently *healthy* release, not the most recently staged. `.qifa-ok` is touched on every deploy or rollback that passes. A named version must exist on every host; one that never passed is allowed with a warning. A rollback that fails its health check is reverted like a failed deploy.
- Pruning runs after each successful host deploy: keep the newest `retain` releases by staging time, plus always the live release and the automatic rollback target, however old.
- Interrupted deploys (a `current` release without `.qifa-ok`, or leftover `*.tmp`) are reported by `status`, flagged by `reconcile`, and warned about at the start of the next deploy.

**Phase 3: docs (done).** User guide `docs/native.md` (config, setup and sudoers, env-on-restart semantics, arch, mixed hosts), README example, `docs/deploying.md` pointer, troubleshooting entries, DESIGN.md section.

## 15. Native and Docker apps on the same host

The mode is per service (per config file), so one host can run Docker apps and native apps side by side. They share nothing qifa manages:

| | Docker app | Native app |
| --- | --- | --- |
| What is deployed | containers labelled `qifa.service=<svc>` | `/opt/qifa/<svc>/current` |
| Process supervision | `docker run --restart unless-stopped` | `qifa-<svc>.service` |
| Deploy lock | `/tmp/qifa-lock-<svc>` | `/tmp/qifa-lock-<svc>` |

What to watch:

- **Service names must be unique per host across modes.** Both modes take the same lock path, and a shared name makes `qifa status` ambiguous. A config cannot see other configs, so `qifa native setup` warns when the host already has containers labelled with the same service name.
- **Ports.** A native app binds host ports directly. If kamal-proxy runs on the host, it owns 80/443.
- **The deploy user** needs the `docker` group for the Docker apps and the per-service sudoers rule for each native app; one shared `deploy` user serves both.
- Mixing modes within one service (a Docker web role and a native worker) is not supported; split it into two services.

A mixed host may already run the containerized kamal-proxy; §16 covers putting native apps behind it, or behind a native kamal-proxy on a Docker-free host.

## 16. Next: native apps behind kamal-proxy

**Status:** phase 1 (§16.6) built: a native kamal-proxy with routes, HTTPS and maintenance mode. Zero downtime (§16.4) is not planned.

Native mode as built gives an app a process on a port. Compared with Docker apps it lacks HTTPS and domain routing (operators bring Caddy or nginx), zero-downtime deploys (`systemctl restart` means the app is down while it starts), and maintenance mode (`qifa app maintenance` / `app live` go through kamal-proxy). kamal-proxy provides all three, and qifa already drives it. It does not care what it forwards to: a Docker deploy registers `--target <container-ip>:<port>`, and a native deploy can register a port on the host.

### 16.1 Where the proxy runs

kamal-proxy is a single Go binary, and its Docker image only packages it. `test/e2e/deploy-host/Dockerfile` already builds it with `go install github.com/basecamp/kamal-proxy/cmd/kamal-proxy@v0.9.0` and runs it as a plain process. The long-running server is `kamal-proxy run`, and `kamal-proxy deploy|remove|stop|resume|list` from the same binary control it locally. That is the CLI qifa calls today through `docker exec`.

| | A: proxy in a container (as today) | B: proxy as a systemd service |
| --- | --- | --- |
| Docker on the host | required | not needed |
| Target address | Docker gateway, e.g. `172.18.0.1:8081` | `127.0.0.1:8081` |
| App listens on | the gateway address, or `0.0.0.0` behind a firewall rule | `127.0.0.1` only |
| Firewall | container-to-host traffic is blocked by some setups (ufw) | nothing to open |
| Ports 80/443 | Docker publishes them | `AmbientCapabilities=CAP_NET_BIND_SERVICE`, own account, no root |
| Fronting Docker apps too | yes (today's behaviour) | only Docker apps that publish their port on `127.0.0.1` |

**Recommendation: B for native hosts.** It keeps the host Docker-free and never exposes an app port. A is right only for a mixed host that already runs the containerized proxy for Docker apps (§15). A host runs one proxy or the other; both cannot own 80/443.

```text
internet ──443──▶ kamal-proxy (qifa-proxy.service) ──▶ 127.0.0.1:8081 ──▶ myapp (qifa-myapp.service)
```

### 16.2 Native proxy (option B)

- **Boot.** `qifa proxy boot` on a host whose `proxy_boot` sets `mode: native` (name to be settled) installs a pinned `kamal-proxy` binary under `/opt/qifa/proxy/bin`, a `kamal-proxy` system account, a state directory (`/var/lib/qifa-proxy`: routes and certificates, the counterpart of the `kamal-proxy-config` volume) and `qifa-proxy.service` running `kamal-proxy run --http-port 80 --https-port 443`. Like `native setup`, this is a root step, run once. `proxy upgrade` swaps the binary and restarts; routes survive because they live in the state directory.
- **Command seam.** `internal/proxy` builds every command as `docker exec kamal-proxy kamal-proxy …` (`Deploy`, `Remove`, `Stop`, `Resume`, `Details`). That prefix becomes a per-host runner: the `docker exec` wrapper, or the binary path run directly. The flags `deployCommand` already builds (host, TLS, path prefixes, health check, timeouts) are unchanged.
- **Access for the deploy user.** `deploy` and the other subcommands reach the running server over a local socket. Grant the deploy user access through group ownership of that socket (a `qifa-proxy` group), which is narrower than sudo and needs no sudoers rule.
- **Certificates.** `tls.source: kamal` (kamal-proxy's own ACME, http-01) and `static` (files the operator places) work natively as they are. `tls.source: qifa` runs lego in a container writing into the proxy's Docker volume (`internal/cert`), so it needs a native path: the lego binary on the host, writing into the state directory. That is deferred until someone needs DNS-01 on a native host.
- **Config.** A native app config regains `proxy:` (host, TLS, path prefixes, healthcheck), which §4 currently rejects next to `native:`. The app port comes from `native.port`, and `proxy.app_port` stays rejected so there is one source of truth for it.

### 16.3 Level 1: route, with a restart

Register the route once per deploy (`kamal-proxy deploy <svc> --target 127.0.0.1:<port>`) and keep today's single port and restart. This brings back HTTPS, domains and maintenance mode. During the restart the proxy answers with a gateway error instead of the client seeing connection refused. It is a small change on top of what exists: the deploy flow gains the registration step, `remove` deregisters, and `app maintenance` / `live` stop refusing in native mode.

### 16.4 Zero downtime (not planned)

Decided against: native deploys restart the process, and the proxy answers with a gateway error for as long as the app takes to start. The design below is kept as a record in case that changes.

Two processes must run at once, so a release needs two ports:

- **Config:** `native.ports: [8081, 8082]` in place of `port`. The app takes its port from qifa: `exec: [myapp, --port, "{port}"]`, or `PORT` in its environment.
- **Units:** a templated unit, `qifa-myapp@.service`, instantiated per port (`qifa-myapp@8081`). Each slot has its own link (`slot-8081 -> releases/<id>`), so the two processes can run different releases. `WorkingDirectory` and `ExecStart` follow the slot's link instead of `current`, and `current` keeps naming the live release for `status` and `rollback`.
- **Deploy:** stage as today, point the idle slot at the new release, start that instance, health-check it on its port, run `kamal-proxy deploy --target 127.0.0.1:<idle>` (kamal-proxy health-checks again and switches atomically, draining the old target), then stop the old instance and update `current`. A failure before the switch only stops the new instance: the old one never stopped serving.
- **Rollback:** the same steps, with the previous release in the idle slot.
- **sudoers:** start/stop/restart of each instance, by exact name, which still means no wildcards (`qifa-myapp@8081.service`, `qifa-myapp@8082.service`).

Not every app can run twice at once: one holding an SQLite write lock, a single-instance scheduler, anything binding a fixed second port. Those stay on level 1. Non-web roles (workers) never go behind the proxy and keep restart mode.

### 16.5 Verified against kamal-proxy v0.10.2

Read from the v0.10.2 source and confirmed by the e2e test. qifa defaults to v0.10.2, the latest release, for the native proxy and the container proxy alike (the container default was v0.9.2). A native proxy needs v0.10 or later.

- **No binaries are published**, only the `basecamp/kamal-proxy` image. `qifa proxy boot` therefore uses `proxy_boot.binary` when set, and otherwise cross-compiles `github.com/basecamp/kamal-proxy@<version>` for the host's architecture (CGO off, version stamped so `kamal-proxy version` reports it), cached under the deployer's user cache directory. That needs Go on the deployer; without it the error says to set `proxy_boot.binary`.
- **State follows `$HOME`.** There is no flag for it: routes are saved to `$HOME/.config/kamal-proxy/kamal-proxy.state` and ACME data to `.../certs`. The unit sets `HOME=/var/lib/qifa-proxy` (also its `StateDirectory=`), and routes survive a restart of `qifa-proxy.service` (tested).
- **Socket.** v0.10 added `KAMAL_PROXY_SOCKET`. Before it, the socket was `$XDG_RUNTIME_DIR/kamal-proxy.sock`, falling back to the temp directory, and every client had to match. The unit and every client set `KAMAL_PROXY_SOCKET=/run/qifa-proxy/kamal-proxy.sock`, which is one reason v0.10+ is required.
- **Socket permissions.** kamal-proxy creates the socket with the process umask and never chmods it, and connecting to a Unix socket needs write permission. `UMask=0007` plus `RuntimeDirectoryMode=0750` give the `qifa-proxy` group access, and `proxy boot` adds the deploy user to that group. Routing needs no sudo.
- **The client derives the ACME cache path from the caller's home.** `kamal-proxy deploy`'s `--tls-acme-cache-path` defaults to `$HOME/.config/kamal-proxy/certs` *of the process running the command*. Inside the container that is the server's user, so Docker mode is fine. Natively, the deploy user would point the server at `/home/deploy/...`. The native client passes `--tls-acme-cache-path /var/lib/qifa-proxy/.config/kamal-proxy/certs` explicitly.
- **`list --json`** (v0.10) gives services keyed by name with hosts, path prefixes (default `/`), targets, state and TLS; `qifa status` parses it rather than the display table.
- **Host-local targets** work as expected: `--target 127.0.0.1:<port>` with kamal-proxy's own health check on the app's `native.healthcheck.path`.
- **Ports 80/443 without root** work through `AmbientCapabilities=CAP_NET_BIND_SERVICE` (tested with a static certificate). Automatic certificates (`tls.source: kamal`) are not exercised by the e2e, which has no public DNS; they use the same listener and the pinned cache path above.

### 16.6 Phases

1. **Done.** Native proxy boot, the command seam in `internal/proxy`, and level 1 for native apps: HTTPS, domains and maintenance mode on a Docker-free host. `qifa proxy boot|upgrade|remove` (root), `start|stop|restart|logs|details` (deploy user), route registration after each healthy deploy, `qifa app maintenance|live`, the route in `qifa status`, deregistration in `native remove`. A host refuses to boot the native proxy while the kamal-proxy container runs on it.
2. `tls.source: qifa` (lego DNS-01) on native hosts, if needed.

Zero downtime (§16.4) is not planned.

## 17. Design decisions

1. **`native:` block selects the mode**: it fits the existing schema and strict key checking without changing what `builder:` means.
2. **Downtime accepted**: a deploy is a `systemctl restart`, and zero downtime is not planned. HTTPS, domains and maintenance mode come from kamal-proxy run as a systemd service (§16), so the host stays Docker-free.
3. **systemd only.**
4. **SSH, no agent**: qifa stays client-driven and the deployer stateless.
5. **Immutable releases + `current` link + `.qifa-ok` marker**: the host filesystem is the source of truth, as Docker labels are in Docker mode.
6. **Reuse, don't abstract**: lock, hooks, state, secrets, version and SSH are shared; the Docker deployer is untouched except for the dispatch interface.
7. **Narrow, explicit privilege**: one-time `native setup` owns the unit and sudoers; deploys can only start, stop or restart their own unit (`sudo -n`, exact allowlist, no wildcards, no file writes as root).

**Verification:** `test/e2e/native-e2e.sh` runs the acceptance criterion against a Debian container with systemd as PID 1, sshd, sudo and no Docker: refusal before setup, idempotent setup, two deploys, an identical rebuild reused, a crashing release rolled back with its journal attached, env values with quotes, backslashes, `$`, `%` and newlines delivered byte-for-byte, the shared directory writable, and the privilege boundaries (deploy user limited to three `systemctl` verbs on its own unit; service account unable to read the env file or write its release).

**Acceptance criterion:** On a Linux server with no Docker installed, after `qifa native setup`, `qifa deploy` ships a non-Docker application under systemd, `qifa status` / `qifa logs` report it, and a subsequent deploy whose health check fails leaves the previous release running, without the deploy user having unrestricted root.
