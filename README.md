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

## Verb Cheatsheet

```text
qifa init [path]                 # write a starter qifa.yaml
qifa version                     # build version + commit
qifa config                      # print loaded+defaulted config

qifa deploy [--dry-run]          # build, ship, switch, prune
qifa rollback [version]          # auto = previous; or explicit version
qifa stop                        # docker stop the running container per role/host
qifa start                       # docker start the most recent labeled container
qifa restart                     # stop then start (re-registers with proxy)
qifa remove                      # full teardown + deregister proxy

qifa prune                       # keep last N stopped (config: prune.retain_containers)
qifa sweep                       # remove orphan running containers (also runs at deploy start)
qifa lock <status|release>       # show or forcibly clear deploy lock
qifa proxy <boot|start|stop|restart|upgrade|remove [--purge]|logs|details>
                                 # manage the shared kamal-proxy container
qifa status                      # deployment history + active containers

qifa logs [--follow] [--lines N] # docker logs from the active container
qifa app exec <command>          # docker exec in the active container
qifa app containers              # list labeled containers per role/host
qifa app maintenance [--message <msg>] [--drain-timeout <duration>]
qifa app live                    # leave maintenance mode

qifa accessory <boot|stop|start|restart|remove|logs|exec> <name> [args]
```

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

## Status

Active development. The CLI is reasonably stable; the schema may still
change before a 1.0 tag.
