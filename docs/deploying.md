# Deploying

A runbook for teams: how to set up a repo and its hosts so that a deploy means
the same thing from anyone's laptop, a jump host, or CI.

qifa keeps nothing authoritative on the deployer. The running containers'
`qifa.service` / `qifa.role` / `qifa.version` labels are the source of truth
for what is deployed, and the deploy lock lives on the target hosts. So any
checkout on any machine can deploy — the setup below is about making sure two
people deploying "the same thing" actually ship the same thing.

- [One-time setup](#one-time-setup)
- [The routine deploy](#the-routine-deploy)
- [Where paths resolve from](#where-paths-resolve-from)
- [Versions](#versions)
- [Secrets](#secrets)
- [Deploying from CI](#deploying-from-ci)
- [Two people at once](#two-people-at-once)
- [Multiple environments](#multiple-environments)
- [Rolling back](#rolling-back)
- [Onboarding a new deployer](#onboarding-a-new-deployer)
- [Cross-machine failures and their fixes](#cross-machine-failures-and-their-fixes)

## One-time setup

### Hosts: one shared deploy user

Give every host a single `deploy` user and put each person's **public** key in
its `authorized_keys`. Do not give people individual accounts on the hosts.

```sh
# on each host, as root
adduser --disabled-password --gecos "" deploy
usermod -aG docker deploy
install -d -m 700 -o deploy -g deploy /home/deploy/.ssh
# append each deployer's public key
cat >> /home/deploy/.ssh/authorized_keys
```

Individual host accounts buy you nothing: the deploy lock records the real
human (local username and hostname) in its `holder.json` regardless of which
SSH account was used, so accountability is unaffected. What they cost you is
per-person docker group membership, per-person credential files, and the
`/tmp` ownership collisions described in
[Cross-machine failures](#cross-machine-failures-and-their-fixes).

Then in the config:

```yaml
ssh:
  user: deploy
```

Boot the shared proxy once per host (idempotent, safe to re-run):

```sh
qifa proxy boot
```

### Repo: config per environment, committed

```text
deploy/
  qifa.production.yaml
  qifa.staging.yaml
  secrets.production.enc.env    # SOPS-encrypted, safe to commit
  secrets.staging.enc.env
```

Everything a deploy needs, other than credentials, is in the repo and gets
code review. Address an environment with `-c`:

```sh
qifa -c deploy/qifa.production.yaml deploy
```

There is no `-d staging` destination concept — one file per environment is the
whole mechanism.

### Pin the build platform

If you build locally (`builder:` with no `host:`), the image inherits the
builder's architecture. A Mac produces arm64 and a Linux box produces amd64
from the same commit, and the amd64 host silently gets whichever one the last
deployer happened to build. Always pin it:

```yaml
builder:
  context: .
  platform: linux/amd64
```

Better, where you can: build in CI, push to a registry, and have humans deploy
an already-built image. See [Deploying from CI](#deploying-from-ci).

### Each person, once

```sh
# 1. install the binary
make build && cp qifa /usr/local/bin/

# 2. confirm SSH reaches every host as the deploy user
qifa -c deploy/qifa.production.yaml doctor

# 3. confirm you can decrypt the secrets (see Secrets below)
sops --decrypt deploy/secrets.production.enc.env | head -1
```

`qifa doctor` is read-only. It checks SSH, the docker CLI and daemon,
DNS/TCP/TLS to the registry, whether your credentials actually authorize the
image's manifest, disk, clock skew, and whether kamal-proxy is up — on every
host qifa will touch. Run it before your first deploy from a new machine and
after any network change.

## The routine deploy

```sh
git pull                                          # 1. current main
qifa -c deploy/qifa.production.yaml env diff      # 2. what would change in env
qifa -c deploy/qifa.production.yaml deploy -n     # 3. dry run
qifa -c deploy/qifa.production.yaml deploy        # 4. go
qifa -c deploy/qifa.production.yaml status        # 5. confirm
```

Step 2 is the one people skip and shouldn't. A deploy takes its environment
from whoever runs it, so the live env can be a colleague's stale secrets file.
`env diff` compares what your config renders *now* against the env file each
running container was actually started with:

```text
web 10.0.0.11 myapp-web-a1b2c3:
  ~ DATABASE_URL  (value differs)
  - LEGACY_FLAG  (on host, not in config)
  + STRIPE_KEY  (in config, missing on host)
```

Key names only — a value that differs is reported as differing and never
printed. `in sync (14 keys)` means there is nothing to think about.

Note that only a fresh `qifa deploy` applies changed env. `--env-file` is read
once, when the container is created, so `qifa restart` bounces the container
with the environment it already had.

Step 3 prints the plan — image and version, builder mode, which hosts in which
batches, what the proxy would be told — and performs no mutating operation.

## Where paths resolve from

**qifa moves into the config file's directory before doing anything.** Every
relative path in the config resolves there, not against your shell:

| path | resolves against |
| --- | --- |
| `builder.context`, `builder.dockerfile` | the config's directory |
| `files[].src` | the config's directory |
| `hooks.pre_build`, `hooks.post_deploy`, `hooks.pre_rollback` | the config's directory |
| `env.secret_command` (its working directory) | the config's directory |
| `.qifa/state.jsonl`, `backups/` (written by qifa) | the config's directory |
| a path you type as an argument (`qifa restore ./dump.tar`) | **your shell** |

So these two are the same deploy:

```sh
cd ~/src/myapp && qifa -c deploy/qifa.production.yaml deploy
cd / && qifa -c ~/src/myapp/deploy/qifa.production.yaml deploy
```

The practical consequence: with the config in `deploy/`, a `builder.context`
of `.` means `deploy/`, not the repo root. Point it at the repo:

```yaml
builder:
  context: ..
```

If you previously ran qifa from a directory other than the config's, your old
`.qifa/state.jsonl` and `backups/` are at that old location. History is
audit-only so nothing breaks, but `qifa status` will look empty until you move
them.

## Versions

The version names the image tag, the container, and the rollback target, so it
must not depend on which machine ran the deploy. With a `builder:` block, qifa
picks it in this order:

| source | when |
| --- | --- |
| `--version <v>` | always wins |
| `QIFA_VERSION` | when no flag is given |
| short git SHA of the config's checkout | when neither is set and the tree is clean |
| UTC timestamp `20060102-150405` | when there is no git repository at all |

**A dirty checkout is refused:**

```text
error: the checkout has uncommitted changes, so version 3b1093b would not
describe what is deployed: commit them, or pass --version <v> or --allow-dirty
```

That tag would name bytes nobody else can reproduce, and a rollback to it
would not restore what was actually running. Commit, or:

```sh
qifa deploy --version hotfix-2026-09-03   # name it deliberately
qifa deploy --allow-dirty                 # tags it <sha>-dirty
```

Without a `builder:` block you are deploying an external image, and the version
comes from the image reference itself — the tag is resolved to the registry
digest at deploy time, so rollback works even with a floating `:latest`.

## Secrets

Three sources, merged into the `--env-file` given to the container, later
winning on collision:

| source | where the value comes from | use for |
| --- | --- | --- |
| `env.clear` | the config file | non-secrets: `APP_ENV`, feature flags |
| `env.secret` | the deployer's shell environment | CI runners only |
| `env.secret_command` | stdout of a command, as `KEY=VALUE` lines | everything shared |

**Prefer `secret_command` for anything more than one person deploys.**
`env.secret` reads whatever that person's shell happened to hold: unreviewable,
different per machine, and silently different between two people who both
believe they are deploying the same thing. It is the right tool only where the
environment is itself managed — a CI runner injecting values from its own
secret store.

The SOPS recipe, which keeps the encrypted file in the repo and gives each
person their own key:

```yaml
env:
  clear:
    APP_ENV: production
  secret_command: sops --decrypt secrets.production.enc.env
```

```sh
# once per repo — list every deployer's age public key
cat > .sops.yaml <<'EOF'
creation_rules:
  - path_regex: deploy/secrets\..*\.enc\.env$
    age: age1alice...,age1bob...,age1ci...
EOF

sops deploy/secrets.production.enc.env   # edit; re-encrypts on save
```

Adding or removing a deployer is `sops updatekeys` on each file plus a commit —
no shared password, no out-of-band handoff, and the diff shows who was granted
access and when.

The same shape works for anything that prints dotenv on stdout:

```yaml
secret_command: op inject -i secrets.tpl
secret_command: vault kv get -format=json secret/myapp | jq -r '.data.data | to_entries[] | "\(.key)=\(.value)"'
```

The command runs on the deployer with its working directory set to the config's
directory, so a relative path to the encrypted file is stable.

After rotating a secret, `qifa deploy` is what applies it — see the note about
`restart` in [The routine deploy](#the-routine-deploy). Confirm with
`qifa env diff`.

### Registry credentials

```yaml
registry:
  server: registry.example.com
  username: deploy
  password_env: REGISTRY_PASSWORD
```

`password_env` names an environment variable read on the deployer, so each
person supplies it from their own keychain or password manager. qifa stages the
resulting docker credentials on each host under
`/tmp/.qifa-docker-config-<user>` — per-user, so separate SSH accounts do not
collide on each other's 0600 files.

## Deploying from CI

CI is just another deployer, with two differences: it should always pass an
explicit version, and it is the one place `env.secret` is appropriate.

```yaml
# .github/workflows/deploy.yml
- run: |
    qifa -c deploy/qifa.production.yaml deploy --version "${GITHUB_SHA::7}"
  env:
    REGISTRY_PASSWORD: ${{ secrets.REGISTRY_PASSWORD }}
    SOPS_AGE_KEY: ${{ secrets.SOPS_AGE_KEY }}
```

The runner needs:

- **An SSH key** in the hosts' `deploy` user, and a `known_hosts` entry. qifa
  verifies host keys by default and will refuse to connect without one — write
  it in the job (`ssh-keyscan`, or a checked-in file), rather than reaching for
  `ssh.strict_host_key_checking: false`.
- **The decryption key** for whatever `secret_command` uses (`SOPS_AGE_KEY`
  above).
- **`--version`**, so the tag is the commit CI built and not whatever the
  workspace looks like. A build step that writes generated files into the tree
  makes it dirty, and qifa would refuse the deploy; a workspace restored from
  an artifact may not be a git repository at all, and would get a timestamp.

A CI runner deploying while a human deploys is handled by the lock — see next.

## Two people at once

Deploy and rollback take a per-service lock on every target host
(`/tmp/qifa-lock-<service>`, created with `mkdir`, which is atomic). The second
deploy fails immediately with who holds it:

```text
error: lock for myapp held on 10.0.0.11: {"user":"alice","host":"alice-mbp",
"service":"myapp","version":"a1b2c3d","acquired_at":"2026-09-03T15:04:05Z"}
```

Wait for them. To see the state without attempting a deploy:

```sh
qifa lock status
```

```text
10.0.0.11	{"user":"alice",...}
10.0.0.12	(free)
```

A partially-held lock like that one means a deploy died between hosts, or is
mid-acquisition. If nobody is deploying, clear it:

```sh
qifa lock release
```

qifa releases the lock through a `defer`, so it survives a failed deploy, a
non-zero exit, and a panic — but not `kill -9` or a dropped connection at the
wrong moment. If a release fails, qifa says so rather than leaving you to find
out on the next deploy:

```text
warning: lock for myapp left behind on 10.0.0.11: ... (clear it with `qifa lock release`)
```

The most common cause of that warning is people using individual SSH accounts:
`/tmp` is sticky, so a lock directory created by `alice` cannot be removed by
`bob`. One shared `deploy` user avoids it.

## Multiple environments

One config file per environment, and nothing else:

```sh
qifa -c deploy/qifa.staging.yaml deploy
qifa -c deploy/qifa.production.yaml deploy
```

Keep `service:` distinct per environment when they share hosts — the service
name scopes the container labels, the deploy lock, and the proxy registration.
Two environments with the same `service:` on the same host will fight over all
three.

A shell alias or a Makefile target per environment removes the chance of
typing the wrong `-c`:

```make
deploy-staging:    ; qifa -c deploy/qifa.staging.yaml deploy
deploy-production: ; qifa -c deploy/qifa.production.yaml deploy
```

## Rolling back

```sh
qifa app containers    # what versions are still on the hosts
qifa rollback          # previous version
qifa rollback a1b2c3d  # a specific one
```

Rollback finds the labeled container for that version, reads the image it was
running, and re-creates the container from it — so it works from any machine
and needs nothing from whoever ran the original deploy. What it does need is
for that container to still be on every host: `prune.retain_containers`
(default 5) is how many stopped versions are kept.

Rollback restores the image, not the environment: the container is re-created
with the env file the config renders *now*. If the bad deploy was an env change,
revert the config or secrets file too.

## Onboarding a new deployer

- [ ] Their public key is in `deploy@` `authorized_keys` on every host
- [ ] Their age/1Password identity is added to the secrets files, and the
      re-encrypted files are committed
- [ ] They have `REGISTRY_PASSWORD` (or whatever `password_env` names) in
      their own shell
- [ ] `qifa -c deploy/qifa.production.yaml doctor` passes on their machine
- [ ] `qifa -c deploy/qifa.production.yaml env diff` reports in sync — proof
      their secrets decrypt to the same values as everyone else's
- [ ] `qifa -c deploy/qifa.production.yaml deploy -n` prints the same version
      as a colleague's dry run of the same commit

The last two are the real test. If `env diff` shows drift on a fresh checkout,
their secret access is wrong; if the dry-run version differs, their checkout is.

## Cross-machine failures and their fixes

| symptom | cause | fix |
| --- | --- | --- |
| `the checkout has uncommitted changes` | dirty tree; the tag would not describe what ships | commit, or `--version <v>` / `--allow-dirty` |
| Deploy succeeds, app behaves like an older build | `builder.context` resolving to the config's directory, not the repo root | set `context: ..` when the config lives in `deploy/` |
| `exec format error` in the container | image built on a different architecture | set `builder.platform: linux/amd64`, or build in CI |
| `required secret env FOO is not set` | `env.secret` reading a shell that has no `FOO` | move it to `env.secret_command` |
| App has a colleague's old config values | the last deploy rendered their env, not yours | `qifa env diff`, then `qifa deploy` |
| `lock for X held on <host>` | someone is deploying | wait; `qifa lock status` to see who |
| `lock ... left behind` warning | crashed deploy, or individual SSH accounts on a sticky `/tmp` | `qifa lock release`; move to a shared `deploy` user |
| `knownhosts: key mismatch` | rebuilt host, or a new machine with no `known_hosts` | see [troubleshooting.md](troubleshooting.md#knownhosts-key-mismatch-during-ssh) |
| `qifa status` shows no history on a teammate's machine | `.qifa/state.jsonl` is a local audit log, per config directory | expected; `qifa app containers` is the real answer |

For deploy failures that are not about *where* you deployed from — registry
errors, proxy boot, certificates — see [troubleshooting.md](troubleshooting.md).
