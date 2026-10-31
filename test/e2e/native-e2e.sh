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
dns=qifa-native-e2e-dns
net=qifa-native-e2e-net
port=${QIFA_NATIVE_E2E_PORT:-2299}

cleanup() {
  status=$?
  if [ "$status" != 0 ]; then
    echo "--- journal (last 60 lines) ---"
    docker exec "$name" journalctl -n 60 --no-pager 2>/dev/null || true
  fi
  docker rm -f "$name" "$pebble" "$dns" >/dev/null 2>&1 || true
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
docker rm -f "$name" "$pebble" "$dns" >/dev/null 2>&1 || true
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
    cmd: [app]
    app_port: 8080
healthcheck:
  path: /up
  deploy_timeout: 10s
  interval: 1s
prune:
  retain: 3
ssh:
  user: deploy
  key: $work/id
  strict_host_key_checking: false
env:
  clear:
    MSG: "say \"hi\" \\\\ \$HOME \`x\` 100% 'q'\nline2"
native:
  artifact: dist/app
  user: nativeapp
  arch: $goarch
  shared_dirs: [storage]
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
grep -q "qifa setup --ssh-user root" "$work/out" || { cat "$work/out"; fail "error does not point at qifa setup"; }
on_host '[ ! -e /opt/qifa/nativeapp/current ]' || fail "deploy before setup changed the host"

step "setup (as root), twice: idempotent; the old name still works"
qifa setup --ssh-user root
# The pre-unification spelling runs the same setup, and says it was renamed.
qifa native setup --ssh-user root >"$work/out" 2>&1
if grep -q "installed\|created" "$work/out"; then cat "$work/out"; fail "second setup changed something"; fi
grep -q "qifa native setup is now qifa setup" "$work/out" || { cat "$work/out"; fail "no rename notice"; }

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

step "app exec: as the service account, in the live release, with the app's env"
qifa app exec 'id -un; pwd -P; printf "%s" "$MSG"' > "$work/out"
expect_eq "$(sed -n 1p "$work/out")" nativeapp "app exec user"
expect_eq "$(sed -n 2p "$work/out")" /opt/qifa/nativeapp/releases/v2 "app exec directory"
expect_eq "$(sed -n '3,$p' "$work/out")" "$expected" "app exec env"
on_host "su deploy -c 'sudo -n -u nativeapp true'" || fail "deploy user may not run commands as the service account"
on_host "su deploy -c 'sudo -n -u root true' 2>/dev/null" && fail "deploy user may run commands as root"
qifa lock status | grep -q free || fail "lock left held"
if qifa sweep >"$work/out" 2>&1; then fail "sweep ran in native mode"; fi
grep -q "not available when the config has native" "$work/out" || { cat "$work/out"; fail "unclear Docker-only error"; }

step "status shows live, known-good and rollback target"
qifa status >"$work/out"
grep -q "^  web      v2  active  qifa-nativeapp.service$" "$work/out" || { cat "$work/out"; fail "status role"; }
grep -q "versions v3 v2\* v1\*" "$work/out" || { cat "$work/out"; fail "status versions"; }
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
qifa prune
expect_eq "$(on_host 'ls /opt/qifa/nativeapp/releases | sort | tr "\n" " "')" "v3 v5 v6 " "qifa prune keeps the same releases"

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
# Pebble's own DNS server answers app.test with this server's address (for
# validation here) and serves the TXT records lego sets below (DNS-01). No
# AAAA answer: its default, ::1, would send Pebble to itself.
host_ip=$(docker inspect -f "{{(index .NetworkSettings.Networks \"$net\").IPAddress}}" "$name")
docker run -d --name "$dns" --network "$net" --network-alias challtestsrv \
  ghcr.io/letsencrypt/pebble-challtestsrv:2.6.0 -defaultIPv4 "$host_ip" -defaultIPv6 "" -dns01 :8053 -management :8055 >/dev/null
docker run -d --name "$pebble" --network "$net" --network-alias pebble \
  -e PEBBLE_VA_NOSLEEP=1 -e PEBBLE_WFE_NONCEREJECT=0 \
  -v "$work/pebble.json:/pebble.json:ro" ghcr.io/letsencrypt/pebble:2.6.0 -config /pebble.json -dnsserver challtestsrv:8053 >/dev/null
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

step "certificates qifa issues (tls.source: qifa): lego on the server, DNS-01"
# lego's exec DNS provider calls this script on the server to set the TXT
# record in Pebble's DNS server. lego reads every flag from LEGO_* too, which
# points it at Pebble and replaces its propagation checks with a short wait
# (the test DNS server answers no SOA queries, as real DNS does).
cat > "$work/qifa-test-dns" <<'EOF'
#!/bin/sh
case "$1" in
  present) curl -fsS -d "{\"host\":\"$2\",\"value\":\"$3\"}" http://challtestsrv:8055/set-txt ;;
  cleanup) curl -fsS -d "{\"host\":\"$2\"}" http://challtestsrv:8055/clear-txt ;;
esac
EOF
docker cp "$work/qifa-test-dns" "$name:/usr/local/bin/qifa-test-dns"
on_host 'chmod 755 /usr/local/bin/qifa-test-dns'
cat > "$work/lego.env" <<EOF
EXEC_PATH=/usr/local/bin/qifa-test-dns
LEGO_SERVER=https://pebble:14000/dir
LEGO_DNS_PROPAGATION_WAIT=2s
EOF
write_config "$work/app/qifa-lego.yaml" "  tls:
    source: qifa
    provider: exec"
lego() { "$work/qifa" -c "$work/app/qifa-lego.yaml" "$@"; }
lego cert issue app.test --provider exec --email e2e@example.test \
  --env-file "$work/lego.env" --dns-resolvers challtestsrv:8053
on_host "su deploy -c 'test -x ~/.qifa/bin/lego-v5.3.1'" || fail "lego not installed in the deploy user's ~/.qifa/bin"
on_host "stat -c '%G %A' /var/lib/qifa-proxy/qifa/certificates/app.test.key" | grep -q '^qifa-proxy -rw-r-----' \
  || fail "the key is not readable by the proxy group and nobody else"
on_host "su -s /bin/sh qifa-proxy -c 'ls /var/lib/qifa-proxy/qifa/accounts' 2>/dev/null" && fail "the ACME account is readable by the proxy"
on_host 'ls /dev/shm/qifa-lego-env.* 2>/dev/null' && fail "lego credentials left in /dev/shm"
lego cert list | grep -qx app.test || fail "cert list"
lego deploy --version v6 >/dev/null
expect_eq "$(on_host "curl -fsS --cacert /tmp/pebble-root.pem --resolve app.test:443:127.0.0.1 https://app.test/version")" v6 \
  "HTTPS with the certificate lego issued"
# The leaf certificate on disk, and the one kamal-proxy serves.
# First PEM block only: curl's %{certs} puts certificate details before it.
first_pem="sed -n '/BEGIN CERTIFICATE/,/END CERTIFICATE/{p;/END CERTIFICATE/q;}'"
leaf_file() { on_host "$first_pem /var/lib/qifa-proxy/qifa/certificates/app.test.crt" | sha256sum; }
leaf_served() { on_host "curl -fsS -o /dev/null --cacert /tmp/pebble-root.pem --resolve app.test:443:127.0.0.1 -w '%{certs}' https://app.test/up | $first_pem" | sha256sum; }
before=$(leaf_file)
expect_eq "$(leaf_served)" "$before" "served certificate before renewal"
# Pebble 2.6.0 issues five-year certificates: a window longer than that
# makes this one due now.
lego cert renew --email e2e@example.test --renew-days 2000 --env-file "$work/lego.env" --dns-resolvers challtestsrv:8053
after=$(leaf_file)
[ "$before" != "$after" ] || fail "renew did not replace the certificate"
# kamal-proxy reads a certificate only when a route is deployed: renew must
# have re-registered the route for the new one to be served.
expect_eq "$(leaf_served)" "$after" "served certificate after renewal"
qifa deploy --version v6 >/dev/null
expect_eq "$(via_proxy /version)" v6 "static certificate again"

step "remove: needs root; keeps data; --purge deletes it after confirmation"
if qifa remove >"$work/out" 2>&1; then fail "qifa remove succeeded without root"; fi
grep -q "ssh-user root" "$work/out" || { cat "$work/out"; fail "remove does not say it needs root"; }
qifa remove --ssh-user root
on_host '[ ! -e /etc/systemd/system/qifa-nativeapp.service ] && [ ! -e /etc/sudoers.d/qifa-nativeapp ]' || fail "units or sudoers left"
qifa proxy details | grep -q "app.test" && fail "route left on the proxy after remove"
on_host '[ -d /opt/qifa/nativeapp/shared/storage ]' || fail "plain remove deleted data"
on_host 'curl -fsS http://127.0.0.1:8080/up' 2>/dev/null && fail "app still running after remove"
echo wrong | qifa remove --ssh-user root --purge >"$work/out" 2>&1 && fail "purge without confirmation"
on_host '[ -d /opt/qifa/nativeapp ]' || fail "unconfirmed purge deleted data"
echo nativeapp | qifa remove --ssh-user root --purge
on_host '[ ! -e /opt/qifa/nativeapp ] && ! id nativeapp 2>/dev/null' || fail "purge left data or user"

step "a program installed on the server: no artifact, its own workdir, no hardening"
mkdir -p "$work/installed"
write_installed() {
  cat > "$work/installed/qifa.yaml" <<EOF
service: installedapp
servers:
  web:
    hosts: ["127.0.0.1:$port"]
    cmd: [/usr/local/bin/qifa-test-app]
    app_port: 8080
healthcheck:
  path: /up
  deploy_timeout: 10s
  interval: 1s
ssh:
  user: deploy
  key: $work/id
  strict_host_key_checking: false
env:
  clear:
    MSG: $1
native:
  user: deploy
  workdir: /srv/installed
  hardening: false
EOF
}
write_installed one
iq() { "$work/qifa" -c "$work/installed/qifa.yaml" "$@"; }
# As on a host with no root login: print the script, run it as root there.
iq setup --print > "$work/setup.sh"
docker exec -i "$name" sh < "$work/setup.sh"
iq doctor >/dev/null 2>&1 && fail "doctor passed before the program was installed"
expect_eq "$(on_host 'stat -c %U /srv/installed')" deploy "setup creates a missing workdir for the service user"
if iq deploy >"$work/out" 2>&1; then fail "deploy without the program installed succeeded"; fi
grep -q "/usr/local/bin/qifa-test-app (servers.web.cmd) is not installed" "$work/out" || { cat "$work/out"; fail "unclear missing-program error"; }
build_app inst1
docker cp "$work/app/dist/app" "$name:/usr/local/bin/qifa-test-app"
on_host 'chmod 0755 /usr/local/bin/qifa-test-app && install -d -o deploy -g deploy /srv/installed/storage'
iq deploy
expect_eq "$(app_get /version)" inst1 "installed program runs"
expect_eq "$(app_get /env)" one "env of the installed program"
expect_eq "$(app_get /write)" wrote "write to the workdir"
expect_eq "$(on_host 'cat /srv/installed/storage/probe')" inst1 "write landed in the workdir"
expect_eq "$(on_host 'systemctl show -p ProtectSystem --value qifa-installedapp.service')" no "hardening off: ProtectSystem"
expect_eq "$(on_host 'systemctl show -p PrivateTmp --value qifa-installedapp.service')" no "hardening off: PrivateTmp"
expect_eq "$(iq app exec 'pwd -P')" /srv/installed "app exec runs in the workdir"
on_host '[ ! -e /opt/qifa/installedapp/current ]' || fail "an installed program got a release"

step "installed program: env change, status, no rollback, reconcile"
write_installed two
iq deploy --version e2
expect_eq "$(app_get /env)" two "env after redeploy"
iq status >"$work/out"
grep -q "^  web      e2  active  qifa-installedapp.service$" "$work/out" || { cat "$work/out"; fail "installed status role"; }
grep -q "runs /usr/local/bin/qifa-test-app, installed on the server" "$work/out" || { cat "$work/out"; fail "installed status note"; }
if iq rollback >"$work/out" 2>&1; then fail "rollback of an installed program succeeded"; fi
grep -q "nothing to roll back to" "$work/out" || { cat "$work/out"; fail "unclear rollback refusal"; }
QIFA_VERSION=e2 iq reconcile | grep -q "in sync" || fail "installed reconcile not in sync"
QIFA_VERSION=e3 iq reconcile -n | grep -q "deployed e2, config is e3" || fail "installed reconcile drift not reported"

step "installed program: purge keeps the program, the workdir and the login user"
echo installedapp | iq remove --ssh-user root --purge >"$work/out"
grep -q "kept user deploy" "$work/out" || { cat "$work/out"; fail "purge did not say it kept the user"; }
on_host 'id deploy >/dev/null && [ -x /usr/local/bin/qifa-test-app ] && [ -f /srv/installed/storage/probe ] && [ ! -e /opt/qifa/installedapp ]' || fail "purge of an installed program removed too much or too little"
on_host 'curl -fsS http://127.0.0.1:8080/up' 2>/dev/null && fail "installed program still running after remove"

step "proxy remove --purge"
echo qifa-proxy | qifa proxy remove --ssh-user root --purge
on_host '[ ! -e /etc/systemd/system/qifa-proxy.service ] && [ ! -e /var/lib/qifa-proxy ] && ! id qifa-proxy 2>/dev/null' || fail "proxy purge left state"
on_host 'curl -s -o /dev/null --max-time 2 http://127.0.0.1/' && fail "port 80 still served"

printf '\nPASS: native e2e\n'
