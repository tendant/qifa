#!/bin/sh
# End-to-end test for native deploys to a Mac (native.init: launchd), run on
# the Mac itself through the `local` host: the app becomes a real LaunchAgent
# in your login session, and is booted out and deleted at the end.
#
#   test/e2e/launchd-e2e.sh
#
# Needs macOS, a login session (run it from Terminal or over SSH while logged
# in at the console), and a free port (QIFA_LAUNCHD_E2E_PORT, default 38085).
set -eu

[ "$(uname -s)" = Darwin ] || { echo "launchd e2e runs on macOS only" >&2; exit 1; }

root=$(cd "$(dirname "$0")/../.." && pwd)
work=$(mktemp -d)
# Resolve /var -> /private/var: the paths qifa reports are the real ones.
work=$(cd "$work" && pwd -P)
port=${QIFA_LAUNCHD_E2E_PORT:-38085}
service=qifa-launchd-e2e
label=qifa.$service
dir=$work/install

cleanup() {
  status=$?
  if [ "$status" != 0 ]; then
    echo "--- launchd ---"
    launchctl print "gui/$(id -u)/$label" 2>&1 | grep -E 'state =|pid =|last exit' || true
    echo "--- log (last 40 lines) ---"
    tail -n 40 "$dir/log/$label.log" 2>/dev/null || true
  fi
  launchctl bootout "gui/$(id -u)/$label" >/dev/null 2>&1 || true
  rm -f "$HOME/Library/LaunchAgents/$label.plist"
  rm -rf "$work"
  exit "$status"
}
trap cleanup EXIT INT TERM

step() { printf '\n=== %s\n' "$*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }
expect_eq() { [ "$1" = "$2" ] || fail "$3: got [$1], want [$2]"; }
app_get() { curl -fsS "http://127.0.0.1:$port$1"; }

if curl -s -o /dev/null "http://127.0.0.1:$port/"; then
  fail "port $port is in use; set QIFA_LAUNCHD_E2E_PORT"
fi

mkdir -p "$work/app/dist"
(cd "$root" && go build -o "$work/qifa" ./cmd/qifa)
build_app() {
  rm -f "$work/app/dist/app"
  (cd "$root" && CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=$1" -o "$work/app/dist/app" ./test/e2e/native-app)
}

cat > "$work/app/qifa.yaml" <<EOF
service: $service
servers:
  web:
    hosts: [local]
    cmd: [app]
    app_port: $port
healthcheck:
  path: /up
  deploy_timeout: 15s
  interval: 1s
prune:
  retain: 3
env:
  clear:
    PORT: "$port"
    MSG: "say \"hi\" \\\\ \$HOME \`x\` 100% 'q'\nline2"
native:
  init: launchd
  artifact: dist/app
  user: $(id -un)
  dir: $dir
  shared_dirs: [storage]
EOF
qifa() { "$work/qifa" -c "$work/app/qifa.yaml" "$@"; }
expected=$(printf 'say "hi" \\ $HOME `x` 100%% '"'"'q'"'"'\nline2')

step "deploy before setup is refused without touching the host"
build_app v1
if qifa deploy --version v1 >"$work/out" 2>&1; then cat "$work/out"; fail "deploy before setup succeeded"; fi
grep -q "run \`qifa setup\`" "$work/out" || { cat "$work/out"; fail "error does not point at qifa setup"; }
[ ! -e "$dir/current" ] || fail "deploy before setup changed the host"

step "setup (no root), twice: idempotent"
qifa setup
qifa setup >"$work/out" 2>&1
if grep -q "installed\|created" "$work/out"; then cat "$work/out"; fail "second setup changed something"; fi
qifa doctor | grep -q "ok (macOS, login session" || fail "doctor"

step "deploy v1: a LaunchAgent, with the env byte for byte"
qifa deploy --version v1
expect_eq "$(app_get /version)" v1 "version after v1"
expect_eq "$(app_get /env)" "$expected" "env value through the env loader"
expect_eq "$(app_get /write)" wrote "write to shared dir"
expect_eq "$(cat "$dir/shared/storage/probe")" v1 "shared dir write"
launchctl print "gui/$(id -u)/$label" | grep -q 'state = running' || fail "not running under launchd"

step "deploy v2"
build_app v2
qifa deploy --version v2
expect_eq "$(app_get /version)" v2 "version after v2"
expect_eq "$(readlink "$dir/current")" releases/v2 "current after v2"

step "deploy v3 that crashes: rolled back to v2"
printf '#!/bin/sh\necho boom >&2\nexit 1\n' > "$work/app/dist/app"
chmod +x "$work/app/dist/app"
if qifa deploy --version v3 >"$work/out" 2>&1; then cat "$work/out"; fail "crashing v3 deployed"; fi
grep -q "restored releases/v2" "$work/out" || { cat "$work/out"; fail "no rollback reported"; }
grep -q "boom" "$work/out" || { cat "$work/out"; fail "log not attached to the error"; }
expect_eq "$(readlink "$dir/current")" releases/v2 "current after failed v3"
expect_eq "$(app_get /version)" v2 "version after failed v3"

step "launchd restarts a crashed app (restart: on-failure)"
pid=$(launchctl print "gui/$(id -u)/$label" | awk '/pid =/ {print $3; exit}')
kill -9 "$pid"
for i in $(seq 1 20); do
  app_get /up >/dev/null 2>&1 && [ "$(launchctl print "gui/$(id -u)/$label" | awk '/pid =/ {print $3; exit}')" != "$pid" ] && break
  sleep 1
done
expect_eq "$(app_get /version)" v2 "version after a crash"

step "restart, stop, start, logs, exec, status"
qifa restart
expect_eq "$(app_get /version)" v2 "version after restart"
qifa stop
if app_get /up >/dev/null 2>&1; then fail "app answers after stop"; fi
qifa start
for i in $(seq 1 15); do app_get /up >/dev/null 2>&1 && break; sleep 1; done
expect_eq "$(app_get /version)" v2 "version after start"
qifa logs -n 50 | grep -q "native-app v2" || fail "logs do not show the app"
qifa app exec 'pwd -P; printf "%s" "$MSG"' > "$work/out"
expect_eq "$(sed -n 1p "$work/out")" "$dir/releases/v2" "app exec directory"
expect_eq "$(sed -n '2,$p' "$work/out")" "$expected" "app exec env"
qifa status >"$work/out"
grep -q "v2  active  $label" "$work/out" || { cat "$work/out"; fail "status role"; }
grep -q "health   up" "$work/out" || { cat "$work/out"; fail "status health"; }

step "rollback to v1"
qifa rollback v1
expect_eq "$(app_get /version)" v1 "version after rollback"

step "remove --purge: agent unloaded, plist and install dir gone"
qifa remove --purge --yes
launchctl print "gui/$(id -u)/$label" >/dev/null 2>&1 && fail "agent still loaded"
[ ! -e "$HOME/Library/LaunchAgents/$label.plist" ] || fail "plist left behind"
[ ! -e "$dir" ] || fail "install dir left behind"

printf '\nlaunchd e2e passed\n'
