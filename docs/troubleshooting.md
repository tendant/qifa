# Troubleshooting

## Start here: `qifa doctor`

Before debugging a failed deploy by hand, run:

```sh
qifa doctor
```

It checks every host qifa will touch (app, proxy, accessory, builder) and
prints one line per check: SSH, the docker CLI and daemon, DNS/TCP/HTTPS to
the registry, whether the image's manifest is actually reachable *and
authorized* with your credentials, the daemon's proxy settings, registry
mirrors and insecure registries, free space under the docker root, clock
skew, and whether kamal-proxy is running. It exits non-zero if anything
failed. Every check is read-only.

The same host report is attached automatically to a failed pull, so you
usually do not need to run `doctor` separately — it is there for checking a
new host before the first deploy, or after changing a network.

## `docker pull` fails on the remote host

qifa retries transient network faults (DNS, TLS handshake timeout, connection
reset, rate limits) before giving up, and the final error names the cause and
what to check. The knobs:

| variable | default | meaning |
| --- | --- | --- |
| `QIFA_PULL_RETRIES` | `2` | extra attempts after the first for pull/push |
| `QIFA_PULL_RETRY_WAIT` | `5s` | wait between attempts |
| `QIFA_PULL_STALL_WARN` | `45s` | warn when a pull/build prints nothing for this long |

Set `QIFA_PULL_RETRIES=0` to fail fast, or raise it on a genuinely flaky link.

If the host already has the image, qifa does not fetch it again — that is the
default:

```yaml
pull_policy: missing   # default; "always" re-pulls every deploy
```

So a redeploy of an unchanged image never touches the registry, and an
unreachable registry cannot fail it. Changing the tag or digest in the config
still pulls, because the host does not have that one.

The cost is that a moving tag stops being re-resolved: with `image: app:latest`
and `pull_policy: missing`, the host keeps whatever `latest` meant when it
first pulled. Images deliberately tracked by a moving tag want
`pull_policy: always`.

What the diagnosed causes mean:

- **DNS failure** — the host has no resolver for the registry name. Private
  registries usually need an internal DNS server or an `/etc/hosts` entry.
  Docker Hub needs `registry-1.docker.io`, `auth.docker.io` **and**
  `production.cloudflare.docker.com` to resolve, not just `docker.io`.
- **The Docker daemon's HTTP proxy is unreachable** (`proxyconnect tcp`) —
  the *daemon* reads `HTTP_PROXY`/`HTTPS_PROXY` from its systemd unit, not
  from your shell or from `/etc/environment`. Exporting a proxy in the SSH
  session changes nothing:

  ```sh
  systemctl show docker --property=Environment
  cat /etc/systemd/system/docker.service.d/*.conf
  # after editing:
  systemctl daemon-reload && systemctl restart docker
  ```

- **TLS handshake timeout / connection reset mid-transfer** — usually a path
  problem rather than a registry problem. If large layers consistently die at
  a similar point on a VPN or overlay network, suspect MTU:
  `ping -M do -s 1400 <registry>` and lower the interface MTU if it fails.
- **The host does not trust the registry's TLS certificate** — install the CA
  at `/etc/docker/certs.d/<registry>/ca.crt` and restart docker. If the
  message says "not yet valid", check the `clock` line in the diagnostics
  first: a host whose clock is days off fails every registry handshake.
- **Unauthorized** — qifa uploads a docker config built from
  `registry.username` + `registry.password_env` to the host; it does not use
  the host's own `~/.docker/config.json`. The password env var must be
  exported **where qifa runs**. `qifa doctor` reports this explicitly.
- **`manifest unknown` / `not found`** — wrong tag, or a private repository
  with no credentials: the registry returns the same answer for both.
- **`toomanyrequests`** — anonymous Docker Hub pulls are rate-limited per
  source IP. Authenticate, or mirror the image into a private registry.
- **`no space left on device`** — free space under the docker root
  (`docker system prune -af --volumes`); the `disk` check fails below 2G.
- **`no matching manifest for linux/...`** — the image has no build for the
  host's architecture. Set `builder.platform: linux/amd64`, or build a
  manifest list with `builder.platform: "linux/amd64,linux/arm64"`.

## `qifa proxy boot` hangs or fails with just an exit status

Booting the proxy runs `docker run basecamp/kamal-proxy`, which pulls the
image if it is missing — buffered, so a stalled 200 MB pull looked like a
hang. qifa now pulls the proxy image explicitly first (live progress,
retries, diagnosed errors), and if the container starts but does not stay
running, the boot failure includes its state and last 30 log lines instead of
`exit status 1`.

## Deploying to the machine qifa runs on

Set the host to the reserved name `local`:

```yaml
servers:
  web:
    hosts: [local]
```

Commands then run through `/bin/sh` on this machine — no sshd, key, or
`known_hosts` entry needed, which also sidesteps every SSH problem below.
`localhost` and `127.0.0.1` still mean SSH by design; only the exact string
`local` switches transport.

On macOS, Docker Desktop keeps containers in a Linux VM whose bridge IPs the
host cannot reach, so the kamal-proxy path (which healthchecks a container by
IP) will not work. Use `proxy: false` with a published `port:` there.

## `knownhosts: key mismatch` during SSH

qifa's Go SSH client (`golang.org/x/crypto/ssh/knownhosts`) treats any
non-matching entry for a target host in `~/.ssh/known_hosts` as a hard
failure. OpenSSH (and most shell-level SSH probes) is more forgiving —
it will accept a match if *any* entry for the host agrees with the
presented key, even if there are stale entries alongside. The result is
that an `ssh user@host` from your shell can succeed while
`qifa proxy boot` (or `deploy`) fails with:

```
error: boot proxy on <host>: ssh: handshake failed: knownhosts: key mismatch
```

Common causes:

- the host's sshd key was regenerated since the entry was written
- the IP-based variant (`127.0.0.1`, `::1`, or the host's real IP) has a
  *different* key from the hostname entry
- `QIFA_HOST=localhost` is used on a system where `localhost`,
  `127.0.0.1`, and `::1` all carry entries from different prior installs

### Fix: refresh `~/.ssh/known_hosts`

Remove the stale entries and re-scan the live ones:

```sh
# Replace with your QIFA_HOST and any IPs it resolves to.
ssh-keygen -R "$HOST"
ssh-keygen -R 127.0.0.1   # only if HOST is localhost
ssh-keygen -R ::1         # only if HOST is localhost

ssh-keyscan -H "$HOST" 127.0.0.1 ::1 >> ~/.ssh/known_hosts
```

Then re-run `qifa proxy boot` (or the deploy that was failing).

### Workaround: disable strict checking — see below.

---

## Cert issuance failure: `flag provided but not defined: -dns`

lego v5 (`goacme/lego:latest` after ~mid-2025) moved `--dns`, `--email`,
`--domains`, `--path`, `--accept-tos`, and `--server` from global flags
to subcommand options. They have to come *after* the action name now,
not before.

lego 5.3.x went further and **removed the `renew` subcommand entirely** —
`run` is now get-or-renew, deciding from `--renew-days` (renamed from
`--days`) whether the cert on disk is due. A qifa build that still emits
`lego renew …` fails on every issue *and* renew:

```
time=... level=ERROR msg=Error error="flag provided but not defined: -dns"
```

**Fix**: upgrade qifa to a build that issues `lego run` (any commit after
this note was revised). The flag ordering is one-way: patched qifa won't
work against lego v4, where those are global flags and `renew` still
exists. Either run patched qifa + lego v5+ (current default), or pin a
pre-patch qifa + a v4 image via `Options.LegoImage`. Don't mix.

## Cert renewal failure: `Could not validate ARI 'replaces' field`

```
acme: error: 403 :: urn:ietf:params:acme:error:unauthorized ::
Could not validate ARI 'replaces' field :: requester account did not
request the certificate being replaced by this order
```

lego v5 uses ARI (RFC 9773) and points the new order at the cert it
replaces. Let's Encrypt rejects that when it doesn't attribute the old
cert to the requesting account, and the whole order 403s. qifa passes
`--ari-disable` for this reason — ARI only schedules renewals early, so
nothing is lost. If you invoke lego by hand, pass it yourself.

## Cert renewal failure: DNS-01 `time limit exceeded`

```
dns01: time limit exceeded: last error: recursive nameservers:
NS 1.1.1.1:53 did not return the expected TXT record
```

The authoritative record is live but a public recursive resolver is
still serving a cached miss for the challenge name, and doesn't pick the
TXT record up inside lego's two-minute window. qifa passes
`--dns.propagation.disable-rns` so propagation is checked against the
authoritative nameservers only — hosts that timed out three times with
the recursive check on succeeded on the first attempt with it off.
`--dns-resolvers` still governs CNAME and apex resolution.

## DNS provider auth: `cloudflare: some credentials information are missing`

lego's Cloudflare provider reads **`CLOUDFLARE_DNS_API_TOKEN`** (plus
optionally `CLOUDFLARE_ZONE_API_TOKEN`). The `CF_DNS_API_TOKEN` name
referenced in some places is NOT picked up by lego — only by some
other tools.

Qifa auto-forwards both `CF_*` and `CLOUDFLARE_*` prefixes from the
deployer's environment to the lego container, so the fix is to export
`CLOUDFLARE_DNS_API_TOKEN` (or set it in your secret store / .env)
matching what lego actually reads.

## Multi-domain (SAN) certs for `proxy.hosts:` apps

For apps that register multiple hostnames via `proxy.hosts:` (apex +
www, or two unrelated FQDNs sharing a backend), kamal-proxy will serve
the same cert to every name, so a single-name cert breaks TLS on every
host but the first.

Pass extra positional hostnames after the primary:

```sh
qifa cert issue tripmemo.ai www.tripmemo.ai \
  --provider cloudflare --email you@example.com
```

The first host is the cert filename (and the lego "primary" domain);
subsequent positionals become Subject Alternative Names. The same
syntax works for `qifa cert renew`. `qifa cert remove` still takes a
single host (it deletes one cert file).

`cert renew --all` does **not** need the positionals: it reads each
cert off the volume and re-requests the SANs already in it, so
multi-domain certs keep every name. A host whose current cert can't be
read is skipped and named in the summary rather than reissued
single-name — shrinking a working cert is worse than not renewing it.

Check what a cert actually covers with `qifa cert list --expiry`, which
prints `host <TAB> days-left <TAB> SANs`.

## Renewal succeeded but the old cert is still being served

kamal-proxy reads the certificate files when a route is deployed and
does **not** watch them afterwards, so a renewed cert on disk changes
nothing on the wire. Restart the proxy (`docker restart kamal-proxy`,
or `qifa proxy restart`) or redeploy the affected app.

This is worth automating together: renewing without restarting looks
entirely successful — clean lego output, fresh files, zero errors —
while clients keep getting the expired cert.

## Cert issuance failure: `Could not validate ARI 'replaces' field`

```
acme: error: 403 :: urn:ietf:params:acme:error:unauthorized ::
Could not validate ARI 'replaces' field :: requester account did not
request the certificate being replaced by this order
```

lego v5 uses ARI (RFC 9773) and points a renewal order at the cert it
replaces. Let's Encrypt rejects that when it doesn't attribute the old
cert to the requesting account, failing the whole order. qifa passes
`--ari-disable` for this reason; pass it yourself if you invoke lego
directly.

## Pinning the lego image

`LegoImage` defaults to an exact version, not `:latest`, and
`--lego-image` / `QIFA_LEGO_IMAGE` override it (the flag wins).

Resist the urge to track `:latest` in an unattended setting. qifa's
command line is lego-v5-specific, so a major bump upstream breaks every
issuance — and if a host prunes unused images on a timer, the broken
version arrives on its own schedule rather than when you're watching.
That combination expired 36 certs at once on 2026-08-13.

Pinning a **v4** tag needs a qifa from before the v5 flag change; the
two are not interchangeable in either direction.

---

### Hidden anchor: ssh strict_host_key_checking workaround

For trusted local hosts only (e.g. `localhost`, an isolated VM, a CI
sandbox where you can tolerate the loss of MITM protection), set:

```yaml
ssh:
  strict_host_key_checking: false
```

This makes qifa accept any key the host presents — fine for
self-hosting on your own machine, **not** acceptable for production
targets reachable over the public internet.
