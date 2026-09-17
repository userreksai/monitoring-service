#!/usr/bin/env bash
# 仓库内执行时使用当前源码；下载到 /tmp 后执行时获取部署仓库源码。
# 不自动重启：完成数据库预览/迁移之后再切换两个服务。
set -euo pipefail
[[ "${CHECK_SOURCE_ONLY:-0}" == 1 || "$(id -u)" == 0 ]] || { echo '请使用 sudo 或 root 执行'; exit 1; }
script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
source_dir="$script_dir/.."
app_dir="${1:-${APP_DIR:-/opt/monitoring-service}}"
env_file="$app_dir/.env"
if [[ ! "$app_dir" =~ ^/[a-zA-Z0-9_/-]+$ ]]; then
  echo '后端安装目录必须是仅含字母、数字、下划线、斜线、连字符的绝对路径。' >&2
  exit 1
fi
if [[ ! -f "$source_dir/go.mod" || ! -f "$source_dir/main.go" || ! -f "$source_dir/database.go" ]]; then
  # 独立下载的脚本不能用 /tmp/.. 当作 Go 项目目录。
  source_dir="$app_dir"
  repo_url="${REPO_URL:-https://github.com/userreksai/monitoring-service.git}"
  branch="${BRANCH:-main}"
  if [[ -d "$app_dir/.git" ]]; then
    command -v git >/dev/null || { echo '请先安装 git'; exit 1; }
    git -c safe.directory="$app_dir" -C "$app_dir" diff --quiet
    git -c safe.directory="$app_dir" -C "$app_dir" diff --cached --quiet
    current_branch="$(git -c safe.directory="$app_dir" -C "$app_dir" branch --show-current)"
    if [[ "$current_branch" != "$branch" ]]; then
      echo "部署目录当前分支为 $current_branch，请先切换到 $branch，或设置 BRANCH。" >&2
      exit 1
    fi
    git -c safe.directory="$app_dir" -C "$app_dir" fetch "$repo_url" "$branch"
    git -c safe.directory="$app_dir" -C "$app_dir" merge --ff-only FETCH_HEAD
  elif [[ ! -f "$app_dir/go.mod" ]]; then
    command -v git >/dev/null || { echo '请先安装 git'; exit 1; }
    # git clone 对非空目录会拒绝，不覆盖已有配置或文件。
    git clone --branch "$branch" --single-branch "$repo_url" "$app_dir"
  fi
fi
for required_file in go.mod main.go database.go .env.example; do
  [[ -f "$source_dir/$required_file" ]] || { echo "源码目录 $source_dir 缺少 $required_file，请使用 MySQL 版完整后端源码。" >&2; exit 1; }
done
source_dir="$(cd -- "$source_dir" && pwd)"
echo "后端源码目录：$source_dir"
if [[ "${CHECK_SOURCE_ONLY:-0}" == 1 ]]; then
  exit 0
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
