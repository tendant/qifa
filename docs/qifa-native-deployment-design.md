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

- Zero-downtime deployment, blue-green switching or traffic routing (see §16 for a later path).
- TLS termination and domain routing. Without kamal-proxy, the operator brings their own (Caddy, nginx) or binds the app port directly.
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

A mixed host already runs kamal-proxy, which makes §16 cheap exactly where it is most useful.

## 16. Later: native app behind kamal-proxy

If the host can run one container, kamal-proxy can front a native process: the target is the host's address on the Docker bridge (e.g. `172.17.0.1:<port>`) instead of a container IP. That restores TLS (`internal/cert`), domain routing, maintenance mode, and zero-downtime switching. Zero downtime works by alternating two ports per release (start the new release on the idle port, health check, `kamal-proxy deploy --target`, stop the old one). This needs a templated unit (`qifa-myapp@<port>.service`), so it is deliberately out of scope for v1, but nothing in the v1 layout prevents it.

## 17. Design decisions

1. **`native:` block selects the mode**: it fits the existing schema and strict key checking without changing what `builder:` means.
2. **Downtime accepted in v1**: a simple `systemctl restart`; zero downtime is the §16 follow-up.
3. **systemd only.**
4. **SSH, no agent**: qifa stays client-driven and the deployer stateless.
5. **Immutable releases + `current` link + `.qifa-ok` marker**: the host filesystem is the source of truth, as Docker labels are in Docker mode.
6. **Reuse, don't abstract**: lock, hooks, state, secrets, version and SSH are shared; the Docker deployer is untouched except for the dispatch interface.
7. **Narrow, explicit privilege**: one-time `native setup` owns the unit and sudoers; deploys can only start, stop or restart their own unit (`sudo -n`, exact allowlist, no wildcards, no file writes as root).

**Verification:** `test/e2e/native-e2e.sh` runs the acceptance criterion against a Debian container with systemd as PID 1, sshd, sudo and no Docker: refusal before setup, idempotent setup, two deploys, an identical rebuild reused, a crashing release rolled back with its journal attached, env values with quotes, backslashes, `$`, `%` and newlines delivered byte-for-byte, the shared directory writable, and the privilege boundaries (deploy user limited to three `systemctl` verbs on its own unit; service account unable to read the env file or write its release).

**Acceptance criterion:** On a Linux server with no Docker installed, after `qifa native setup`, `qifa deploy` ships a non-Docker application under systemd, `qifa status` / `qifa logs` report it, and a subsequent deploy whose health check fails leaves the previous release running, without the deploy user having unrestricted root.
