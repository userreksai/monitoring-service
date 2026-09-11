# monitoring-service

短信计费监控系统的 Go + SQLite 后端。服务仅提供 JSON API，默认监听 `8901`；Vue 前端默认运行在 `http://localhost:8900`。

## 本地运行

需要 Go 1.22 或更高版本：

```bash
go mod download
go test ./...
go run .
```

默认管理员仅用于本地首次运行：

- 用户名：`admin`
- 密码：`Admin@123456`

生产环境务必通过环境变量覆盖密码和 JWT 密钥。

## 配置

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `PORT` | `8901` | API 监听端口 |
| `DB_PATH` | `monitoring.db` | SQLite 文件路径 |
| `ADMIN_USERNAME` | `admin` | 首次初始化的管理员账号 |
| `ADMIN_PASSWORD` | `Admin@123456` | 首次初始化的管理员密码 |
| `JWT_SECRET` | 临时随机值 | JWT 签名密钥；生产环境必须固定且至少 32 字节 |
| `CORS_ORIGINS` | `http://localhost:8900,http://127.0.0.1:8900` | 允许的前端 Origin，多个地址使用英文逗号分隔；可用 `*` 放行全部来源 |

管理员只会在用户名不存在时创建；修改环境变量不会覆盖数据库中已有账号。

## API

除健康检查和登录外，其余接口需发送 `Authorization: Bearer <token>`。

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| `GET` | `/api/health` | 健康检查 |
| `POST` | `/api/auth/login` | 登录 |
| `GET` | `/api/auth/me` | 当前用户 |
| `GET` | `/api/stats` | 业务、规则和告警统计 |
| `GET/POST` | `/api/businesses` | 告警业务列表/新增 |
| `GET/PUT/PATCH/DELETE` | `/api/businesses/{id}` | 告警业务详情/修改/删除；`PATCH` 支持仅提交 `enabled` |
| `GET/POST` | `/api/rules` | 告警设置列表/新增 |
| `GET/PUT/PATCH/DELETE` | `/api/rules/{code}` | 告警设置详情/修改/删除；`PATCH` 支持部分字段 |
| `GET/POST` | `/api/records` | 告警记录列表/新增 |
| `GET/PATCH/DELETE` | `/api/records/{id}` | 告警记录详情/修改状态/删除 |

告警规则的 `debounce` 必须是正数加单位 `m`、`h` 或 `d`，例如 `10m`、`2h`、`1d`。服务端会去除首尾空格、统一小写单位并规范数字格式。

## Ubuntu 24.04 LTS 部署

部署脚本会安装依赖、克隆或更新代码、运行测试、编译程序、初始化 systemd 服务并重启。首次运行还会创建独立系统用户、持久化数据库目录、随机管理员密码和 JWT 密钥。

```bash
curl -fsSL https://raw.githubusercontent.com/userreksai/monitoring-service/main/deploy/install.sh -o /tmp/install-monitoring-service.sh
sudo bash /tmp/install-monitoring-service.sh
```

指定前端域名和端口：

```bash
sudo CORS_ORIGINS="https://monitor.example.com" PORT=8901 bash /tmp/install-monitoring-service.sh
```

安装脚本在未指定 `CORS_ORIGINS` 时使用 `*`，确保通过服务器 IP 访问的前端也能联调。正式上线时建议像上例一样收紧到实际前端域名。

首次部署后配置保存在 `/opt/monitoring-service/.env`。再次执行同一脚本会拉取 `main` 最新代码、重新测试与构建，然后重启 `monitoring-service`，并保留现有 `.env` 和 `/var/lib/monitoring-service/monitoring.db`。因此重复安装时传入的新端口或 CORS 参数不会覆盖现有配置；请使用 `sudoedit /opt/monitoring-service/.env` 修改后执行 `sudo systemctl restart monitoring-service`。

常用命令：

```bash
sudo systemctl status monitoring-service
sudo journalctl -u monitoring-service -f
sudo systemctl restart monitoring-service
```
