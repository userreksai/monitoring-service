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

	_ "modernc.org/sqlite"
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
	Fluctuation int     `json:"fluctuation"`
	Debounce    string  `json:"debounce"`
	Purpose     string  `json:"purpose"`
	Tag         string  `json:"tag"`
	Enabled     bool    `json:"enabled"`
}

type record struct {
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
	Fluctuation *int     `json:"fluctuation"`
	Debounce    *string  `json:"debounce"`
	Purpose     *string  `json:"purpose"`
	Tag         *string  `json:"tag"`
	Enabled     *bool    `json:"enabled"`
}

var debouncePattern = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)([mhdMHD])$`)

func main() {
	port := env("PORT", "8901")
	dbPath := env("DB_PATH", "monitoring.db")
	db, err := openDatabase(dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	if err := migrate(db); err != nil {
		log.Fatal(err)
	}
	adminUser := env("ADMIN_USERNAME", "admin")
	adminPass := env("ADMIN_PASSWORD", "Admin@123456")
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

func openDatabase(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
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

func migrate(db *sql.DB) error {
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS users (
 id INTEGER PRIMARY KEY AUTOINCREMENT, username TEXT NOT NULL UNIQUE,
 password_hash TEXT NOT NULL, display_name TEXT NOT NULL, created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS businesses (
 id INTEGER PRIMARY KEY AUTOINCREMENT, code TEXT NOT NULL UNIQUE, name TEXT NOT NULL,
 tag TEXT NOT NULL DEFAULT '', description TEXT NOT NULL DEFAULT '', enabled INTEGER NOT NULL DEFAULT 1,
 tone TEXT NOT NULL DEFAULT 'green', created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS rules (
 code TEXT PRIMARY KEY, business_id INTEGER NOT NULL, provider TEXT NOT NULL, account TEXT NOT NULL,
 threshold REAL NOT NULL DEFAULT 0, fluctuation INTEGER NOT NULL DEFAULT 0, debounce TEXT NOT NULL,
 purpose TEXT NOT NULL, tag TEXT NOT NULL DEFAULT '', enabled INTEGER NOT NULL DEFAULT 1,
 created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
 FOREIGN KEY (business_id) REFERENCES businesses(id) ON UPDATE CASCADE ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS alert_records (
 id INTEGER PRIMARY KEY AUTOINCREMENT, rule_code TEXT NOT NULL, business TEXT NOT NULL,
 provider TEXT NOT NULL, account TEXT NOT NULL, alert_type TEXT NOT NULL, value TEXT NOT NULL,
 detail TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT '待处理', level TEXT NOT NULL DEFAULT 'warning',
 occurred_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
 FOREIGN KEY (rule_code) REFERENCES rules(code) ON UPDATE CASCADE ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_rules_business ON rules(business_id);
CREATE INDEX IF NOT EXISTS idx_records_occurred ON alert_records(occurred_at DESC);
`)
	return err
}

func seed(db *sql.DB, username, password string) error {
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	if _, err = db.Exec(`INSERT INTO users(username,password_hash,display_name) VALUES(?,?,?) ON CONFLICT(username) DO NOTHING`, username, hash, "运维中心主控"); err != nil {
		return err
	}
	var count int
	if err = db.QueryRow(`SELECT COUNT(*) FROM businesses`).Scan(&count); err != nil || count > 0 {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	items := [][]any{
		{"534784", "短信商", "SMS-GATEWAY", "短信计费与余额监控", 1, "green"},
		{"882190", "支付网关", "CORE-FIN", "支付通道监控", 1, "orange"},
		{"319024", "物流推送", "EXPRESS", "物流通知服务", 0, "gray"},
		{"671042", "邮件服务", "SMTP-RELAY", "邮件发送服务", 1, "green"},
		{"920411", "身份认证通道", "OAUTH-IAM", "身份认证服务", 1, "orange"},
	}
	for _, item := range items {
		if _, err = tx.Exec(`INSERT INTO businesses(code,name,tag,description,enabled,tone) VALUES(?,?,?,?,?,?)`, item...); err != nil {
			return err
		}
	}
	rules := [][]any{
		{"664851", "534784", "阿里云", "123alibab", 1000, 50, "1d", "银行卡", "关联业务为B", 1},
		{"379465", "534784", "华为云", "333alibab", 1000, 40, "1h", "备用专线", "暂停充值", 1},
		{"725076", "534784", "阿里云", "222alibab", 1000, 20, "10m", "高敏通道", "暂停充值-用完截至", 1},
		{"164534", "534784", "阿里云", "443alibab", 1000, 10, "1d", "兜底通道", "暂停充值-用完截至", 1},
	}
	for _, item := range rules {
		_, err = tx.Exec(`INSERT INTO rules(code,business_id,provider,account,threshold,fluctuation,debounce,purpose,tag,enabled) SELECT ?,id,?,?,?,?,?,?,?,? FROM businesses WHERE code=?`, item[0], item[2], item[3], item[4], item[5], item[6], item[7], item[8], item[9], item[1])
		if err != nil {
			return err
		}
	}
	records := [][]any{
		{"664851", "短信商", "阿里云", "123alibab", "余额低于阈值", "当前余额: ¥84.20", "可用额度不足最低配置预警线(¥500.00)，预计15分钟内短信分发将阻断", "待处理", "warning", "2024-05-18 13:00:24"},
		{"664851", "短信商", "阿里云", "222alibab", "浮动百分比超出预设", "瞬时消耗环比 +340%", "5分钟内验证码发送激增，单IP突发速率偏离常规业务基线", "处理中", "processing", "2024-05-18 13:00:10"},
	}
	for _, item := range records {
		if _, err = tx.Exec(`INSERT INTO alert_records(rule_code,business,provider,account,alert_type,value,detail,status,level,occurred_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, item...); err != nil {
			return err
		}
	}
	return tx.Commit()
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
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "time": time.Now()})
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
	err := s.db.QueryRow(`SELECT id,password_hash,display_name FROM users WHERE username=?`, strings.TrimSpace(in.Username)).Scan(&id, &hash, &displayName)
	if err != nil || !verifyPassword(hash, in.Password) {
		writeError(w, http.StatusUnauthorized, "账号或密码错误")
		return
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
	if err := s.db.QueryRow(`SELECT display_name FROM users WHERE id=?`, claims.UserID).Scan(&name); err != nil {
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
		{&businesses, `SELECT COUNT(*) FROM businesses`},
		{&enabledBusinesses, `SELECT COUNT(*) FROM businesses WHERE enabled=1`},
		{&rules, `SELECT COUNT(*) FROM rules`},
		{&enabledRules, `SELECT COUNT(*) FROM rules WHERE enabled=1`},
		{&records, `SELECT COUNT(*) FROM alert_records`},
		{&todayAlerts, `SELECT COUNT(*) FROM alert_records WHERE date(occurred_at)=date('now','localtime')`},
		{&pendingAlerts, `SELECT COUNT(*) FROM alert_records WHERE status='待处理'`},
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
		where := ` WHERE (b.code LIKE ? OR b.name LIKE ? OR b.tag LIKE ?)`
		args := []any{q, q, q}
		if status == "enabled" || status == "disabled" {
			where += ` AND b.enabled=?`
			args = append(args, status == "enabled")
		}
		rows, err := s.db.Query(`SELECT b.id,b.code,b.name,b.tag,b.description,b.enabled,b.tone,strftime('%Y-%m-%d %H:%M',b.created_at),COUNT(r.code) FROM businesses b LEFT JOIN rules r ON r.business_id=b.id`+where+` GROUP BY b.id ORDER BY b.id`, args...)
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
		result, err := s.db.Exec(`INSERT INTO businesses(code,name,tag,description,enabled,tone) VALUES(?,?,?,?,?,?)`, in.Code, in.Name, in.Tag, in.Description, in.Enabled, in.Tone)
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
		result, err := s.db.Exec(`DELETE FROM businesses WHERE id=?`, id)
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
	err := s.db.QueryRow(`SELECT b.id,b.code,b.name,b.tag,b.description,b.enabled,b.tone,strftime('%Y-%m-%d %H:%M',b.created_at),COUNT(r.code) FROM businesses b LEFT JOIN rules r ON r.business_id=b.id WHERE b.id=? GROUP BY b.id`, id).Scan(&item.ID, &item.Code, &item.Name, &item.Tag, &item.Description, &enabled, &item.Tone, &item.Time, &item.Rules)
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
	result, err := s.db.Exec(`UPDATE businesses SET code=?,name=?,tag=?,description=?,enabled=?,tone=? WHERE id=?`, in.Code, in.Name, in.Tag, in.Description, in.Enabled, in.Tone, id)
	if constraint(w, err) {
		return false
	}
	if affected(result) == 0 {
		writeError(w, http.StatusNotFound, "业务不存在")
		return false
	}
	return true
}

func (s *server) rules(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		q := "%" + strings.TrimSpace(r.URL.Query().Get("q")) + "%"
		where := ` WHERE (r.code LIKE ? OR b.name LIKE ? OR r.provider LIKE ? OR r.account LIKE ? OR r.purpose LIKE ?)`
		args := []any{q, q, q, q, q}
		if value := strings.TrimSpace(r.URL.Query().Get("business")); value != "" {
			where += ` AND (b.code=? OR b.name=?)`
			args = append(args, value, value)
		}
		if value := strings.TrimSpace(r.URL.Query().Get("provider")); value != "" {
			where += ` AND r.provider=?`
			args = append(args, value)
		}
		if status := r.URL.Query().Get("status"); status == "enabled" || status == "disabled" {
			where += ` AND r.enabled=?`
			args = append(args, status == "enabled")
		}
		rows, err := s.db.Query(`SELECT r.code,b.code,b.name,r.provider,r.account,r.threshold,r.fluctuation,r.debounce,r.purpose,r.tag,r.enabled FROM rules r JOIN businesses b ON b.id=r.business_id`+where+` ORDER BY r.created_at,r.code`, args...)
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
		result, err := s.db.Exec(`INSERT INTO rules(code,business_id,provider,account,threshold,fluctuation,debounce,purpose,tag,enabled) SELECT ?,id,?,?,?,?,?,?,?,? FROM businesses WHERE code=?`, in.Code, in.Provider, in.Account, in.Threshold, in.Fluctuation, in.Debounce, in.Purpose, in.Tag, in.Enabled, in.Parent)
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
		result, err := s.db.Exec(`DELETE FROM rules WHERE code=?`, code)
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
	err := s.db.QueryRow(`SELECT r.code,b.code,b.name,r.provider,r.account,r.threshold,r.fluctuation,r.debounce,r.purpose,r.tag,r.enabled FROM rules r JOIN businesses b ON b.id=r.business_id WHERE r.code=?`, code).Scan(&item.Code, &item.Parent, &item.Business, &item.Provider, &item.Account, &item.Threshold, &item.Fluctuation, &item.Debounce, &item.Purpose, &item.Tag, &enabled)
	item.Enabled = enabled == 1
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
	result, err := s.db.Exec(`UPDATE rules SET code=?,business_id=(SELECT id FROM businesses WHERE code=?),provider=?,account=?,threshold=?,fluctuation=?,debounce=?,purpose=?,tag=?,enabled=? WHERE code=?`, in.Code, in.Parent, in.Provider, in.Account, in.Threshold, in.Fluctuation, in.Debounce, in.Purpose, in.Tag, in.Enabled, oldCode)
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
		rows, err := s.db.Query(`SELECT id,strftime('%H:%M:%S',occurred_at),strftime('%Y-%m-%d',occurred_at),rule_code,business,provider,account,alert_type,value,detail,status,level FROM alert_records WHERE rule_code LIKE ? OR business LIKE ? OR provider LIKE ? OR account LIKE ? OR alert_type LIKE ? ORDER BY occurred_at DESC,id DESC`, q, q, q, q, q)
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
		occurredAt := time.Now().Format("2006-01-02 15:04:05")
		if in.Date != "" {
			if _, err := time.ParseInLocation("2006-01-02 15:04:05", in.Date+" "+in.Time, time.Local); err != nil {
				writeError(w, http.StatusBadRequest, "告警日期时间格式无效")
				return
			}
			occurredAt = in.Date + " " + in.Time
		}
		result, err := s.db.Exec(`INSERT INTO alert_records(rule_code,business,provider,account,alert_type,value,detail,status,level,occurred_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, in.Code, in.Business, in.Provider, in.Account, in.Type, in.Value, in.Detail, in.Status, in.Level, occurredAt)
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
		result, err := s.db.Exec(`UPDATE alert_records SET status=?,level=? WHERE id=?`, in.Status, level, id)
		if constraint(w, err) {
			return
		}
		if affected(result) == 0 {
			writeError(w, http.StatusNotFound, "告警记录不存在")
			return
		}
		s.getRecord(w, id)
	case http.MethodDelete:
		result, err := s.db.Exec(`DELETE FROM alert_records WHERE id=?`, id)
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
	err := s.db.QueryRow(`SELECT id,strftime('%H:%M:%S',occurred_at),strftime('%Y-%m-%d',occurred_at),rule_code,business,provider,account,alert_type,value,detail,status,level FROM alert_records WHERE id=?`, id).Scan(&item.ID, &item.Time, &item.Date, &item.Code, &item.Business, &item.Provider, &item.Account, &item.Type, &item.Value, &item.Detail, &item.Status, &item.Level)
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
	if err != nil || amount <= 0 || math.IsNaN(amount) || math.IsInf(amount, 0) {
		return "", errors.New("防抖告警跨度必须大于 0")
	}
	return strconv.FormatFloat(amount, 'f', -1, 64) + strings.ToLower(matches[2]), nil
}

func (s *server) normalizeRecord(item *record) error {
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
	err := s.db.QueryRow(`SELECT b.name,r.provider,r.account FROM rules r JOIN businesses b ON b.id=r.business_id WHERE r.code=?`, item.Code).Scan(&businessName, &provider, &account)
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
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iterations, err := strconv.Atoi(parts[1])
	if err != nil {
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
	if strings.Contains(strings.ToLower(err.Error()), "constraint") {
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
