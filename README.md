# monitoring-service（MySQL 版）

Go 1.22+、MySQL 8.0.16+，默认 API 端口 8901。前端接口保持兼容。

数据库默认 `sms_billing_monitor`，沿用 `business`、`business_detail`、`notification_log`、`user_account`。本版本没有 SQLite 驱动和 SQLite 回退。防抖沿用 `alert_silence_seconds`，通过 API 显示为 `m/h/d`。示例配置见 `.env.example`。

## 运行

```bash
go mod download
go build -o bin/monitoring-service .
# 创建/升级表结构，不启动服务
./bin/monitoring-service --env-file .env --migrate-only
# 启动 API
./bin/monitoring-service --env-file .env
```

配置：`MYSQL_HOST`、`MYSQL_PORT`、`MYSQL_DATABASE`、`MYSQL_USER`、`MYSQL_PASSWORD`、`MYSQL_TIME_ZONE`、`MYSQL_TLS`；其他配置为 `PORT`、`JWT_SECRET`、`CORS_ORIGINS`、`ADMIN_USERNAME`、`ADMIN_PASSWORD`。已有环境变量优先于 `--env-file`。新管理员仅在该账号不存在时创建，已有密码不会被初始化参数覆盖；旧 SHA-256 密码登录成功后升级为 PBKDF2。

启动迁移只增加缺失字段和表，不插入示例业务，不覆盖已有账号/阈值。`schema.sql` 定义完整新库结构，`database.go` 补齐原表字段。MySQL 账号需要本库 SELECT、INSERT、UPDATE、DELETE、CREATE、ALTER、INDEX、REFERENCES 权限。

## 生产升级

迁移说明位于 [monitoring-web 仓库的 MYSQL_UPGRADE.md](https://github.com/userreksai/monitoring-web/blob/main/MYSQL_UPGRADE.md)。在本仓库目录执行 `sudo bash deploy/install.sh /opt/monitoring-service`，安装器从当前源码构建，升级表结构并准备二进制，**不自动启动**，留出数据迁移步骤。安装器可独立使用，不依赖前端仓库目录。配置缺失时会创建模板并退出，填写后重试。

## API

除健康检查、登录外需 `Authorization: Bearer <token>`。

| 方法 | 路径 | 功能 |
|---|---|---|
| GET | `/api/health` | 状态及 `database: mysql` |
| POST | `/api/auth/login` | 登录 |
| GET | `/api/auth/me` | 当前账号 |
| GET | `/api/stats` | 统计 |
| GET/POST | `/api/businesses` | 业务列表/新增 |
| GET/PUT/PATCH/DELETE | `/api/businesses/{id}` | 单个业务 |
| GET/POST | `/api/rules` | 规则列表/新增 |
| GET/PUT/PATCH/DELETE | `/api/rules/{code}` | 单个规则 |
| GET/POST | `/api/records` | 告警记录列表/新增 |
| GET/PATCH/DELETE | `/api/records/{id}` | 单个记录 |

`POST /api/records` 增加可选 `eventId`（32 位小写十六进制），同一事件重试返回同一条记录。原始通知内容保存在 `alert_content`；新增 `alert_type`、`alert_value`、`status`、`level`、`event_id` 支持页面。原 MySQL 记录没有告警类型时，以原 `alert_content` 展示。

保留原外键 RESTRICT，已有记录的规则、已有规则的业务不能直接删除，返回 409。重复保存未改变的配置仍返回成功。

## 测试

```bash
go test -v ./...
# 设置后才能执行真实数据库测试，账号需 CREATE/DROP DATABASE 权限
MYSQL_TEST_DSN='testuser:password@tcp(127.0.0.1:3306)/' go test -v ./...
```

集成测试仅创建/删除随机 `monitoring_test_*` 数据库；没设置 `MYSQL_TEST_DSN` 时跳过。不要把测试管理员凭据写入仓库。
