# qifa — Go Kamal-Compatible Deployer

## Goal

A Go-native deployment CLI for Docker apps on plain Linux servers. Reuses the
existing **kamal-proxy** for zero-downtime traffic switching; everything else
(orchestration, build, lifecycle) is qifa's own.

```text
qifa CLI
  -> SSH into servers
  -> Docker build/push/pull/run
  -> register/deregister with kamal-proxy
  -> health check
  -> switch traffic
  -> rollback if needed
```

## Non-goals

Do not rebuild kamal-proxy, Docker, Kubernetes, Nomad, or a full PaaS control
plane.

## Repo Layout

```text
qifa/
  cmd/qifa/            CLI entry point
  internal/app/        CLI dispatch
  internal/config/     YAML schema, validation, defaults
  internal/deploy/     orchestrator (Deploy, Rollback, Stop, Start, ...)
  internal/docker/     Docker shell wrappers (Local + Remote over SSH)
  internal/proxy/      kamal-proxy boot + register/deregister
  internal/registry/   per-host docker login isolation
  internal/secrets/    env file rendering
  internal/hooks/      pre/post hook execution
  internal/lock/       per-service mutex held across target hosts during deploy
  internal/state/      append-only audit log (deployments + events)
  internal/ssh/        SSH client (parallel fanout, sudo, known_hosts)
  internal/logs/       stdout/stderr formatting
  DESIGN.md
```

## CLI

```bash
qifa init [path]        # write a starter qifa.yml
qifa config             # print loaded+defaulted config as YAML
qifa deploy             # build (if needed) + ship + healthcheck + switch
qifa rollback [version] # roll back to the previous version (or a specific one)
qifa stop               # stop the running container per role/host
qifa start              # start the most recent labeled container
qifa restart            # stop then start
qifa remove             # tear down all labeled containers + deregister proxy
qifa prune              # keep last N stopped containers; prune dangling images
qifa sweep              # stop+remove orphan running labeled containers (also runs at the start of every deploy)
qifa lock status        # show the deploy-lock holder per host
qifa lock release       # forcibly clear a stale deploy lock (recovery)
qifa status             # deployment history (audit) + active containers (live)
qifa logs               # docker logs from the active container
qifa app exec <command> # docker exec in the active container
qifa app containers     # list labeled containers per role/host (rollback targets)
qifa app maintenance    # put service into maintenance mode (kamal-proxy stop)
qifa app live           # take service out of maintenance mode (kamal-proxy resume)
qifa accessory boot <name>
qifa accessory logs <name>
```

## Config

```yaml
service: myapp
image: registry.example.com/myapp     # built image: bare repo (tag computed)
                                       # external image: must include :tag or @digest

servers:
  web:
    hosts:
      - 10.0.0.11
      - 10.0.0.12
    port: 3000          # host port to publish (only used when proxy: false)
    app_port: 3000      # container port
  worker:
    hosts:
      - 10.0.0.13
    cmd: ./worker
    proxy: false        # workers don't go behind the proxy

proxy:
  host: app.example.com
  app_port: 3000
  http_port: 80
  https_port: 443
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
  secret:
    - DATABASE_URL

# Builder is OPTIONAL. Omit it entirely to deploy an externally produced
# image (image must then carry :tag or @digest).
builder:
  context: .
  dockerfile: Dockerfile
  platform: linux/amd64

prune:
  retain_containers: 5  # default; how many stopped containers to keep per role

rollout:
  batch_size: 1   # default; hosts per batch in a role's rollout (0 = all at once)
  batch_wait: 0s  # sleep between batches

ssh:
  user: ubuntu
  key: ~/.ssh/id_ed25519

hooks:
  pre_build: ./scripts/pre_build.sh
  post_deploy: ./scripts/post_deploy.sh
  pre_rollback: ./scripts/pre_rollback.sh

accessories:
  redis:
    image: redis:7
    host: 10.0.0.13
```

## Build & Distribution Model

`builder.host` encodes where (and whether) qifa builds the image:

| `builder.host`    | Behavior                                                | Registry  |
|-------------------|---------------------------------------------------------|-----------|
| (omitted block)   | External image — pull and run, never build              | optional  |
| `""`              | Build locally where qifa runs, push to registry         | required  |
| `"per_target"`    | Build on each deployment target (no shared artifact)    | forbidden |
| `"<host-or-ip>"`  | Build on that SSH-reachable host, push to registry      | required  |

`builder.repo` (when set) makes the source git: qifa clones `repo` at `ref`,
optionally descends into `subdir`, and uses that as the build context. Without
`repo`, the source is local files at `builder.context` (uploaded to the build
machine if it isn't local).

### Image Semantics

- **Built**: version is `git rev-parse --short HEAD` if available, else a
  timestamp. The full image reference is `image:version`.
- **External**: tag is informational. At deploy time qifa pulls the tag on the
  first target host, reads the registry digest, and uses the first 12 hex chars
  as the version label. Containers run by digest (`image@sha256:...`) so every
  host in a single deploy runs identical bits. Two deploys of the same floating
  tag (`:latest`, `:alpine`) either resolve to the same digest (idempotent) or
  become distinct deploys with rollback between them.

## Proxy Model

The proxy is one shared container per host, not per app.

- proxy boot creates a persistent Docker volume for runtime route state and a
  shared Docker network (default `kamal`)
- app containers attach to the same network so the proxy can reach them by IP
- app deploys register/update their route in the shared proxy via
  `docker exec kamal-proxy kamal-proxy deploy ...`
- if the proxy container is restarted, routes survive as long as the volume is
  preserved
- if the proxy volume is deleted, routes must be replayed by redeploying

The root `proxy` block configures both the proxy container itself (image,
network, ports, state volume) and per-app routing (host, healthcheck, TLS,
path prefixes).

## Discovery & State Model

**Docker is the source of truth.** App containers are stamped with three
labels:

```text
qifa.service=<service>
qifa.role=<role>
qifa.version=<version>
```

All runtime queries (which container is active for service+role+host, what was
the previous version, what's stale) are answered by `docker ps` filtered by
those labels. There is no on-disk index of what is running.

**`.qifa/state.jsonl`** is an append-only audit log only. It records:

```text
deployments     id, service, version, image, status, started_at, finished_at
deployment_events  id, deployment_id, host, role, event_type, message, created_at
```

`qifa status` reads the audit log for history (including failures that never
produced a container, e.g. build/push errors) and queries Docker live for the
active set. Removing or losing the file does not break any other command.

## Lifecycle

### Deploy Flow

1. Resolve image and version
   - Built: `image:resolveVersion()`
   - External: pull on first host → read digest → `image@sha256:...`, version = short digest
2. Run `pre_build` hook
3. Sweep stale containers across all role/host pairs (stop+remove any running
   labeled container that isn't the most-recently-created one — defends
   against half-finished prior deploys)
4. Prepare image
   - `builder == nil` or `per_target`: no-op (per-target builds happen on each host)
   - `local`: build locally + push
   - `remote`: build on `builder.host` + push
5. Render env file (clear + secret env vars)
6. For each role (web first), batch the role's hosts according to `rollout.batch_size`
   (default 1 = strict rolling; 0 = all hosts in one batch). Hosts within a batch
   run in parallel; batches run sequentially with `rollout.batch_wait` between them.
   On any host failure, remaining batches are skipped and the deploy errors out.
   For each host in the current batch:
   1. `connecting` event
   2. Ensure Docker is running on the host
   3. If proxy is used: ensure shared kamal-proxy container, network, volume
   4. Find currently running container by labels (= previous version)
   5. Upload env file
   6. Build on host (if `per_target`) OR pull (if registry / external)
   7. For non-proxy: stop the previous container in place (port collision)
   8. Remove any same-named container (rollback case)
   9. `docker run` the new container with `--network <proxy.network>` (if proxy),
      `--restart unless-stopped`, and the three qifa labels
   10. Health-check (curl from host into the container)
   11. If proxy: `kamal-proxy deploy <service> --target <ip:port>` (atomic
       traffic switch performed by kamal-proxy)
   12. `deployed` event
   13. For proxy: stop (don't remove) the previous container so rollback can
       find it later
7. Run `post_deploy` hook
8. Auto-prune: keep last `prune.retain_containers` stopped containers per role,
   prune dangling service images

### Rollback Flow

1. Run `pre_rollback` hook
2. Resolve target version:
   - `qifa rollback` (no arg): walk labeled containers across all role/host
     pairs, skip the currently running version, pick the most recent
     next-newest container with a different version.
   - `qifa rollback <version>`: verify a labeled container with that exact
     version exists on every role/host; reuse its image. Errors if any host
     is missing it.
3. Re-run the deploy flow with that version's image.

Stopped labeled containers from prior deploys are preserved as rollback
candidates (subject to `prune.retain_containers`); only the auto-prune step
removes them, never the per-deploy cleanup.

### Stop / Start / Restart

- **Stop**: find the running container per role/host, `docker stop`. Old
  container is left in place; proxy route is left registered (will 503 until
  Start or Remove).
- **Start**: find the most recently created labeled container per role/host,
  `docker start`. If the role uses the proxy, re-register with kamal-proxy.
- **Restart**: Stop then Start.

### Remove

For each host: deregister all labeled services from kamal-proxy, then
`docker rm -f` every labeled container (running or stopped), then prune
dangling service images.

### Prune

For each role/host: list labeled containers, drop the running ones, keep the
most recent N stopped containers, `docker rm -f` the rest. Then
`docker image prune --force --filter label=qifa.service=<service>`.

## Deployment State Machine

```text
Pending → Building → Pushing → Pulling → Starting → HealthChecking
       → SwitchingTraffic → Succeeded
                         → Failed
                         → RolledBack
```

External-image deploys skip Building/Pushing. Per-target builds skip Pushing.
Each step is idempotent.

## Hooks

```yaml
hooks:
  pre_build: ./scripts/pre_build.sh   # before resolveImage / prepareImage
  post_deploy: ./scripts/post_deploy.sh # after success, before auto-prune
  pre_rollback: ./scripts/pre_rollback.sh # at start of rollback
```

Hooks receive `QIFA_VERSION` in their environment and run on the machine
invoking qifa, not on target hosts.

## Secrets

MVP: read from local environment (`env.secret: [VAR_NAME]`), render to a
`.env` file, copy to each host with mode 0600, pass to docker via
`--env-file`. No secret manager integration yet.

## Accessories

```yaml
accessories:
  redis:
    image: redis:7
    host: 10.0.0.13
```

`qifa accessory boot redis` pulls the image, removes any prior container with
the same name, and runs it with `--restart unless-stopped`. Accessories do not
get qifa labels and are not affected by `prune` or `remove`.

## SSH Layer

`golang.org/x/crypto/ssh` with parallel fanout, per-host timeout, streaming
logs, sudo support, and known_hosts verification.

## Docker Layer

Shells out to `docker` over SSH on remote hosts and locally for the local
build path. Methods used today:

```bash
docker build / push / pull / run / stop / rm / start / ps / inspect / logs / exec / image prune
```

## Proxy Integration

```go
type Proxy interface {
    EnsureInstalled(ctx, host) error           // boot kamal-proxy container
    Deploy(ctx, host, target) error            // register service route
    Remove(ctx, host, service) error           // deregister service
}
```

Implementation shells out: `docker run -d ... basecamp/kamal-proxy` for boot,
`docker exec kamal-proxy kamal-proxy deploy ...` for register,
`docker exec kamal-proxy kamal-proxy remove <service>` for deregister.

## Deploy Lock

Deploy and Rollback acquire a per-service mutex held across every target host.
The lock is a directory at `/tmp/qifa-lock-<service>` on each host, created
via `mkdir` (atomic). The directory contains a `holder.json` with the user,
host, service, version, and acquisition timestamp. The lock is released via
`defer` so it always runs (success, failure, or panic).

If the lock is held when another deploy tries to start, the new deploy errors
out with the existing holder's metadata. Stale locks (from a deploy that
crashed without unwinding `defer`) can be cleared with `qifa lock release`.

## Maintenance Mode

`qifa app maintenance [--message <msg>] [--drain-timeout <duration>]` invokes
`kamal-proxy stop <service>` on every host where the proxy is running. The
proxy returns the configured message for incoming requests (default 503) and
drains in-flight requests over `drain-timeout`. Note: requests to the
configured `proxy.healthcheck.path` still return 200 OK during maintenance —
this is kamal-proxy behavior to keep downstream load balancers from dropping
the host while it's intentionally offline.

`qifa app live` invokes `kamal-proxy resume <service>` to come back online.

## Out of Scope (For Now)

- Primary-role healthcheck barrier for multi-role apps (qifa serializes
  roles strictly, so the barrier kamal needs for parallel-role booting
  doesn't apply here)
- Maintenance mode / explicit traffic on/off
- Deploy locks (multiple deployers racing)
- Multi-arch builds
- Secret managers (SOPS, Vault, AWS SM, 1Password)
- OpenTelemetry / web UI
- GitHub Actions integration

## Positioning

A Go-native, Kamal-inspired deployer for Docker apps on plain Linux servers
that uses kamal-proxy for zero-downtime traffic switching, Docker labels as
the source of truth for what's running, and an append-only audit log for
human debugging.
