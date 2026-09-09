// Package db 封装 PostgreSQL 数据库连接池的创建与生命周期管理。
//
// 引入动机：Phase 1 需要一个统一的数据库连接入口，供 migration runner 和后续业务模块复用。
// 使用 pgx 作为 database/sql 驱动，因为它是最成熟的纯 Go PostgreSQL 驱动，
// 支持 context、预编译语句缓存等特性，且兼容标准 database/sql 接口。
package db

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // 注册 pgx 为 database/sql 驱动
)

// New 创建并验证一个 PostgreSQL 连接池。
//
// 参数：
//   - dsn：PostgreSQL 连接字符串
//   - maxOpenConns：最大并发连接数
//   - maxIdleConns：最大空闲连接数
//   - connMaxLifetime：单个连接的最大存活时间
//
// 返回的 *sql.DB 已通过 Ping 验证连通性。调用方负责调用 Close 释放连接。
// 任何错误都会通过 slog 记录并向上返回，不会吞错。
func New(ctx context.Context, dsn string, maxOpenConns, maxIdleConns int, connMaxLifetime time.Duration) (*sql.DB, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		slog.Error("打开 PostgreSQL 连接失败", "dsn_masked", maskDSN(dsn), "error", err)
		return nil, fmt.Errorf("打开 PostgreSQL 连接: %w", err)
	}

	db.SetMaxOpenConns(maxOpenConns)
	db.SetMaxIdleConns(maxIdleConns)
	db.SetConnMaxLifetime(connMaxLifetime)

	// 使用带超时的 context 执行 Ping，避免无限等待
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if err := db.PingContext(pingCtx); err != nil {
		slog.Error("Ping PostgreSQL 失败", "dsn_masked", maskDSN(dsn), "error", err)
		_ = db.Close()
		return nil, fmt.Errorf("Ping PostgreSQL: %w", err)
	}

	slog.Info("PostgreSQL 连接池已建立", "max_open", maxOpenConns, "max_idle", maxIdleConns)
	return db, nil
}

// Close 安全关闭数据库连接池。
// 错误通过 slog 记录但不向上返回，因为关闭阶段通常无法做更多恢复。
func Close(db *sql.DB) {
	if db == nil {
		return
	}
	if err := db.Close(); err != nil {
		slog.Error("关闭 PostgreSQL 连接池时出错", "error", err)
	}
}

// maskDSN 隐藏 DSN 中的密码部分，用于日志输出。
func maskDSN(dsn string) string {
	// pgx DSN 格式: host=... password=secret dbname=...
	// 使用标准库 strings.Index 定位 password= 字段
	idx := strings.Index(dsn, "password=")
	if idx < 0 {
		return dsn
	}
	start := idx + len("password=")
	end := start
	for end < len(dsn) && dsn[end] != ' ' {
		end++
	}
	if end > start {
		return dsn[:start] + "***" + dsn[end:]
	}
	return dsn
}
