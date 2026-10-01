#!/usr/bin/env bash
set -Eeuo pipefail

BASE_DIR="${BASE_DIR:-/opt/cliproxy-cluster}"
COMPOSE_FILE="$BASE_DIR/docker-compose.yml"
ENV_FILE="$BASE_DIR/.env"
BOOTSTRAP_MARKER="$BASE_DIR/home-data/.bootstrap-complete"

HEARTBEAT_TIMEOUT_SECONDS="${HEARTBEAT_TIMEOUT_SECONDS:-20}"
WORKER_START_DELAY_SECONDS="${WORKER_START_DELAY_SECONDS:-25}"
ACTION="${1:-deploy}"
ACTION_ARG="${2:-}"

log() {
  printf '\n[%s] %s\n' "$(date '+%F %T')" "$*"
}

fail() {
  printf '\nERROR: %s\n' "$*" >&2
  exit 1
}

compose() {
  docker compose --env-file "$ENV_FILE" -f "$COMPOSE_FILE" "$@"
}

require_root() {
  if [[ "${EUID}" -ne 0 ]]; then
    fail "Jalankan sebagai root: sudo bash $0 ${ACTION}"
  fi
}

require_commands() {
  local missing=()
  local command_name

  for command_name in docker curl jq openssl awk sed grep sha256sum; do
    if ! command -v "$command_name" >/dev/null 2>&1; then
      missing+=("$command_name")
    fi
  done

  if ! docker compose version >/dev/null 2>&1; then
    missing+=("docker-compose-plugin")
  fi

  if ((${#missing[@]} > 0)); then
    fail "Dependency belum tersedia: ${missing[*]}. Install Docker Engine, Docker Compose plugin, curl, jq, dan openssl terlebih dahulu."
  fi
}

require_existing_installation() {
  [[ -f "$ENV_FILE" ]] || fail "File $ENV_FILE tidak ditemukan. Jalankan '$0 deploy' terlebih dahulu."
  [[ -f "$COMPOSE_FILE" ]] || fail "File $COMPOSE_FILE tidak ditemukan. Jalankan '$0 deploy' terlebih dahulu."
}

random_hex() {
  openssl rand -hex "$1"
}

read_env_value() {
  local key="$1"
  grep -E "^${key}=" "$ENV_FILE" | tail -n1 | cut -d= -f2-
}

set_env_value() {
  local key="$1"
  local value="$2"

  if grep -qE "^${key}=" "$ENV_FILE"; then
    sed -i "s|^${key}=.*|${key}=${value}|" "$ENV_FILE"
  else
    printf '%s=%s\n' "$key" "$value" >> "$ENV_FILE"
  fi
}

wait_for_postgres() {
  local attempt

  for attempt in $(seq 1 60); do
    if compose exec -T postgres \
      pg_isready \
      -U "$(read_env_value POSTGRES_USER)" \
      -d "$(read_env_value POSTGRES_DB)" >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
  done

  compose logs --tail=100 postgres || true
  fail "PostgreSQL tidak menjadi ready."
}

home_is_ready() {
  local management_key="$1"

  curl -fsS \
    --connect-timeout 2 \
    --max-time 5 \
    -H "X-MANAGEMENT-KEY: ${management_key}" \
    http://127.0.0.1:8327/v0/management/nodes >/dev/null 2>&1
}

wait_for_home_seconds() {
  local management_key="$1"
  local timeout_seconds="$2"
  local elapsed=0

  while ((elapsed < timeout_seconds)); do
    if home_is_ready "$management_key"; then
      return 0
    fi

    sleep 2
    elapsed=$((elapsed + 2))
  done

  return 1
}

wait_for_home() {
  local management_key="$1"

  if wait_for_home_seconds "$management_key" 180; then
    return 0
  fi

  compose logs --tail=150 home || true
  fail "CLIProxyAPIHome tidak menjadi ready."
}

generate_home_jwt() {
  local management_key="$1"

  curl -fsS -X POST \
    http://127.0.0.1:8327/v0/management/certificates/clients \
    -H "X-MANAGEMENT-KEY: ${management_key}" \
    | jq -er '.home_jwt'
}

validate_unique_jwts() {
  local jwt_1 jwt_2 jwt_3

  jwt_1="$(read_env_value HOME_JWT_1 || true)"
  jwt_2="$(read_env_value HOME_JWT_2 || true)"
  jwt_3="$(read_env_value HOME_JWT_3 || true)"

  if [[ -n "$jwt_1" && "$jwt_1" == "$jwt_2" ]] || \
     [[ -n "$jwt_1" && "$jwt_1" == "$jwt_3" ]] || \
     [[ -n "$jwt_2" && "$jwt_2" == "$jwt_3" ]]; then
    fail "HOME_JWT_1, HOME_JWT_2, dan HOME_JWT_3 harus berbeda. Gunakan perintah repair-worker untuk worker yang bentrok."
  fi
}

write_files() {
  local postgres_password="$1"
  local management_key="$2"
  local gateway_api_key="$3"

  mkdir -p \
    "$BASE_DIR/postgres-data" \
    "$BASE_DIR/home-data" \
    "$BASE_DIR/home-logs" \
    "$BASE_DIR/home-plugins" \
    "$BASE_DIR/worker-1-home" \
    "$BASE_DIR/worker-1-logs" \
    "$BASE_DIR/worker-1-plugins" \
    "$BASE_DIR/worker-2-home" \
    "$BASE_DIR/worker-2-logs" \
    "$BASE_DIR/worker-2-plugins" \
    "$BASE_DIR/worker-3-home" \
    "$BASE_DIR/worker-3-logs" \
    "$BASE_DIR/worker-3-plugins"

  cat > "$BASE_DIR/config.yaml" <<EOF_CONFIG
host: ""
# CPA worker listener; Home uses node.port in cluster.yaml.
port: 8317

allow-host: []

remote-management:
  allow-remote: true
  secret-key: "${management_key}"
  disable-control-panel: false

auth-dir: "~/.cli-proxy-api"

api-keys:
  - "${gateway_api_key}"

debug: false
EOF_CONFIG

  cat > "$BASE_DIR/cluster.yaml" <<EOF_CLUSTER
pgsql:
  host: "postgres"
  port: 5432
  user: "cliproxy"
  password: "${postgres_password}"
  database: "cliproxy_home"
  sslmode: "disable"

node:
  external-ip: "home"
  external-port: 8327
  port: 8327
  heartbeat-interval: "5s"
  heartbeat-timeout: "${HEARTBEAT_TIMEOUT_SECONDS}s"
  event-poll-interval: "3s"
EOF_CLUSTER

  cat > "$BASE_DIR/nginx.conf" <<'EOF_NGINX'
worker_processes auto;

events {
    worker_connections 4096;
}

http {
    map $http_upgrade $connection_upgrade {
        default upgrade;
        ''      close;
    }

    upstream cliproxy_backend {
        least_conn;
        server cliproxy-1:8317 max_fails=3 fail_timeout=10s;
        server cliproxy-2:8317 max_fails=3 fail_timeout=10s;
        server cliproxy-3:8317 max_fails=3 fail_timeout=10s;
        keepalive 96;
    }

    server {
        listen 8317;
        server_name _;

        client_max_body_size 100m;

        location / {
            proxy_pass http://cliproxy_backend;
            proxy_http_version 1.1;

            proxy_set_header Host $host;
            proxy_set_header X-Real-IP $remote_addr;
            proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
            proxy_set_header X-Forwarded-Proto $scheme;
            proxy_set_header Upgrade $http_upgrade;
            proxy_set_header Connection $connection_upgrade;

            proxy_buffering off;
            proxy_cache off;
            proxy_request_buffering off;

            proxy_connect_timeout 15s;
            proxy_read_timeout 3600s;
            proxy_send_timeout 3600s;
        }
    }
}
EOF_NGINX

  cat > "$COMPOSE_FILE" <<EOF_COMPOSE
services:
  postgres:
    image: postgres:17-alpine
    container_name: cliproxy-postgres
    environment:
      POSTGRES_USER: \${POSTGRES_USER}
      POSTGRES_PASSWORD: \${POSTGRES_PASSWORD}
      POSTGRES_DB: \${POSTGRES_DB}
    volumes:
      - ./postgres-data:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U \${POSTGRES_USER} -d \${POSTGRES_DB}"]
      interval: 5s
      timeout: 5s
      retries: 20
    networks:
      - cliproxy
    restart: unless-stopped

  home:
    image: meongbego/cliphome
    container_name: cliproxy-home
    depends_on:
      postgres:
        condition: service_healthy
    ports:
      - "8327:8327"
    volumes:
      - ./cluster.yaml:/CLIProxyAPIHome/cluster.yaml:ro
      - ./home-data:/CLIProxyAPIHome/data
      - ./home-logs:/CLIProxyAPIHome/logs
      - ./home-plugins:/CLIProxyAPIHome/plugins
    networks:
      cliproxy:
        aliases:
          - home
    stop_grace_period: 30s
    restart: unless-stopped

  cliproxy-1:
    image: eceasy/cli-proxy-api:v8.0.4
    container_name: cliproxy-worker-1
    environment:
      HOME_JWT: \${HOME_JWT_1}
      WORKER_START_DELAY: "${WORKER_START_DELAY_SECONDS}"
    depends_on:
      - home
    volumes:
      - ./worker-1-home:/root/.cli-proxy-api
      - ./worker-1-logs:/CLIProxyAPI/logs
      - ./worker-1-plugins:/CLIProxyAPI/plugins
    command: >
      sh -eu -c '
        test -n "\$\$HOME_JWT" || { echo "HOME_JWT_1 kosong" >&2; exit 1; }
        echo "Menunggu \$\${WORKER_START_DELAY}s agar membership lama kedaluwarsa"
        sleep "\$\${WORKER_START_DELAY}"
        exec ./CLIProxyAPI -home-jwt "\$\$HOME_JWT"
      '
    networks:
      - cliproxy
    stop_grace_period: 30s
    restart: unless-stopped

  cliproxy-2:
    image: eceasy/cli-proxy-api:v8.0.4
    container_name: cliproxy-worker-2
    environment:
      HOME_JWT: \${HOME_JWT_2}
      WORKER_START_DELAY: "$((WORKER_START_DELAY_SECONDS + 2))"
    depends_on:
      - home
    volumes:
      - ./worker-2-home:/root/.cli-proxy-api
      - ./worker-2-logs:/CLIProxyAPI/logs
      - ./worker-2-plugins:/CLIProxyAPI/plugins
    command: >
      sh -eu -c '
        test -n "\$\$HOME_JWT" || { echo "HOME_JWT_2 kosong" >&2; exit 1; }
        echo "Menunggu \$\${WORKER_START_DELAY}s agar membership lama kedaluwarsa"
        sleep "\$\${WORKER_START_DELAY}"
        exec ./CLIProxyAPI -home-jwt "\$\$HOME_JWT"
      '
    networks:
      - cliproxy
    stop_grace_period: 30s
    restart: unless-stopped

  cliproxy-3:
    image: eceasy/cli-proxy-api:v8.0.4
    container_name: cliproxy-worker-3
    environment:
      HOME_JWT: \${HOME_JWT_3}
      WORKER_START_DELAY: "$((WORKER_START_DELAY_SECONDS + 4))"
    depends_on:
      - home
    volumes:
      - ./worker-3-home:/root/.cli-proxy-api
      - ./worker-3-logs:/CLIProxyAPI/logs
      - ./worker-3-plugins:/CLIProxyAPI/plugins
    command: >
      sh -eu -c '
        test -n "\$\$HOME_JWT" || { echo "HOME_JWT_3 kosong" >&2; exit 1; }
        echo "Menunggu \$\${WORKER_START_DELAY}s agar membership lama kedaluwarsa"
        sleep "\$\${WORKER_START_DELAY}"
        exec ./CLIProxyAPI -home-jwt "\$\$HOME_JWT"
      '
    networks:
      - cliproxy
    stop_grace_period: 30s
    restart: unless-stopped

  nginx:
    image: nginx:alpine
    container_name: cliproxy-lb
    depends_on:
      - cliproxy-1
      - cliproxy-2
      - cliproxy-3
    ports:
      - "8317:8317"
    volumes:
      - ./nginx.conf:/etc/nginx/nginx.conf:ro
    networks:
      - cliproxy
    stop_grace_period: 15s
    restart: unless-stopped

networks:
  cliproxy:
    name: cliproxy-network
EOF_COMPOSE

  chmod 600 "$ENV_FILE" "$BASE_DIR/config.yaml" "$BASE_DIR/cluster.yaml"
}

ensure_home_initialized() {
  local management_key="$1"

  compose up -d home

  if [[ -f "$BOOTSTRAP_MARKER" ]]; then
    wait_for_home "$management_key"
    return 0
  fi

  log "Mengecek apakah database Home sudah memiliki config snapshot"
  if wait_for_home_seconds "$management_key" 45; then
    touch "$BOOTSTRAP_MARKER"
    return 0
  fi

  log "Home belum terinisialisasi; menjalankan import awal satu kali"
  compose stop home >/dev/null 2>&1 || true

  compose run --rm --no-deps \
    -v "$BASE_DIR/config.yaml:/CLIProxyAPIHome/config.yaml:ro" \
    home ./CLIProxyAPIHome -import

  touch "$BOOTSTRAP_MARKER"
  compose up -d home
  wait_for_home "$management_key"
}

ensure_worker_jwts() {
  local management_key="$1"
  local worker_number current_jwt new_jwt

  for worker_number in 1 2 3; do
    current_jwt="$(read_env_value "HOME_JWT_${worker_number}" || true)"

    if [[ -z "$current_jwt" ]]; then
      log "Membuat sertifikat/JWT unik untuk worker ${worker_number}"
      new_jwt="$(generate_home_jwt "$management_key")"
      set_env_value "HOME_JWT_${worker_number}" "$new_jwt"
    fi
  done

  validate_unique_jwts
}

show_status() {
  local management_key

  require_existing_installation
  management_key="$(read_env_value MANAGEMENT_KEY)"

  compose ps
  printf '\nNode yang terhubung ke Home:\n'
  curl -fsS \
    -H "X-MANAGEMENT-KEY: ${management_key}" \
    http://127.0.0.1:8327/v0/management/nodes | jq . || true
}

start_cluster() {
  local management_key

  require_existing_installation
  management_key="$(read_env_value MANAGEMENT_KEY)"
  validate_unique_jwts

  log "Menyalakan PostgreSQL"
  compose up -d postgres
  wait_for_postgres

  log "Menyalakan CLIProxyAPIHome"
  compose up -d home
  wait_for_home "$management_key"

  log "Menyalakan worker dan Nginx"
  compose up -d cliproxy-1 cliproxy-2 cliproxy-3 nginx

  log "Worker akan reconnect setelah ${WORKER_START_DELAY_SECONDS}-$((WORKER_START_DELAY_SECONDS + 4)) detik"
  compose ps
}

stop_cluster() {
  require_existing_installation

  log "Menghentikan Nginx dan semua worker secara graceful"
  compose stop nginx cliproxy-1 cliproxy-2 cliproxy-3

  log "Menghentikan Home dan PostgreSQL"
  compose stop home postgres
}

restart_cluster() {
  local management_key

  require_existing_installation
  management_key="$(read_env_value MANAGEMENT_KEY)"
  validate_unique_jwts

  log "Menghentikan traffic masuk dan semua worker"
  compose stop nginx cliproxy-1 cliproxy-2 cliproxy-3

  log "Merestart PostgreSQL dan Home"
  compose up -d postgres
  wait_for_postgres
  compose restart home
  wait_for_home "$management_key"

  log "Menyalakan worker; startup delay mencegah konflik active membership"
  compose up -d cliproxy-1 cliproxy-2 cliproxy-3 nginx

  log "Worker akan reconnect setelah ${WORKER_START_DELAY_SECONDS}-$((WORKER_START_DELAY_SECONDS + 4)) detik"
  compose ps
}

repair_worker() {
  local worker_number="$1"
  local management_key backup_dir new_jwt

  require_existing_installation

  [[ "$worker_number" =~ ^[123]$ ]] || fail "Nomor worker harus 1, 2, atau 3."

  management_key="$(read_env_value MANAGEMENT_KEY)"

  log "Menghentikan Nginx dan worker ${worker_number}"
  compose stop nginx "cliproxy-${worker_number}" || true

  backup_dir="$BASE_DIR/worker-${worker_number}-home.backup-$(date '+%Y%m%d-%H%M%S')"
  if [[ -d "$BASE_DIR/worker-${worker_number}-home" ]]; then
    log "Membackup identity lama ke $backup_dir"
    mv "$BASE_DIR/worker-${worker_number}-home" "$backup_dir"
  fi
  mkdir -p "$BASE_DIR/worker-${worker_number}-home"
  chmod 700 "$BASE_DIR/worker-${worker_number}-home"

  if ! home_is_ready "$management_key"; then
    compose up -d postgres home
    wait_for_postgres
    wait_for_home "$management_key"
  fi

  log "Membuat JWT/certificate baru untuk worker ${worker_number}"
  new_jwt="$(generate_home_jwt "$management_key")"
  set_env_value "HOME_JWT_${worker_number}" "$new_jwt"
  validate_unique_jwts

  log "Menyalakan kembali worker ${worker_number} dan Nginx"
  compose up -d --force-recreate "cliproxy-${worker_number}" nginx

  log "Pantau dengan: cd $BASE_DIR && docker compose logs -f cliproxy-${worker_number}"
}

deploy_cluster() {
  local postgres_password management_key gateway_api_key

  mkdir -p "$BASE_DIR"

  if [[ ! -f "$ENV_FILE" ]]; then
    umask 077
    cat > "$ENV_FILE" <<EOF_ENV
POSTGRES_USER=cliproxy
POSTGRES_DB=cliproxy_home
POSTGRES_PASSWORD=$(random_hex 32)
MANAGEMENT_KEY=$(random_hex 32)
GATEWAY_API_KEY=sk-gateway-$(random_hex 24)
HOME_JWT_1=
HOME_JWT_2=
HOME_JWT_3=
EOF_ENV
  fi

  postgres_password="$(read_env_value POSTGRES_PASSWORD)"
  management_key="$(read_env_value MANAGEMENT_KEY)"
  gateway_api_key="$(read_env_value GATEWAY_API_KEY)"

  [[ -n "$postgres_password" ]] || fail "POSTGRES_PASSWORD kosong di $ENV_FILE"
  [[ -n "$management_key" ]] || fail "MANAGEMENT_KEY kosong di $ENV_FILE"
  [[ -n "$gateway_api_key" ]] || fail "GATEWAY_API_KEY kosong di $ENV_FILE"

  if ((WORKER_START_DELAY_SECONDS <= HEARTBEAT_TIMEOUT_SECONDS)); then
    fail "WORKER_START_DELAY_SECONDS harus lebih besar dari HEARTBEAT_TIMEOUT_SECONDS."
  fi

  log "Menulis konfigurasi deployment ke $BASE_DIR"
  write_files "$postgres_password" "$management_key" "$gateway_api_key"

  cd "$BASE_DIR"

  log "Memvalidasi Docker Compose"
  compose config >/dev/null

  log "Mengunduh image"
  compose pull

  log "Menyalakan PostgreSQL"
  compose up -d postgres
  wait_for_postgres

  log "Menyiapkan CLIProxyAPIHome"
  ensure_home_initialized "$management_key"

  ensure_worker_jwts "$management_key"

  log "Menyalakan tiga worker dan Nginx load balancer"
  compose up -d cliproxy-1 cliproxy-2 cliproxy-3 nginx

  log "Worker akan reconnect setelah ${WORKER_START_DELAY_SECONDS}-$((WORKER_START_DELAY_SECONDS + 4)) detik"

  log "Status container"
  compose ps

  cat <<EOF_RESULT

============================================================
SELESAI
============================================================
Gateway/LB       : http://IP-SERVER:8317
Management lokal: http://127.0.0.1:8327/management.html
Management key  : ${management_key}
Gateway API key : ${gateway_api_key}
Directory       : ${BASE_DIR}

Perintah operasional:
  sudo bash $0 restart
  sudo bash $0 stop
  sudo bash $0 start
  sudo bash $0 status
  sudo bash $0 repair-worker 3

Tes dari server setelah worker selesai startup:
  curl -sS http://127.0.0.1:8317/v1/models \\
    -H 'Authorization: Bearer ${gateway_api_key}' | jq

Catatan:
- HOME_JWT dan identity worker dipertahankan pada bind mount masing-masing.
- Worker menunggu lebih lama daripada heartbeat-timeout sebelum reconnect.
- Import konfigurasi PostgreSQL hanya dilakukan saat bootstrap pertama.
- Jangan menghapus worker-*-home, home-data, atau postgres-data.
============================================================
EOF_RESULT
}

main() {
  require_root
  require_commands

  case "$ACTION" in
    deploy|install)
      deploy_cluster
      ;;
    restart)
      restart_cluster
      ;;
    start)
      start_cluster
      ;;
    stop)
      stop_cluster
      ;;
    status)
      show_status
      ;;
    repair-worker)
      [[ -n "$ACTION_ARG" ]] || fail "Gunakan: $0 repair-worker <1|2|3>"
      repair_worker "$ACTION_ARG"
      ;;
    *)
      fail "Action tidak dikenal: $ACTION. Gunakan deploy, restart, start, stop, status, atau repair-worker."
      ;;
  esac
}

main "$@"
