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
