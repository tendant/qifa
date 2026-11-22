#!/bin/sh
# End-to-end test of docs/vps.md: from a fresh VPS to the app at its domain
# over HTTPS, running the guide's commands as written.
#
#   test/e2e/vps-e2e.sh        (make test-e2e-vps)
#
# The "VPS" is a container (test/e2e/vps-host): Ubuntu 24.04, systemd, sshd,
# root logging in with a key, no Docker. It answers as app.test on a test
# network, where Pebble, Let's Encrypt's test server, issues the certificate
# after validating against it the way Let's Encrypt would.
#
# Test-only, and nothing a reader of the guide does:
#   - the box's SSH is on 127.0.0.1:$port rather than port 22 of a public IP
#   - kamal-proxy is a build whose staging ACME URL is Pebble's, so the
#     config adds proxy_boot.image and tls.staging (Let's Encrypt itself
#     needs a public name)
#   - DNS for app.test comes from the test network, not a registrar
# Not covered at all: real DNS, cloud firewalls, Let's Encrypt rate limits.
#
# Needs Docker locally, privileged containers allowed, and the internet
# (the box installs Docker from get.docker.com and pulls nginx).
set -eu

root=$(cd "$(dirname "$0")/../.." && pwd)
work=$(mktemp -d)
name=qifa-vps-e2e
pebble=qifa-vps-e2e-pebble
dns=qifa-vps-e2e-dns
net=qifa-vps-e2e-net
port=${QIFA_VPS_E2E_PORT:-2499}
proxy_version=v0.10.2

cleanup() {
  status=$?
  if [ "$status" != 0 ]; then
    echo "--- kamal-proxy ---"
    docker exec "$name" docker logs --tail 30 kamal-proxy 2>&1 || true
    echo "--- pebble ---"
    docker logs --tail 30 "$pebble" 2>&1 || true
  fi
  if [ "${KEEP_E2E_ENV:-0}" != 1 ]; then
    docker rm -f -v "$name" "$pebble" "$dns" >/dev/null 2>&1 || true
    docker network rm "$net" >/dev/null 2>&1 || true
    rm -rf "$work"
  fi
  exit "$status"
}
trap cleanup EXIT INT TERM

step() { printf '\n=== %s\n' "$*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }
on_box() { docker exec "$name" sh -c "$1"; }
# guide <command>: a command from docs/vps.md, printed and run as written
# (with this test's address for the server).
guide() { printf '$ %s\n' "$*"; sh -c "$*"; }

step "a fresh VPS: Ubuntu 24.04, root over SSH, no Docker"
ssh-keygen -t ed25519 -N '' -f "$work/id" -q
mkdir -p "$work/keys" "$work/home/.ssh" "$work/hello-vps"
cp "$work/id.pub" "$work/keys/authorized_keys"
docker build -q -t qifa-vps-host "$root/test/e2e/vps-host" >/dev/null
docker rm -f -v "$name" "$pebble" "$dns" >/dev/null 2>&1 || true
docker network rm "$net" >/dev/null 2>&1 || true
docker network create "$net" >/dev/null
docker run -d --name "$name" --privileged --cgroupns=host \
  --network "$net" --network-alias app.test \
  -v /sys/fs/cgroup:/sys/fs/cgroup:rw -v "$work/keys:/test-ssh:ro" -v /var/lib/docker -v /var/lib/containerd \
  -p "127.0.0.1:$port:22" qifa-vps-host >/dev/null
for i in $(seq 1 60); do
  on_box 'systemctl is-active --quiet ssh' 2>/dev/null && break
  sleep 1
done
on_box 'systemctl is-active --quiet ssh' || fail "sshd did not start"
on_box '! command -v docker' >/dev/null || fail "the box already has Docker"

# The reader's machine: their key in ~/.ssh, and the server's host key
# accepted (the guide's first ssh does that). qifa is the one built here.
cp "$work/id" "$work/home/.ssh/id_ed25519"
chmod 600 "$work/home/.ssh/id_ed25519"
for i in $(seq 1 30); do
  ssh-keyscan -p "$port" 127.0.0.1 > "$work/home/.ssh/known_hosts" 2>/dev/null && [ -s "$work/home/.ssh/known_hosts" ] && break
  sleep 1
done
mkdir -p "$work/bin"
(cd "$root" && go build -o "$work/bin/qifa" ./cmd/qifa)
# ssh reads ~/.ssh from the account, not $HOME: this one reads the test's.
cat > "$work/bin/ssh" <<EOF
#!/bin/sh
exec $(command -v ssh) -i "$work/home/.ssh/id_ed25519" -o UserKnownHostsFile="$work/home/.ssh/known_hosts" "\$@"
EOF
chmod +x "$work/bin/ssh"
# Go keeps its caches where they are, not under the test's HOME.
export GOMODCACHE="$(go env GOMODCACHE)" GOCACHE="$(go env GOCACHE)" GOPATH="$(go env GOPATH)"
export HOME="$work/home" PATH="$work/bin:$PATH"
unset SSH_AUTH_SOCK
SERVER=127.0.0.1
ssh_opts="-p $port -o BatchMode=yes"

step "test plumbing: Pebble, its DNS, and a kamal-proxy that orders from it"
box_ip=$(docker inspect -f "{{(index .NetworkSettings.Networks \"$net\").IPAddress}}" "$name")
# Pebble serves its ACME API as pebble.test, with a certificate from a CA
# made here: the box's Docker resolver forwards only dotted names, so the
# bundled one, for "pebble", would not do.
mkdir -p "$work/pebble-certs"
(
  cd "$work/pebble-certs"
  openssl req -x509 -newkey rsa:2048 -nodes -keyout ca.key -out ca.pem -days 2 -subj "/CN=qifa vps e2e CA" 2>/dev/null
  openssl req -newkey rsa:2048 -nodes -keyout pebble.key -out pebble.csr -subj "/CN=pebble.test" 2>/dev/null
  printf 'subjectAltName=DNS:pebble.test\n' > ext
  openssl x509 -req -in pebble.csr -CA ca.pem -CAkey ca.key -CAcreateserial -out pebble.pem -days 2 -extfile ext 2>/dev/null
  chmod 644 pebble.key
) || fail "could not make Pebble's certificate"
cat > "$work/pebble.json" <<EOF
{"pebble": {"listenAddress": "0.0.0.0:14000", "managementListenAddress": "0.0.0.0:15000",
  "certificate": "/certs/pebble.pem", "privateKey": "/certs/pebble.key",
  "httpPort": 80, "tlsPort": 443, "ocspResponderURL": "", "externalAccountBindingRequired": false}}
EOF
# It also answers on :53 for the box's containers: kamal-proxy runs in the
# box's own Docker, which cannot use this network's resolver, and must find
# pebble.test by name.
docker run -d --name "$dns" --network "$net" --network-alias challtestsrv \
  ghcr.io/letsencrypt/pebble-challtestsrv:2.6.0 -defaultIPv4 "$box_ip" -defaultIPv6 "" -dns01 :8053,:53 -management :8055 >/dev/null
docker run -d --name "$pebble" --network "$net" --network-alias pebble.test \
  -e PEBBLE_VA_NOSLEEP=1 -e PEBBLE_WFE_NONCEREJECT=0 \
  -v "$work/pebble.json:/pebble.json:ro" -v "$work/pebble-certs:/certs:ro" ghcr.io/letsencrypt/pebble:2.6.0 -config /pebble.json -dnsserver challtestsrv:8053 >/dev/null
pebble_ip=$(docker inspect -f "{{(index .NetworkSettings.Networks \"$net\").IPAddress}}" "$pebble")
dns_ip=$(docker inspect -f "{{(index .NetworkSettings.Networks \"$net\").IPAddress}}" "$dns")
for i in $(seq 1 30); do
  docker run --rm --network "$net" curlimages/curl:8.11.1 -fsS -X POST \
    -d "{\"host\":\"pebble.test.\",\"addresses\":[\"$pebble_ip\"]}" http://challtestsrv:8055/add-a >/dev/null 2>&1 && break
  sleep 1
done
# The box's Docker hands containers that resolver. Written before the guide
# installs Docker, which keeps an existing daemon.json.
on_box "mkdir -p /etc/docker && printf '{\"dns\": [\"%s\"]}\n' $dns_ip > /etc/docker/daemon.json"
goarch=$(on_box 'uname -m' | sed 's/x86_64/amd64/; s/aarch64/arm64/')
kp_src=$(cd "$work" && go mod download -json "github.com/basecamp/kamal-proxy@$proxy_version" | sed -n 's/^[[:space:]]*"Dir": "\(.*\)",$/\1/p')
[ -d "$kp_src" ] || fail "could not fetch kamal-proxy source"
mkdir -p "$work/kp-image"
cp -R "$kp_src" "$work/kamal-proxy-src" && chmod -R u+w "$work/kamal-proxy-src"
sed 's#https://acme-staging-v02.api.letsencrypt.org/directory#https://pebble.test:14000/dir#' \
  "$work/kamal-proxy-src/internal/server/server.go" > "$work/server.go"
mv "$work/server.go" "$work/kamal-proxy-src/internal/server/server.go"
grep -q 'https://pebble.test:14000/dir' "$work/kamal-proxy-src/internal/server/server.go" || fail "staging URL not patched"
(cd "$work/kamal-proxy-src" && GOOS=linux GOARCH="$goarch" CGO_ENABLED=0 GOFLAGS=-mod=mod \
  go build -trimpath -ldflags "-X github.com/basecamp/kamal-proxy/internal/version.Version=$proxy_version-pebble" \
  -o "$work/kp-image/kamal-proxy" ./cmd/kamal-proxy)
cp "$work/pebble-certs/ca.pem" "$work/kp-image/pebble-ca.crt"
cat > "$work/kp-image/Dockerfile" <<EOF
FROM basecamp/kamal-proxy:$proxy_version
USER root
COPY kamal-proxy /usr/local/bin/kamal-proxy
COPY pebble-ca.crt /usr/local/share/ca-certificates/pebble-ca.crt
RUN update-ca-certificates >/dev/null 2>&1 || cat /usr/local/share/ca-certificates/pebble-ca.crt >> /etc/ssl/certs/ca-certificates.crt
USER kamal-proxy
EOF
docker build -q --platform "linux/$goarch" -t "qifa-kamal-proxy:$proxy_version-pebble" "$work/kp-image" >/dev/null

# ---------------------------------------------------------------------------
# docs/vps.md, step by step.
# ---------------------------------------------------------------------------

step "guide 1: install Docker on the server"
guide "ssh $ssh_opts root@$SERVER 'curl -fsSL https://get.docker.com | sh' >/dev/null"
on_box 'systemctl is-active --quiet docker && docker info >/dev/null' || fail "Docker is not running after the install"

step "guide 2: a deploy user with your key"
guide "ssh $ssh_opts root@$SERVER 'adduser --disabled-password --gecos \"\" deploy && install -d -m 700 -o deploy -g deploy /home/deploy/.ssh && install -m 600 -o deploy -g deploy /root/.ssh/authorized_keys /home/deploy/.ssh/'"

# The test plumbing again: hand the box the Pebble-ordering proxy image
# (a real server pulls basecamp/kamal-proxy itself).
docker save "qifa-kamal-proxy:$proxy_version-pebble" | docker exec -i "$name" docker load >/dev/null

step "guide 3: qifa init in the project"
cat > "$work/hello-vps/Dockerfile" <<'EOF'
FROM nginx:1.27-alpine
RUN echo 'hello from qifa' > /usr/share/nginx/html/index.html
EXPOSE 80
EOF
cd "$work/hello-vps"
guide "qifa init --host deploy@$SERVER:$port --domain app.test"
grep -q '^  user: deploy$' qifa.yaml || fail "init did not set ssh.user"
grep -q "^    hosts: \[$SERVER:$port\]" qifa.yaml || fail "init did not set the host"
# Test-only: order the certificate from Pebble.
awk '{ print } /^    source: kamal$/ { print "    staging: true" }' qifa.yaml > qifa.yaml.new && mv qifa.yaml.new qifa.yaml
grep -q '^    staging: true$' qifa.yaml || fail "could not add tls.staging"
cat >> qifa.yaml <<EOF

proxy_boot:
  image: qifa-kamal-proxy
  version: $proxy_version-pebble
EOF
qifa config >/dev/null

step "guide 4: qifa setup, as root"
guide "qifa setup --ssh-user root"
on_box 'id -nG deploy' | tr ' ' '\n' | grep -qx docker || fail "setup did not put deploy in the docker group"

step "guide 5: qifa doctor"
guide "qifa doctor"

step "guide 6: qifa deploy"
guide "qifa deploy"

step "the app answers at https://app.test, with a certificate from the ACME server"
curl_box() {
  docker run --rm --network "$net" -v "$work/pebble-root.pem:/root.pem:ro" curlimages/curl:8.11.1 "$@"
}
docker run --rm --network "$net" curlimages/curl:8.11.1 -fsSk https://pebble.test:15000/roots/0 > "$work/pebble-root.pem" \
  || fail "cannot fetch Pebble's issuing root"
body=""
for i in $(seq 1 30); do
  # The first TLS handshake for app.test makes kamal-proxy order the certificate.
  if body=$(curl_box -fsS --cacert /root.pem https://app.test/ 2>/dev/null); then break; fi
  body=""
  sleep 2
done
if [ "$body" != "hello from qifa" ]; then
  curl_box -sS -v --cacert /root.pem https://app.test/ 2>&1 | tail -15 || true
  fail "https://app.test/ did not answer with the app over a trusted certificate"
fi
echo "https://app.test/: $body"
code=$(docker run --rm --network "$net" curlimages/curl:8.11.1 -s -o /dev/null -w '%{http_code}' http://app.test/)
[ "$code" = 301 ] || [ "$code" = 308 ] || fail "http://app.test/ answered $code, not a redirect to HTTPS"
echo "http://app.test/: $code to HTTPS"

step "guide 7: a second deploy replaces the app without a new proxy"
proxy_id=$(on_box 'docker ps -q --filter name=^kamal-proxy$')
echo "RUN echo 'hello again' > /usr/share/nginx/html/index.html" >> Dockerfile
guide "qifa deploy"
[ "$(on_box 'docker ps -q --filter name=^kamal-proxy$')" = "$proxy_id" ] || fail "the second deploy replaced kamal-proxy"
body=$(curl_box -fsS --cacert /root.pem https://app.test/)
[ "$body" = "hello again" ] || fail "after the second deploy: got [$body]"
echo "https://app.test/: $body"

printf '\nPASS: vps e2e\n'
