# qifa

A Go-native deployment CLI for Docker apps on plain Linux servers, modeled on
[Kamal](https://github.com/basecamp/kamal). Reuses
[kamal-proxy](https://github.com/basecamp/kamal-proxy) for zero-downtime
traffic switching; everything else (orchestration, build, lifecycle,
discovery) is qifa's own.

```text
qifa CLI
  -> SSH into servers
  -> Docker build/push/pull/run
  -> register/deregister with kamal-proxy
  -> health check, switch traffic, rollback if needed
```

For the full design, see [DESIGN.md](DESIGN.md).

## Install

```bash
git clone https://github.com/gokamal/qifa.git
cd qifa
make build
./qifa version
```

The binary is statically linkable (no cgo) and self-contained. Drop it
anywhere on `$PATH`.

## Quickstart

```bash
qifa init       # writes a starter qifa.yaml
qifa config     # show the loaded config (defaults applied)
qifa deploy     # build, ship, healthcheck, switch traffic
qifa logs -f    # tail the running container
qifa rollback   # roll back to the previous version
```

## Three Common Configs

### 1. Build on each target (no registry)

Simplest setup: code lives where you run qifa, qifa uploads it to each host
and builds the image there. No registry needed.

```yaml
service: myapp
image: myapp

servers:
  web:
    hosts: [10.0.0.11, 10.0.0.12]
    port: 8080
    app_port: 80

builder:
  host: per_target
  context: .

ssh:
  user: deploy
```

### 2. Build locally, push to a registry

Build once where qifa runs, push to a private registry, every target host
pulls. Standard model.

```yaml
service: myapp
image: registry.example.com/myapp

servers:
  web:
    hosts: [10.0.0.11, 10.0.0.12]
    app_port: 3000

# Operator runs `qifa proxy boot` once to launch kamal-proxy on these hosts.
# App deploys verify it's running but never modify boot config.
proxy_boot:
  hosts: [10.0.0.11, 10.0.0.12]

# Per-app routing — set on every deploy.
proxy:
  host: app.example.com
  app_port: 3000
  healthcheck:
    path: /up

registry:
  server: registry.example.com
  username: deploy
  password_env: REGISTRY_PASSWORD

builder:
  context: .
  platform: linux/amd64,linux/arm64   # multi-arch via buildx --push

env:
  clear:
    APP_ENV: production
  secret_command: sops --decrypt secrets.enc.env
```

First-time setup on each new host: `qifa proxy boot` (idempotent).
To upgrade the proxy: `qifa proxy upgrade` (state volume preserved, routes
survive).

### 3. Deploy an externally produced image

No build, just pull and run. Image must include `:tag` or `@digest`. The
tag is resolved to the actual registry digest at deploy time so rollback
works even with floating tags like `:latest`.

```yaml
service: nginx
image: nginx:1.27-alpine     # or nginx:latest, or ghcr.io/org/app:v1

servers:
  web:
    hosts: [10.0.0.11]
    port: 80
    app_port: 80
    proxy: false
```

### 4. Deploy to the machine you are already on

Use the reserved host name `local` to run every step through `/bin/sh` on this
machine instead of dialling SSH. No sshd, no key, no `known_hosts` entry — for
single-box self-hosting, a laptop, or trying qifa out.

```yaml
service: myapp
image: nginx:1.27-alpine

servers:
  web:
    hosts: [local]     # reserved: this machine, no SSH
    port: 8080
    app_port: 80
    proxy: false       # publish the port directly

proxy_boot:
  hosts: [local]
```

`local` is opt-in by exact name. `localhost` and `127.0.0.1` still mean SSH,
because targeting a local SSH daemon (a VM, a container, the e2e harness on
`127.0.0.1:2222`) is a different and legitimate thing. Hosts can be mixed:
`hosts: [local, 10.0.0.12]` deploys to this machine and a remote one in the
same roll-out.

Two caveats:

- The commands still run through `/bin/sh`, so the host needs `docker` and
  `curl` on `PATH` — the same requirements a remote target has.
- On macOS, Docker Desktop runs containers in a Linux VM, so the host cannot
  reach container bridge IPs. The kamal-proxy path healthchecks the container
  by IP and will not work there; use `proxy: false` with a published `port:`,
  which is checked over `127.0.0.1`. On Linux the full proxy path works.

## Verb Cheatsheet

```text
qifa init [path]                 # write a starter qifa.yaml
qifa version                     # build version + commit
qifa config                      # print loaded+defaulted config

qifa deploy [--dry-run]          # build, ship, switch, prune
  [--version <v>] [--allow-dirty] # pin the version instead of deriving it from git
qifa rollback [version]          # auto = previous; or explicit version
qifa stop                        # docker stop the running container per role/host
qifa start                       # docker start the most recent labeled container
qifa restart                     # stop then start (re-registers with proxy)
qifa remove                      # full teardown + deregister proxy

qifa prune                       # keep last N stopped (config: prune.retain_containers)
qifa sweep                       # remove orphan running containers (also runs at deploy start)
qifa env diff                    # compare config env vs. what the running containers got
qifa lock <status|release>       # show or forcibly clear deploy lock
qifa proxy <boot|start|stop|restart|upgrade|remove [--purge]|logs|details>
                                 # manage the shared kamal-proxy container
qifa status                      # deployment history + active containers
qifa doctor                      # preflight every host: ssh, docker, registry,
                                 # DNS/TCP/TLS, credentials, disk, clock, proxy

qifa logs [--follow] [--lines N] # docker logs from the active container
qifa app exec <command>          # docker exec in the active container
qifa app containers              # list labeled containers per role/host
qifa app maintenance [--message <msg>] [--drain-timeout <duration>]
qifa app live                    # leave maintenance mode

qifa accessory <boot|stop|start|restart|remove|logs|exec> <name> [args]
```

## When A Deploy Fails

Most deploy failures are really "this host could not reach the registry",
and the reason is one line buried in docker's pull output on a machine you
are not watching. qifa handles that specifically:

- **An image the host already has is not fetched again** (`pull_policy:
  missing`, the default). A deploy of an unchanged image contacts no registry
  at all, so a flaky link cannot fail it. Bumping the tag or digest in the
  config still pulls, because the host does not have that one. Set
  `pull_policy: always` for an image tracked by a moving tag (`:latest`),
  where re-resolving on every deploy is the point.
- **Transient network faults are retried.** DNS failures, TLS handshake
  timeouts, resets and rate limits get 3 attempts by default. Tune with
  `QIFA_PULL_RETRIES` (default 2 extra attempts) and `QIFA_PULL_RETRY_WAIT`
  (default 5s).
- **Silent hangs announce themselves.** If a pull or build prints nothing for
  45s, qifa says so rather than looking frozen (`QIFA_PULL_STALL_WARN`).
- **Failures are explained, not just reported.** The error names the likely
  cause (DNS, proxy, credentials, missing tag, disk, clock skew, architecture
  mismatch), lists what to check, and includes what the host itself says about
  reaching the registry:

```text
pull image ghcr.io/acme/app:v1 on 10.0.0.11 failed after 3 attempts:
the host cannot resolve the registry hostname (DNS failure)

  docker said:
    Error response from daemon: Get "https://ghcr.io/v2/": dial tcp:
    lookup ghcr.io on 10.0.0.2:53: no such host

  what to check:
    - check the host's resolvers: cat /etc/resolv.conf ; getent hosts ghcr.io
    - private registries often need an internal DNS server or /etc/hosts entry

  host diagnostics (10.0.0.11):
    docker-daemon      ok    server 27.1.1
    dns ghcr.io        FAIL  no address (check /etc/resolv.conf ...)
    tcp ghcr.io:443    FAIL  cannot open a TCP connection (firewall?)
    daemon-proxy       info  none configured for the docker daemon
    disk               ok    /var/lib/docker: 40G free of 80G
    clock              ok    2026-09-01T15:57:34Z (skew vs local: 1s)
```

Run those same checks before deploying — on every app, proxy, accessory and
builder host — with `qifa doctor`. It only reads; it changes nothing.

See [docs/troubleshooting.md](docs/troubleshooting.md) for the specific
failures and their fixes.

## Lifecycle Model In One Paragraph

App containers are stamped with `qifa.service`, `qifa.role`, `qifa.version`
labels at `docker run` time. All discovery (which container is active, what
was the previous version, what's stale) is answered by `docker ps` filtered
by those labels — Docker is the source of truth, not an on-disk index.
Stopped versions are kept around as rollback candidates (subject to
`prune.retain_containers`, default 5). `qifa rollback <version>` re-runs the
container with that label. `.qifa/state.jsonl` is an append-only audit log
only — every command works without it.

## Roles And Rollouts

Roles deploy sequentially (web first, then worker, etc.). Within a role,
hosts deploy in batches:

```yaml
rollout:
  batch_size: 1     # default: strict rolling (one host at a time)
  batch_wait: 5s    # sleep between batches
```

Set `batch_size: 0` to deploy every host in the role in parallel.

## Secrets

Three sources, merged into the `--env-file` passed to containers (later
wins on collision):

1. `env.clear`: cleartext key/value
2. `env.secret`: env var names read from the deployer's local env at deploy
3. `env.secret_command`: arbitrary shell command whose stdout is parsed as
   `KEY=VALUE` lines. Works with SOPS (`sops --decrypt`), Vault, 1Password
   (`op inject`), AWS Secrets Manager, anything that prints dotenv format.

## Deploying From More Than One Machine

qifa keeps no authoritative state on the deployer — the containers' `qifa.*`
labels are the source of truth and the deploy lock lives on the target hosts —
so any checkout on any machine can deploy. Four things keep that honest:

- **Relative paths resolve against the config file**, not your shell: qifa
  moves into the config's directory first, so `builder.context`, `files[].src`,
  `hooks.*` and `env.secret_command` mean the same thing from anywhere.
- **The version is explicit or a clean commit.** The tag is `--version` /
  `QIFA_VERSION` if given, else the short SHA of the config's checkout; a dirty
  tree is refused unless `--allow-dirty` tags it `-dirty`.
- **Everyone SSHes as one `deploy` user.** The lock records the real human
  regardless, so individual host accounts only cost you collisions.
- **`qifa env diff`** compares what the config renders now against the env file
  each running container was started with — a deploy takes its environment from
  whoever ran it, so the live env can be someone's stale secrets file. Key names
  only; a differing value is reported, never printed.

Host setup, secrets, CI and concurrent deploys: [docs/deploying.md](docs/deploying.md).

## Hooks

```yaml
hooks:
  pre_build: ./scripts/pre_build.sh
  post_deploy: ./scripts/post_deploy.sh
  pre_rollback: ./scripts/pre_rollback.sh
```

Hooks run on the deployer (not target hosts) and receive `QIFA_VERSION`
in their environment.

## Troubleshooting

Common issues and fixes: [docs/troubleshooting.md](docs/troubleshooting.md).
Team and multi-machine setup: [docs/deploying.md](docs/deploying.md).

## Status

Active development. The CLI is reasonably stable; the schema may still
change before a 1.0 tag.
