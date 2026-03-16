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
SOURCE_DIR="/opt/bwtest-src"

SERVER_URL=""
INIT_TOKEN=""
CLIENT_NAME=""
REMARK=""
VERSION="latest"

log() { echo -e "\033[1;32m[INFO]\033[0m $*"; }
err() { echo -e "\033[1;31m[ERR ]\033[0m $*" >&2; }

usage() {
  cat <<EOF
Usage:
  bash install_client.sh [options]

Options:
  --server-url     必填, e.g. http://1.2.3.4:8080
  --init-token     必填
  --client-name    默认 hostname
  --remark         备注 (可选)
  --version        默认 latest
EOF
}

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64)  echo "amd64" ;;
    aarch64|arm64) echo "arm64" ;;
    *) err "Unsupported architecture: $(uname -m)"; exit 1 ;;
  esac
}

parse_args() {
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --server-url)  SERVER_URL="${2:-}";  shift 2 ;;
      --init-token)  INIT_TOKEN="${2:-}";  shift 2 ;;
      --client-name) CLIENT_NAME="${2:-}"; shift 2 ;;
      --remark)      REMARK="${2:-}";      shift 2 ;;
      --version)     VERSION="${2:-}";     shift 2 ;;
      -h|--help) usage; exit 0 ;;
      *) err "Unknown arg: $1"; usage; exit 1 ;;
    esac
  done
}

install_deps() {
  if command -v apt-get >/dev/null 2>&1; then
    DEBIAN_FRONTEND=noninteractive apt-get update -qq 2>/dev/null || true
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends \
      curl ca-certificates 2>/dev/null || true
  elif command -v dnf >/dev/null 2>&1; then
    dnf install -y -q curl ca-certificates shadow-utils
  elif command -v yum >/dev/null 2>&1; then
    yum install -y -q curl ca-certificates shadow-utils
  elif command -v apk >/dev/null 2>&1; then
    apk add --no-cache curl ca-certificates
  fi
}

ensure_user_group() {
  getent group "${APP_GROUP}" >/dev/null 2>&1 || groupadd --system "${APP_GROUP}" 2>/dev/null || true
  id -u "${APP_USER}" >/dev/null 2>&1 || \
    useradd --system --gid "${APP_GROUP}" --home "${INSTALL_DIR}" --shell /usr/sbin/nologin "${APP_USER}" 2>/dev/null || \
    useradd -r -g "${APP_GROUP}" -s /sbin/nologin "${APP_USER}" 2>/dev/null || true
}

# 读取文件头 4 字节判断是否 ELF 二进制，不依赖 file 命令
is_elf() {
  local magic
  magic=$(od -An -N4 -tx1 "$1" 2>/dev/null | tr -d ' \n')
  [[ "${magic}" == "7f454c46" ]]
}

# 优先 Release 下载，失败则回退源码编译
download_binary() {
  local arch asset release_url tmp http_code
  arch="$(detect_arch)"
  asset="${BIN_NAME}-linux-${arch}"

  if [[ "${VERSION}" == "latest" ]]; then
    release_url="https://github.com/${REPO}/releases/latest/download/${asset}"
  else
    release_url="https://github.com/${REPO}/releases/download/${VERSION}/${asset}"
  fi

  tmp="$(mktemp)"
  log "尝试从 Release 下载预编译二进制..."
  http_code=$(curl --proto '=https' --tlsv1.2 -fsSL -w "%{http_code}" -o "${tmp}" "${release_url}" 2>/dev/null || echo "000")

  if [[ "${http_code}" == "200" ]] && is_elf "${tmp}"; then
    log "Release 下载成功"
    systemctl stop "${BIN_NAME}" 2>/dev/null || true
    install -m 0755 "${tmp}" "${BIN_PATH}"
    rm -f "${tmp}"
  else
    rm -f "${tmp}"
    log "Release 未找到或校验失败，回退至源码编译..."
    build_from_source
  fi
}

build_from_source() {
  if ! command -v go >/dev/null 2>&1; then
    log "安装 Go 运行时..."
    local go_ver="1.22.5" arch
    arch="$(detect_arch)"
    curl -fsSL "https://go.dev/dl/go${go_ver}.linux-${arch}.tar.gz" | tar -C /usr/local -xz
    export PATH="/usr/local/go/bin:${PATH}"
  fi

  log "拉取最新源码..."
  if [[ -d "${SOURCE_DIR}/.git" ]]; then
    git -C "${SOURCE_DIR}" pull --ff-only
  else
    git clone --depth 1 "https://github.com/${REPO}.git" "${SOURCE_DIR}"
  fi

  log "编译 ${BIN_NAME}..."
  CGO_ENABLED=0 go build -C "${SOURCE_DIR}" \
    -trimpath -ldflags "-s -w" \
    -o /tmp/${BIN_NAME}_new ./client/

  systemctl stop "${BIN_NAME}" 2>/dev/null || true
  install -m 0755 /tmp/${BIN_NAME}_new "${BIN_PATH}"
  rm -f /tmp/${BIN_NAME}_new
  log "编译完成"
}

write_config() {
  [[ -n "${CLIENT_NAME}" ]] || CLIENT_NAME="$(hostname)"
  mkdir -p "${CONFIG_DIR}"

  if [[ -f "${CONFIG_FILE}" ]]; then
    log "配置文件已存在，更新 server_url / init_token / name"
    local tmp_cfg
    tmp_cfg=$(cat "${CONFIG_FILE}")
    echo "${tmp_cfg}" \
      | sed "s|\"server_url\":[^,}]*|\"server_url\": \"${SERVER_URL}\"|" \
      | sed "s|\"init_token\":[^,}]*|\"init_token\": \"${INIT_TOKEN}\"|" \
      | sed "s|\"name\":[^,}]*|\"name\": \"${CLIENT_NAME}\"|" \
      > "${CONFIG_FILE}.tmp" && mv "${CONFIG_FILE}.tmp" "${CONFIG_FILE}"
    return
  fi

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
  write_service

  systemctl daemon-reload
  systemctl enable --now "${BIN_NAME}"

  log "安装/升级完成"
  echo
  echo "配置文件: ${CONFIG_FILE}"
  echo "常用命令:"
  echo "  systemctl status ${BIN_NAME}"
  echo "  journalctl -u ${BIN_NAME} -f"
}

main "$@"
