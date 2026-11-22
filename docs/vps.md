# From a fresh server to HTTPS

This guide takes a new VPS and a project with a `Dockerfile` to the app
running at `https://app.example.com`, with a Let's Encrypt certificate and
redeploys that don't drop requests.

Steps 1 to 6 are what [`test/e2e/vps-e2e.sh`](../test/e2e/vps-e2e.sh) runs
against a fresh Ubuntu 24.04 machine, word for word. That machine is a
container standing in for the VPS, with a test certificate authority
standing in for Let's Encrypt. Step 0, DNS and the firewall, is what that
test can't cover. When a first deploy goes wrong, the cause is usually
there.

## What you need

- **A server:** Ubuntu 24.04 or Debian 12, with a public IP address. You need
  to be able to log in as root with your SSH key, which is how most providers
  hand one over. The server builds your image, so give it the memory your
  build needs.
- **A domain name** whose DNS you can edit.
- **qifa** on your machine:
  `curl -fsSL https://raw.githubusercontent.com/tendant/qifa/main/install.sh | sh`
- **A project with a `Dockerfile`** for an app that answers HTTP. You don't
  need a registry: the server builds the image itself.

Below, `203.0.113.10` stands for your server's IP address and
`app.example.com` for your domain.

## 0. DNS and the firewall

These can't be tested from here. Check each one by hand before going on.

- **DNS:** add an `A` record for `app.example.com` pointing at
  `203.0.113.10` (and an `AAAA` record if the server has IPv6). Wait until
  `dig +short app.example.com` prints the address. Let's Encrypt checks the
  name from the internet, so the certificate fails until DNS points at the
  server.
- **Firewall:** the server must accept TCP on ports 22 (SSH), 80 and 443.
  Check two places:
  - **Your provider's firewall or security group,** in its web console. Many
    providers block everything but 22 by default.
  - **A firewall on the server itself.** If `ufw status` says active, run
    `ufw allow 80/tcp && ufw allow 443/tcp`.

## 1. Install Docker on the server

```bash
ssh root@203.0.113.10 'curl -fsSL https://get.docker.com | sh'
```

The first `ssh` asks you to accept the server's host key. Answer yes: qifa
checks the server against that saved key on every deploy.

## 2. A deploy user with your key

qifa deploys as an ordinary user rather than root. Create one called
`deploy` that logs in with the same key:

```bash
ssh root@203.0.113.10 'adduser --disabled-password --gecos "" deploy && install -d -m 700 -o deploy -g deploy /home/deploy/.ssh && install -m 600 -o deploy -g deploy /root/.ssh/authorized_keys /home/deploy/.ssh/'
```

## 3. `qifa init` in your project

```bash
cd my-app
qifa init --host deploy@203.0.113.10 --domain app.example.com
```

This writes `qifa.yaml`. It builds your Dockerfile on the server, serves
the app through kamal-proxy at your domain over HTTPS, and connects as
`deploy`. The service is named after the directory, and the app's port
comes from the Dockerfile's last `EXPOSE` (80 when there is none).

Before deploying, check one thing in `qifa.yaml`. The deploy only switches
traffic to the new version once `healthcheck.path` answers with a 2xx
status. It starts as `/`; set it to your app's health route, such as `/up`,
if `/` answers anything else.

## 4. `qifa setup`, as root

```bash
qifa setup --ssh-user root
```

This checks that Docker is running and adds `deploy` to the `docker` group.
You only run it once per server.

## 5. `qifa doctor`

```bash
qifa doctor
```

This checks, without changing anything: SSH as `deploy`, Docker, the
server's route to Docker Hub (for your Dockerfile's base images), disk
space and the clock. Fix any `FAIL` line before deploying.

## 6. `qifa deploy`

```bash
qifa deploy
```

The first deploy starts kamal-proxy on the server, on ports 80 and 443. It
then uploads your project, builds the image on the server, starts the
container, waits for the health check, and routes `app.example.com` to it.
It ends with `live at https://app.example.com/`.

kamal-proxy gets the certificate from Let's Encrypt the first time someone
opens the site over HTTPS, so the first request takes a few seconds. Plain
`http://` redirects to `https://`.

## After that

- **Every change:** run `qifa deploy` again. The new container starts beside
  the old one, and traffic moves once it is healthy.
- **A bad release:** `qifa rollback` goes back to the previous version.
- **Watching it:** `qifa logs -f` follows the logs, and `qifa status` shows
  what runs where.
- **Settings and secrets:** add `env:` to `qifa.yaml` (see the commented
  block). Secrets are read from your shell when you deploy, never stored in
  the file.

## When it goes wrong

- **The certificate never arrives** (the browser warns, or `curl` reports a
  TLS error): DNS doesn't point at the server yet, or ports 80/443 are
  blocked (step 0). `qifa proxy logs` shows what kamal-proxy tried. Let's
  Encrypt limits failed attempts per hour. While experimenting, set
  `staging: true` under `proxy.tls` to use its staging service, which has
  generous limits but certificates browsers don't trust.
- **The deploy stops at the health check** with `404` or another non-2xx
  status: set `healthcheck.path` (step 3).
- **`Host key verification failed`:** `ssh root@203.0.113.10` once by hand
  to accept the key (step 1).
- **`permission denied` from Docker as `deploy`:** run `qifa setup --ssh-user
  root` (step 4).
- **Large uploads:** qifa uploads the whole project directory to the server
  each deploy, apart from its own `.qifa/` directory. `.dockerignore` decides
  what goes into the image, but not what is uploaded. Keep big directories
  you don't build from (`node_modules`, build output) out of the project
  directory, or deploy from a clean checkout.

[docs/troubleshooting.md](troubleshooting.md) covers more, and
[docs/deploying.md](deploying.md) covers deploying from several machines
or from CI.
