#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
E2E_DIR="$ROOT_DIR/test/e2e"
TMP_DIR="$E2E_DIR/.tmp"
SSH_DIR="$TMP_DIR/ssh"
BIN_DIR="$TMP_DIR/bin"
RUN_ROOT=${RUN_ROOT:-$(mktemp -d /tmp/qifa-zot-e2e-XXXXXX)}
WEB_DIR="$RUN_ROOT/web-run"
WORKER_DIR="$RUN_ROOT/worker-run"
COMPOSE_FILE="$E2E_DIR/docker-compose.yml"
REGISTRY_HOST="zottest:5001"
REGISTRY_USER="testuser"
REGISTRY_PASSWORD_VALUE="testpass"
# Host ports, overridable when the defaults are taken on this machine. Inside
# the compose network the registry is always zottest:5001.
ZOT_PORT=${QIFA_E2E_ZOT_PORT:-5001}
SSH_PORT=${QIFA_E2E_SSH_PORT:-2222}
DOCKER_PORT=${QIFA_E2E_DOCKER_PORT:-23750}
HTTP_PORT=${QIFA_E2E_HTTP_PORT:-8080}
export QIFA_E2E_ZOT_PORT=$ZOT_PORT QIFA_E2E_SSH_PORT=$SSH_PORT QIFA_E2E_DOCKER_PORT=$DOCKER_PORT QIFA_E2E_HTTP_PORT=$HTTP_PORT

# The script later points DOCKER_HOST at the deploy host's inner daemon and
# moves HOME; teardown must talk to the outer daemon that runs the compose
# project, with the original HOME (Docker Desktop's context lives there).
ORIG_HOME=$HOME
ORIG_DOCKER_HOST=${DOCKER_HOST:-}

cleanup() {
  if [[ "${KEEP_E2E_ENV:-0}" != "1" ]]; then
    (
      export HOME="$ORIG_HOME"
      if [[ -n "$ORIG_DOCKER_HOST" ]]; then export DOCKER_HOST="$ORIG_DOCKER_HOST"; else unset DOCKER_HOST; fi
      docker compose -f "$COMPOSE_FILE" down -v >/dev/null 2>&1 || true
    )
  fi
  if [[ "${KEEP_E2E_ENV:-0}" != "1" ]]; then
    rm -rf "$RUN_ROOT"
  fi
}

trap cleanup EXIT

mkdir -p "$SSH_DIR" "$BIN_DIR" "$WEB_DIR" "$WORKER_DIR"

if [[ ! -f "$SSH_DIR/id_ed25519" ]]; then
  ssh-keygen -q -t ed25519 -N "" -f "$SSH_DIR/id_ed25519"
fi
cp "$SSH_DIR/id_ed25519.pub" "$SSH_DIR/authorized_keys"

docker compose -f "$COMPOSE_FILE" down -v >/dev/null 2>&1 || true
docker compose -f "$COMPOSE_FILE" up -d --build zot deploy-host

wait_for() {
  local label=$1
  local command=$2
  local attempts=${3:-60}
  local delay=${4:-1}

  for _ in $(seq 1 "$attempts"); do
    if bash -lc "$command" >/dev/null 2>&1; then
      return 0
    fi
    sleep "$delay"
  done

  echo "timed out waiting for $label" >&2
  return 1
}

wait_for "zot registry" "curl -fsS -u $REGISTRY_USER:$REGISTRY_PASSWORD_VALUE http://127.0.0.1:$ZOT_PORT/v2/"
wait_for "deploy host ssh" "ssh-keyscan -p $SSH_PORT 127.0.0.1 >/dev/null 2>&1"
wait_for "deploy host docker daemon" "unset DOCKER_TLS_VERIFY DOCKER_CERT_PATH DOCKER_TLS_CERTDIR; DOCKER_HOST=tcp://127.0.0.1:$DOCKER_PORT docker info"

HOME_DIR="$TMP_DIR/home"
mkdir -p "$HOME_DIR/.ssh"
cp "$SSH_DIR/id_ed25519" "$HOME_DIR/.ssh/id_ed25519"
chmod 600 "$HOME_DIR/.ssh/id_ed25519"
ssh-keyscan -p $SSH_PORT 127.0.0.1 > "$HOME_DIR/.ssh/known_hosts"
printf 'zottest 127.0.0.1\n' > "$TMP_DIR/hosts.aliases"

export HOME="$HOME_DIR"
export DOCKER_HOST="tcp://127.0.0.1:$DOCKER_PORT"
export HOSTALIASES="$TMP_DIR/hosts.aliases"
export REGISTRY_PASSWORD="$REGISTRY_PASSWORD_VALUE"
unset DOCKER_TLS_CERTDIR
unset DOCKER_TLS_VERIFY
unset DOCKER_CERT_PATH

go build -o "$BIN_DIR/qifa" "$ROOT_DIR/cmd/qifa"

cat > "$WEB_DIR/qifa.yaml" <<EOF
service: demo-web
image: $REGISTRY_HOST/demo-web

servers:
  web:
    hosts:
      - 127.0.0.1:$SSH_PORT
    port: 3000

# The shared proxy container is the operator's to launch (qifa proxy boot);
# app deploys only verify it is running.
proxy_boot:
  hosts:
    - 127.0.0.1:$SSH_PORT

proxy:
  host: localhost
  app_port: 3000
  healthcheck:
    path: /up
    interval: 2s
    timeout: 5s

registry:
  server: $REGISTRY_HOST
  username: $REGISTRY_USER
  password_env: REGISTRY_PASSWORD

env:
  clear:
    APP_ENV: test
    APP_VERSION: dev
  secret: []

builder:
  context: $E2E_DIR/demo-app
  dockerfile: $E2E_DIR/demo-app/Dockerfile

ssh:
  user: root
  key: ~/.ssh/id_ed25519
EOF

cat > "$WORKER_DIR/qifa.yaml" <<EOF
service: demo-worker
image: $REGISTRY_HOST/demo-worker

servers:
  worker:
    hosts:
      - 127.0.0.1:$SSH_PORT

proxy:
  host: localhost
  app_port: 3000
  healthcheck:
    path: /up
    interval: 2s
    timeout: 5s

registry:
  server: $REGISTRY_HOST
  username: $REGISTRY_USER
  password_env: REGISTRY_PASSWORD

env:
  clear:
    APP_ENV: test
  secret: []

builder:
  context: $E2E_DIR/demo-app
  dockerfile: $E2E_DIR/demo-app/Dockerfile

ssh:
  user: root
  key: ~/.ssh/id_ed25519
EOF

(
  cd "$WEB_DIR"
  "$BIN_DIR/qifa" proxy boot
  "$BIN_DIR/qifa" deploy
  curl -fsS -H 'Host: localhost' http://127.0.0.1:$HTTP_PORT/up
  curl -fsS -H 'Host: localhost' http://127.0.0.1:$HTTP_PORT/version
  "$BIN_DIR/qifa" deploy
  curl -fsS -H 'Host: localhost' http://127.0.0.1:$HTTP_PORT/up
  "$BIN_DIR/qifa" status
  "$BIN_DIR/qifa" logs
  "$BIN_DIR/qifa" app exec "echo web-ok"
)

# Zero downtime: requests through kamal-proxy must not fail while the web
# role moves between versions, on deploy and on rollback. One client sends
# requests back to back for the whole operation; each response records the
# version that answered, so the check also proves the switch happened under
# load rather than before or after it. Versions must differ: redeploying the
# same version replaces the container by name, which is not zero-downtime.
LOAD_LOG="$RUN_ROOT/load.log"
LOAD_STOP="$RUN_ROOT/load.stop"

set_app_version() {
  sed -i.bak "s/^    APP_VERSION: .*/    APP_VERSION: $1/" "$WEB_DIR/qifa.yaml"
}

load_start() {
  : > "$LOAD_LOG"
  rm -f "$LOAD_STOP"
  (
    while [[ ! -f "$LOAD_STOP" ]]; do
      r=$(curl -s --max-time 3 -H 'Host: localhost' -w ' %{http_code}' "http://127.0.0.1:$HTTP_PORT/version" 2>/dev/null || true)
      printf '%s\n' "$(printf '%s' "$r" | tr -d '\n')" >> "$LOAD_LOG"
    done
  ) &
  LOAD_PID=$!
}

# load_check <from> <to> <what>
load_check() {
  local from=$1 to=$2 what=$3
  sleep 2 # keep requesting past the switch
  touch "$LOAD_STOP"
  wait "$LOAD_PID"
  local total failed seen
  total=$(wc -l < "$LOAD_LOG" | tr -d ' ')
  failed=$(grep -vc ' 200$' "$LOAD_LOG" || true)
  seen=$(grep -o '"version":"[^"]*"' "$LOAD_LOG" | cut -d'"' -f4 | uniq | paste -sd' ' -)
  echo "zero downtime ($what): $total requests, $failed failed, versions: $seen"
  if [[ "$failed" != 0 ]]; then
    echo "FAIL: $failed request(s) failed during $what (count, status; 000 = no response):" >&2
    grep -v ' 200$' "$LOAD_LOG" | awk '{print $NF}' | sort | uniq -c >&2
    exit 1
  fi
  if [[ "$total" -lt 20 ]]; then
    echo "FAIL: only $total requests during $what; the check proves nothing" >&2
    exit 1
  fi
  if [[ "$seen" != "$from $to" ]]; then
    echo "FAIL: during $what expected versions \"$from $to\", saw \"$seen\"" >&2
    exit 1
  fi
}

(
  cd "$WEB_DIR"
  set_app_version zd1
  "$BIN_DIR/qifa" deploy --version zd1
  curl -fsS -H 'Host: localhost' "http://127.0.0.1:$HTTP_PORT/version" | grep -q '"zd1"'

  load_start
  set_app_version zd2
  "$BIN_DIR/qifa" deploy --version zd2
  load_check zd1 zd2 "deploy"

  # A rollback renders today's env, so put the old value back first.
  load_start
  set_app_version zd1
  "$BIN_DIR/qifa" rollback zd1
  load_check zd2 zd1 "rollback"
)

(
  cd "$WORKER_DIR"
  "$BIN_DIR/qifa" deploy
  sleep 1
  "$BIN_DIR/qifa" deploy
  "$BIN_DIR/qifa" rollback
  "$BIN_DIR/qifa" status
  "$BIN_DIR/qifa" logs
  "$BIN_DIR/qifa" app exec "echo worker-ok"
)

docker --host "$DOCKER_HOST" ps
docker --host "$DOCKER_HOST" images | grep "$REGISTRY_HOST/demo"

echo "zot e2e test completed"
if [[ "${KEEP_E2E_ENV:-0}" == "1" ]]; then
  echo "environment kept running"
fi
