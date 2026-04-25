# Go Kamal-Compatible Deployer Design

## Goal

Build a **Go-native deployment CLI** that works like Kamal but reuses the existing **kamal-proxy**.

```text
Go deploy CLI
  -> SSH into servers
  -> Docker build/push/pull/run
  -> configure kamal-proxy
  -> health check
  -> switch traffic
  -> rollback if needed
```

## Non-goals

Do **not** rebuild:

- kamal-proxy
- Docker
- Kubernetes
- Nomad
- full PaaS control plane

## Core Components

```text
cmd/godeploy
internal/config
internal/ssh
internal/docker
internal/registry
internal/proxy
internal/deploy
internal/secrets
internal/hooks
internal/state
internal/logs
```

## CLI

```bash
godeploy init
godeploy deploy
godeploy rollback
godeploy status
godeploy logs
godeploy app exec
godeploy accessory boot
godeploy accessory logs
```

## Config Example

```yaml
service: myapp
image: registry.example.com/myapp

servers:
  web:
    hosts:
      - 10.0.0.11
      - 10.0.0.12
    port: 3000
  worker:
    hosts:
      - 10.0.0.13
    cmd: ./worker

proxy:
  host: app.example.com
  app_port: 3000
  healthcheck:
    path: /up
    interval: 2s
    timeout: 5s

registry:
  server: registry.example.com
  username: reg
  password_env: REGISTRY_PASSWORD

env:
  clear:
    APP_ENV: production
    LOG_LEVEL: info
  secret:
    - DATABASE_URL
    - REDIS_URL

builder:
  mode: local
  source: local
  context: .
  dockerfile: Dockerfile
  platform: linux/amd64

ssh:
  user: ubuntu
  key: ~/.ssh/id_ed25519
```

## Build And Distribution Model

Separate the machine that builds an image from the machines that run it.

- builder machine: the machine that runs `docker build`
- deployment target: any host in `servers.*.hosts` that runs the container

The config stays small and models build location, code source, and optional registry usage:

```yaml
service: myapp
image: registry.example.com/myapp

builder:
  mode: local # local | remote | per_target
  host: 10.0.0.21 # required only for mode=remote
  source: local # local | git, default local
  context: .
  repo: git@github.com:org/app.git
  ref: v1.2.3
  subdir: . # default .
  dockerfile: Dockerfile
  platform: linux/amd64

registry:
  server: registry.example.com
  username: reg
  password_env: REGISTRY_PASSWORD
```

### Builder Modes

`local`

- build on the machine running `qifa`
- requires `registry`
- produces one shared image that deployment targets pull

`remote`

- build on one SSH-reachable remote machine in `builder.host`
- requires `registry`
- that remote machine may be either a deploy target or a dedicated build host
- produces one shared image that deployment targets pull

`per_target`

- build on each deployment target individually
- requires no `registry`
- `builder.host` must not be set
- each host runs the image it built locally

### Builder Source

`source=local`

- use files from the machine running `qifa`
- `builder.context` selects the local build context directory
- `builder.dockerfile` is relative to that context
- `local` mode builds directly from that local path
- `remote` and `per_target` upload that local path before building

`source=git`

- clone a repository at a pinned `builder.ref`
- `builder.repo` is the clone URL
- `builder.subdir` selects the build context inside the checked out repo
- `builder.dockerfile` is relative to that subdirectory
- the clone happens on the machine that runs the build

### Source Resolution By Mode

- `builder.mode=local`
  - `source=local`: read files locally and build locally
  - `source=git`: clone locally and build locally
- `builder.mode=remote`
  - `source=local`: upload local files to `builder.host` and build there
  - `source=git`: clone on `builder.host` and build there
- `builder.mode=per_target`
  - `source=local`: upload local files to each deployment target and build there
  - `source=git`: clone on each deployment target and build there

### Validation Rules

- `builder.mode` must be one of `local`, `remote`, `per_target`
- `builder.mode=local` requires `registry`
- `builder.mode=remote` requires `registry`
- `builder.mode=remote` requires `builder.host`
- `builder.mode=per_target` forbids `registry`
- `builder.mode=per_target` forbids `builder.host`
- `builder.source` must be one of `local`, `git`
- `builder.source=local` requires `builder.context`
- `builder.source=local` forbids `builder.repo`, `builder.ref`, and `builder.subdir`
- `builder.source=git` requires `builder.repo` and `builder.ref`
- `builder.source=git` defaults `builder.subdir` to `.`
- `builder.source=git` forbids `builder.context`
- `builder.dockerfile` is always relative to the active build context

### Git Authentication

When `builder.source=git`, the machine doing the build must already have git access configured.

- `builder.mode=local`: local machine must be able to clone
- `builder.mode=remote`: `builder.host` must be able to clone
- `builder.mode=per_target`: each deployment target must be able to clone

### Image Semantics

- with `registry`, `image` is the registry reference that gets pushed and pulled
- without `registry`, `image` is a host-local Docker tag used independently on each deployment target

### Supported Combinations

`local + registry`

1. build locally
2. push once to registry
3. each target logs into registry
4. each target pulls and runs the image

`remote + registry`

1. materialize the build context on `builder.host`
2. build once on `builder.host`
3. push once to registry from `builder.host`
4. each target logs into registry
5. each target pulls and runs the image

`per_target + no registry`

1. materialize the build context on each deployment target
2. each target builds the image locally
3. each target runs its locally built image

### Reproducibility Note

`per_target` does not create one shared artifact. Different hosts may build different images unless base images and build inputs are pinned tightly.

## Deploy Flow

1. Load config
2. Resolve version (git SHA or timestamp)
3. Materialize the build context according to `builder.source`
4. Build the Docker image according to `builder.mode`
5. If `registry` is configured, push the image once
6. SSH into each deployment target
7. Ensure Docker is installed/running
8. Ensure kamal-proxy is installed/running
9. If `registry` is configured, install per-host Docker auth and pull the image
10. If `builder.mode=per_target`, build the image on the target host
11. Start new container with unique name
12. Health-check new container
13. Switch traffic
14. Stop old container
15. Record deployment state
16. Prune old resources

## State Model

Use append-only facts.

```sql
deployments
- id
- service
- version
- image
- status
- started_at
- finished_at

deployment_events
- id
- deployment_id
- host
- role
- event_type
- message
- created_at
```

MVP local storage:

```text
.godeploy/state.jsonl
```

## Rollback Flow

1. Find last successful deployment
2. Pull previous image if needed
3. Start previous container
4. Health-check
5. Switch traffic back
6. Stop failed/current container
7. Record rollback event

## SSH Layer

Use Go SSH library:

```text
golang.org/x/crypto/ssh
```

Features:

- parallel fanout
- per-host timeout
- streaming logs
- retries
- sudo support
- known_hosts verification

## Docker Layer

MVP uses remote shell commands:

```bash
docker build
docker push
docker pull
docker run
docker ps
docker stop
docker rm
docker logs
```

## Proxy Integration

Reuse **kamal-proxy**.

Conceptual interface:

```go
type Proxy interface {
    EnsureInstalled() error
    Deploy() error
    Remove() error
}
```

## Deployment State Machine

```text
Pending
Building
Pushing
Pulling
Starting
HealthChecking
SwitchingTraffic
CleaningUp
Succeeded
Failed
RolledBack
```

Each step should be idempotent.

## Rollout Strategy

### Web Role

Rolling deploy:

```text
host1 -> deploy -> healthy -> switch
host2 -> deploy -> healthy -> switch
```

### Worker Role

Restart strategy:

```text
start new worker
wait ready
stop old worker
```

## Hooks

```yaml
hooks:
  pre_build: ./scripts/pre_build.sh
  post_deploy: ./scripts/post_deploy.sh
  pre_rollback: ./scripts/pre_rollback.sh
```

## Secrets

MVP:

- read local environment
- render env file
- copy to host
- use docker --env-file

Later:

- SOPS + age
- Vault
- AWS Secrets Manager
- 1Password

## Accessories

```yaml
accessories:
  redis:
    image: redis:7
    host: 10.0.0.13
```

Commands:

```bash
godeploy accessory boot redis
godeploy accessory logs redis
```

## MVP Scope

- single app
- web + worker roles
- multiple hosts
- Docker registry
- kamal-proxy integration
- health checks
- rollback
- logs/status
- env secrets
- append-only state

## Later Features

- remote builders
- multi-arch builds
- maintenance mode
- deploy locks
- OpenTelemetry
- web UI
- GitHub Actions integration

## Suggested Repo Layout

```text
godeploy/
  cmd/godeploy/
  internal/config/
  internal/deploy/
  internal/ssh/
  internal/docker/
  internal/proxy/
  internal/state/
  docs/
```

## Positioning

**A Go-native, Kamal-inspired deployer for Docker apps on plain Linux servers using kamal-proxy for zero-downtime deploys.**
