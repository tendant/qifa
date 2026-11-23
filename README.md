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

No Docker on the server? qifa can also deploy a binary or directory as a
systemd service: see [Native deploys](docs/native.md).

## Install

The current release is **v0.7.0** ([CHANGELOG](CHANGELOG.md)).

Each release from v0.6.0 on publishes a prebuilt binary for Linux and macOS
(amd64 and arm64), and one line installs it. You need no Go, only
`curl` or `wget`:

```bash
curl -fsSL https://raw.githubusercontent.com/tendant/qifa/main/install.sh | sh
```

[`install.sh`](install.sh) checks the binary against the release's
`SHA256SUMS` before installing it. It puts it in `/usr/local/bin` if you can
write there, otherwise in `~/.local/bin`, and never uses sudo. Set
`QIFA_VERSION=v0.7.0` to pin a release, `QIFA_INSTALL_DIR` to choose the
directory, or `QIFA_DOWNLOAD_URL` to install from a mirror that holds the
release's files. You can also download a binary from the
[releases page](https://github.com/tendant/qifa/releases) and put it on
`$PATH` yourself.

Binaries from v0.7.0 on carry a signed build-provenance
attestation: `gh attestation verify qifa-linux-amd64 --repo tendant/qifa`
checks that a binary was built by this repository's release workflow.

To upgrade, run the same line again: it replaces the binary with the latest
release (or `QIFA_VERSION`'s), checked the same way.

With Go 1.27.1 or newer (an older `go` fetches the right toolchain itself
unless `GOTOOLCHAIN=local`), this works for every release, including ones before v0.6.0:

```bash
go install github.com/tendant/qifa/cmd/qifa@v0.7.0
qifa version
```

Or from a checkout, which stamps the version from git:

```bash
git clone https://github.com/tendant/qifa.git    # mirror: https://gitee.com/tendant/qifa.git
cd qifa
make install        # or: make build, for ./qifa
```

The binary is statically linkable (no cgo) and self-contained. Drop it
anywhere on `$PATH`. Install the same version on every machine that deploys:
configs written for one version may be rejected by an older one.

## Quickstart

```bash
qifa init       # writes a starter qifa.yaml that deploys as written
qifa deploy     # runs it on this machine: open http://localhost:8765
qifa config     # after editing: validate and show the loaded config
qifa logs -f    # tail the running container
qifa rollback   # roll back to the previous version
qifa help       # every command; qifa <command> --help for one
```

The starter deploys a public image to this machine (`hosts: [local]`), with
no SSH, registry or proxy, so the first deploy works before you change
anything. Building your own image, env and secrets, a domain with HTTPS, and
remote hosts are in it as commented blocks. `qifa init` will not replace an
existing file; pass `--force` to do that.

`qifa init` also shapes the starter to your project:

- **A `Dockerfile` next to it:** the starter builds that instead of pulling
  nginx. qifa uploads the directory to each host and builds there, with no
  registry. The app port comes from the Dockerfile's last `EXPOSE`. Set
  `healthcheck.path` to a route your app answers with 2xx (it starts as `/`).
- **`--host 10.0.0.11`:** deploy there over SSH instead of to this machine
  (`deploy@host:2222` works too). The host needs Docker; `qifa doctor` checks.
- **`--domain app.example.com`:** serve the app at that name over HTTPS
  through kamal-proxy, which the first deploy starts and which gets a Let's
  Encrypt certificate. Point the name's DNS at the host and open ports 80 and
  443 first.

The service is named after the directory. So in a project with a Dockerfile:

```bash
qifa init --host 10.0.0.11 --domain app.example.com
qifa deploy
```

Starting from a new VPS? [docs/vps.md](docs/vps.md) goes from a fresh
server to the app over HTTPS: Docker, a deploy user, DNS and the firewall.

## Three Common Configs

### 1. Build on each target (no registry)

Simplest setup: code lives where you run qifa, qifa uploads it to each host
and builds the image there. No registry needed. What `.dockerignore`
excludes is left out of the upload, so a `.env` or `.git` listed there
never reaches the hosts.

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

# Per-app routing — set on every deploy. The first deploy to a host starts
# kamal-proxy there (ports 80 and 443) if it is not running.
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

There is no separate proxy step. A deploy that finds no kamal-proxy on a host
it routes to boots one there, exactly as `qifa proxy boot` would, and leaves a
running one alone. It does this only when `proxy:` names a host. A role that
is proxied only because it is called `web` still gets an error saying to run
`qifa proxy boot`, so qifa never takes ports 80 and 443 unasked.

`proxy_boot:` is optional. It sets how the proxy runs (`http_port`,
`https_port`, `bind_ips`, image and version). Its `hosts` default to the
hosts of the proxied roles, which is where `qifa proxy boot`, `upgrade`,
`logs` and the rest act when it is left out. To upgrade the proxy: `qifa
proxy upgrade` (state volume preserved, routes survive).

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
```

`local` is opt-in by exact name. `localhost` and `127.0.0.1` still mean SSH,
because targeting a local SSH daemon (a VM, a container, the e2e harness on
`127.0.0.1:2222`) is a different and legitimate thing. Hosts can be mixed:
`hosts: [local, 10.0.0.12]` deploys to this machine and a remote one in the
same roll-out.

Two caveats:

- The commands still run through `/bin/sh`, so the host needs `docker` and
  `curl` on `PATH` — the same requirements a remote target has.
- On macOS (Docker Desktop, Colima, OrbStack), Docker runs in a Linux VM, so
  the host cannot reach container IPs. A container checked by its IP (a
  proxied role, or one with no published port) is checked from a throwaway
  container on its docker network instead. That container uses the app's own
  image, so no pull is needed, or `busybox` (pulled once) if that image has no
  `sh` and no curl or wget. Linux hosts check from the host, as before.

### 5. An internal-only service

A service other services use and nobody else can reach: no hostname, no
certificate, no published host port. Give the role a name other than `web` (or
set `proxy: false`) and leave `port:` out — `app_port` alone means the port is
open on the docker network and nowhere else.

```yaml
service: dolico
image: reg.example.com/dolico-api:f21aa24

servers:
  api:
    hosts: [10.0.0.11]
    app_port: 8080      # no port: -> nothing published on the host

healthcheck:
  path: /healthz        # read even with no proxy route registered

accessories:
  ocr:
    image: reg.example.com/dolico-ocr:f21aa24
    host: 10.0.0.11
```

Every app container joins `proxy_boot.network` (default `kamal`) whether or not
it is proxied, so this one can reach `dolico-accessory-ocr:8181` and its peers
can reach it. Peers address it as **`<service>-<role>`** — `dolico-api:8080`
here — which is a docker network alias, not the container name: the container
name carries the deployed version and changes under them on every deploy, and
the alias does not. Only non-proxied roles get one, because a proxied role runs
its old and new containers side by side during a rollout and a shared alias
would resolve to either.

A role with no published port is healthchecked by container IP, exactly as
the proxied path is. On macOS, that check runs inside the docker network (see
the caveats above).

Nothing here registers a route with kamal-proxy, which is the point: a route
would make the service answer to anyone who sends that `Host:` header to the
proxy's public port.

### GPUs

`gpus:` gives a role's containers, or an accessory, GPUs, passed to
`docker run --gpus`: `all`, a count (`"1"`), or specific devices
(`"device=0"`, `"device=0,1"`, or GPU UUIDs from `nvidia-smi -L`). The host
needs the NVIDIA Container Toolkit. A model server beside an app:

```yaml
accessories:
  llm:
    image: vllm/vllm-openai:v0.8.0
    host: 10.0.0.12
    gpus: all
    port: 8000
    app_port: 8000
    volumes:
      - /data/hf-cache:/root/.cache/huggingface
    cmd: --model Qwen/Qwen2.5-7B-Instruct
```

The value is checked when the config loads. Native mode rejects it: a native
app reaches the host's GPUs directly. An accessory picks up a changed `gpus`
on its next `qifa accessory boot`, as with its other settings.

### Beside Docker Compose

qifa and Compose can run on the same host without stepping on each other.
Every qifa command finds its containers by their `qifa.*` labels, so `sweep`,
`prune` and `remove` never touch a container Compose or a plain `docker run`
started, and a deploy that needs no proxy never starts one. An app with no
`proxy:` block and only non-proxied roles needs nothing on the host but
Docker.

To reach services Compose runs, such as a database, join the Compose
project's network with `networks:`, on a role or an accessory:

```yaml
service: shop
image: reg.example.com/shop:1.4

servers:
  web:
    hosts: [10.0.0.11]
    port: 8080
    app_port: 3000
    proxy: false
    networks: [shop_default]   # Compose's network for project "shop"

env:
  clear:
    DATABASE_URL: postgres://app@db:5432/shop   # "db" is the Compose service
```

The container stays on the estate network (`kamal`) as well, so it still
reaches its accessories. On every network it joins it answers to the same
`<service>-<role>` alias (`shop-web` here), so Compose services can call it
by that name. Accessories are reached by their container name.

qifa never creates or removes these networks. They belong to Compose, and
Compose refuses to adopt a network it did not create. A deploy or
`accessory boot` checks for them first and stops, before touching the
running container, if one is missing. Bring the project up (`docker compose
up -d`) before the first deploy. `qifa doctor` checks them too. `docker
compose down` leaves the network in place while a qifa container still uses
it ("Resource is still in use"). That is expected: the app keeps running, and
the next `compose up` reuses the network.

The other direction also works without `networks:`. A Compose service can join
qifa's network as an external one and reach `shop-web` there:

```yaml
# compose.yaml
services:
  worker:
    image: shop-worker
    networks: [default, kamal]
networks:
  kamal:
    external: true
```

The built-in `bridge`, `host` and `none` networks are refused. A changed
`networks:` list takes effect on the next deploy, or the accessory's next
boot.

### 6. Without Docker: a systemd service

A static binary run by systemd: no image, registry or proxy on the server.
`qifa setup --ssh-user root` once, then `qifa deploy` as usual.

```yaml
service: myapp

servers:
  web:
    hosts: [10.0.0.11]
    cmd: [myapp, --port, "8080"]
    app_port: 8080

healthcheck:
  path: /up

ssh:
  user: deploy

native:
  build: GOOS=linux GOARCH=amd64 go build -o dist/myapp ./cmd/myapp
  artifact: dist/myapp
  user: myapp
```

Releases, rollback, status and env work as they do for containers. For HTTPS
and domains, kamal-proxy runs natively too (`qifa proxy boot --ssh-user root`,
then a `proxy:` block). Deploys restart the process; there is no
zero-downtime switch. On a Mac, `native.init: launchd` runs the app as a
LaunchAgent instead, with no root needed. See
[docs/native.md](docs/native.md).

### Keys shared by both modes

These settings mean the same thing for containers and native apps, and live
in one place. The older keys still load, with a warning naming the new one;
setting both is an error.

| Setting | Key | Replaced |
| --- | --- | --- |
| Health check | `healthcheck: {path, port, interval, timeout, deploy_timeout}` | `proxy.healthcheck`, `native.healthcheck` |
| App port | `servers.<role>.app_port` (or `proxy.app_port` for the routed role) | `native.port` |
| Versions kept | `prune.retain` | `prune.retain_containers`, `native.retain` |
| Command | `servers.<role>.cmd`: a string or a list | `servers.<role>.exec`, `native.exec` |

`healthcheck.timeout` bounds one attempt (default 5s) and
`healthcheck.deploy_timeout` the whole wait for a new container or release
(default: room for five attempts, 5 × (timeout + interval), 35s with the
defaults); with a proxy, `deploy_timeout` is also kamal-proxy's unless
`proxy.deploy_timeout` overrides it. A native `cmd` given as a string is split
into words without a shell; one that needs a shell (`$`, pipes, globs) is
refused, so write it as a list.

## Verb Cheatsheet

```text
qifa help [command]              # all commands, or one; also <command> --help
qifa init [path] [--force]       # write a starter qifa.yaml (never overwrites without --force)
qifa version                     # build version + commit
qifa config                      # validate (unknown keys are errors), print loaded+defaulted config

qifa deploy [--dry-run]          # build, ship, switch, prune
  [--version <v>] [--allow-dirty] # pin the version instead of deriving it from git
qifa rollback [version]          # auto = previous; or explicit version
qifa stop                        # docker stop the running container per role/host
qifa start                       # docker start the most recent labeled container
qifa restart                     # stop then start (re-registers with proxy)
qifa remove [--purge]            # full teardown + deregister proxy; --purge: env files (Docker), releases+data (native)

qifa prune                       # keep last N stopped (config: prune.retain)
qifa sweep                       # remove orphan running containers (also runs at deploy start)
qifa env diff                    # compare config env vs. what the running containers got
qifa lock <status|release>       # show or forcibly clear deploy lock
qifa proxy <boot|start|stop|restart|upgrade|remove [--purge]|logs|details>
                                 # manage the shared kamal-proxy container
qifa status                      # deployment history + active containers
qifa reconcile [--dry-run]       # converge only if the host differs from the config;
                                 # a no-op when it already matches, so it is safe
                                 # to run unattended on a schedule
qifa doctor                      # preflight every host: ssh, docker, registry,
                                 # DNS/TCP/TLS, credentials, disk, clock, proxy

qifa logs [--follow] [--lines N] # docker logs from the active container
qifa app exec <command>          # docker exec in the active container
qifa app containers              # list labeled containers per role/host
qifa app maintenance [--message <msg>] [--drain-timeout <duration>]
qifa app live                    # leave maintenance mode

qifa accessory <boot|stop|start|restart|remove|logs|exec> <name> [args]
qifa setup --ssh-user root       # once per host: docker group (Docker), or account/units/sudoers (native)
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
`prune.retain`, default 5). `qifa rollback <version>` re-runs the
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

Accessories take `env` (cleartext) and their own `secret_command`, merged the
same way, so a database password stays out of the config:

```yaml
accessories:
  postgres:
    image: postgres:16
    host: 10.0.0.11
    env:
      POSTGRES_USER: app
    secret_command: sops --decrypt postgres.sops.env   # POSTGRES_PASSWORD=...
```

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

From a fresh server to HTTPS: [docs/vps.md](docs/vps.md).
Common issues and fixes: [docs/troubleshooting.md](docs/troubleshooting.md).
Team and multi-machine setup: [docs/deploying.md](docs/deploying.md).
Deploying without Docker: [docs/native.md](docs/native.md).
Building and testing qifa: [CONTRIBUTING.md](CONTRIBUTING.md).

## Status

Active development; current release v0.7.0, changes in
[CHANGELOG.md](CHANGELOG.md). The CLI is reasonably stable; the schema may
still change before a 1.0 tag, and renamed keys keep loading with a warning
when it does.

## License

MIT — see [LICENSE](LICENSE).
