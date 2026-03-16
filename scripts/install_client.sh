#!/usr/bin/env bash
set -euo pipefail

REPO="ctsunny/bwtest"
APP_USER="bwtest"
APP_GROUP="bwtest"
BIN_NAME="bwagent"
INSTALL_DIR="/opt/bwtest"
BIN_PATH="/usr/local/bin/${BIN_NAME}"
CONFIG_DIR="/etc/bwagent"
CONFIG_FILE="${CONFIG_DIR}/config.json"
ENV_FILE="/etc/default/${BIN_NAME}"
SERVICE_FILE="/etc/systemd/system/${BIN_NAME}.service"

SERVER_URL=""
INIT_TOKEN=""
CLIENT_NAME=""
VERSION="latest"

log() { echo -e "\033[1;32m[INFO]\033[0m $*"; }
err() { echo -e "\033[1;31m[ERR ]\033[0m $*" >&2; }

usage() {
  cat <<EOF
Usage:
  bash install_client.sh [options]

Options:
  --server-url     Required, e.g. http://1.2.3.4:8080
  --init-token     Required
  --client-name    Default hostname
  --version        Default latest
EOF
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
      --server-url) SERVER_URL="${2:-}"; shift 2 ;;
      --init-token) INIT_TOKEN="${2:-}"; shift 2 ;;
      --client-name) CLIENT_NAME="${2:-}"; shift 2 ;;
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

write_config() {
  [[ -n "${CLIENT_NAME}" ]] || CLIENT_NAME="$(hostname)"
  mkdir -p "${CONFIG_DIR}"

  cat > "${CONFIG_FILE}" <<EOF
{
  "server_url": "${SERVER_URL}",
  "name": "${CLIENT_NAME}",
  "init_token": "${INIT_TOKEN}",
  "client_id": "",
  "client_token": ""
}
EOF

  chmod 600 "${CONFIG_FILE}"
  chown -R "${APP_USER}:${APP_GROUP}" "${CONFIG_DIR}"
}

write_env() {
  mkdir -p /etc/default
  cat > "${ENV_FILE}" <<'EOF'
EOF
  chmod 644 "${ENV_FILE}"
}

write_service() {
  cat > "${SERVICE_FILE}" <<EOF
[Unit]
Description=Bandwidth Test Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${APP_USER}
Group=${APP_GROUP}
WorkingDirectory=${INSTALL_DIR}
EnvironmentFile=-${ENV_FILE}
ExecStart=${BIN_PATH} ${CONFIG_FILE}
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
  [[ -n "${SERVER_URL}" ]] || { err "--server-url is required"; exit 1; }
  [[ -n "${INIT_TOKEN}" ]] || { err "--init-token is required"; exit 1; }

  install_deps
  mkdir -p "${INSTALL_DIR}"
  ensure_user_group
  chown -R "${APP_USER}:${APP_GROUP}" "${INSTALL_DIR}"

  download_binary
  write_config
  write_env
  write_service

  systemctl daemon-reload
  systemctl enable --now "${BIN_NAME}"

  log "Installed successfully"
  echo
  echo "Config: ${CONFIG_FILE}"
  echo "Commands:"
  echo "  systemctl status ${BIN_NAME}"
  echo "  journalctl -u ${BIN_NAME} -f"
  echo "  cat ${CONFIG_FILE}"
}

main "$@"
