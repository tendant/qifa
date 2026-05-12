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

### Workaround: disable strict checking

For trusted local hosts only (e.g. `localhost`, an isolated VM, a CI
sandbox where you can tolerate the loss of MITM protection), set:

```yaml
ssh:
  strict_host_key_checking: false
```

This makes qifa accept any key the host presents — fine for
self-hosting on your own machine, **not** acceptable for production
targets reachable over the public internet.
