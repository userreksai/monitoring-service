#!/usr/bin/env bash
# 从本次交付的源码构建并安装，避免拉取尚未更新的远端旧 SQLite 版本。
# 不自动重启：完成数据库预览/迁移之后再切换两个服务。
set -euo pipefail
[[ "$(id -u)" == 0 ]] || { echo '请使用 sudo 或 root 执行'; exit 1; }
script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
source_dir="$script_dir/.."
app_dir="${1:-/opt/monitoring-service}"
env_file="$app_dir/.env"
if [[ ! "$app_dir" =~ ^/[a-zA-Z0-9_/-]+$ ]]; then
  echo '后端安装目录必须是仅含字母、数字、下划线、斜线、连字符的绝对路径。' >&2
  exit 1
fi
if [[ ! -f "$env_file" ]]; then
  mkdir -p "$app_dir"
  install -m 0600 "$source_dir/.env.example" "$env_file"
  echo "已创建 $env_file，请填写 MySQL、管理员及 JWT_SECRET，再执行安装。"
  exit 1
fi
grep -q '^MYSQL_USER=' "$env_file" || { echo "请先在 $env_file 增加 MYSQL_* 配置"; exit 1; }
command -v go >/dev/null || { echo '请执行 apt-get install -y golang-go'; exit 1; }
if ! id monitoring-service >/dev/null 2>&1; then
  useradd --system --user-group --home-dir "$app_dir" --shell /usr/sbin/nologin monitoring-service
fi
cd "$source_dir"
go mod download
go test ./...
mkdir -p "$app_dir/bin"
go build -trimpath -o "$app_dir/bin/monitoring-service.new" .
chmod 0755 "$app_dir/bin/monitoring-service.new"
chown root:monitoring-service "$env_file"
chmod 0640 "$env_file"
runuser -u monitoring-service -- "$app_dir/bin/monitoring-service.new" --env-file "$env_file" --migrate-only
if [[ -f "$app_dir/bin/monitoring-service" ]]; then
  cp -p "$app_dir/bin/monitoring-service" "$app_dir/bin/monitoring-service.before-mysql.$(date +%Y%m%d%H%M%S)"
fi
mv "$app_dir/bin/monitoring-service.new" "$app_dir/bin/monitoring-service"
cat > /etc/systemd/system/monitoring-service.service <<EOF
[Unit]
Description=SMS Monitoring API (MySQL)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=monitoring-service
Group=monitoring-service
WorkingDirectory=$app_dir
EnvironmentFile=$env_file
ExecStart=$app_dir/bin/monitoring-service
Restart=on-failure
RestartSec=15s
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF
systemd-analyze verify /etc/systemd/system/monitoring-service.service
systemctl daemon-reload
systemctl enable monitoring-service
echo 'MySQL 版后端及表结构已准备；尚未重启服务。请先完成旧数据迁移，再 systemctl restart monitoring-service。'
