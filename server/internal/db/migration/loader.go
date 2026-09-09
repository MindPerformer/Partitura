// 迁移文件加载器。
//
// 引入动机：需要从 server/migrations/ 目录读取 SQL 文件并构建 Migration 对象。
// 文件命名约定：M001_name_up.sql / M001_name_down.sql
package migration

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// LoadMigrations 从指定目录读取所有迁移 SQL 文件并构建 Migration 列表。
// 目录中应包含成对的 M{NNN}_{name}_up.sql 和 M{NNN}_{name}_down.sql 文件。
// 返回的列表按版本号升序排列。
func LoadMigrations(dir string) ([]Migration, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("读取迁移目录 %s: %w", dir, err)
	}

	// 按 version+direction 收集 SQL 内容
	type fileKey struct {
		version  int
		name     string
		direction string
	}
	contents := make(map[fileKey]string)

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		filename := entry.Name()
		if !strings.HasSuffix(filename, ".sql") {
			continue
		}

		version, name, direction, ok := ParseMigrationName(filename)
		if !ok {
			return nil, fmt.Errorf("无法解析迁移文件名 %q：应为 M{NNN}_{name}_up.sql 或 M{NNN}_{name}_down.sql 格式", filename)
		}

		path := filepath.Join(dir, filename)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("读取迁移文件 %s: %w", path, err)
		}

		key := fileKey{version: version, name: name, direction: direction}
		if _, exists := contents[key]; exists {
			return nil, fmt.Errorf("迁移文件重复：版本 %d 方向 %s 出现多次", version, direction)
		}
		contents[key] = string(data)
	}

	// 按版本号聚合 up/down
	versionMap := make(map[int]Migration)
	for key, sqlContent := range contents {
		m, exists := versionMap[key.version]
		if !exists {
			m = Migration{Version: key.version, Name: key.name}
		}
		if m.Name != key.name {
			return nil, fmt.Errorf("版本 %d 的 up/down 文件名不一致: %q vs %q", key.version, m.Name, key.name)
		}
		switch key.direction {
		case "up":
			m.UpSQL = sqlContent
		case "down":
			m.DownSQL = sqlContent
		}
		versionMap[key.version] = m
	}

	// 验证每个版本都有 up 和 down
	var migrations []Migration
	for version, m := range versionMap {
		if m.UpSQL == "" {
			return nil, fmt.Errorf("版本 %d (%s) 缺少 up SQL 文件", version, m.Name)
		}
		if m.DownSQL == "" {
			return nil, fmt.Errorf("版本 %d (%s) 缺少 down SQL 文件", version, m.Name)
		}
		migrations = append(migrations, m)
	}

	sort.Slice(migrations, func(i, j int) bool {
		return migrations[i].Version < migrations[j].Version
	})

	if len(migrations) == 0 {
		return nil, fmt.Errorf("迁移目录 %s 中未找到任何迁移文件", dir)
	}

	return migrations, nil
}
