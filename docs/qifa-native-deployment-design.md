# Qifa Native Deployment — Design

**Status:** Proposed  
**Scope:** Single Linux host, native application deployment without Docker  
**Principle:** Keep Qifa simple: build → SSH upload → activate release → systemd restart → health check → rollback on failure.

## 1. Goals

- Extend Qifa with a `native` deployment option while preserving existing Docker behavior.
- Deploy executable binaries or application directories to one Linux server over SSH.
- Use systemd for process supervision, restart and logs.
- Support repeatable releases, manual rollback and automatic rollback on failed deployment.
- Reuse existing Qifa configuration, SSH, secrets and command patterns where practical.
- Avoid a resident Qifa agent, central database, artifact registry or large refactor.

## 2. Non-goals

- Zero-downtime deployment, blue-green switching or traffic routing.
- Multi-host orchestration, load balancing or rolling updates.
- Non-Linux platforms or init systems other than systemd.
- Container image building for the native backend.
- Language-specific packaging or dependency installation.

## 3. Architecture

```text
                         Qifa CLI
              deploy / rollback / status / logs
                            |
                 Backend selection
                    /           \
             Docker (existing)   Native (new)
                                     |
                              SSH transport
                                     |
                              Linux server
                                     |
                        releases + current link
                                     |
                                  systemd
```

Native deployment is a new path behind backend selection, not a new standalone CLI. Prefer minimal dispatch and reuse of existing utilities over introducing a generic deployment framework prematurely.

## 4. Proposed configuration

> Illustrative schema; reconcile with Qifa's existing YAML and CLI before implementation.

```yaml
service: myapp

deploy:
  type: native # docker (existing/default) | native

servers:
  web:
    hosts:
      - server.example.com

builder:
  command: make build
  artifact: dist/myapp

native:
  command: ./myapp
  user: myapp
  port: 8080
  healthcheck:
    path: /healthz
```

Suggested defaults:

| Property | Default |
| --- | --- |
| Runtime | systemd |
| Install directory | `/opt/qifa/<service>` |
| Working directory | `/opt/qifa/<service>/current` |
| Release retention | 5 |
| Restart policy | `on-failure` |
| Health-check timeout | 30 seconds |
| Transport | SSH |
| Rollback on failed deployment | enabled |

`builder.artifact` may be a single executable or directory. The build is a user-supplied command, so Qifa remains language-agnostic. For v1, assume the artifact is built for the target Linux OS/architecture and has its runtime dependencies available. Reject ambiguous or missing artifacts.

A service with no HTTP health endpoint can use `systemctl is-active` as a minimal check; this tests process activity, not application readiness. Do not require `port` if no HTTP health check is configured.

## 5. Target filesystem

```text
/opt/qifa/myapp/
├── releases/
│   ├── abc123/
│   │   └── myapp
│   └── def456/
│       └── myapp
├── current -> releases/def456
├── shared/
└── .deploy.lock

/etc/qifa/myapp.env
/etc/systemd/system/qifa-myapp.service
```

- Releases are immutable after staging and validation.
- `current` points to the selected release; persistent data lives under `shared/` or another external location.
- Secrets are not stored in release archives or source-controlled metadata.
- The previous release is captured before activation, for rollback.
- Release IDs should be unique and safe as directory names (e.g., commit SHA plus build identifier or artifact digest prefix).

## 6. Deployment lifecycle

1. **Validate** configuration, host, artifact path, and required privileges.
2. **Build** locally using the configured command.
3. **Package and transfer** the artifact to a temporary directory on the target; verify its checksum, then move into `releases/<id>`.
4. **Acquire a host-side deployment lock** before changing live state; reject concurrent activation/rollback operations.
5. **Record the previous release** from the existing `current` symlink. Verify the new release has a valid entry point and permissions.
6. **Activate** using a temporary symlink followed by atomic rename on the same filesystem. Do not replace a live release directory in place.
7. **Restart** `qifa-myapp.service` using systemd. A short outage is acceptable.
8. **Verify** `systemctl is-active` and, if configured, poll the HTTP health endpoint until the timeout.
9. **Commit** success and prune older releases, retaining the active and previous releases.
10. **On failure after activation**, restore the prior `current` link, restart the service, and verify recovery. Return an explicit error if rollback also fails.

A failed first deployment has no prior release: leave the failed service stopped or failed, report the problem, and retain diagnostics. Staging failures must not affect the running release.

### Interrupted deploys

A symlink switch and a systemd restart are not a single atomic transaction. On a subsequent command, inspect `current`, the service state and available releases; show an actionable warning when the active process cannot be confirmed healthy. Keep recovery simple rather than building a database-backed state machine. A small deployment metadata file may be introduced only if needed to identify the previous known-good release across process crashes.

## 7. systemd unit

```ini
[Unit]
Description=Qifa managed myapp
After=network-online.target
Wants=network-online.target

[Service]
Type=exec
User=myapp
WorkingDirectory=/opt/qifa/myapp/current
ExecStart=/opt/qifa/myapp/current/myapp
EnvironmentFile=-/etc/qifa/myapp.env
Restart=on-failure
RestartSec=3
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
```

The unit is provisioned once, or updated only when service configuration changes. Reload the systemd daemon when the unit changes. The example assumes `native.command` resolves to a valid executable path; arguments and quoting must be rendered safely, not concatenated into shell commands. Application execution should not depend on a shell unless explicitly requested.

## 8. CLI behavior

Retain Qifa's existing command names where possible:

```bash
qifa deploy
qifa rollback
qifa status
qifa logs -f
qifa restart
qifa remove
```

Native mapping:

| Operation | Native behavior |
| --- | --- |
| Deploy | Build, stage, switch link, restart, check health |
| Rollback | Select prior release, switch link, restart, verify |
| Status | Show host, release ID, systemd state, optional health |
| Logs | `journalctl -u qifa-myapp.service` |
| Restart | `systemctl restart qifa-myapp.service` |
| Remove | Explicitly stop/disable service; destructive file deletion requires clear confirmation |

For v1, keep backend-specific logic under `internal/deploy/native/` (or the closest existing package convention), for example `backend.go`, `release.go`, `systemd.go`. Avoid requiring Docker backend changes beyond routing and genuinely shared helpers.

## 9. Security and provisioning

- Provision a dedicated unprivileged service account, installation directories and systemd unit.
- Separate one-time privileged setup from ordinary deployments. Define narrow `sudo` permissions where systemd operations require elevation; do not require unrestricted root SSH.
- Make release directories non-writable by the running application account where practical.
- Restrict secret environment files (e.g., root-owned with permissions compatible with systemd loading).
- Reuse Qifa's existing secret-command/SOPS approach; never print decrypted secrets in logs.
- Escape/validate service names, paths and arguments; avoid assembling untrusted values into remote shell strings.
- Use SSH host-key verification and artifact integrity checks.
- Never delete `shared/` or external persistent data during release cleanup.

## 10. Failure cases

| Failure | Expected result |
| --- | --- |
| Build or upload fails | Existing release continues running |
| Checksum or staging validation fails | Existing release untouched |
| systemd restart fails | Restore previous release and restart |
| Health check times out | Restore previous release and verify health |
| Rollback restart fails | Surface both errors; preserve release files and logs |
| Deployment command is interrupted | Next command inspects current link and service state |
| Concurrent deployment | Reject or wait on host-side lock |
| First deploy fails | Report failure; no rollback target |

## 11. Testing

- Unit tests for config defaults, validation, release IDs, symlink operations and retention.
- Integration tests on a disposable systemd-enabled Linux VM/container environment with SSH.
- Happy path: initial deploy, second deploy, status, logs, restart and manual rollback.
- Fault injection: invalid artifact, SSH failure, missing executable, service startup failure, health timeout, interrupted activation and rollback failure.
- Verify that native changes do not alter existing Docker deployment behavior.
- Confirm permissions, secret redaction, lock behavior and persistent data preservation.

## 12. Implementation phases

**Phase 1 — Minimal native deployment:** Backend selection, local build, SSH upload, immutable releases, systemd unit setup, activation and restart.

**Phase 2 — Reliability:** Health checks, automatic/manual rollback, locking, safe cleanup and interruption diagnostics.

**Phase 3 — Operations:** Status, logs, restart, removal and documentation. Reuse existing commands when available.

## 13. Design decisions

1. **Single host only:** no fleet scheduling or proxy coordination.
2. **Downtime accepted:** use a simple `systemctl restart` rather than blue-green.
3. **systemd only:** rely on the standard Linux service manager.
4. **SSH, no agent:** Qifa remains client-driven.
5. **Immutable releases:** simple rollback through release pointers.
6. **No mandatory database or registry:** host filesystem and systemd provide sufficient state.
7. **No premature abstraction:** keep the Docker implementation stable; extract common code only where justified.

## 14. Open implementation questions

- What are Qifa's exact current YAML fields, command names and SSH abstractions? Match them before finalizing the schema.
- Should first-time provisioning be an explicit `qifa setup` step or part of `deploy` with narrow elevation?
- How should artifact directories and executable paths be mapped without requiring shell execution?
- What is the safest host-side locking mechanism compatible with Qifa's existing SSH command execution?
- What minimal persistent metadata is necessary to recover a known-good previous release after an interrupted deployment?

**Acceptance criterion:** On a freshly provisioned single Linux server, Qifa can deploy a non-Docker application, restart it under systemd, report its status/logs, and restore the previous working version when a subsequent deployment fails—without introducing zero-downtime infrastructure.
