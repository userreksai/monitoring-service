CREATE TABLE IF NOT EXISTS user_account (
 account VARCHAR(64) PRIMARY KEY,
 password_hash VARCHAR(255) NOT NULL,
 display_name VARCHAR(100) NOT NULL DEFAULT '运维中心主控'
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS business (
 id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
 business_code VARCHAR(32) NOT NULL UNIQUE,
 business_name VARCHAR(100) NOT NULL,
 notify_enabled BOOLEAN NOT NULL DEFAULT TRUE,
 tag VARCHAR(255) NOT NULL DEFAULT '',
 description TEXT,
 tone VARCHAR(32) NOT NULL DEFAULT 'green',
 created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
 updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS business_detail (
 id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
 detail_code VARCHAR(32) NOT NULL UNIQUE,
 business_code VARCHAR(32) NOT NULL,
 business_name VARCHAR(100) NOT NULL,
 fluctuation_percent DECIMAL(6,2) NOT NULL,
 balance_threshold DECIMAL(18,4) NOT NULL,
 vendor VARCHAR(100) NOT NULL,
 account VARCHAR(255) NOT NULL,
 notify_enabled BOOLEAN NOT NULL DEFAULT TRUE,
 tag VARCHAR(255),
 alert_silence_seconds BIGINT UNSIGNED NOT NULL DEFAULT 0,
 business_purpose VARCHAR(255),
 created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
 updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
 KEY idx_detail_business_code (business_code),
 KEY idx_detail_vendor_account (vendor,account),
 CONSTRAINT fk_detail_business_code FOREIGN KEY (business_code) REFERENCES business(business_code) ON UPDATE CASCADE ON DELETE RESTRICT,
 CONSTRAINT chk_fluctuation_percent CHECK (fluctuation_percent BETWEEN 0 AND 100),
 CONSTRAINT chk_balance_threshold CHECK (balance_threshold >= 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS notification_log (
 id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
 alert_time DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
 detail_code VARCHAR(32) NOT NULL,
 business_name VARCHAR(100) NOT NULL,
 vendor VARCHAR(100) NOT NULL,
 account VARCHAR(255) NOT NULL,
 alert_content TEXT NOT NULL,
 alert_type VARCHAR(255) NOT NULL DEFAULT '',
 alert_value VARCHAR(255) NOT NULL DEFAULT '',
 status VARCHAR(32) NOT NULL DEFAULT '待处理',
 level VARCHAR(32) NOT NULL DEFAULT 'warning',
 event_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL UNIQUE,
 created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
 KEY idx_log_detail_time (detail_code,alert_time),
 KEY idx_log_alert_time (alert_time),
 CONSTRAINT fk_log_detail_code FOREIGN KEY (detail_code) REFERENCES business_detail(detail_code) ON UPDATE CASCADE ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
