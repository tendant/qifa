#!/bin/sh
# End-to-end test for native (non-Docker) deploys against a real systemd host.
#
#   test/e2e/native-e2e.sh
#
# Needs Docker on the machine running it, only to provide the throwaway
# "server": the server itself runs systemd, sshd and sudo, and no Docker.
set -eu

root=$(cd "$(dirname "$0")/../.." && pwd)
work=$(mktemp -d)
name=qifa-native-e2e
pebble=qifa-native-e2e-pebble
net=qifa-native-e2e-net
port=${QIFA_NATIVE_E2E_PORT:-2299}

cleanup() {
  status=$?
  if [ "$status" != 0 ]; then
    echo "--- journal (last 60 lines) ---"
    docker exec "$name" journalctl -n 60 --no-pager 2>/dev/null || true
  fi
  docker rm -f "$name" "$pebble" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$work"
  exit "$status"
}
trap cleanup EXIT INT TERM

step() { printf '\n=== %s\n' "$*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }
on_host() { docker exec "$name" sh -c "$1"; }
expect_eq() { [ "$1" = "$2" ] || fail "$3: got [$1], want [$2]"; }

step "boot systemd host"
ssh-keygen -t ed25519 -N '' -f "$work/id" -q
mkdir -p "$work/keys" "$work/app/dist"
cp "$work/id.pub" "$work/keys/authorized_keys"
docker build -q -t qifa-native-host "$root/test/e2e/native-host" >/dev/null
docker rm -f "$name" "$pebble" >/dev/null 2>&1 || true
docker network rm "$net" >/dev/null 2>&1 || true
docker network create "$net" >/dev/null
# The server answers as app.test on the test network, so Pebble (below) can
# validate certificate requests against it the way Let's Encrypt would.
docker run -d --name "$name" --privileged --cgroupns=host \
  --network "$net" --network-alias app.test \
  -v /sys/fs/cgroup:/sys/fs/cgroup:rw -v "$work/keys:/test-ssh:ro" \
  -p "127.0.0.1:$port:22" qifa-native-host >/dev/null
for i in $(seq 1 60); do
  on_host 'systemctl is-active --quiet ssh' 2>/dev/null && break
  sleep 1
done
on_host 'systemctl is-active --quiet ssh' || fail "sshd did not start"

goarch=$(on_host 'uname -m' | sed 's/x86_64/amd64/; s/aarch64/arm64/')
(cd "$root" && go build -o "$work/qifa" ./cmd/qifa)
build_app() {
  rm -f "$work/app/dist/app"
  (cd "$root" && GOOS=linux GOARCH="$goarch" CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=$1" -o "$work/app/dist/app" ./test/e2e/native-app)
}

# write_config <file> <proxy tls block> [extra yaml]
write_config() {
  cat > "$1" <<EOF
service: nativeapp
servers:
  web:
    hosts: ["127.0.0.1:$port"]
ssh:
  user: deploy
  key: $work/id
  strict_host_key_checking: false
env:
  clear:
    MSG: "say \"hi\" \\\\ \$HOME \`x\` 100% 'q'\nline2"
native:
  artifact: dist/app
  exec: [app]
  user: nativeapp
  arch: $goarch
  port: 8080
  healthcheck:
    path: /up
    timeout: 10s
    interval: 1s
  shared_dirs: [storage]
  retain: 3
proxy:
  host: app.test
  ssl: true
$2
${3:-}
EOF
}
write_config "$work/app/qifa.yaml" "  tls:
    source: static
    cert_path: /etc/qifa-proxy-certs/app.test.crt
    key_path: /etc/qifa-proxy-certs/app.test.key"
qifa() { "$work/qifa" -c "$work/app/qifa.yaml" "$@"; }
app_get() { on_host "curl -fsS http://127.0.0.1:8080$1"; }
# Through kamal-proxy, over TLS, by name: what a client on the internet does.
via_proxy() { on_host "curl -fsS -k --resolve app.test:443:127.0.0.1 https://app.test$1"; }
proxy_status() { on_host "curl -s -k -o /dev/null -w '%{http_code}' --resolve app.test:443:127.0.0.1 https://app.test$1"; }

step "deploy before setup is refused without touching the host"
build_app v1
if qifa deploy --version v1 >"$work/out" 2>&1; then cat "$work/out"; fail "deploy before setup succeeded"; fi
grep -q "qifa native setup" "$work/out" || { cat "$work/out"; fail "error does not point at native setup"; }
on_host '[ ! -e /opt/qifa/nativeapp/current ]' || fail "deploy before setup changed the host"

step "native setup (as root), twice: idempotent"
qifa native setup --ssh-user root
qifa native setup --ssh-user root >"$work/out" 2>&1
if grep -q "installed\|created" "$work/out"; then cat "$work/out"; fail "second setup changed something"; fi

step "deploy is refused until the proxy runs"
if qifa deploy --version v1 >"$work/out" 2>&1; then cat "$work/out"; fail "deploy without a proxy succeeded"; fi
grep -q "qifa proxy boot" "$work/out" || { cat "$work/out"; fail "error does not point at proxy boot"; }

step "proxy boot: kamal-proxy as a systemd service (built from source), twice: idempotent"
qifa proxy boot --ssh-user root
qifa proxy boot --ssh-user root >"$work/out" 2>&1
if grep -q "installed\|created" "$work/out"; then cat "$work/out"; fail "second proxy boot changed something"; fi
on_host 'systemctl is-active --quiet qifa-proxy.service' || fail "proxy not running"
on_host '/opt/qifa/proxy/bin/kamal-proxy version' | grep -q "v0.10" || fail "proxy is not the pinned latest version"
on_host 'stat -c %U /proc/$(systemctl show -p MainPID --value qifa-proxy.service)' | grep -qx qifa-proxy || fail "proxy does not run as qifa-proxy"
openssl req -x509 -newkey rsa:2048 -nodes -subj /CN=app.test -addext subjectAltName=DNS:app.test -days 1 \
  -keyout "$work/app.test.key" -out "$work/app.test.crt" 2>/dev/null
on_host 'install -d -m 0750 -o root -g qifa-proxy /etc/qifa-proxy-certs'
docker cp "$work/app.test.crt" "$name:/etc/qifa-proxy-certs/app.test.crt"
docker cp "$work/app.test.key" "$name:/etc/qifa-proxy-certs/app.test.key"
on_host 'chown root:qifa-proxy /etc/qifa-proxy-certs/* && chmod 0640 /etc/qifa-proxy-certs/*'

step "deploy v1"
qifa deploy --version v1
expect_eq "$(via_proxy /version)" v1 "via kamal-proxy over TLS after v1"
expect_eq "$(app_get /version)" v1 "version after v1"
expected=$(printf 'say "hi" \\ $HOME `x` 100%% '"'"'q'"'"'\nline2')
expect_eq "$(app_get /env)" "$expected" "env value through systemd EnvironmentFile"
expect_eq "$(app_get /write)" wrote "write to shared dir"
on_host 'cat /opt/qifa/nativeapp/shared/storage/probe' | grep -qx v1 || fail "shared dir write not persisted"
on_host '[ -f /opt/qifa/nativeapp/releases/v1/.qifa-ok ]' || fail "v1 not marked known-good"

step "deploy v2"
build_app v2
qifa deploy --version v2
expect_eq "$(app_get /version)" v2 "version after v2"
expect_eq "$(via_proxy /version)" v2 "via kamal-proxy after v2"
expect_eq "$(on_host 'readlink /opt/qifa/nativeapp/current')" releases/v2 "current after v2"

step "redeploy v2 reuses the release"
qifa deploy --version v2 >"$work/out" 2>&1 || { cat "$work/out"; fail "redeploy of v2 failed"; }
grep -q "same contents" "$work/out" || { cat "$work/out"; fail "identical rebuild of v2 not reused"; }

step "deploy v3 that crashes: rolled back to v2"
printf '#!/bin/sh\necho boom >&2\nexit 1\n' > "$work/app/dist/app"
chmod +x "$work/app/dist/app"
if qifa deploy --version v3 >"$work/out" 2>&1; then cat "$work/out"; fail "crashing v3 deployed"; fi
grep -q "restored releases/v2" "$work/out" || { cat "$work/out"; fail "no rollback reported"; }
grep -q "boom" "$work/out" || { cat "$work/out"; fail "journal not attached to the error"; }
expect_eq "$(on_host 'readlink /opt/qifa/nativeapp/current')" releases/v2 "current after failed v3"
expect_eq "$(app_get /version)" v2 "version after failed v3"
expect_eq "$(via_proxy /version)" v2 "via kamal-proxy after failed v3"
expect_eq "$(app_get /env)" "$expected" "env after rollback"
on_host '[ ! -f /opt/qifa/nativeapp/releases/v3/.qifa-ok ]' || fail "failed v3 marked known-good"

step "privilege boundaries"
on_host "su deploy -c 'sudo -n systemctl restart ssh' 2>/dev/null" && fail "deploy user may restart other units"
on_host "su deploy -c 'sudo -n sh -c true' 2>/dev/null" && fail "deploy user has a root shell"
on_host "su deploy -c 'sudo -n systemctl daemon-reload' 2>/dev/null" && fail "deploy user may reload units"
on_host "su -s /bin/sh nativeapp -c 'cat /opt/qifa/nativeapp/etc/env' 2>/dev/null" && fail "service account can read the env file"
on_host "su -s /bin/sh nativeapp -c 'touch /opt/qifa/nativeapp/current/x' 2>/dev/null" && fail "service account can write its release"

step "restart, logs, lock, Docker-only commands"
qifa restart
expect_eq "$(app_get /version)" v2 "version after restart"
qifa logs -n 20 | grep -q "native-app v2" || fail "logs do not show the app"
qifa lock status | grep -q free || fail "lock left held"
if qifa sweep >"$work/out" 2>&1; then fail "sweep ran in native mode"; fi
grep -q "not available when the config has native" "$work/out" || { cat "$work/out"; fail "unclear Docker-only error"; }

step "status shows live, known-good and rollback target"
qifa status >"$work/out"
grep -q "current  v2$" "$work/out" || { cat "$work/out"; fail "status current"; }
grep -q "releases v3 v2\* v1\*" "$work/out" || { cat "$work/out"; fail "status releases"; }
grep -q "rollback v1" "$work/out" || { cat "$work/out"; fail "status rollback target"; }
grep -q "health   up" "$work/out" || { cat "$work/out"; fail "status health"; }
grep -q "route    app.test -> 127.0.0.1:8080 (running, tls)" "$work/out" || { cat "$work/out"; fail "status route"; }

step "maintenance mode through the proxy"
qifa app maintenance --message "back soon" >/dev/null
expect_eq "$(proxy_status /version)" 503 "status in maintenance"
qifa app live >/dev/null
expect_eq "$(proxy_status /version)" 200 "status after live"

step "proxy restart keeps routes; deploy user may restart the proxy, nothing more"
qifa proxy restart
for i in $(seq 1 20); do [ "$(proxy_status /up)" = 200 ] && break; sleep 0.25; done
expect_eq "$(via_proxy /version)" v2 "route after proxy restart"
qifa proxy details | grep -q "app.test" || fail "proxy details lacks the route"
on_host "su deploy -c 'sudo -n systemctl stop ssh' 2>/dev/null" && fail "deploy user may stop other units"

step "env diff: in sync, then drift by key name only"
qifa env diff | grep -q "in sync (1 keys)" || fail "env diff not in sync"
sed 's/MSG: .*/MSG: "changed-secret"/' "$work/app/qifa.yaml" > "$work/app/qifa-changed.yaml"
"$work/qifa" -c "$work/app/qifa-changed.yaml" env diff >"$work/out"
grep -q "~ MSG  (value differs)" "$work/out" || { cat "$work/out"; fail "env drift not reported"; }
grep -q "changed-secret\|say" "$work/out" && fail "env diff printed a value"

step "doctor"
qifa doctor | grep -q ": ok" || fail "doctor not ok"

step "rollback (no version) goes to the most recently healthy: v1"
qifa rollback
expect_eq "$(app_get /version)" v1 "version after rollback"
step "rollback v2 by name"
qifa rollback v2
expect_eq "$(app_get /version)" v2 "version after rollback v2"
step "rollback to never-healthy, crashing v3 is reverted"
if qifa rollback v3 >"$work/out" 2>&1; then cat "$work/out"; fail "rollback to crashing v3 succeeded"; fi
expect_eq "$(app_get /version)" v2 "version after failed rollback"
step "rollback to a missing version changes nothing"
if qifa rollback nope >"$work/out" 2>&1; then fail "rollback to missing version succeeded"; fi
grep -q "nothing was changed" "$work/out" || { cat "$work/out"; fail "missing-version message"; }

step "reconcile"
QIFA_VERSION=v2 qifa reconcile | grep -q "in sync" || fail "reconcile not in sync at v2"
QIFA_VERSION=v9 qifa reconcile -n | grep -q "live release is v2, config is v9" || fail "reconcile drift not reported"

step "interrupted deploy is reported"
on_host "su deploy -c 'cd /opt/qifa/nativeapp && ln -sfn releases/v3 current'"
qifa status | grep -q "never passed a health check" || fail "interrupted state not reported by status"
QIFA_VERSION=v2 qifa reconcile -n | grep -q "live release is v3" || fail "reconcile missed interrupted state"
qifa rollback v2
expect_eq "$(app_get /version)" v2 "recovered from interrupted state"

step "retention: retain 3 keeps the live and rollback releases"
build_app v5; qifa deploy --version v5 >/dev/null
build_app v6; qifa deploy --version v6 >/dev/null
releases=$(on_host 'ls /opt/qifa/nativeapp/releases | sort | tr "\n" " "')
expect_eq "$releases" "v3 v5 v6 " "releases after pruning"

step "automatic certificates: kamal-proxy gets one from an ACME server (Pebble)"
# kamal-proxy's ACME server is fixed: Let's Encrypt, or its staging URL with
# --tls-staging. A test build swaps that one constant for Pebble, Let's
# Encrypt's test server; everything qifa controls runs as in production: the
# pinned certificate cache, the qifa-proxy account, ports 80/443 through the
# capability. Pebble validates for real, over the test network, against
# app.test on this server.
cat > "$work/pebble.json" <<EOF
{"pebble": {"listenAddress": "0.0.0.0:14000", "managementListenAddress": "0.0.0.0:15000",
  "certificate": "/test/certs/localhost/cert.pem", "privateKey": "/test/certs/localhost/key.pem",
  "httpPort": 80, "tlsPort": 443, "ocspResponderURL": "", "externalAccountBindingRequired": false}}
EOF
docker run -d --name "$pebble" --network "$net" --network-alias pebble \
  -e PEBBLE_VA_NOSLEEP=1 -e PEBBLE_WFE_NONCEREJECT=0 \
  -v "$work/pebble.json:/pebble.json:ro" ghcr.io/letsencrypt/pebble:2.6.0 -config /pebble.json >/dev/null
# Trust the CA behind Pebble's own HTTPS endpoint, as a server trusts Let's
# Encrypt's. kamal-proxy reads the trust store when it starts, below.
docker cp "$pebble:/test/certs/pebble.minica.pem" "$work/pebble-minica.pem"
docker cp "$work/pebble-minica.pem" "$name:/usr/local/share/ca-certificates/pebble-minica.crt"
on_host 'update-ca-certificates >/dev/null 2>&1'
kp_src=$(cd "$work" && go mod download -json github.com/basecamp/kamal-proxy@v0.10.2 | sed -n 's/^[[:space:]]*"Dir": "\(.*\)",$/\1/p')
[ -d "$kp_src" ] || fail "could not fetch kamal-proxy source"
cp -R "$kp_src" "$work/kamal-proxy-src" && chmod -R u+w "$work/kamal-proxy-src"
sed 's#https://acme-staging-v02.api.letsencrypt.org/directory#https://pebble:14000/dir#' \
  "$work/kamal-proxy-src/internal/server/server.go" > "$work/server.go"
mv "$work/server.go" "$work/kamal-proxy-src/internal/server/server.go"
grep -q 'https://pebble:14000/dir' "$work/kamal-proxy-src/internal/server/server.go" || fail "staging URL not patched"
(cd "$work/kamal-proxy-src" && GOOS=linux GOARCH="$goarch" CGO_ENABLED=0 GOFLAGS=-mod=mod \
  go build -trimpath -ldflags "-X github.com/basecamp/kamal-proxy/internal/version.Version=v0.10.2-pebble" \
  -o "$work/kamal-proxy-pebble" ./cmd/kamal-proxy)
write_config "$work/app/qifa-acme.yaml" "  tls:
    source: kamal
    staging: true" "proxy_boot:
  binary: $work/kamal-proxy-pebble"
acme() { "$work/qifa" -c "$work/app/qifa-acme.yaml" "$@"; }
acme proxy upgrade --ssh-user root
on_host '/opt/qifa/proxy/bin/kamal-proxy version' | grep -q pebble || fail "test proxy not installed"
acme deploy --version v6 >/dev/null
on_host 'curl -fsS https://pebble:15000/roots/0 > /tmp/pebble-root.pem' || fail "cannot fetch Pebble's issuing root"
# The first TLS handshake for app.test makes kamal-proxy order the certificate.
issued=""
for i in $(seq 1 30); do
  if out=$(on_host "curl -fsS --cacert /tmp/pebble-root.pem --resolve app.test:443:127.0.0.1 https://app.test/version" 2>/dev/null); then
    issued=$out; break
  fi
  sleep 1
done
if [ "$issued" != v6 ]; then
  echo "--- kamal-proxy ---"; on_host 'journalctl -u qifa-proxy.service -n 30 --no-pager' || true
  echo "--- pebble ---"; docker logs --tail 30 "$pebble" 2>&1 || true
  echo "--- curl ---"; on_host "curl -sS -v --cacert /tmp/pebble-root.pem --resolve app.test:443:127.0.0.1 https://app.test/version" 2>&1 | tail -15 || true
fi
expect_eq "$issued" v6 "HTTPS with a certificate issued by the ACME server"
on_host 'find /var/lib/qifa-proxy/.config/kamal-proxy/certs -type f -user qifa-proxy | grep -q .' \
  || fail "no certificate in the proxy's pinned cache, owned by qifa-proxy"
on_host '! ls /home/deploy/.config/kamal-proxy/certs/* >/dev/null 2>&1' \
  || fail "ACME data landed in the deploy user's home"
# Back to the stock proxy and the static certificate for the rest of the run.
qifa proxy upgrade --ssh-user root >/dev/null
qifa deploy --version v6 >/dev/null
expect_eq "$(via_proxy /version)" v6 "static certificate again"

step "remove: plain remove points at native remove; teardown keeps data; purge deletes it"
if qifa remove >"$work/out" 2>&1; then fail "qifa remove succeeded without root"; fi
grep -q "qifa native remove" "$work/out" || { cat "$work/out"; fail "remove hint"; }
qifa native remove --ssh-user root
on_host '[ ! -e /etc/systemd/system/qifa-nativeapp.service ] && [ ! -e /etc/sudoers.d/qifa-nativeapp ]' || fail "units or sudoers left"
qifa proxy details | grep -q "app.test" && fail "route left on the proxy after remove"
on_host '[ -d /opt/qifa/nativeapp/shared/storage ]' || fail "plain remove deleted data"
on_host 'curl -fsS http://127.0.0.1:8080/up' 2>/dev/null && fail "app still running after remove"
echo wrong | qifa native remove --ssh-user root --purge >"$work/out" 2>&1 && fail "purge without confirmation"
on_host '[ -d /opt/qifa/nativeapp ]' || fail "unconfirmed purge deleted data"
echo nativeapp | qifa native remove --ssh-user root --purge
on_host '[ ! -e /opt/qifa/nativeapp ] && ! id nativeapp 2>/dev/null' || fail "purge left data or user"

step "proxy remove --purge"
echo qifa-proxy | qifa proxy remove --ssh-user root --purge
on_host '[ ! -e /etc/systemd/system/qifa-proxy.service ] && [ ! -e /var/lib/qifa-proxy ] && ! id qifa-proxy 2>/dev/null' || fail "proxy purge left state"
on_host 'curl -s -o /dev/null --max-time 2 http://127.0.0.1/' && fail "port 80 still served"

printf '\nPASS: native e2e\n'
