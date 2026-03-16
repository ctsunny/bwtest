#!/usr/bin/env bash
set -euo pipefail

REPO="ctsunny/bwtest"
APP_USER="bwtest"
APP_GROUP="bwtest"
BIN_NAME="bwpanel"
INSTALL_DIR="/opt/bwtest"
BIN_PATH="/usr/local/bin/${BIN_NAME}"
ENV_FILE="/etc/default/${BIN_NAME}"
SERVICE_FILE="/etc/systemd/system/${BIN_NAME}.service"

SERVER_HOST=""
PANEL_PORT="8080"
DATA_PORT="9000"
ADMIN_USER="admin"
ADMIN_PASS=""
INIT_TOKEN=""
VERSION="latest"

log() { echo -e "\033[1;32m[INFO]\033[0m $*"; }
err() { echo -e "\033[1;31m[ERR ]\033[0m $*" >&2; }

usage() {
  cat <<EOF
Usage:
  bash install_server.sh [options]

Options:
  --server-host    Required, public IP or domain
  --panel-port     Default 8080
  --data-port      Default 9000
  --admin-user     Default admin
  --admin-pass     Auto-generate if empty
  --init-token     Auto-generate if empty
  --version        Default latest
EOF
}

rand_hex() {
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex "${1:-16}"
  else
    head -c "${1:-16}" /dev/urandom | xxd -p -c 256
  fi
}

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo "amd64" ;;
    aarch64|arm64) echo "arm64" ;;
    *) err "Unsupported architecture: $(uname -m)"; exit 1 ;;
  esac
}

parse_args() {
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --server-host) SERVER_HOST="${2:-}"; shift 2 ;;
      --panel-port) PANEL_PORT="${2:-}"; shift 2 ;;
      --data-port) DATA_PORT="${2:-}"; shift 2 ;;
      --admin-user) ADMIN_USER="${2:-}"; shift 2 ;;
      --admin-pass) ADMIN_PASS="${2:-}"; shift 2 ;;
      --init-token) INIT_TOKEN="${2:-}"; shift 2 ;;
      --version) VERSION="${2:-}"; shift 2 ;;
      -h|--help) usage; exit 0 ;;
      *) err "Unknown arg: $1"; usage; exit 1 ;;
    esac
  done
}

install_deps() {
  if command -v apt-get >/dev/null 2>&1; then
    apt-get update -y
    apt-get install -y curl ca-certificates
  elif command -v dnf >/dev/null 2>&1; then
    dnf install -y curl ca-certificates shadow-utils
  elif command -v yum >/dev/null 2>&1; then
    yum install -y curl ca-certificates shadow-utils
  elif command -v apk >/dev/null 2>&1; then
    apk add --no-cache curl ca-certificates
  fi
}

ensure_user_group() {
  if ! getent group "${APP_GROUP}" >/dev/null 2>&1; then
    groupadd --system "${APP_GROUP}" 2>/dev/null || true
  fi
  if ! id -u "${APP_USER}" >/dev/null 2>&1; then
    useradd --system --gid "${APP_GROUP}" --home "${INSTALL_DIR}" --shell /usr/sbin/nologin "${APP_USER}" 2>/dev/null || \
    useradd -r -g "${APP_GROUP}" -s /sbin/nologin "${APP_USER}" 2>/dev/null || true
  fi
}

download_binary() {
  local arch asset url tmp
  arch="$(detect_arch)"
  asset="${BIN_NAME}-linux-${arch}"

  if [[ "${VERSION}" == "latest" ]]; then
    url="https://github.com/${REPO}/releases/latest/download/${asset}"
  else
    url="https://github.com/${REPO}/releases/download/${VERSION}/${asset}"
  fi

  tmp="$(mktemp)"
  log "Downloading ${url}"
  curl --proto '=https' --tlsv1.2 -fsSL "$url" -o "$tmp"
  install -m 0755 "$tmp" "${BIN_PATH}"
  rm -f "$tmp"
}

write_env_file() {
  [[ -n "${ADMIN_PASS}" ]] || ADMIN_PASS="$(rand_hex 12)"
  [[ -n "${INIT_TOKEN}" ]] || INIT_TOKEN="$(rand_hex 16)"

  mkdir -p /etc/default
  cat > "${ENV_FILE}" <<EOF
PANEL_ADDR=:${PANEL_PORT}
DATA_ADDR=:${DATA_PORT}
SERVER_HOST=${SERVER_HOST}
ADMIN_USER=${ADMIN_USER}
ADMIN_PASS=${ADMIN_PASS}
INIT_TOKEN=${INIT_TOKEN}
DB_PATH=/opt/bwtest/bwtest.db
EOF
  chmod 600 "${ENV_FILE}"
}

write_service() {
  cat > "${SERVICE_FILE}" <<EOF
[Unit]
Description=Bandwidth Test Panel
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${APP_USER}
Group=${APP_GROUP}
WorkingDirectory=${INSTALL_DIR}
EnvironmentFile=${ENV_FILE}
ExecStart=${BIN_PATH}
Restart=always
RestartSec=3
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
EOF
}

main() {
  parse_args "$@"

  [[ $EUID -eq 0 ]] || { err "Please run as root"; exit 1; }
  [[ -n "${SERVER_HOST}" ]] || { err "--server-host is required"; exit 1; }

  install_deps
  mkdir -p "${INSTALL_DIR}"
  ensure_user_group
  chown -R "${APP_USER}:${APP_GROUP}" "${INSTALL_DIR}"

  download_binary
  write_env_file
  write_service

  systemctl daemon-reload
  systemctl enable --now "${BIN_NAME}"

  log "Installed successfully"
  echo
  echo "Panel URL : http://${SERVER_HOST}:${PANEL_PORT}/admin"
  echo "Admin User: ${ADMIN_USER}"
  echo "Admin Pass: ${ADMIN_PASS}"
  echo "Init Token: ${INIT_TOKEN}"
  echo
  echo "Commands:"
  echo "  systemctl status ${BIN_NAME}"
  echo "  journalctl -u ${BIN_NAME} -f"
  echo "  cat ${ENV_FILE}"
}

main "$@"
