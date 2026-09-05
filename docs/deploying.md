# Deploying as a team

qifa keeps nothing authoritative on the deployer: the containers' `qifa.*`
labels say what is deployed, and the deploy lock lives on the target hosts. Any
checkout on any machine can deploy. This is the setup that makes two people
deploying "the same thing" actually ship the same thing.

## Setup

**One shared `deploy` user on every host.** Each person's public key goes in its
`authorized_keys`; nobody gets an individual host account. The lock records the
real human either way, so you lose no accountability — and you avoid per-person
docker group membership and `/tmp` ownership collisions.

```sh
# on each host, as root
adduser --disabled-password --gecos "" deploy
usermod -aG docker deploy
install -d -m 700 -o deploy -g deploy /home/deploy/.ssh
cat >> /home/deploy/.ssh/authorized_keys   # paste each deployer's public key
```

```yaml
ssh:
  user: deploy
```

**One config file per environment, in the repo**, addressed with `-c`. There is
no destination flag; this is the whole mechanism.

```text
deploy/qifa.production.yaml
deploy/secrets.production.enc.env   # SOPS-encrypted, safe to commit
```

**Pin the build platform** if you build locally, or the image inherits the
builder's architecture — a Mac ships arm64 and a Linux box ships amd64 from the
same commit:

```yaml
builder:
  context: ..            # the config lives in deploy/, so ".." is the repo
  platform: linux/amd64
```

Then `qifa proxy boot` once per host, and `qifa doctor` from each person's
machine to confirm SSH, docker, and registry access before their first deploy.

## Deploying

```sh
git pull
qifa -c deploy/qifa.production.yaml env diff     # what would change in env
qifa -c deploy/qifa.production.yaml deploy -n    # dry run
qifa -c deploy/qifa.production.yaml deploy
```

`env diff` is the step worth not skipping. A deploy takes its environment from
whoever runs it, so the live env can be a colleague's stale secrets file:

```text
web 10.0.0.11 myapp-web-a1b2c3:
  ~ DATABASE_URL  (value differs)
  + STRIPE_KEY  (in config, missing on host)
```

Key names only — a differing value is reported, never printed. Only `qifa
deploy` applies changed env; `--env-file` is read once at container create time,
so `qifa restart` keeps the environment the container already had.

## The two rules that make it portable

**Relative paths resolve against the config file, not your shell.** qifa moves
into the config's directory first, so `builder.context`, `files[].src`,
`hooks.*`, `env.secret_command`, `.qifa/state.jsonl` and `backups/` mean the
same thing from anywhere. Paths you type as arguments (`qifa restore ./x.tar`)
stay relative to your shell.

**The version is explicit, or it is a clean commit.** With a `builder:` block
the tag is `--version` / `QIFA_VERSION` if given, otherwise the short SHA of the
config's checkout. A dirty tree is refused — that tag would name bytes nobody
else can reproduce. Use `--version v1.2.3`, or `--allow-dirty` to tag it
`<sha>-dirty`.

## Secrets

Use `env.secret_command` for anything more than one person deploys.
`env.secret` reads the deployer's own shell, so what ships is whatever their
environment happened to hold — unreviewable and different per machine. Keep it
for CI runners that inject values themselves.

```yaml
env:
  clear:
    APP_ENV: production
  secret_command: sops --decrypt secrets.production.enc.env
```

The encrypted file lives in the repo and each person has their own age key, so
granting or revoking access is `sops updatekeys` plus a commit. Any command that
prints `KEY=VALUE` on stdout works the same way (`op inject`, `vault kv get`).

Registry credentials come from `registry.password_env`, read on the deployer, so
each person supplies it from their own keychain.

## CI

CI is another deployer. It should always pass `--version` — a build step that
writes generated files makes the tree dirty and qifa will refuse, and a
workspace restored from an artifact is not a git repo at all.

```yaml
- run: qifa -c deploy/qifa.production.yaml deploy --version "${GITHUB_SHA::7}"
  env:
    REGISTRY_PASSWORD: ${{ secrets.REGISTRY_PASSWORD }}
    SOPS_AGE_KEY: ${{ secrets.SOPS_AGE_KEY }}
```

The runner also needs an SSH key in the `deploy` user and a `known_hosts` entry
— write it in the job with `ssh-keyscan` rather than setting
`ssh.strict_host_key_checking: false`.

## Two people at once

Deploy and rollback take a per-service lock on every host. The second one fails
immediately, naming the holder:

```text
error: lock for myapp held on 10.0.0.11: {"user":"alice","host":"alice-mbp",...}
```

`qifa lock status` shows it per host; `qifa lock release` clears one left by a
crashed deploy. If you see `lock ... left behind` in a warning, someone is using
individual SSH accounts — `/tmp` is sticky, so one person's lock directory
cannot be removed by another.

## When something looks machine-specific

| symptom | fix |
| --- | --- |
| `the checkout has uncommitted changes` | commit, or `--version <v>` / `--allow-dirty` |
| app behaves like an older build | `builder.context` is relative to the config — use `..` from `deploy/` |
| `exec format error` | set `builder.platform`, or build in CI |
| `required secret env FOO is not set` | move it from `env.secret` to `env.secret_command` |
| app has a colleague's old config values | `qifa env diff`, then `qifa deploy` |
| `qifa status` empty on a teammate's machine | expected — it is a local audit log; use `qifa app containers` |

Registry, proxy and certificate failures are in
[troubleshooting.md](troubleshooting.md).
