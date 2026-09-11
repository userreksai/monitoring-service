#!/usr/bin/env bash
set -Eeuo pipefail

REPO_URL="${REPO_URL:-https://github.com/userreksai/monitoring-service.git}"
BRANCH="${BRANCH:-main}"
APP_DIR="${APP_DIR:-/opt/monitoring-service}"
DATA_DIR="${DATA_DIR:-/var/lib/monitoring-service}"
SERVICE_NAME="${SERVICE_NAME:-monitoring-service}"
SERVICE_USER="${SERVICE_USER:-monitoring-service}"
PORT="${PORT:-8901}"
CORS_ORIGINS="${CORS_ORIGINS:-*}"
UNIT_FILE="/etc/systemd/system/${SERVICE_NAME}.service"
ENV_FILE="${APP_DIR}/.env"
BINARY="${APP_DIR}/bin/monitoring-service"

log() {
  printf '[monitoring-service] %s\n' "$*"
}

fail() {
  printf '[monitoring-service] ERROR: %s\n' "$*" >&2
  exit 1
}

if [[ "${EUID}" -ne 0 ]]; then
  fail "请使用 root 用户运行，或执行：sudo bash $0"
fi

if [[ ! -f /etc/os-release ]]; then
  fail "无法识别当前 Linux 系统"
fi

# shellcheck disable=SC1091
source /etc/os-release
if [[ "${ID:-}" != "ubuntu" || "${VERSION_ID:-}" != "24.04" ]]; then
  fail "此脚本仅支持 Ubuntu 24.04 LTS（检测到 ${PRETTY_NAME:-unknown}）"
fi

if [[ ! "${PORT}" =~ ^[0-9]+$ ]] || (( PORT < 1 || PORT > 65535 )); then
  fail "PORT 必须是 1-65535 之间的整数"
fi

export DEBIAN_FRONTEND=noninteractive
log "安装系统依赖"
apt-get update
apt-get install -y --no-install-recommends ca-certificates curl git golang-go openssl

if ! id "${SERVICE_USER}" >/dev/null 2>&1; then
  log "创建系统用户 ${SERVICE_USER}"
  useradd --system --user-group --home-dir "${APP_DIR}" --shell /usr/sbin/nologin "${SERVICE_USER}"
fi

if [[ -d "${APP_DIR}/.git" ]]; then
  log "拉取 ${BRANCH} 分支最新代码"
  git -c safe.directory="${APP_DIR}" -C "${APP_DIR}" remote set-url origin "${REPO_URL}"
  git -c safe.directory="${APP_DIR}" -C "${APP_DIR}" fetch --prune origin "${BRANCH}"
  git -c safe.directory="${APP_DIR}" -C "${APP_DIR}" checkout "${BRANCH}"
  git -c safe.directory="${APP_DIR}" -C "${APP_DIR}" pull --ff-only origin "${BRANCH}"
elif [[ -e "${APP_DIR}" ]] && [[ -n "$(find "${APP_DIR}" -mindepth 1 -maxdepth 1 -print -quit)" ]]; then
  fail "安装目录已存在且不是空 Git 仓库：${APP_DIR}"
else
  log "克隆代码到 ${APP_DIR}"
  mkdir -p "$(dirname "${APP_DIR}")"
  git clone --branch "${BRANCH}" --single-branch "${REPO_URL}" "${APP_DIR}"
fi

cd "${APP_DIR}"
[[ -f go.mod ]] || fail "仓库中缺少 go.mod"

REQUIRED_GO="$(awk '$1 == "go" { print $2; exit }' go.mod)"
INSTALLED_GO="$(go env GOVERSION | sed 's/^go//')"
if [[ -z "${REQUIRED_GO}" || -z "${INSTALLED_GO}" ]]; then
  fail "无法确认 Go 版本"
fi
if ! dpkg --compare-versions "${INSTALLED_GO}" ge "${REQUIRED_GO}"; then
  fail "Go ${INSTALLED_GO} 不满足 go.mod 要求的 Go ${REQUIRED_GO}，请先升级 Go"
fi

log "运行测试并构建后端"
go mod download
go test ./...
mkdir -p "$(dirname "${BINARY}")"
go build -trimpath -ldflags="-s -w" -o "${BINARY}.new" .
chmod 0755 "${BINARY}.new"
mv -f "${BINARY}.new" "${BINARY}"

log "初始化持久化目录"
mkdir -p "${DATA_DIR}"
chown "${SERVICE_USER}:${SERVICE_USER}" "${DATA_DIR}"
chmod 0750 "${DATA_DIR}"

if [[ ! -f "${ENV_FILE}" ]]; then
  umask 077
  ADMIN_PASSWORD="$(openssl rand -hex 16)"
  JWT_SECRET="$(openssl rand -hex 32)"
  cat >"${ENV_FILE}" <<EOF
PORT=${PORT}
DB_PATH=${DATA_DIR}/monitoring.db
ADMIN_USERNAME=admin
ADMIN_PASSWORD=${ADMIN_PASSWORD}
JWT_SECRET=${JWT_SECRET}
CORS_ORIGINS=${CORS_ORIGINS}
EOF
  log "已创建 ${ENV_FILE}"
  log "初始管理员：admin"
  log "初始密码：${ADMIN_PASSWORD}"
  log "请立即保存该密码；后续运行不会重新生成或显示"
else
  log "保留现有配置 ${ENV_FILE}"
fi
chown root:"${SERVICE_USER}" "${ENV_FILE}"
chmod 0640 "${ENV_FILE}"

SERVICE_PORT="$(sed -n 's/^PORT=\([0-9][0-9]*\)$/\1/p' "${ENV_FILE}" | tail -n 1)"
if [[ -z "${SERVICE_PORT}" || ! "${SERVICE_PORT}" =~ ^[0-9]+$ ]] || (( SERVICE_PORT < 1 || SERVICE_PORT > 65535 )); then
  fail "${ENV_FILE} 中缺少有效的 PORT"
fi

log "初始化 systemd 服务 ${SERVICE_NAME}"
cat >"${UNIT_FILE}" <<EOF
[Unit]
Description=SMS Billing Monitoring API
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${SERVICE_USER}
Group=${SERVICE_USER}
WorkingDirectory=${APP_DIR}
EnvironmentFile=${ENV_FILE}
ExecStart=${BINARY}
Restart=on-failure
RestartSec=3
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=${DATA_DIR}
UMask=0027

[Install]
WantedBy=multi-user.target
EOF

chmod 0644 "${UNIT_FILE}"
systemctl daemon-reload
systemctl enable "${SERVICE_NAME}"
systemctl restart "${SERVICE_NAME}"

log "等待服务通过健康检查"
for _ in {1..30}; do
  if curl --fail --silent --max-time 2 "http://127.0.0.1:${SERVICE_PORT}/api/health" >/dev/null; then
    log "部署完成：http://服务器IP:${SERVICE_PORT}"
    systemctl --no-pager --full status "${SERVICE_NAME}" | sed -n '1,8p'
    exit 0
  fi
  sleep 1
done

systemctl --no-pager --full status "${SERVICE_NAME}" || true
journalctl -u "${SERVICE_NAME}" -n 50 --no-pager || true
fail "服务未能通过健康检查"
