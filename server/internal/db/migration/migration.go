// Package migration 实现 PostgreSQL 数据库迁移框架。
//
// 引入动机：Phase 1 需要一个可执行、可复用的 migration runner，支持版本追踪、顺序执行、失败停止。
// 设计原则：
//   - 使用 schema_migrations 表幂等追踪已执行版本
//   - 按版本号升序执行 up，降序执行 down
//   - 失败时停止后续迁移并回滚当前事务
//   - 不使用反射，纯 SQL 文件 + Go 编排
package migration

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sort"
	"strings"
)

// Migration 表示单个迁移版本，包含 up 和 down 的 SQL 语句。
type Migration struct {
	// Version 是迁移版本号，必须唯一且递增。
	Version int
	// Name 是迁移的描述性名称，用于日志和追踪。
	Name string
	// UpSQL 是执行升级时的 SQL 语句。
	UpSQL string
	// DownSQL 是执行回退时的 SQL 语句。
	DownSQL string
}

// Runner 是迁移执行器，负责按版本顺序执行迁移并记录状态。
type Runner struct {
	db         *sql.DB
	migrations []Migration
}

// NewRunner 创建迁移执行器。
// migrations 参数中的顺序不重要，Runner 内部会按 Version 排序。
func NewRunner(db *sql.DB, migrations []Migration) *Runner {
	sorted := make([]Migration, len(migrations))
	copy(sorted, migrations)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Version < sorted[j].Version
	})
	return &Runner{db: db, migrations: sorted}
}

// ensureSchemaMigrations 创建 schema_migrations 追踪表（如果不存在）。
// 该表记录已成功执行的迁移版本号和执行时间。
func (r *Runner) ensureSchemaMigrations(ctx context.Context) error {
	const sql_ = `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
	);`
	_, err := r.db.ExecContext(ctx, sql_)
	if err != nil {
		return fmt.Errorf("创建 schema_migrations 表: %w", err)
	}
	return nil
}

// getAppliedVersions 查询已执行的迁移版本号集合。
func (r *Runner) getAppliedVersions(ctx context.Context) (map[int]bool, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT version FROM schema_migrations ORDER BY version")
	if err != nil {
		return nil, fmt.Errorf("查询已执行迁移版本: %w", err)
	}
	defer rows.Close()

	applied := make(map[int]bool)
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("扫描迁移版本号: %w", err)
		}
		applied[v] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历迁移版本结果集: %w", err)
	}
	return applied, nil
}

// Up 执行所有未应用的迁移（按版本升序）。
// 每个迁移在独立事务中执行，失败时回滚当前事务并停止后续迁移。
// 返回实际执行的迁移数量。
func (r *Runner) Up(ctx context.Context) (int, error) {
	if err := r.ensureSchemaMigrations(ctx); err != nil {
		return 0, err
	}

	applied, err := r.getAppliedVersions(ctx)
	if err != nil {
		return 0, err
	}

	executed := 0
	for _, m := range r.migrations {
		if applied[m.Version] {
			continue
		}

		slog.Info("执行迁移", "version", m.Version, "name", m.Name, "direction", "up")

		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return executed, fmt.Errorf("开启迁移事务 v%d: %w", m.Version, err)
		}

		if _, err := tx.ExecContext(ctx, m.UpSQL); err != nil {
			_ = tx.Rollback()
			slog.Error("迁移执行失败，已回滚", "version", m.Version, "name", m.Name, "error", err)
			return executed, fmt.Errorf("执行迁移 v%d (%s) up SQL: %w", m.Version, m.Name, err)
		}

		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", m.Version); err != nil {
			_ = tx.Rollback()
			slog.Error("记录迁移版本失败，已回滚", "version", m.Version, "name", m.Name, "error", err)
			return executed, fmt.Errorf("记录迁移版本 v%d: %w", m.Version, err)
		}

		if err := tx.Commit(); err != nil {
			slog.Error("提交迁移事务失败", "version", m.Version, "name", m.Name, "error", err)
			return executed, fmt.Errorf("提交迁移事务 v%d: %w", m.Version, err)
		}

		executed++
		slog.Info("迁移完成", "version", m.Version, "name", m.Name)
	}

	if executed == 0 {
		slog.Info("无待执行迁移，数据库已是最新版本")
	}

	return executed, nil
}

// Down 回退指定数量的迁移（按版本降序）。
// steps=1 表示回退最近一个迁移。每个回退在独立事务中执行。
// 返回实际回退的迁移数量。
func (r *Runner) Down(ctx context.Context, steps int) (int, error) {
	if err := r.ensureSchemaMigrations(ctx); err != nil {
		return 0, err
	}

	applied, err := r.getAppliedVersions(ctx)
	if err != nil {
		return 0, err
	}

	// 构建已应用迁移的降序列表
	var appliedList []Migration
	for _, m := range r.migrations {
		if applied[m.Version] {
			appliedList = append(appliedList, m)
		}
	}
	sort.Slice(appliedList, func(i, j int) bool {
		return appliedList[i].Version > appliedList[j].Version
	})

	if steps > len(appliedList) {
		steps = len(appliedList)
	}

	reverted := 0
	for i := 0; i < steps; i++ {
		m := appliedList[i]

		slog.Info("执行回退", "version", m.Version, "name", m.Name, "direction", "down")

		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return reverted, fmt.Errorf("开启回退事务 v%d: %w", m.Version, err)
		}

		if _, err := tx.ExecContext(ctx, m.DownSQL); err != nil {
			_ = tx.Rollback()
			slog.Error("回退执行失败，已回滚", "version", m.Version, "name", m.Name, "error", err)
			return reverted, fmt.Errorf("执行迁移 v%d (%s) down SQL: %w", m.Version, m.Name, err)
		}

		if _, err := tx.ExecContext(ctx, "DELETE FROM schema_migrations WHERE version = $1", m.Version); err != nil {
			_ = tx.Rollback()
			slog.Error("删除迁移版本记录失败，已回滚", "version", m.Version, "name", m.Name, "error", err)
			return reverted, fmt.Errorf("删除迁移版本记录 v%d: %w", m.Version, err)
		}

		if err := tx.Commit(); err != nil {
			slog.Error("提交回退事务失败", "version", m.Version, "name", m.Name, "error", err)
			return reverted, fmt.Errorf("提交回退事务 v%d: %w", m.Version, err)
		}

		reverted++
		slog.Info("回退完成", "version", m.Version, "name", m.Name)
	}

	if reverted == 0 {
		slog.Info("无可回退迁移")
	}

	return reverted, nil
}

// CurrentVersion 返回当前已应用的最高迁移版本号。
// 如果没有任何迁移被应用，返回 0。
func (r *Runner) CurrentVersion(ctx context.Context) (int, error) {
	if err := r.ensureSchemaMigrations(ctx); err != nil {
		return 0, err
	}

	var version sql.NullInt64
	err := r.db.QueryRowContext(ctx, "SELECT MAX(version) FROM schema_migrations").Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("查询当前迁移版本: %w", err)
	}
	if !version.Valid {
		return 0, nil
	}
	return int(version.Int64), nil
}

// PendingVersions 返回尚未应用的迁移版本号列表（升序）。
func (r *Runner) PendingVersions(ctx context.Context) ([]int, error) {
	if err := r.ensureSchemaMigrations(ctx); err != nil {
		return nil, err
	}

	applied, err := r.getAppliedVersions(ctx)
	if err != nil {
		return nil, err
	}

	var pending []int
	for _, m := range r.migrations {
		if !applied[m.Version] {
			pending = append(pending, m.Version)
		}
	}
	return pending, nil
}

// ParseMigrationName 从 SQL 文件名中提取版本号和名称。
// 文件名格式：M001_name_up.sql / M001_name_down.sql
// 返回版本号和名称（如 1, "name"）。
func ParseMigrationName(filename string) (version int, name string, direction string, ok bool) {
	// 去除 .sql 后缀
	base := strings.TrimSuffix(filename, ".sql")
	if base == filename {
		return 0, "", "", false
	}

	// 查找最后一个 _up 或 _down
	var dir string
	if strings.HasSuffix(base, "_up") {
		dir = "up"
		base = strings.TrimSuffix(base, "_up")
	} else if strings.HasSuffix(base, "_down") {
		dir = "down"
		base = strings.TrimSuffix(base, "_down")
	} else {
		return 0, "", "", false
	}

	// 解析 M001_name 格式
	if !strings.HasPrefix(base, "M") {
		return 0, "", "", false
	}
	rest := base[1:]

	// 找到第一个非数字字符
	i := 0
	for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, "", "", false
	}

	num := 0
	for j := 0; j < i; j++ {
		num = num*10 + int(rest[j]-'0')
	}

	// 剩余部分去掉前导 _ 即为名称
	nm := strings.TrimPrefix(rest[i:], "_")
	if nm == "" {
		nm = "unnamed"
	}

	return num, nm, dir, true
}
