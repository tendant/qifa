# Troubleshooting

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
to options of the `run` (and `renew`) subcommand. They have to come
*after* the action name now, not before.

If you see:
```
time=... level=ERROR msg=Error error="flag provided but not defined: -dns"
```
…you're running an older qifa binary against `goacme/lego:latest`.

**Fix**: upgrade qifa to a build that puts the action name before the
flags (any commit after this troubleshooting note landed). The flag
ordering for v5+ is one-way: the patched qifa won't work with lego v4
either, since `--dns` etc. are *global* flags in v4 and subcommand
options in v5. Either run patched qifa + lego v5+ (current default),
or pin a pre-patch qifa + lego v4. Don't mix.

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
