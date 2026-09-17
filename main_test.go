package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

type apiFixture struct {
	url   string
	token string
}

func newAPIFixture(t *testing.T) apiFixture {
	t.Helper()
	db := newTestDatabase(t)
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := seed(db, "admin", "test-password"); err != nil {
		t.Fatal(err)
	}
	_, err := db.Exec(`INSERT INTO business(business_code,business_name) VALUES('534784','短信商')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO business_detail(detail_code,business_code,business_name,fluctuation_percent,balance_threshold,vendor,account,alert_silence_seconds) VALUES('664851','534784','短信商',50,1000,'阿里云','123alibab',86400)`)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(newHandler(
		&server{db: db, secret: []byte("test-secret-that-is-at-least-32-bytes")},
		[]string{"http://localhost:8900"},
	))
	t.Cleanup(httpServer.Close)

	fixture := apiFixture{url: httpServer.URL}
	status, body := fixture.request(t, http.MethodPost, "/api/auth/login", map[string]any{
		"username": "admin", "password": "test-password",
	}, false)
	if status != http.StatusOK {
		t.Fatalf("login: status=%d body=%s", status, body)
	}
	var login struct {
		Token string `json:"token"`
	}
	decodeResponse(t, body, &login)
	if login.Token == "" {
		t.Fatal("login response has no token")
	}
	fixture.token = login.Token
	return fixture
}

// Tests create/drop only a unique monitoring_test_* database, never a supplied production DB.
func newTestDatabase(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("MYSQL_TEST_DSN 未配置：跳过真实 MySQL 集成测试")
	}
	config, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.DBName = ""
	admin, err := openDatabase(config.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("monitoring_test_%d", time.Now().UnixNano())
	if _, err = admin.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec("DROP DATABASE `" + name + "`"); admin.Close() })
	config.DBName = name
	db, err := openDatabase(config.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func (f apiFixture) request(t *testing.T, method, path string, payload any, authenticated bool) (int, []byte) {
	t.Helper()
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, f.url+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if authenticated {
		req.Header.Set("Authorization", "Bearer "+f.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, data
}

func decodeResponse(t *testing.T, data []byte, target any) {
	t.Helper()
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatalf("decode response %q: %v", data, err)
	}
}

func itoa(value int64) string {
	return strconv.FormatInt(value, 10)
}

func TestHealthAuthenticationAndCORS(t *testing.T) {
	f := newAPIFixture(t)
	status, body := f.request(t, http.MethodGet, "/api/health", nil, false)
	if status != http.StatusOK || !strings.Contains(string(body), `"status":"ok"`) {
		t.Fatalf("health: status=%d body=%s", status, body)
	}
	status, _ = f.request(t, http.MethodGet, "/api/businesses", nil, false)
	if status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated request: got %d, want 401", status)
	}
	status, body = f.request(t, http.MethodGet, "/api/auth/me", nil, true)
	if status != http.StatusOK || !strings.Contains(string(body), `"username":"admin"`) {
		t.Fatalf("me: status=%d body=%s", status, body)
	}

	allowed, err := http.NewRequest(http.MethodOptions, f.url+"/api/businesses", nil)
	if err != nil {
		t.Fatal(err)
	}
	allowed.Header.Set("Origin", "http://localhost:8900")
	resp, err := http.DefaultClient.Do(allowed)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || resp.Header.Get("Access-Control-Allow-Origin") != "http://localhost:8900" {
		t.Fatalf("allowed preflight: status=%d origin=%q", resp.StatusCode, resp.Header.Get("Access-Control-Allow-Origin"))
	}

	blocked, err := http.NewRequest(http.MethodOptions, f.url+"/api/businesses", nil)
	if err != nil {
		t.Fatal(err)
	}
	blocked.Header.Set("Origin", "https://not-allowed.example")
	resp, err = http.DefaultClient.Do(blocked)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("blocked preflight: got %d, want 403", resp.StatusCode)
	}
}

func TestBusinessCRUD(t *testing.T) {
	f := newAPIFixture(t)
	status, body := f.request(t, http.MethodPost, "/api/businesses", map[string]any{
		"code": " 777001 ", "name": " 测试业务 ", "tag": "TEST", "description": "集成测试", "enabled": true, "tone": "green",
	}, true)
	if status != http.StatusOK {
		t.Fatalf("create: status=%d body=%s", status, body)
	}
	var created business
	decodeResponse(t, body, &created)
	if created.ID == 0 || created.Code != "777001" || created.Name != "测试业务" {
		t.Fatalf("unexpected business: %+v", created)
	}

	status, body = f.request(t, http.MethodPatch, "/api/businesses/"+itoa(created.ID), map[string]any{"enabled": false}, true)
	if status != http.StatusOK {
		t.Fatalf("patch: status=%d body=%s", status, body)
	}
	var patched business
	decodeResponse(t, body, &patched)
	if patched.Enabled || patched.Code != created.Code || patched.Name != created.Name {
		t.Fatalf("partial patch did not preserve fields: %+v", patched)
	}

	status, body = f.request(t, http.MethodPut, "/api/businesses/"+itoa(created.ID), map[string]any{
		"code": "777002", "name": "测试业务二", "tag": "UPDATED", "description": "已修改", "enabled": true, "tone": "orange",
	}, true)
	if status != http.StatusOK {
		t.Fatalf("put: status=%d body=%s", status, body)
	}
	decodeResponse(t, body, &patched)
	if patched.Code != "777002" || patched.Description != "已修改" || !patched.Enabled {
		t.Fatalf("unexpected update: %+v", patched)
	}

	status, body = f.request(t, http.MethodDelete, "/api/businesses/"+itoa(created.ID), nil, true)
	if status != http.StatusNoContent {
		t.Fatalf("delete: status=%d body=%s", status, body)
	}
	status, _ = f.request(t, http.MethodGet, "/api/businesses/"+itoa(created.ID), nil, true)
	if status != http.StatusNotFound {
		t.Fatalf("get deleted: got %d, want 404", status)
	}
}

func TestRuleCRUDAndDebounceValidation(t *testing.T) {
	f := newAPIFixture(t)
	payload := map[string]any{
		"code": "880001", "parent": "534784", "provider": "测试云", "account": "account-01",
		"threshold": 188.5, "fluctuation": 35, "debounce": "invalid", "purpose": "测试用途", "tag": "自动测试", "enabled": true,
	}
	status, _ := f.request(t, http.MethodPost, "/api/rules", payload, true)
	if status != http.StatusBadRequest {
		t.Fatalf("invalid debounce: got %d, want 400", status)
	}

	payload["debounce"] = " 01.500H "
	status, body := f.request(t, http.MethodPost, "/api/rules", payload, true)
	if status != http.StatusOK {
		t.Fatalf("create: status=%d body=%s", status, body)
	}
	var created rule
	decodeResponse(t, body, &created)
	if created.Debounce != "1.5h" || created.Threshold != 188.5 {
		t.Fatalf("rule was not normalized: %+v", created)
	}

	status, body = f.request(t, http.MethodPatch, "/api/rules/880001", map[string]any{
		"debounce": "0002D", "threshold": 250.25, "enabled": false,
	}, true)
	if status != http.StatusOK {
		t.Fatalf("patch: status=%d body=%s", status, body)
	}
	var patched rule
	decodeResponse(t, body, &patched)
	if patched.Debounce != "2d" || patched.Enabled || patched.Provider != "测试云" || patched.Threshold != 250.25 {
		t.Fatalf("unexpected patch: %+v", patched)
	}

	missingParent := map[string]any{
		"code": "880002", "parent": "missing", "provider": "测试云", "account": "account-02",
		"threshold": 1, "fluctuation": 1, "debounce": "1m", "purpose": "测试", "enabled": true,
	}
	status, _ = f.request(t, http.MethodPost, "/api/rules", missingParent, true)
	if status != http.StatusBadRequest {
		t.Fatalf("missing parent: got %d, want 400", status)
	}
	status, body = f.request(t, http.MethodDelete, "/api/rules/880001", nil, true)
	if status != http.StatusNoContent {
		t.Fatalf("delete: status=%d body=%s", status, body)
	}
}

func TestRecordCRUD(t *testing.T) {
	f := newAPIFixture(t)
	status, body := f.request(t, http.MethodPost, "/api/records", map[string]any{
		"code": "664851", "type": "测试告警", "value": "¥88.00", "detail": "集成测试记录",
		"date": "2026-09-11", "time": "10:20:30",
	}, true)
	if status != http.StatusOK {
		t.Fatalf("create: status=%d body=%s", status, body)
	}
	var created record
	decodeResponse(t, body, &created)
	if created.ID == 0 || created.Business != "短信商" || created.Provider != "阿里云" || created.Status != "待处理" {
		t.Fatalf("unexpected record: %+v", created)
	}

	status, body = f.request(t, http.MethodPatch, "/api/records/"+itoa(created.ID), map[string]any{"status": "已确认"}, true)
	if status != http.StatusOK {
		t.Fatalf("patch: status=%d body=%s", status, body)
	}
	var patched record
	decodeResponse(t, body, &patched)
	if patched.Status != "已确认" || patched.Level != "confirmed" {
		t.Fatalf("unexpected status: %+v", patched)
	}
	status, _ = f.request(t, http.MethodPatch, "/api/records/"+itoa(created.ID), map[string]any{"status": "未知"}, true)
	if status != http.StatusBadRequest {
		t.Fatalf("invalid status: got %d, want 400", status)
	}
	status, body = f.request(t, http.MethodDelete, "/api/records/"+itoa(created.ID), nil, true)
	if status != http.StatusNoContent {
		t.Fatalf("delete: status=%d body=%s", status, body)
	}
}
