package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"testing"
)

func TestLegacySHA256AndDebounce(t *testing.T) {
	sum := sha256.Sum256([]byte("legacy-password"))
	encoded := hex.EncodeToString(sum[:])
	if !verifyPassword(encoded, "legacy-password") || verifyPassword(encoded, "wrong") {
		t.Fatal("legacy SHA256 login failed")
	}
	for _, value := range []string{"1.5h", "2d", "10m", "0m", "0.5m"} {
		normal, err := normalizeDebounce(value)
		if err != nil {
			t.Fatal(err)
		}
		seconds := debounceSeconds(normal)
		if seconds != debounceSeconds(displayDebounce(strconv.FormatUint(seconds, 10))) {
			t.Fatal("round trip failed", value)
		}
	}
	if _, err := normalizeDebounce("0.001m"); err == nil {
		t.Fatal("fractional seconds must be rejected")
	}
	if displayDebounce("5400") != "1.5h" || debounceSeconds("1.5h") != 5400 {
		t.Fatal("duration conversion")
	}
}

func TestUpgradeOriginalMySQLSchema(t *testing.T) {
	db := newTestDatabase(t)
	statements := []string{
		`CREATE TABLE user_account(account VARCHAR(64) PRIMARY KEY,password_hash CHAR(64) NOT NULL) ENGINE=InnoDB`,
		`INSERT INTO user_account VALUES('admin',SHA2('legacy-password',256))`,
		`CREATE TABLE business(id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,business_code VARCHAR(32) UNIQUE NOT NULL,business_name VARCHAR(100) NOT NULL,notify_enabled BOOLEAN NOT NULL DEFAULT TRUE,created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP) ENGINE=InnoDB`,
		`INSERT INTO business(business_code,business_name) VALUES('534784','原有短信商')`,
	}
	for _, q := range statements {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db); err != nil {
		t.Fatal("repeat migration", err)
	}
	if err := seed(db, "admin", "do-not-overwrite"); err != nil {
		t.Fatal(err)
	}
	var hash, name string
	if err := db.QueryRow(`SELECT password_hash FROM user_account WHERE account='admin'`).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if !verifyPassword(hash, "legacy-password") {
		t.Fatal("existing user was overwritten")
	}
	if err := db.QueryRow(`SELECT business_name FROM business WHERE business_code='534784'`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "原有短信商" {
		t.Fatal("existing business overwritten")
	}
}

func TestMySQLListsStatsNoopAndIdempotency(t *testing.T) {
	f := newAPIFixture(t)
	for _, path := range []string{"/api/businesses", "/api/rules", "/api/records", "/api/stats"} {
		status, body := f.request(t, "GET", path, nil, true)
		if status != 200 {
			t.Fatalf("%s: %d %s", path, status, body)
		}
	}
	// MySQL normally reports zero changed rows for an unchanged update.
	status, body := f.request(t, "PATCH", "/api/rules/664851", map[string]any{"enabled": true}, true)
	if status != 200 {
		t.Fatalf("noop patch: %d %s", status, body)
	}
	payload := map[string]any{"code": "664851", "type": "余额不足", "value": "0", "detail": "test", "eventId": "0123456789abcdef0123456789abcdef"}
	var first, second record
	status, body = f.request(t, "POST", "/api/records", payload, true)
	if status != 200 {
		t.Fatalf("first record: %d %s", status, body)
	}
	decodeResponse(t, body, &first)
	status, body = f.request(t, "POST", "/api/records", payload, true)
	if status != 200 {
		t.Fatalf("retry record: %d %s", status, body)
	}
	decodeResponse(t, body, &second)
	if first.ID != second.ID {
		t.Fatal("retry duplicated record")
	}
	// Preserve original RESTRICT behavior instead of silently cascading historical records.
	status, _ = f.request(t, http.MethodDelete, "/api/rules/664851", nil, true)
	if status != http.StatusConflict {
		t.Fatalf("delete with records got %d", status)
	}
}
