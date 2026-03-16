#!/usr/bin/env bash
# BWPanel 服务端安装与管理脚本
# 用法：bash install_server.sh
# 重复运行即可进入管理菜单
set -euo pipefail

REPO="ctsunny/bwtest"
BIN_NAME="bwpanel"
APP_USER="bwtest"
APP_GROUP="bwtest"
INSTALL_DIR="/opt/bwtest"
BIN_PATH="/usr/local/bin/${BIN_NAME}"
ENV_FILE="/etc/default/${BIN_NAME}"
SERVICE_FILE="/etc/systemd/system/${BIN_NAME}.service"
MGR_PATH="/usr/local/bin/bwpanel-menu"

GREEN="\033[1;32m"; YELLOW="\033[1;33m"; RED="\033[1;31m"; CYAN="\033[1;36m"; RESET="\033[0m"
log()  { echo -e "${GREEN}[INFO]${RESET} $*"; }
warn() { echo -e "${YELLOW}[WARN]${RESET} $*"; }
err()  { echo -e "${RED}[ERR ]${RESET} $*" >&2; }
title(){ echo -e "\n${CYAN}$*${RESET}"; }

rand_hex() { openssl rand -hex "${1:-16}" 2>/dev/null || head -c "${1:-16}" /dev/urandom | xxd -p -c256; }

rand_port() {
  while true; do
    local p=$(( RANDOM % 40000 + 20000 ))
    # check port not in use
    if ! ss -lntp 2>/dev/null | grep -q ":${p} " ; then
      echo "${p}"
      return
    fi
  done
}

rand_path() {
  echo "/console-$(rand_hex 4)"
}

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo "amd64" ;;
    aarch64|arm64) echo "arm64" ;;
    *) err "不支持的架构: $(uname -m)"; exit 1 ;;
  esac
}

detect_server_ip() {
  curl -s --max-time 5 https://api.ipify.org 2>/dev/null \
    || curl -s --max-time 5 https://ifconfig.me 2>/dev/null \
    || hostname -I 2>/dev/null | awk '{print $1}' \
    || echo "127.0.0.1"
}

install_deps() {
  if command -v apt-get >/dev/null 2>&1; then
    apt-get update -qq && apt-get install -y -qq curl ca-certificates xxd 2>/dev/null || apt-get install -y -qq curl ca-certificates
  elif command -v dnf >/dev/null 2>&1; then
    dnf install -y -q curl ca-certificates vim-common
  elif command -v yum >/dev/null 2>&1; then
    yum install -y -q curl ca-certificates vim-common
  fi
}

ensure_user_group() {
  getent group "${APP_GROUP}" >/dev/null 2>&1 || groupadd --system "${APP_GROUP}" 2>/dev/null || true
  id -u "${APP_USER}" >/dev/null 2>&1 || \
    useradd --system --gid "${APP_GROUP}" --home "${INSTALL_DIR}" --shell /usr/sbin/nologin "${APP_USER}" 2>/dev/null || \
    useradd -r -g "${APP_GROUP}" -s /sbin/nologin "${APP_USER}" 2>/dev/null || true
}

download_binary() {
  local version="${1:-latest}" arch asset url tmp
  arch="$(detect_arch)"
  asset="${BIN_NAME}-linux-${arch}"
  if [[ "${version}" == "latest" ]]; then
    url="https://github.com/${REPO}/releases/latest/download/${asset}"
  else
    url="https://github.com/${REPO}/releases/download/${version}/${asset}"
  fi
  log "下载: ${url}"
  tmp="$(mktemp)"
  curl --proto '=https' --tlsv1.2 -fsSL "${url}" -o "${tmp}"
  # 必须先停服务再替换二进制，否则 Linux 会报 Text file busy
  systemctl stop "${BIN_NAME}" 2>/dev/null || true
  install -m 0755 "${tmp}" "${BIN_PATH}"
  rm -f "${tmp}"
}

write_env_file() {
  local server_host panel_port data_port admin_pass init_token panel_path version
  server_host="$1"; panel_port="$2"; data_port="$3"
  admin_pass="$4"; init_token="$5"; panel_path="$6"; version="$7"
  mkdir -p /etc/default
  cat > "${ENV_FILE}" <<EOF
PANEL_ADDR=:${panel_port}
DATA_ADDR=:${data_port}
SERVER_HOST=${server_host}
ADMIN_USER=admin
ADMIN_PASS=${admin_pass}
INIT_TOKEN=${init_token}
DB_PATH=${INSTALL_DIR}/bwtest.db
PANEL_PATH=${panel_path}
BWPANEL_VERSION=${version}
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

install_mgr_script() {
  cp -f "$0" "${MGR_PATH}" 2>/dev/null || curl -fsSL https://raw.githubusercontent.com/${REPO}/main/scripts/install_server.sh -o "${MGR_PATH}"
  chmod +x "${MGR_PATH}"
}

print_info() {
  local server_host panel_port panel_path admin_pass init_token
  [[ -f "${ENV_FILE}" ]] || return
  server_host=$(grep SERVER_HOST "${ENV_FILE}" | cut -d= -f2)
  panel_port=$(grep PANEL_ADDR "${ENV_FILE}" | cut -d= -f2 | tr -d ':')
  panel_path=$(grep PANEL_PATH "${ENV_FILE}" | cut -d= -f2)
  admin_pass=$(grep ADMIN_PASS "${ENV_FILE}" | cut -d= -f2)
  init_token=$(grep INIT_TOKEN "${ENV_FILE}" | cut -d= -f2)
  echo
  echo -e "  ${CYAN}面板地址${RESET}    : http://${server_host}:${panel_port}${panel_path}"
  echo -e "  ${CYAN}用户名  ${RESET}    : admin"
  echo -e "  ${CYAN}密码    ${RESET}    : ${admin_pass}"
  echo -e "  ${CYAN}注册Token${RESET} : ${init_token}"
  echo -e "  ${CYAN}常用命令${RESET}    : systemctl status ${BIN_NAME}  |  journalctl -u ${BIN_NAME} -f"
  echo -e "  ${CYAN}管理脚本${RESET}    : bwpanel-menu"
  echo
}

do_install() {
  title "=== BWPanel 服务端安装 ==="

  local auto_ip
  auto_ip=$(detect_server_ip)
  echo -e "检测到公网 IP: ${CYAN}${auto_ip}${RESET}"
  read -rp "服务器公网 IP/域名 [回车使用 ${auto_ip}]: " SERVER_HOST
  SERVER_HOST=${SERVER_HOST:-${auto_ip}}

  local auto_panel_port; auto_panel_port=$(rand_port)
  read -rp "面板端口 [回车随机 ${auto_panel_port}]: " PANEL_PORT
  PANEL_PORT=${PANEL_PORT:-${auto_panel_port}}

  local auto_data_port; auto_data_port=$(rand_port)
  while [[ "${auto_data_port}" == "${PANEL_PORT}" ]]; do auto_data_port=$(rand_port); done
  read -rp "数据端口 [回车随机 ${auto_data_port}]: " DATA_PORT
  DATA_PORT=${DATA_PORT:-${auto_data_port}}

  local auto_path; auto_path=$(rand_path)
  read -rp "面板访问路径 [回车随机 ${auto_path}]: " PANEL_PATH
  PANEL_PATH=${PANEL_PATH:-${auto_path}}
  [[ "${PANEL_PATH}" == /* ]] || PANEL_PATH="/${PANEL_PATH}"

  local auto_pass; auto_pass=$(rand_hex 12)
  read -rp "管理员密码 [回车随机生成]: " ADMIN_PASS
  ADMIN_PASS=${ADMIN_PASS:-${auto_pass}}

  local auto_token; auto_token=$(rand_hex 20)
  read -rp "客户端注册 Token [回车随机生成]: " INIT_TOKEN
  INIT_TOKEN=${INIT_TOKEN:-${auto_token}}

  read -rp "版本号 [回车使用 latest]: " VERSION
  VERSION=${VERSION:-latest}

  echo
  log "开始安装..."
  install_deps
  mkdir -p "${INSTALL_DIR}"
  ensure_user_group
  chown -R "${APP_USER}:${APP_GROUP}" "${INSTALL_DIR}"
  download_binary "${VERSION}"
  write_env_file "${SERVER_HOST}" "${PANEL_PORT}" "${DATA_PORT}" "${ADMIN_PASS}" "${INIT_TOKEN}" "${PANEL_PATH}" "${VERSION}"
  write_service
  systemctl daemon-reload
  systemctl enable --now "${BIN_NAME}"
  install_mgr_script

  log "安装完成！"
  print_info
}

do_upgrade() {
  title "=== 升级 BWPanel ==="
  local current_ver
  current_ver=$(grep BWPANEL_VERSION "${ENV_FILE}" 2>/dev/null | cut -d= -f2 || echo "unknown")
  echo "当前版本: ${current_ver}"
  read -rp "目标版本 [回车使用 latest]: " VERSION
  VERSION=${VERSION:-latest}
  # download_binary 内部已处理 stop/replace/start
  download_binary "${VERSION}"
  sed -i "s/^BWPANEL_VERSION=.*/BWPANEL_VERSION=${VERSION}/" "${ENV_FILE}" 2>/dev/null || true
  systemctl start "${BIN_NAME}"
  systemctl status "${BIN_NAME}" --no-pager -l
  log "升级完成！"
  print_info
}

do_reset_pass() {
  title "=== 重置管理员密码 ==="
  local auto_pass; auto_pass=$(rand_hex 12)
  read -rp "新密码 [回车随机生成]: " NEW_PASS
  NEW_PASS=${NEW_PASS:-${auto_pass}}
  sed -i "s/^ADMIN_PASS=.*/ADMIN_PASS=${NEW_PASS}/" "${ENV_FILE}"
  systemctl restart "${BIN_NAME}"
  log "密码已更新"
  print_info
}

do_reset_path() {
  title "=== 重置面板访问路径 ==="
  local auto_path; auto_path=$(rand_path)
  read -rp "新路径 [回车随机生成 ${auto_path}]: " NEW_PATH
  NEW_PATH=${NEW_PATH:-${auto_path}}
  [[ "${NEW_PATH}" == /* ]] || NEW_PATH="/${NEW_PATH}"
  sed -i "s|^PANEL_PATH=.*|PANEL_PATH=${NEW_PATH}|" "${ENV_FILE}"
  systemctl restart "${BIN_NAME}"
  log "路径已更新"
  print_info
}

do_reset_token() {
  title "=== 重置客户端注册 Token ==="
  local auto_token; auto_token=$(rand_hex 20)
  read -rp "新 Token [回车随机生成]: " NEW_TOKEN
  NEW_TOKEN=${NEW_TOKEN:-${auto_token}}
  sed -i "s/^INIT_TOKEN=.*/INIT_TOKEN=${NEW_TOKEN}/" "${ENV_FILE}"
  systemctl restart "${BIN_NAME}"
  log "Token 已更新"
  print_info
}

do_status() {
  title "=== 服务状态 ==="
  systemctl status "${BIN_NAME}" --no-pager -l || true
  echo
  journalctl -u "${BIN_NAME}" -n 40 --no-pager
  print_info
}

do_uninstall() {
  title "=== 完整卸载 BWPanel ==="
  warn "即将删除所有数据和配置，此操作不可恢复！"
  read -rp "确认卸载？输入 YES 继续: " CONFIRM
  [[ "${CONFIRM}" == "YES" ]] || { log "已取消"; return; }

  systemctl disable --now "${BIN_NAME}" 2>/dev/null || true
  rm -f "${SERVICE_FILE}"
  systemctl daemon-reload
  rm -f "${BIN_PATH}"
  rm -f "${ENV_FILE}"
  rm -rf "${INSTALL_DIR}"
  rm -f "${MGR_PATH}"

  userdel "${APP_USER}" 2>/dev/null || true
  groupdel "${APP_GROUP}" 2>/dev/null || true

  log "卸载完成，所有文件已清理。"
}

menu() {
  while true; do
    echo
    echo -e "${CYAN}======== BWPanel 管理菜单 ========${RESET}"
    echo "  1. 安装 / 重新安装服务端"
    echo "  2. 升级（指定或最新版本）"
    echo "  3. 查看当前配置与面板地址"
    echo "  4. 重置管理员密码"
    echo "  5. 重置面板访问路径"
    echo "  6. 重置客户端注册 Token"
    echo "  7. 查看服务状态与日志"
    echo "  8. 完整卸载"
    echo "  0. 退出"
    echo -e "${CYAN}====================================${RESET}"
    read -rp "请选择操作 [0-8]: " choice
    case "${choice}" in
      1) do_install ;;
      2) do_upgrade ;;
      3) print_info ;;
      4) do_reset_pass ;;
      5) do_reset_path ;;
      6) do_reset_token ;;
      7) do_status ;;
      8) do_uninstall ;;
      0) echo "退出"; exit 0 ;;
      *) warn "无效选项，请重新输入" ;;
    esac
  done
}

[[ $EUID -eq 0 ]] || { err "请使用 root 权限运行"; exit 1; }

if [[ -f "${ENV_FILE}" && -f "${BIN_PATH}" ]]; then
  menu
else
  do_install
  menu
fi
