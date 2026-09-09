// path_test.go 测试文档路径安全校验。
//
// 测试覆盖：
//   - 合法路径通过
//   - 空路径拒绝
//   - 绝对路径拒绝（/ 和 \ 开头）
//   - Windows 盘符拒绝
//   - 反斜杠拒绝
//   - path traversal（..）拒绝
//   - 重复分隔符拒绝
//   - 控制字符拒绝
//   - 非 .md 后缀拒绝
//   - 超长路径拒绝
//   - 特殊文件路径判断
package document

import (
	"strings"
	"testing"
)

func TestValidatePath_ValidPaths(t *testing.T) {
	validPaths := []string{
		"PROJECT.md",
		"AGENTS.md",
		"architecture/overview.md",
		"codebase/auth/middleware.md",
		"development/setup.md",
		"decisions/ADR-001.md",
		"issues/bug-123.md",
		"roadmap/2024-q1.md",
		"research/embedding-providers.md",
		"references/links.md",
		"operations/deploy.md",
		"standards/coding.md",
		"a/b/c/d/e/f/g.md",
	}

	for _, path := range validPaths {
		t.Run(path, func(t *testing.T) {
			if err := ValidatePath(path); err != nil {
				t.Errorf("合法路径 %q 被错误拒绝: %v", path, err)
			}
		})
	}
}

func TestValidatePath_InvalidPaths(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{"空路径", ""},
		{"绝对路径-正斜杠", "/etc/passwd.md"},
		{"绝对路径-反斜杠", "\\windows\\system32.md"},
		{"Windows盘符", "C:/Users/test.md"},
		{"包含反斜杠", "architecture\\overview.md"},
		{"path traversal", "../etc/passwd.md"},
		{"path traversal 中间", "architecture/../etc/passwd.md"},
		{"path traversal 末尾", "architecture/overview/..md"},
		{"重复分隔符", "architecture//overview.md"},
		{"以分隔符开头", "/architecture/overview.md"},
		{"控制字符", "architecture\x00/overview.md"},
		{"非md后缀", "architecture/overview.txt"},
		{"无后缀", "architecture/overview"},
		{"点开头路径段", "architecture/.hidden.md"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePath(tt.path)
			if err == nil {
				t.Errorf("非法路径 %q 应被拒绝，但通过了校验", tt.path)
			}
		})
	}
}

func TestValidatePath_TooLong(t *testing.T) {
	longPath := strings.Repeat("a", MaxPathLength+1) + ".md"
	if err := ValidatePath(longPath); err == nil {
		t.Error("超长路径应被拒绝")
	}
}

func TestIsSpecialFilePath(t *testing.T) {
	if !IsSpecialFilePath(SpecialFileProject) {
		t.Error("PROJECT.md 应为特殊文件路径")
	}
	if !IsSpecialFilePath(SpecialFileAgents) {
		t.Error("AGENTS.md 应为特殊文件路径")
	}
	if IsSpecialFilePath("architecture/overview.md") {
		t.Error("普通路径不应为特殊文件路径")
	}
}
