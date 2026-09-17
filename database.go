package main

import (
	"bufio"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

func loadEnvFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(scanner.Text(), "\uFEFF"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if !ok || key == "" {
			return errors.New("配置文件行格式错误")
		}
		if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
			value, err = strconv.Unquote(value)
			if err != nil {
				return errors.New("配置文件引号格式错误")
			}
		} else if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
			value = value[1 : len(value)-1]
		}
		if _, exists := os.LookupEnv(key); !exists {
			if err := os.Setenv(key, value); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}

//go:embed schema.sql
var schemaSQL string

func mysqlDSN() (string, error) {
	if os.Getenv("MYSQL_USER") == "" || os.Getenv("MYSQL_PASSWORD") == "" {
		return "", errors.New("必须配置 MYSQL_USER 和 MYSQL_PASSWORD；不再使用 DB_PATH/SQLite")
	}
	config := mysql.NewConfig()
	config.User, config.Passwd = os.Getenv("MYSQL_USER"), os.Getenv("MYSQL_PASSWORD")
	config.Net = "tcp"
	config.Addr = net.JoinHostPort(env("MYSQL_HOST", "127.0.0.1"), env("MYSQL_PORT", "3306"))
	config.DBName = env("MYSQL_DATABASE", "sms_billing_monitor")
	config.TLSConfig = env("MYSQL_TLS", "false")
	config.Params = map[string]string{"charset": "utf8mb4", "time_zone": "'" + env("MYSQL_TIME_ZONE", "+08:00") + "'"}
	return config.FormatDSN(), nil
}

func openDatabase(dsn string) (*sql.DB, error) {
	config, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, errors.New("MySQL 连接配置无效")
	}
	config.ClientFoundRows = true // 重复保存相同配置也应返回成功。
	config.Timeout, config.ReadTimeout, config.WriteTimeout = 10*time.Second, 30*time.Second, 30*time.Second
	db, err := sql.Open("mysql", config.FormatDSN())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(3 * time.Minute)
	if err = db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// 只创建缺失表、补充 UI 字段，不插入示例业务，不覆盖已有阈值/账号。
func migrate(db *sql.DB) error {
	for _, statement := range strings.Split(schemaSQL, ";") {
		if strings.TrimSpace(statement) == "" {
			continue
		}
		if _, err := db.Exec(statement); err != nil {
			return err
		}
	}
	additions := [][3]string{
		{"user_account", "display_name", "VARCHAR(100) NOT NULL DEFAULT '运维中心主控'"},
		{"business", "tag", "VARCHAR(255) NOT NULL DEFAULT ''"},
		{"business", "description", "TEXT NULL"},
		{"business", "tone", "VARCHAR(32) NOT NULL DEFAULT 'green'"},
		{"notification_log", "alert_type", "VARCHAR(255) NOT NULL DEFAULT ''"},
		{"notification_log", "alert_value", "VARCHAR(255) NOT NULL DEFAULT ''"},
		{"notification_log", "status", "VARCHAR(32) NOT NULL DEFAULT '待处理'"},
		{"notification_log", "level", "VARCHAR(32) NOT NULL DEFAULT 'warning'"},
		{"notification_log", "event_id", "VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL"},
	}
	for _, a := range additions {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name=? AND column_name=?`, a[0], a[1]).Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			if _, err := db.Exec(fmt.Sprintf("ALTER TABLE `%s` ADD COLUMN `%s` %s", a[0], a[1], a[2])); err != nil {
				return err
			}
		}
	}
	var hashLength int
	if err := db.QueryRow(`SELECT character_maximum_length FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='user_account' AND column_name='password_hash'`).Scan(&hashLength); err != nil {
		return err
	}
	if hashLength < 255 {
		if _, err := db.Exec(`ALTER TABLE user_account MODIFY password_hash VARCHAR(255) NOT NULL`); err != nil {
			return err
		}
	}
	var contentType string
	if err := db.QueryRow(`SELECT data_type FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='notification_log' AND column_name='alert_content'`).Scan(&contentType); err != nil {
		return err
	}
	if contentType == "varchar" {
		if _, err := db.Exec(`ALTER TABLE notification_log MODIFY alert_content TEXT NOT NULL`); err != nil {
			return err
		}
	}
	var indexes int
	if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='notification_log' AND column_name='event_id' AND non_unique=0`).Scan(&indexes); err != nil {
		return err
	}
	if indexes == 0 {
		if _, err := db.Exec(`ALTER TABLE notification_log ADD UNIQUE KEY uk_notification_event (event_id)`); err != nil {
			return err
		}
	}
	return nil
}

func seed(db *sql.DB, username, password string) error {
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM user_account WHERE account=?`, username).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	if password == "" {
		return errors.New("管理员不存在，请配置 ADMIN_PASSWORD 创建账号")
	}
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO user_account(account,password_hash,display_name) VALUES(?,?,?) ON DUPLICATE KEY UPDATE account=account`, username, hash, "运维中心主控")
	return err
}

func debounceSeconds(value string) uint64 {
	match := debouncePattern.FindStringSubmatch(strings.ToLower(value))
	if match == nil {
		return 0
	}
	amount, _ := strconv.ParseFloat(match[1], 64)
	return uint64(math.Round(amount * map[string]float64{"m": 60, "h": 3600, "d": 86400}[match[2]]))
}

func displayDebounce(seconds string) string {
	value, _ := strconv.ParseFloat(seconds, 64)
	if value >= 86400 {
		return strconv.FormatFloat(value/86400, 'f', -1, 64) + "d"
	}
	if value >= 3600 {
		return strconv.FormatFloat(value/3600, 'f', -1, 64) + "h"
	}
	return strconv.FormatFloat(value/60, 'f', -1, 64) + "m"
}
