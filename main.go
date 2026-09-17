package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

type server struct {
	db     *sql.DB
	secret []byte
}

type business struct {
	ID          int64  `json:"id"`
	Code        string `json:"code"`
	Name        string `json:"name"`
	Tag         string `json:"tag"`
	Description string `json:"description"`
	Rules       int    `json:"rules"`
	Enabled     bool   `json:"enabled"`
	Time        string `json:"time"`
	Tone        string `json:"tone"`
}

type rule struct {
	Code        string  `json:"code"`
	Parent      string  `json:"parent"`
	Business    string  `json:"business"`
	Provider    string  `json:"provider"`
	Account     string  `json:"account"`
	Threshold   float64 `json:"threshold"`
	Fluctuation float64 `json:"fluctuation"`
	Debounce    string  `json:"debounce"`
	Purpose     string  `json:"purpose"`
	Tag         string  `json:"tag"`
	Enabled     bool    `json:"enabled"`
}

type record struct {
	EventID  string `json:"eventId,omitempty"`
	ID       int64  `json:"id"`
	Time     string `json:"time"`
	Date     string `json:"date"`
	Code     string `json:"code"`
	Business string `json:"business"`
	Provider string `json:"provider"`
	Account  string `json:"account"`
	Type     string `json:"type"`
	Value    string `json:"value"`
	Detail   string `json:"detail"`
	Status   string `json:"status"`
	Level    string `json:"level"`
}

type businessPatch struct {
	ID          *int64  `json:"id"`
	Code        *string `json:"code"`
	Name        *string `json:"name"`
	Tag         *string `json:"tag"`
	Description *string `json:"description"`
	Rules       *int    `json:"rules"`
	Enabled     *bool   `json:"enabled"`
	Time        *string `json:"time"`
	Tone        *string `json:"tone"`
}

type rulePatch struct {
	Code        *string  `json:"code"`
	Parent      *string  `json:"parent"`
	Business    *string  `json:"business"`
	Provider    *string  `json:"provider"`
	Account     *string  `json:"account"`
	Threshold   *float64 `json:"threshold"`
	Fluctuation *float64 `json:"fluctuation"`
	Debounce    *string  `json:"debounce"`
	Purpose     *string  `json:"purpose"`
	Tag         *string  `json:"tag"`
	Enabled     *bool    `json:"enabled"`
}

var debouncePattern = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)([mhdMHD])$`)

func main() {
	onlyMigrate := flag.Bool("migrate-only", false, "只创建/升级 MySQL 表，不启动 API")
	envFile := flag.String("env-file", "", "读取 KEY=value 配置文件（环境变量优先）")
	flag.Parse()
	if *envFile != "" {
		if err := loadEnvFile(*envFile); err != nil {
			log.Fatal(err)
		}
	}
	port := env("PORT", "8901")
	dsn, err := mysqlDSN()
	if err != nil {
		log.Fatal(err)
	}
	db, err := openDatabase(dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	if err := migrate(db); err != nil {
		log.Fatal(err)
	}
	if *onlyMigrate {
		log.Println("MySQL 表结构已就绪，原业务数据保持不变")
		return
	}
	adminUser := env("ADMIN_USERNAME", "admin")
	adminPass := os.Getenv("ADMIN_PASSWORD")
	if err := seed(db, adminUser, adminPass); err != nil {
		log.Fatal(err)
	}
	secret := []byte(env("JWT_SECRET", ""))
	if len(secret) < 32 {
		secret = make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			log.Fatal(err)
		}
		log.Println("JWT_SECRET 未配置，本次启动使用临时密钥；重启后已有登录令牌会失效")
	}
	handler := newHandler(&server{db: db, secret: secret}, parseOrigins(env("CORS_ORIGINS", "http://localhost:8900,http://127.0.0.1:8900")))
	addr := "0.0.0.0:" + port
	log.Printf("短信计费监控 API 已启动: http://localhost:%s（管理员: %s）", port, adminUser)
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Fatal(httpServer.ListenAndServe())
}

func env(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func newHandler(s *server, origins []string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", s.health)
	mux.HandleFunc("/api/auth/login", s.login)
	mux.Handle("/api/auth/me", s.auth(http.HandlerFunc(s.me)))
	mux.Handle("/api/stats", s.auth(http.HandlerFunc(s.stats)))
	mux.Handle("/api/businesses", s.auth(http.HandlerFunc(s.businesses)))
	mux.Handle("/api/businesses/", s.auth(http.HandlerFunc(s.businessByID)))
	mux.Handle("/api/rules", s.auth(http.HandlerFunc(s.rules)))
	mux.Handle("/api/rules/", s.auth(http.HandlerFunc(s.ruleByCode)))
	mux.Handle("/api/records", s.auth(http.HandlerFunc(s.records)))
	mux.Handle("/api/records/", s.auth(http.HandlerFunc(s.recordByID)))
	mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "接口不存在")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "接口不存在")
	})
	return accessLog(cors(origins, mux))
}

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if err := s.db.PingContext(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "数据库不可用")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "database": "mysql", "time": time.Now()})
}

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decode(w, r, &in) {
		return
	}
	var id int64
	var hash, displayName string
	err := s.db.QueryRow(`SELECT 0,password_hash,display_name FROM user_account WHERE account=?`, strings.TrimSpace(in.Username)).Scan(&id, &hash, &displayName)
	if err != nil || !verifyPassword(hash, in.Password) {
		writeError(w, http.StatusUnauthorized, "账号或密码错误")
		return
	}
	if len(hash) == 64 {
		upgraded, hashErr := hashPassword(in.Password)
		if hashErr != nil {
			writeError(w, 500, "密码升级失败")
			return
		}
		if _, err := s.db.Exec("UPDATE user_account SET password_hash=? WHERE account=? AND password_hash=?", upgraded, strings.TrimSpace(in.Username), hash); err != nil {
			writeError(w, 500, "密码升级失败")
			return
		}
	}
	token, err := s.issueToken(id, strings.TrimSpace(in.Username))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "生成登录令牌失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token": token, "expiresIn": 86400,
		"user": map[string]any{"id": id, "username": strings.TrimSpace(in.Username), "displayName": displayName},
	})
}

func (s *server) me(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	claims, _ := verifyToken(s.secret, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	var name string
	if err := s.db.QueryRow(`SELECT display_name FROM user_account WHERE account=?`, claims.Username).Scan(&name); err != nil {
		writeError(w, http.StatusUnauthorized, "用户不存在")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": claims.UserID, "username": claims.Username, "displayName": name})
}

func (s *server) stats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	var businesses, enabledBusinesses, rules, enabledRules, records, todayAlerts, pendingAlerts int
	queries := []struct {
		dst   *int
		query string
	}{
		{&businesses, `SELECT COUNT(*) FROM business`},
		{&enabledBusinesses, `SELECT COUNT(*) FROM business WHERE notify_enabled=1`},
		{&rules, `SELECT COUNT(*) FROM business_detail`},
		{&enabledRules, `SELECT COUNT(*) FROM business_detail WHERE notify_enabled=1`},
		{&records, `SELECT COUNT(*) FROM notification_log`},
		{&todayAlerts, `SELECT COUNT(*) FROM notification_log WHERE date(alert_time)=CURRENT_DATE()`},
		{&pendingAlerts, `SELECT COUNT(*) FROM notification_log WHERE status='待处理'`},
	}
	for _, query := range queries {
		if err := s.db.QueryRow(query.query).Scan(query.dst); err != nil {
			writeError(w, http.StatusInternalServerError, "读取统计数据失败")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]int{
		"businesses": businesses, "enabledBusinesses": enabledBusinesses,
		"rules": rules, "enabledRules": enabledRules, "records": records,
		"todayAlerts": todayAlerts, "pendingAlerts": pendingAlerts,
	})
}

func (s *server) businesses(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		q := "%" + strings.TrimSpace(r.URL.Query().Get("q")) + "%"
		status := r.URL.Query().Get("status")
		where := ` WHERE (b.business_code LIKE ? OR b.business_name LIKE ? OR b.tag LIKE ?)`
		args := []any{q, q, q}
		if status == "enabled" || status == "disabled" {
			where += ` AND b.notify_enabled=?`
			args = append(args, status == "enabled")
		}
		rows, err := s.db.Query(`SELECT b.id,b.business_code,b.business_name,b.tag,COALESCE(b.description,''),b.notify_enabled,b.tone,DATE_FORMAT(b.created_at,'%Y-%m-%d %H:%i'),COUNT(r.detail_code) FROM business b LEFT JOIN business_detail r ON r.business_code=b.business_code`+where+` GROUP BY b.id ORDER BY b.id`, args...)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		defer rows.Close()
		out := []business{}
		for rows.Next() {
			var item business
			var enabled int
			if err := rows.Scan(&item.ID, &item.Code, &item.Name, &item.Tag, &item.Description, &enabled, &item.Tone, &item.Time, &item.Rules); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			item.Enabled = enabled == 1
			out = append(out, item)
		}
		if err := rows.Err(); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		var in business
		if !decode(w, r, &in) {
			return
		}
		if err := normalizeBusiness(&in); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		result, err := s.db.Exec(`INSERT INTO business(business_code,business_name,tag,description,notify_enabled,tone) VALUES(?,?,?,?,?,?)`, in.Code, in.Name, in.Tag, in.Description, in.Enabled, in.Tone)
		if constraint(w, err) {
			return
		}
		id, _ := result.LastInsertId()
		s.getBusiness(w, id)
	default:
		methodNotAllowed(w)
	}
}

func (s *server) businessByID(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r.URL.Path, "/api/businesses/")
	if err != nil {
		writeError(w, http.StatusBadRequest, "无效的业务 ID")
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.getBusiness(w, id)
	case http.MethodPut:
		var in business
		if !decode(w, r, &in) {
			return
		}
		if err := normalizeBusiness(&in); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if s.updateBusiness(w, id, in) {
			s.getBusiness(w, id)
		}
	case http.MethodPatch:
		current, err := s.findBusiness(id)
		if err != nil {
			s.writeLookupError(w, err, "业务不存在")
			return
		}
		var in businessPatch
		if !decode(w, r, &in) {
			return
		}
		applyBusinessPatch(&current, in)
		if err := normalizeBusiness(&current); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if s.updateBusiness(w, id, current) {
			s.getBusiness(w, id)
		}
	case http.MethodDelete:
		result, err := s.db.Exec(`DELETE FROM business WHERE id=?`, id)
		if constraint(w, err) {
			return
		}
		if affected(result) == 0 {
			writeError(w, http.StatusNotFound, "业务不存在")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		methodNotAllowed(w)
	}
}

func (s *server) findBusiness(id int64) (business, error) {
	var item business
	var enabled int
	err := s.db.QueryRow(`SELECT b.id,b.business_code,b.business_name,b.tag,COALESCE(b.description,''),b.notify_enabled,b.tone,DATE_FORMAT(b.created_at,'%Y-%m-%d %H:%i'),COUNT(r.detail_code) FROM business b LEFT JOIN business_detail r ON r.business_code=b.business_code WHERE b.id=? GROUP BY b.id`, id).Scan(&item.ID, &item.Code, &item.Name, &item.Tag, &item.Description, &enabled, &item.Tone, &item.Time, &item.Rules)
	item.Enabled = enabled == 1
	return item, err
}

func (s *server) getBusiness(w http.ResponseWriter, id int64) {
	item, err := s.findBusiness(id)
	if err != nil {
		s.writeLookupError(w, err, "业务不存在")
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *server) updateBusiness(w http.ResponseWriter, id int64, in business) bool {
	tx, err := s.db.Begin()
	if constraint(w, err) {
		return false
	}
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE business SET business_code=?,business_name=?,tag=?,description=?,notify_enabled=?,tone=? WHERE id=?`, in.Code, in.Name, in.Tag, in.Description, in.Enabled, in.Tone, id)
	if constraint(w, err) {
		return false
	}
	if affected(result) == 0 {
		writeError(w, http.StatusNotFound, "业务不存在")
		return false
	}
	_, err = tx.Exec(`UPDATE business_detail SET business_name=? WHERE business_code=?`, in.Name, in.Code)
	if constraint(w, err) {
		return false
	}
	return !constraint(w, tx.Commit())
}

func (s *server) rules(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		q := "%" + strings.TrimSpace(r.URL.Query().Get("q")) + "%"
		where := ` WHERE (r.detail_code LIKE ? OR b.business_name LIKE ? OR r.vendor LIKE ? OR r.account LIKE ? OR COALESCE(r.business_purpose,'') LIKE ?)`
		args := []any{q, q, q, q, q}
		if value := strings.TrimSpace(r.URL.Query().Get("business")); value != "" {
			where += ` AND (b.business_code=? OR b.business_name=?)`
			args = append(args, value, value)
		}
		if value := strings.TrimSpace(r.URL.Query().Get("provider")); value != "" {
			where += ` AND r.vendor=?`
			args = append(args, value)
		}
		if status := r.URL.Query().Get("status"); status == "enabled" || status == "disabled" {
			where += ` AND r.notify_enabled=?`
			args = append(args, status == "enabled")
		}
		rows, err := s.db.Query(`SELECT r.detail_code,b.business_code,b.business_name,r.vendor,r.account,r.balance_threshold,r.fluctuation_percent,r.alert_silence_seconds,COALESCE(r.business_purpose,''),COALESCE(r.tag,''),r.notify_enabled FROM business_detail r JOIN business b ON b.business_code=r.business_code`+where+` ORDER BY r.created_at,r.detail_code`, args...)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		defer rows.Close()
		out := []rule{}
		for rows.Next() {
			var item rule
			var enabled int
			if err := rows.Scan(&item.Code, &item.Parent, &item.Business, &item.Provider, &item.Account, &item.Threshold, &item.Fluctuation, &item.Debounce, &item.Purpose, &item.Tag, &enabled); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			item.Enabled = enabled == 1
			item.Debounce = displayDebounce(item.Debounce)
			out = append(out, item)
		}
		if err := rows.Err(); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		var in rule
		if !decode(w, r, &in) {
			return
		}
		if err := normalizeRule(&in); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		result, err := s.db.Exec(`INSERT INTO business_detail(detail_code,business_code,business_name,vendor,account,balance_threshold,fluctuation_percent,alert_silence_seconds,business_purpose,tag,notify_enabled) SELECT ?,business_code,business_name,?,?,?,?,?,?,?,? FROM business WHERE business_code=?`, in.Code, in.Provider, in.Account, in.Threshold, in.Fluctuation, debounceSeconds(in.Debounce), in.Purpose, in.Tag, in.Enabled, in.Parent)
		if constraint(w, err) {
			return
		}
		if affected(result) == 0 {
			writeError(w, http.StatusBadRequest, "父业务不存在")
			return
		}
		s.getRule(w, in.Code)
	default:
		methodNotAllowed(w)
	}
}

func (s *server) ruleByCode(w http.ResponseWriter, r *http.Request) {
	code := strings.TrimPrefix(r.URL.Path, "/api/rules/")
	if code == "" || strings.Contains(code, "/") {
		writeError(w, http.StatusBadRequest, "无效的规则编码")
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.getRule(w, code)
	case http.MethodPut:
		var in rule
		if !decode(w, r, &in) {
			return
		}
		if strings.TrimSpace(in.Code) == "" {
			in.Code = code
		}
		if err := normalizeRule(&in); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if s.updateRule(w, code, in) {
			s.getRule(w, in.Code)
		}
	case http.MethodPatch:
		current, err := s.findRule(code)
		if err != nil {
			s.writeLookupError(w, err, "规则不存在")
			return
		}
		var in rulePatch
		if !decode(w, r, &in) {
			return
		}
		applyRulePatch(&current, in)
		if err := normalizeRule(&current); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if s.updateRule(w, code, current) {
			s.getRule(w, current.Code)
		}
	case http.MethodDelete:
		result, err := s.db.Exec(`DELETE FROM business_detail WHERE detail_code=?`, code)
		if constraint(w, err) {
			return
		}
		if affected(result) == 0 {
			writeError(w, http.StatusNotFound, "规则不存在")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		methodNotAllowed(w)
	}
}

func (s *server) findRule(code string) (rule, error) {
	var item rule
	var enabled int
	err := s.db.QueryRow(`SELECT r.detail_code,b.business_code,b.business_name,r.vendor,r.account,r.balance_threshold,r.fluctuation_percent,r.alert_silence_seconds,COALESCE(r.business_purpose,''),COALESCE(r.tag,''),r.notify_enabled FROM business_detail r JOIN business b ON b.business_code=r.business_code WHERE r.detail_code=?`, code).Scan(&item.Code, &item.Parent, &item.Business, &item.Provider, &item.Account, &item.Threshold, &item.Fluctuation, &item.Debounce, &item.Purpose, &item.Tag, &enabled)
	item.Enabled = enabled == 1
	item.Debounce = displayDebounce(item.Debounce)
	return item, err
}

func (s *server) getRule(w http.ResponseWriter, code string) {
	item, err := s.findRule(code)
	if err != nil {
		s.writeLookupError(w, err, "规则不存在")
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *server) updateRule(w http.ResponseWriter, oldCode string, in rule) bool {
	result, err := s.db.Exec(`UPDATE business_detail r JOIN business b ON b.business_code=? SET r.detail_code=?,r.business_code=b.business_code,r.business_name=b.business_name,r.vendor=?,r.account=?,r.balance_threshold=?,r.fluctuation_percent=?,r.alert_silence_seconds=?,r.business_purpose=?,r.tag=?,r.notify_enabled=? WHERE r.detail_code=?`, in.Parent, in.Code, in.Provider, in.Account, in.Threshold, in.Fluctuation, debounceSeconds(in.Debounce), in.Purpose, in.Tag, in.Enabled, oldCode)
	if constraint(w, err) {
		return false
	}
	if affected(result) == 0 {
		writeError(w, http.StatusNotFound, "规则不存在")
		return false
	}
	return true
}

func (s *server) records(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		q := "%" + strings.TrimSpace(r.URL.Query().Get("q")) + "%"
		rows, err := s.db.Query(`SELECT id,DATE_FORMAT(alert_time,'%H:%i:%s'),DATE_FORMAT(alert_time,'%Y-%m-%d'),detail_code,business_name,vendor,account,COALESCE(NULLIF(alert_type,''),alert_content),alert_value,alert_content,status,level FROM notification_log WHERE detail_code LIKE ? OR business_name LIKE ? OR vendor LIKE ? OR account LIKE ? OR alert_type LIKE ? ORDER BY alert_time DESC,id DESC`, q, q, q, q, q)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		defer rows.Close()
		out := []record{}
		for rows.Next() {
			var item record
			if err := rows.Scan(&item.ID, &item.Time, &item.Date, &item.Code, &item.Business, &item.Provider, &item.Account, &item.Type, &item.Value, &item.Detail, &item.Status, &item.Level); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			out = append(out, item)
		}
		if err := rows.Err(); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		var in record
		if !decode(w, r, &in) {
			return
		}
		if err := s.normalizeRecord(&in); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		occurredAt := ""
		if in.Date != "" {
			if _, err := time.ParseInLocation("2006-01-02 15:04:05", in.Date+" "+in.Time, time.Local); err != nil {
				writeError(w, http.StatusBadRequest, "告警日期时间格式无效")
				return
			}
			occurredAt = in.Date + " " + in.Time
		}
		result, err := s.db.Exec(`INSERT INTO notification_log(detail_code,business_name,vendor,account,alert_type,alert_value,alert_content,status,level,alert_time,event_id) VALUES(?,?,?,?,?,?,?,?,?,COALESCE(NULLIF(?,''),CURRENT_TIMESTAMP),NULLIF(?,'')) ON DUPLICATE KEY UPDATE id=LAST_INSERT_ID(id)`, in.Code, in.Business, in.Provider, in.Account, in.Type, in.Value, in.Detail, in.Status, in.Level, occurredAt, in.EventID)
		if constraint(w, err) {
			return
		}
		id, _ := result.LastInsertId()
		s.getRecord(w, id)
	default:
		methodNotAllowed(w)
	}
}

func (s *server) recordByID(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r.URL.Path, "/api/records/")
	if err != nil {
		writeError(w, http.StatusBadRequest, "无效的记录 ID")
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.getRecord(w, id)
	case http.MethodPut, http.MethodPatch:
		var in record
		if !decode(w, r, &in) {
			return
		}
		in.Status = strings.TrimSpace(in.Status)
		level := statusLevel(in.Status)
		if level == "" {
			writeError(w, http.StatusBadRequest, "无效的告警状态")
			return
		}
		result, err := s.db.Exec(`UPDATE notification_log SET status=?,level=? WHERE id=?`, in.Status, level, id)
		if constraint(w, err) {
			return
		}
		if affected(result) == 0 {
			writeError(w, http.StatusNotFound, "告警记录不存在")
			return
		}
		s.getRecord(w, id)
	case http.MethodDelete:
		result, err := s.db.Exec(`DELETE FROM notification_log WHERE id=?`, id)
		if constraint(w, err) {
			return
		}
		if affected(result) == 0 {
			writeError(w, http.StatusNotFound, "告警记录不存在")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		methodNotAllowed(w)
	}
}

func (s *server) getRecord(w http.ResponseWriter, id int64) {
	var item record
	err := s.db.QueryRow(`SELECT id,DATE_FORMAT(alert_time,'%H:%i:%s'),DATE_FORMAT(alert_time,'%Y-%m-%d'),detail_code,business_name,vendor,account,COALESCE(NULLIF(alert_type,''),alert_content),alert_value,alert_content,status,level FROM notification_log WHERE id=?`, id).Scan(&item.ID, &item.Time, &item.Date, &item.Code, &item.Business, &item.Provider, &item.Account, &item.Type, &item.Value, &item.Detail, &item.Status, &item.Level)
	if err != nil {
		s.writeLookupError(w, err, "告警记录不存在")
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func normalizeBusiness(item *business) error {
	item.Code = strings.TrimSpace(item.Code)
	item.Name = strings.TrimSpace(item.Name)
	item.Tag = strings.TrimSpace(item.Tag)
	item.Description = strings.TrimSpace(item.Description)
	item.Tone = strings.TrimSpace(item.Tone)
	if item.Code == "" || item.Name == "" {
		return errors.New("业务编码和名称不能为空")
	}
	if item.Tone == "" {
		item.Tone = "green"
	}
	return nil
}

func normalizeRule(item *rule) error {
	item.Code = strings.TrimSpace(item.Code)
	item.Parent = strings.TrimSpace(item.Parent)
	item.Provider = strings.TrimSpace(item.Provider)
	item.Account = strings.TrimSpace(item.Account)
	item.Purpose = strings.TrimSpace(item.Purpose)
	item.Tag = strings.TrimSpace(item.Tag)
	if item.Code == "" || item.Parent == "" || item.Provider == "" || item.Account == "" {
		return errors.New("规则编码、父业务、厂商和账号不能为空")
	}
	if math.IsNaN(item.Threshold) || math.IsInf(item.Threshold, 0) || item.Threshold < 0 || item.Fluctuation < 0 || item.Fluctuation > 100 {
		return errors.New("阈值或浮动百分比无效")
	}
	debounce, err := normalizeDebounce(item.Debounce)
	if err != nil {
		return err
	}
	item.Debounce = debounce
	return nil
}

func normalizeDebounce(value string) (string, error) {
	matches := debouncePattern.FindStringSubmatch(strings.TrimSpace(value))
	if matches == nil {
		return "", errors.New("防抖告警跨度格式必须为正数加 m、h 或 d，例如 10m、2h、1d")
	}
	amount, err := strconv.ParseFloat(matches[1], 64)
	if err != nil || amount < 0 || math.IsNaN(amount) || math.IsInf(amount, 0) {
		return "", errors.New("防抖告警跨度不能小于 0")
	}
	seconds := amount * map[string]float64{"m": 60, "h": 3600, "d": 86400}[strings.ToLower(matches[2])]
	if seconds > 315360000 || math.Abs(seconds-math.Round(seconds)) > 0.000001 {
		return "", errors.New("防抖跨度需为整数秒，且不超过十年")
	}
	return strconv.FormatFloat(amount, 'f', -1, 64) + strings.ToLower(matches[2]), nil
}

func (s *server) normalizeRecord(item *record) error {
	if item.EventID != "" && !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(item.EventID) {
		return errors.New("eventId 必须为 32 位小写十六进制事件标识")
	}
	item.Code = strings.TrimSpace(item.Code)
	item.Business = strings.TrimSpace(item.Business)
	item.Provider = strings.TrimSpace(item.Provider)
	item.Account = strings.TrimSpace(item.Account)
	item.Type = strings.TrimSpace(item.Type)
	item.Value = strings.TrimSpace(item.Value)
	item.Detail = strings.TrimSpace(item.Detail)
	item.Status = strings.TrimSpace(item.Status)
	item.Date = strings.TrimSpace(item.Date)
	item.Time = strings.TrimSpace(item.Time)
	if item.Code == "" || item.Type == "" {
		return errors.New("规则编码和告警类型不能为空")
	}
	var businessName, provider, account string
	err := s.db.QueryRow(`SELECT b.business_name,r.vendor,r.account FROM business_detail r JOIN business b ON b.business_code=r.business_code WHERE r.detail_code=?`, item.Code).Scan(&businessName, &provider, &account)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("关联的告警规则不存在")
	}
	if err != nil {
		return fmt.Errorf("读取关联规则失败: %w", err)
	}
	if item.Business == "" {
		item.Business = businessName
	}
	if item.Provider == "" {
		item.Provider = provider
	}
	if item.Account == "" {
		item.Account = account
	}
	if item.Status == "" {
		item.Status = "待处理"
	}
	item.Level = statusLevel(item.Status)
	if item.Level == "" {
		return errors.New("无效的告警状态")
	}
	if (item.Date == "") != (item.Time == "") {
		return errors.New("告警日期和时间必须同时填写")
	}
	return nil
}

func applyBusinessPatch(dst *business, patch businessPatch) {
	if patch.Code != nil {
		dst.Code = *patch.Code
	}
	if patch.Name != nil {
		dst.Name = *patch.Name
	}
	if patch.Tag != nil {
		dst.Tag = *patch.Tag
	}
	if patch.Description != nil {
		dst.Description = *patch.Description
	}
	if patch.Enabled != nil {
		dst.Enabled = *patch.Enabled
	}
	if patch.Tone != nil {
		dst.Tone = *patch.Tone
	}
}

func applyRulePatch(dst *rule, patch rulePatch) {
	if patch.Code != nil {
		dst.Code = *patch.Code
	}
	if patch.Parent != nil {
		dst.Parent = *patch.Parent
	}
	if patch.Provider != nil {
		dst.Provider = *patch.Provider
	}
	if patch.Account != nil {
		dst.Account = *patch.Account
	}
	if patch.Threshold != nil {
		dst.Threshold = *patch.Threshold
	}
	if patch.Fluctuation != nil {
		dst.Fluctuation = *patch.Fluctuation
	}
	if patch.Debounce != nil {
		dst.Debounce = *patch.Debounce
	}
	if patch.Purpose != nil {
		dst.Purpose = *patch.Purpose
	}
	if patch.Tag != nil {
		dst.Tag = *patch.Tag
	}
	if patch.Enabled != nil {
		dst.Enabled = *patch.Enabled
	}
}

type claims struct {
	UserID   int64  `json:"uid"`
	Username string `json:"usr"`
	Expires  int64  `json:"exp"`
}

func (s *server) issueToken(id int64, username string) (string, error) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	body, err := json.Marshal(claims{UserID: id, Username: username, Expires: time.Now().Add(24 * time.Hour).Unix()})
	if err != nil {
		return "", err
	}
	payload := header + "." + base64.RawURLEncoding.EncodeToString(body)
	mac := hmac.New(sha256.New, s.secret)
	_, _ = mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func verifyToken(secret []byte, token string) (claims, error) {
	var out claims
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return out, errors.New("invalid token")
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(parts[0] + "." + parts[1]))
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(signature, mac.Sum(nil)) {
		return out, errors.New("invalid signature")
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || json.Unmarshal(body, &out) != nil || out.Expires < time.Now().Unix() {
		return out, errors.New("expired token")
	}
	return out, nil
}

func (s *server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, "Bearer ") {
			writeError(w, http.StatusUnauthorized, "请先登录")
			return
		}
		if _, err := verifyToken(s.secret, strings.TrimPrefix(header, "Bearer ")); err != nil {
			writeError(w, http.StatusUnauthorized, "登录已失效，请重新登录")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func hashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	sum := deriveKey([]byte(password), salt, 120000)
	return fmt.Sprintf("pbkdf2-sha256$120000$%s$%s", hex.EncodeToString(salt), hex.EncodeToString(sum)), nil
}

func verifyPassword(encoded, password string) bool {
	if len(encoded) == 64 {
		want, err := hex.DecodeString(encoded)
		sum := sha256.Sum256([]byte(password))
		return err == nil && hmac.Equal(want, sum[:])
	}
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iterations, err := strconv.Atoi(parts[1])
	if err != nil || iterations < 1 || iterations > 2000000 {
		return false
	}
	salt, err := hex.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := hex.DecodeString(parts[3])
	if err != nil {
		return false
	}
	return hmac.Equal(want, deriveKey([]byte(password), salt, iterations))
}

func deriveKey(password, salt []byte, iterations int) []byte {
	block := make([]byte, len(salt)+4)
	copy(block, salt)
	binary.BigEndian.PutUint32(block[len(salt):], 1)
	mac := hmac.New(sha256.New, password)
	_, _ = mac.Write(block)
	u := mac.Sum(nil)
	out := append([]byte(nil), u...)
	for i := 1; i < iterations; i++ {
		mac.Reset()
		_, _ = mac.Write(u)
		u = mac.Sum(nil)
		for j := range out {
			out[j] ^= u[j]
		}
	}
	return out
}

func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "请求数据格式错误: "+err.Error())
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "请求只能包含一个 JSON 对象")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func methodNotAllowed(w http.ResponseWriter) {
	writeError(w, http.StatusMethodNotAllowed, "不支持的请求方法")
}

func constraint(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) && (mysqlErr.Number == 1062 || mysqlErr.Number == 1451 || mysqlErr.Number == 1452 || mysqlErr.Number == 3819) {
		writeError(w, http.StatusConflict, "数据冲突：编码可能已存在，或关联数据不存在")
	} else {
		writeError(w, http.StatusInternalServerError, err.Error())
	}
	return true
}

func affected(result sql.Result) int64 {
	if result == nil {
		return 0
	}
	count, _ := result.RowsAffected()
	return count
}

func pathInt(value, prefix string) (int64, error) {
	value = strings.TrimPrefix(value, prefix)
	if value == "" || strings.Contains(value, "/") {
		return 0, errors.New("invalid identifier")
	}
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("invalid identifier")
	}
	return id, nil
}

func (s *server) writeLookupError(w http.ResponseWriter, err error, missing string) {
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, missing)
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error())
}

func statusLevel(status string) string {
	return map[string]string{"待处理": "warning", "处理中": "processing", "已确认": "confirmed", "已忽略": "ignored"}[status]
}

func parseOrigins(raw string) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0)
	for _, value := range strings.Split(raw, ",") {
		value = strings.TrimSuffix(strings.TrimSpace(value), "/")
		if value == "" {
			continue
		}
		if _, exists := seen[value]; !exists {
			seen[value] = struct{}{}
			out = append(out, value)
		}
	}
	return out
}

func cors(origins []string, next http.Handler) http.Handler {
	allowed := make(map[string]struct{}, len(origins))
	for _, origin := range origins {
		allowed[origin] = struct{}{}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := strings.TrimSuffix(r.Header.Get("Origin"), "/")
		if origin != "" {
			_, exact := allowed[origin]
			_, wildcard := allowed["*"]
			if !exact && !wildcard {
				writeError(w, http.StatusForbidden, "请求来源不在 CORS 白名单")
				return
			}
			w.Header().Add("Vary", "Origin")
			if wildcard {
				w.Header().Set("Access-Control-Allow-Origin", "*")
			} else {
				w.Header().Set("Access-Control-Allow-Origin", origin)
			}
		}
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(started).Round(time.Millisecond))
	})
}
