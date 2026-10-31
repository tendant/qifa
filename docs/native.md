# Native deploys (no Docker)

qifa can deploy an app as a plain process supervised by systemd, without
Docker on the server. Add a `native:` block to the config and qifa builds your
artifact locally, uploads it over SSH into a release directory, points
`current` at it, restarts the systemd unit and checks its health. If the check
fails, qifa puts the previous release back.

Use it for a single static binary (Go, Rust, Zig), or a directory with its
runtime already on the server (a JVM jar, a Python venv you build for the
target). There is no image or registry. For HTTPS on a domain, qifa can run
kamal-proxy as a systemd service in front of the app ([below](#https-and-domains-kamal-proxy)).
There is no zero-downtime switch yet: a deploy restarts the process, and the
app is down for as long as it takes to start.

## Requirements on the server

- Linux with systemd, and `dbus` (as a non-root user, `systemctl` talks to
  systemd over D-Bus; minimal images sometimes leave it out)
- `sudo`, `tar`, `sha256sum`, and `curl` or `wget` for the HTTP health check
- the shared `deploy` user from [deploying.md](deploying.md); it does **not**
  need the `docker` group for native apps

## Config

```yaml
service: myapp

servers:
  web:
    hosts: [10.0.0.11]
  worker:                        # optional: more roles from the same artifact
    hosts: [10.0.0.11]
    exec: [bin/myapp, work]

ssh:
  user: deploy

env:
  clear:
    APP_ENV: production
  secret_command: sops --decrypt secrets.production.enc.env

native:
  build: GOOS=linux GOARCH=amd64 go build -trimpath -o dist/bin/myapp ./cmd/myapp
  artifact: dist                 # a file or a directory
  exec: [bin/myapp, --port, "8080"]
  user: myapp                    # the account the app runs as
  arch: amd64                    # optional: checked against the binary and each host
  port: 8080
  healthcheck:
    path: /up                    # without it, the check is "the unit stays active"
    timeout: 30s
  shared_dirs: [storage]         # persistent across releases
  retain: 5
```

| Key | Default | Meaning |
| --- | --- | --- |
| `build` | none | Shell command run on your machine, in the config's directory, before upload. `QIFA_VERSION` is set. |
| `artifact` | required | What to ship. A directory's contents become the release root; a single file lands at the root under its own name. |
| `exec` | required | The command as a list: no shell, no quoting surprises. `exec[0]` is relative to the release unless absolute. A role's `exec` overrides it. |
| `user` | required | Service account, created by `qifa native setup`. Not root. |
| `arch` | none | `amd64`, `arm64`, `arm`, `386` or `riscv64`. ELF binaries are checked before upload, so a Mac-built arm64 binary is caught before it reaches an amd64 server. |
| `port`, `healthcheck` | interval 2s, timeout 30s | HTTP check on `127.0.0.1:<port><path>`, for the `web` role (or the only role). Other roles are checked for staying active. |
| `shared_dirs` | none | Directories under `shared/` linked into every release, for data that must outlive it. The only place the app can write besides `/tmp`. |
| `dir` | `/opt/qifa/<service>` | Install root on the server. |
| `retain` | 5 | Releases kept per host. The live release and the rollback target are always kept. |
| `restart` | `on-failure` | systemd `Restart=`. |

Container settings (`image`, `builder`, `registry`, `proxy`, `proxy_boot`,
`accessories`, `volumes`, `port`/`app_port` on roles, ...) are rejected next to `native:`,
because a config that sets them was written for the other mode.

## First time: `qifa native setup`

Setup is the only step that needs root. Run it as a user with passwordless
sudo, or as root:

```sh
qifa native setup --ssh-user root
```

On every host it creates the service account, `/opt/qifa/<service>`, the
systemd unit (`qifa-<service>.service`, or one per role), and a sudoers rule
for the deploy user. It is safe to re-run; it prints only what it changed.

The sudoers rule allows exactly `systemctl start|stop|restart` of this
service's units, and nothing else. The deploy user cannot write unit files,
reload systemd, or run anything else as root. A unit file can say
`User=root`, so the ability to write one would be root access.

Re-run setup whenever `exec`, `user`, `restart` or the roles change. Deploy
compares the unit on each host with the config and refuses, naming the unit,
until you do.

## Deploying

```sh
qifa deploy -n                   # print the plan
qifa deploy                      # build, ship, switch, restart, check
```

Before changing any host, deploy checks every host: the install directory,
the sudo rule, that the unit matches, the architecture, and D-Bus. A problem
on the third host never leaves the first two upgraded. Then, per host
(in `rollout.batch_size` batches, as with Docker):

1. upload the artifact and verify its sha256
2. unpack it to `releases/<version>`, link `shared_dirs`, make it read-only
3. write the env file, point `current` at the new release
4. `systemctl restart` each unit and wait for the health check
5. on success, mark the release healthy and prune old releases; on failure,
   put back the previous release *and its env*, restart, and report the
   failure with the unit's journal attached

The version is the same as in Docker mode: `--version`, `QIFA_VERSION`, or the
short git SHA, refusing a dirty checkout. Redeploying a version whose release is
already on the host reuses it when the bytes are identical. A version that is
live with *different* bytes is refused. Ship that build under a new
`--version`.

The env file is rendered for systemd (`EnvironmentFile=`), which parses quotes
and backslashes differently from Docker's `--env-file`. Values arrive
byte-for-byte, newlines included. The file is readable by systemd but not by
the app's own account. As in Docker mode, only `qifa deploy` writes it.
Unlike a container, `qifa restart` re-reads it, but since nothing else writes
it, the effect is the same.

## HTTPS and domains: kamal-proxy

kamal-proxy, the proxy qifa uses for Docker apps, also runs as a plain systemd
service, so the server still needs no Docker. Give the app a `proxy:` block:

```yaml
native:
  # ...as above; port and healthcheck.path are required with a proxy
  port: 8080
  healthcheck:
    path: /up

proxy:
  host: app.example.com          # or hosts: [...], path_prefixes: [...]
  ssl: true
  tls:
    source: kamal                # automatic Let's Encrypt certificates
    # source: static
    # cert_path: /etc/qifa-proxy-certs/app.example.com.crt
    # key_path:  /etc/qifa-proxy-certs/app.example.com.key

proxy_boot:
  hosts: [10.0.0.11]             # default: the web role's hosts
  # version: v0.10.2             # default; v0.10 or later is required
  # binary: ./bin/kamal-proxy    # instead of building it with Go
```

Once per server, as root:

```sh
qifa proxy boot --ssh-user root
```

kamal-proxy publishes no binaries, so qifa builds the pinned version from
source for the server's architecture (this needs Go where you run qifa; the
build is cached). Set `proxy_boot.binary` to ship one you built instead. Boot
installs `qifa-proxy.service`, which runs as its own `qifa-proxy` account and
binds ports 80 and 443 through a capability, not root. It also adds the deploy
user to the `qifa-proxy` group, which is what lets deploys register routes.
`qifa proxy upgrade` does the same after a version change, and restarts the
proxy only when something changed.

From then on, every deploy registers the app's route once the new release
passes its health check: kamal-proxy forwards `app.example.com` to
`127.0.0.1:8080`. The app should listen on `127.0.0.1` only; nothing outside
the server needs to reach its port. A deploy is refused while the proxy is not
running.

```sh
qifa app maintenance --message "Back soon"   # the proxy answers 503
qifa app live
qifa proxy details                           # routes on each proxy host
qifa proxy logs -f
qifa proxy restart                           # routes survive restarts
qifa proxy remove --ssh-user root [--purge]  # --purge also deletes routes and certificates
```

Static certificate files must be readable by the `qifa-proxy` account, for
example `root:qifa-proxy`, mode 0640. Certificates managed by qifa itself
(`tls.source: qifa`) need Docker and are not available here.

A server runs one proxy or the other: `proxy boot` refuses while the
kamal-proxy container is running there.

## Day to day

```sh
qifa status            # per host: live release, unit, health, releases, rollback target
qifa logs -f           # journalctl for the units (first host)
qifa restart           # also stop / start
qifa rollback          # the most recently healthy release that is not live
qifa rollback v1.4.2   # or a named one, present on every host
qifa env diff          # config env vs. the host's env file, key names only
qifa reconcile -n      # what differs from the config; without -n, deploy if anything does
qifa doctor            # the deploy preflight, as a report
qifa lock status       # same lock as Docker mode
```

`qifa status` marks releases that passed a health check with `*`. It also
warns when the live release never did, for example when a deploy was killed
between switching `current` and finishing its check. `qifa deploy` or
`qifa rollback` repairs that.

Docker-only commands (`cert`, `app exec`, `accessory`, `prune`,
`sweep`, `backup`, `restore`, `sync`) say so and do nothing.

## Removing

```sh
qifa native remove --ssh-user root            # stop and delete units and sudoers; keep data
qifa native remove --ssh-user root --purge    # also delete releases, shared/ and the account
```

`--purge` asks you to type the service name (`--yes` skips the prompt, for
scripts). Plain `qifa remove` only explains this, since it needs root.

## On the server

```text
/opt/qifa/myapp/
├── releases/
│   ├── a1b2c3d/          read-only to the app; .qifa-ok once it passed
│   └── e4f5a6b/
├── current -> releases/e4f5a6b
├── etc/env               0600, deploy user; read by systemd, not the app
└── shared/storage/       owned by the app; never pruned
/etc/systemd/system/qifa-myapp.service
/etc/sudoers.d/qifa-myapp

# with the proxy
/opt/qifa/proxy/bin/kamal-proxy
/var/lib/qifa-proxy/               routes and certificates
/run/qifa-proxy/kamal-proxy.sock   group qifa-proxy
/etc/systemd/system/qifa-proxy.service
/etc/sudoers.d/qifa-proxy          start/stop/restart of qifa-proxy.service
```

The server is the source of truth, as Docker labels are in Docker mode, so any
checkout on any machine can deploy, roll back, or ask for status.

## Next to Docker apps

The mode is per config file, so one server can run Docker apps and native apps
side by side. They share only the lock directory naming
(`/tmp/qifa-lock-<service>`), so **service names must be unique per host
across both modes**; `native setup` warns when a host already has containers
for the same service name. If kamal-proxy runs on the server, it owns ports
80 and 443, so give native apps other ports. One service cannot mix modes
between roles: split it into two services. The same goes for accessories: a
database container for a native app is a separate Docker config on that host.

Design notes: [qifa-native-deployment-design.md](qifa-native-deployment-design.md).
