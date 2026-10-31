#!/bin/sh
# End-to-end test for a server running Docker and native apps behind one
# kamal-proxy: the Docker one, with the native app joining it through the
# Docker network's gateway (proxy_boot.mode: container).
#
#   test/e2e/mixed-e2e.sh
set -eu

root=$(cd "$(dirname "$0")/../.." && pwd)
work=$(mktemp -d)
name=qifa-mixed-e2e
port=${QIFA_MIXED_E2E_PORT:-2399}

cleanup() {
  status=$?
  if [ "$status" != 0 ]; then
    echo "--- journal (last 40 lines) ---"
    docker exec "$name" journalctl -n 40 --no-pager 2>/dev/null || true
    echo "--- kamal-proxy ---"
    docker exec "$name" docker logs --tail 20 kamal-proxy 2>/dev/null || true
  fi
  docker rm -f -v "$name" >/dev/null 2>&1 || true
  rm -rf "$work"
  exit "$status"
}
trap cleanup EXIT INT TERM

step() { printf '\n=== %s\n' "$*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }
on_host() { docker exec "$name" sh -c "$1"; }
expect_eq() { [ "$1" = "$2" ] || fail "$3: got [$1], want [$2]"; }
# Through the shared kamal-proxy on the server, by name.
via_proxy() { on_host "curl -s -m 10 -H 'Host: $1' -w '\n%{http_code}' http://127.0.0.1$2"; }
code_via_proxy() { on_host "curl -s -o /dev/null -m 10 -H 'Host: $1' -w '%{http_code}' http://127.0.0.1$2"; }

step "boot a server with systemd and Docker"
ssh-keygen -t ed25519 -N '' -f "$work/id" -q
mkdir -p "$work/keys" "$work/native/dist" "$work/docker"
cp "$work/id.pub" "$work/keys/authorized_keys"
docker build -q -t qifa-mixed-host "$root/test/e2e/mixed-host" >/dev/null
docker rm -f -v "$name" >/dev/null 2>&1 || true
docker run -d --name "$name" --privileged --cgroupns=host \
  -v /sys/fs/cgroup:/sys/fs/cgroup:rw -v "$work/keys:/test-ssh:ro" -v /var/lib/docker \
  -p "127.0.0.1:$port:22" qifa-mixed-host >/dev/null
for i in $(seq 1 90); do
  on_host 'systemctl is-active --quiet ssh && docker info >/dev/null 2>&1' 2>/dev/null && break
  sleep 1
done
on_host 'systemctl is-active --quiet ssh' || fail "sshd did not start"
on_host 'docker info >/dev/null 2>&1' || fail "dockerd did not start"

goarch=$(on_host 'uname -m' | sed 's/x86_64/amd64/; s/aarch64/arm64/')
(cd "$root" && go build -o "$work/qifa" ./cmd/qifa)
(cd "$root" && GOOS=linux GOARCH="$goarch" CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=n1" -o "$work/native/dist/app" ./test/e2e/native-app)

cat > "$work/docker/qifa.yaml" <<EOF
service: whoami
image: traefik/whoami:v1.10.3
servers:
  web:
    hosts: ["127.0.0.1:$port"]
proxy:
  host: docker.test
  app_port: 80
proxy_boot:
  hosts: ["127.0.0.1:$port"]
healthcheck:
  path: /
ssh:
  user: deploy
  key: $work/id
  strict_host_key_checking: false
EOF
cat > "$work/native/qifa.yaml" <<EOF
service: nativeapp
servers:
  web:
    hosts: ["127.0.0.1:$port"]
    cmd: [app]
    app_port: 8080
healthcheck:
  path: /up
  deploy_timeout: 15s
  interval: 1s
proxy:
  host: native.test
proxy_boot:
  mode: container
ssh:
  user: deploy
  key: $work/id
  strict_host_key_checking: false
native:
  artifact: dist/app
  user: nativeapp
  arch: $goarch
EOF
dq() { "$work/qifa" -c "$work/docker/qifa.yaml" "$@"; }
nq() { "$work/qifa" -c "$work/native/qifa.yaml" "$@"; }

step "Docker side: setup, the proxy, a container app"
dq setup --ssh-user root --deploy-user deploy
dq proxy boot
dq deploy --version d1
expect_eq "$(code_via_proxy docker.test /)" 200 "docker app via the proxy"

step "native side: setup, then deploy behind the Docker proxy"
nq setup --ssh-user root
nq deploy --version n1
expect_eq "$(via_proxy native.test /version | head -1)" n1 "native app via the Docker proxy"
expect_eq "$(code_via_proxy docker.test /)" 200 "docker app still served"
gateway=$(on_host "docker network inspect -f '{{range .IPAM.Config}}{{.Gateway}}{{end}}' kamal")
nq status > "$work/out"
grep -q "route    native.test -> $gateway:8080 (running)" "$work/out" || { cat "$work/out"; fail "status route through the gateway"; }
dq proxy details | grep -q native.test || fail "the Docker config's proxy details lack the native route"

step "native redeploy and rollback through the shared proxy"
(cd "$root" && GOOS=linux GOARCH="$goarch" CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=n2" -o "$work/native/dist/app" ./test/e2e/native-app)
nq deploy --version n2
expect_eq "$(via_proxy native.test /version | head -1)" n2 "native v2 via the Docker proxy"
nq rollback n1
expect_eq "$(via_proxy native.test /version | head -1)" n1 "native rollback"
expect_eq "$(code_via_proxy docker.test /)" 200 "docker app untouched"

step "maintenance on the native app leaves the Docker app alone"
nq app maintenance >/dev/null
expect_eq "$(code_via_proxy native.test /version)" 503 "native in maintenance"
expect_eq "$(code_via_proxy docker.test /)" 200 "docker app during native maintenance"
nq app live >/dev/null
expect_eq "$(code_via_proxy native.test /version)" 200 "native live again"

step "the Docker configs own the proxy"
if nq proxy boot >"$work/out" 2>&1; then fail "a container-mode native config booted a proxy"; fi
grep -q "Docker configs manage" "$work/out" || { cat "$work/out"; fail "unclear refusal"; }
nq proxy details | grep -q docker.test || fail "proxy details from the native config"

step "removing the native app leaves the proxy and the Docker app"
nq remove --ssh-user root
dq proxy details | grep -q native.test && fail "native route left after remove"
expect_eq "$(code_via_proxy docker.test /)" 200 "docker app after native remove"

printf '\nPASS: mixed e2e\n'
