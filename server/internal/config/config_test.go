// Package config — 配置加载与验证测试。
//
// 测试覆盖：
//   - 必填项缺失时返回错误
//   - 非法值（端口超范围、超时为负）时返回错误
//   - 全部配置正确时返回有效 Config
//   - DSN 生成正确
package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

// setEnv 设置环境变量并在测试结束后恢复。
func setEnv(t *testing.T, key, value string) {
	t.Helper()
	old, ok := os.LookupEnv(key)
	os.Setenv(key, value)
	t.Cleanup(func() {
		if ok {
			os.Setenv(key, old)
		} else {
			os.Unsetenv(key)
		}
	})
}

// clearEnv 清除所有配置相关环境变量。
func clearEnv(t *testing.T) {
	t.Helper()
	keys := []string{"DB_HOST", "DB_PORT", "DB_USER", "DB_PASSWORD", "DB_NAME",
		"DB_SSLMODE", "HTTP_ADDR", "LOG_LEVEL", "SHUTDOWN_TIMEOUT",
		"HEALTH_PROVIDER_CHECK_TIMEOUT_SECONDS"}
	for _, k := range keys {
		old, ok := os.LookupEnv(k)
		os.Unsetenv(k)
		t.Cleanup(func() {
			if ok {
				os.Setenv(k, old)
			} else {
				os.Unsetenv(k)
			}
		})
	}
}

// setRequiredEnv 设置所有必填环境变量，便于聚焦单个配置项的测试。
func setRequiredEnv(t *testing.T) {
	t.Helper()
	setEnv(t, "DB_HOST", "localhost")
	setEnv(t, "DB_PORT", "5432")
	setEnv(t, "DB_USER", "testuser")
	setEnv(t, "DB_PASSWORD", "secret")
	setEnv(t, "DB_NAME", "testdb")
	setEnv(t, "DB_SSLMODE", "disable")
}

func TestLoad_MissingAllRequired(t *testing.T) {
	clearEnv(t)
	_, err := Load()
	if err == nil {
		t.Fatal("期望返回错误，但 Load 成功")
	}
	// 应该报告多个缺失项
	for _, key := range []string{"DB_HOST", "DB_PORT", "DB_USER", "DB_PASSWORD", "DB_NAME", "DB_SSLMODE"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("错误信息中应包含 %s，实际: %s", key, err.Error())
		}
	}
}

func TestLoad_InvalidPort(t *testing.T) {
	clearEnv(t)
	setEnv(t, "DB_HOST", "localhost")
	setEnv(t, "DB_PORT", "99999")
	setEnv(t, "DB_USER", "test")
	setEnv(t, "DB_PASSWORD", "test123")
	setEnv(t, "DB_NAME", "testdb")
	setEnv(t, "DB_SSLMODE", "disable")

	_, err := Load()
	if err == nil {
		t.Fatal("期望返回端口非法错误")
	}
	if !strings.Contains(err.Error(), "DB_PORT") {
		t.Errorf("错误信息应包含 DB_PORT，实际: %s", err.Error())
	}
}

func TestLoad_InvalidShutdownTimeout(t *testing.T) {
	clearEnv(t)
	setEnv(t, "DB_HOST", "localhost")
	setEnv(t, "DB_PORT", "5432")
	setEnv(t, "DB_USER", "test")
	setEnv(t, "DB_PASSWORD", "test123")
	setEnv(t, "DB_NAME", "testdb")
	setEnv(t, "DB_SSLMODE", "disable")
	setEnv(t, "SHUTDOWN_TIMEOUT", "-5s")

	_, err := Load()
	if err == nil {
		t.Fatal("期望返回超时非法错误")
	}
	if !strings.Contains(err.Error(), "SHUTDOWN_TIMEOUT") {
		t.Errorf("错误信息应包含 SHUTDOWN_TIMEOUT，实际: %s", err.Error())
	}
}

func TestLoad_ValidConfig(t *testing.T) {
	clearEnv(t)
	setEnv(t, "DB_HOST", "localhost")
	setEnv(t, "DB_PORT", "5432")
	setEnv(t, "DB_USER", "testuser")
	setEnv(t, "DB_PASSWORD", "secret")
	setEnv(t, "DB_NAME", "testdb")
	setEnv(t, "DB_SSLMODE", "require")
	setEnv(t, "HTTP_ADDR", ":9090")
	setEnv(t, "LOG_LEVEL", "debug")
	setEnv(t, "SHUTDOWN_TIMEOUT", "45s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}

	if cfg.DBHost != "localhost" {
		t.Errorf("DBHost = %q, 期望 localhost", cfg.DBHost)
	}
	if cfg.DBPort != 5432 {
		t.Errorf("DBPort = %d, 期望 5432", cfg.DBPort)
	}
	if cfg.DBUser != "testuser" {
		t.Errorf("DBUser = %q, 期望 testuser", cfg.DBUser)
	}
	if cfg.DBPassword != "secret" {
		t.Errorf("DBPassword = %q, 期望 secret", cfg.DBPassword)
	}
	if cfg.DBName != "testdb" {
		t.Errorf("DBName = %q, 期望 testdb", cfg.DBName)
	}
	if cfg.DBSSLMode != "require" {
		t.Errorf("DBSSLMode = %q, 期望 require", cfg.DBSSLMode)
	}
	if cfg.HTTPAddr != ":9090" {
		t.Errorf("HTTPAddr = %q, 期望 :9090", cfg.HTTPAddr)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, 期望 debug", cfg.LogLevel)
	}
	if cfg.ShutdownTimeout != 45*time.Second {
		t.Errorf("ShutdownTimeout = %v, 期望 45s", cfg.ShutdownTimeout)
	}
}

func TestLoad_DefaultValues(t *testing.T) {
	clearEnv(t)
	setEnv(t, "DB_HOST", "localhost")
	setEnv(t, "DB_PORT", "5432")
	setEnv(t, "DB_USER", "testuser")
	setEnv(t, "DB_PASSWORD", "secret")
	setEnv(t, "DB_NAME", "testdb")
	setEnv(t, "DB_SSLMODE", "disable")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}

	// 未设置的可选项应有安全默认值
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr 默认值 = %q, 期望 :8080", cfg.HTTPAddr)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel 默认值 = %q, 期望 info", cfg.LogLevel)
	}
	if cfg.ShutdownTimeout != 30*time.Second {
		t.Errorf("ShutdownTimeout 默认值 = %v, 期望 30s", cfg.ShutdownTimeout)
	}
	if cfg.ProviderHealthCheckTimeout != 30*time.Second {
		t.Errorf("ProviderHealthCheckTimeout 默认值 = %v, 期望 30s", cfg.ProviderHealthCheckTimeout)
	}
}

// TestLoad_ProviderHealthCheckTimeoutFromEnv 验证构造器级替换：环境变量覆盖默认的超时。
// 引入动机：/readyz 的 Provider 检查超时必须由部署方配置，而非写死。
func TestLoad_ProviderHealthCheckTimeoutFromEnv(t *testing.T) {
	clearEnv(t)
	setRequiredEnv(t)
	setEnv(t, "HEALTH_PROVIDER_CHECK_TIMEOUT_SECONDS", "45")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if cfg.ProviderHealthCheckTimeout != 45*time.Second {
		t.Errorf("ProviderHealthCheckTimeout = %v, 期望 45s", cfg.ProviderHealthCheckTimeout)
	}
}

// TestLoad_InvalidProviderHealthCheckTimeout 验证非法超时值 fail fast，不静默回退默认值。
func TestLoad_InvalidProviderHealthCheckTimeout(t *testing.T) {
	for _, raw := range []string{"abc", "0", "-3", "1.5"} {
		t.Run(raw, func(t *testing.T) {
			clearEnv(t)
			setRequiredEnv(t)
			setEnv(t, "HEALTH_PROVIDER_CHECK_TIMEOUT_SECONDS", raw)

			cfg, err := Load()
			if err == nil {
				t.Fatalf("非法值 %q 期望返回错误，但 Load 成功（ProviderHealthCheckTimeout=%v）", raw, cfg.ProviderHealthCheckTimeout)
			}
			if !strings.Contains(err.Error(), "HEALTH_PROVIDER_CHECK_TIMEOUT_SECONDS") {
				t.Errorf("错误信息应包含 HEALTH_PROVIDER_CHECK_TIMEOUT_SECONDS，实际: %s", err.Error())
			}
		})
	}
}

func TestLoad_MissingDBSSLMode(t *testing.T) {
	clearEnv(t)
	setEnv(t, "DB_HOST", "localhost")
	setEnv(t, "DB_PORT", "5432")
	setEnv(t, "DB_USER", "testuser")
	setEnv(t, "DB_PASSWORD", "secret")
	setEnv(t, "DB_NAME", "testdb")
	// 故意不设置 DB_SSLMODE

	_, err := Load()
	if err == nil {
		t.Fatal("期望返回 DB_SSLMODE 缺失错误")
	}
	if !strings.Contains(err.Error(), "DB_SSLMODE") {
		t.Errorf("错误信息应包含 DB_SSLMODE，实际: %s", err.Error())
	}
}

func TestLoad_InvalidDBSSLMode(t *testing.T) {
	clearEnv(t)
	setEnv(t, "DB_HOST", "localhost")
	setEnv(t, "DB_PORT", "5432")
	setEnv(t, "DB_USER", "testuser")
	setEnv(t, "DB_PASSWORD", "secret")
	setEnv(t, "DB_NAME", "testdb")
	setEnv(t, "DB_SSLMODE", "invalid-mode")

	_, err := Load()
	if err == nil {
		t.Fatal("期望返回 DB_SSLMODE 非法错误")
	}
	if !strings.Contains(err.Error(), "DB_SSLMODE") {
		t.Errorf("错误信息应包含 DB_SSLMODE，实际: %s", err.Error())
	}
}

func TestLoad_InvalidLogLevel(t *testing.T) {
	clearEnv(t)
	setEnv(t, "DB_HOST", "localhost")
	setEnv(t, "DB_PORT", "5432")
	setEnv(t, "DB_USER", "testuser")
	setEnv(t, "DB_PASSWORD", "secret")
	setEnv(t, "DB_NAME", "testdb")
	setEnv(t, "DB_SSLMODE", "disable")
	setEnv(t, "LOG_LEVEL", "trace")

	_, err := Load()
	if err == nil {
		t.Fatal("期望返回 LOG_LEVEL 非法错误")
	}
	if !strings.Contains(err.Error(), "LOG_LEVEL") {
		t.Errorf("错误信息应包含 LOG_LEVEL，实际: %s", err.Error())
	}
}

func TestLoad_ValidSSLModes(t *testing.T) {
	validModes := []string{"disable", "allow", "prefer", "require", "verify-ca", "verify-full"}
	for _, mode := range validModes {
		t.Run(mode, func(t *testing.T) {
			clearEnv(t)
			setEnv(t, "DB_HOST", "localhost")
			setEnv(t, "DB_PORT", "5432")
			setEnv(t, "DB_USER", "testuser")
			setEnv(t, "DB_PASSWORD", "secret")
			setEnv(t, "DB_NAME", "testdb")
			setEnv(t, "DB_SSLMODE", mode)

			cfg, err := Load()
			if err != nil {
				t.Fatalf("合法 SSL 模式 %q 应通过验证: %v", mode, err)
			}
			if cfg.DBSSLMode != mode {
				t.Errorf("DBSSLMode = %q, 期望 %q", cfg.DBSSLMode, mode)
			}
		})
	}
}

func TestLoad_ValidLogLevels(t *testing.T) {
	validLevels := []string{"debug", "info", "warn", "error"}
	for _, level := range validLevels {
		t.Run(level, func(t *testing.T) {
			clearEnv(t)
			setEnv(t, "DB_HOST", "localhost")
			setEnv(t, "DB_PORT", "5432")
			setEnv(t, "DB_USER", "testuser")
			setEnv(t, "DB_PASSWORD", "secret")
			setEnv(t, "DB_NAME", "testdb")
			setEnv(t, "DB_SSLMODE", "disable")
			setEnv(t, "LOG_LEVEL", level)

			cfg, err := Load()
			if err != nil {
				t.Fatalf("合法日志级别 %q 应通过验证: %v", level, err)
			}
			if cfg.LogLevel != level {
				t.Errorf("LogLevel = %q, 期望 %q", cfg.LogLevel, level)
			}
		})
	}
}

func TestDSN(t *testing.T) {
	cfg := &Config{
		DBHost:     "localhost",
		DBPort:     5432,
		DBUser:     "user",
		DBPassword: "pass",
		DBName:     "mydb",
		DBSSLMode:  "disable",
	}

	dsn := cfg.DSN()
	// DSN 应包含所有连接参数
	for _, part := range []string{"host=localhost", "port=5432", "user=user", "password=pass", "dbname=mydb", "sslmode=disable"} {
		if !strings.Contains(dsn, part) {
			t.Errorf("DSN 中应包含 %q, 实际: %s", part, dsn)
		}
	}
}
