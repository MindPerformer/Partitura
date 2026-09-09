// outline_test.go 测试 Markdown ATX heading 解析、section 读取和行范围读取。
//
// 测试覆盖：
//   - 多级 heading 解析（level, text, line numbers, section_path）
//   - 空 Markdown / 无 heading
//   - Setext heading 不识别
//   - heading 尾部 # 序列去除
//   - section read 按 section_path 精确定位
//   - section read 未找到
//   - lines read 精确行范围
//   - lines read 超限拒绝
//   - lines read 起始行超出文档行数
package document

import (
	"strings"
	"testing"
)

func TestParseOutline_MultiLevel(t *testing.T) {
	content := `# Architecture

## Components

### Database
PostgreSQL is used.

### API
REST API.

## Security

# Development

## Setup
Run the server.
`
	headings := ParseOutline(content)

	if len(headings) != 7 {
		t.Fatalf("应解析出 7 个 heading，实际 %d", len(headings))
	}

	// 验证第一个 heading
	if headings[0].Level != 1 || headings[0].Text != "Architecture" || headings[0].StartLine != 1 {
		t.Errorf("heading[0] = %+v, 期望 level=1, text=Architecture, start_line=1", headings[0])
	}
	if len(headings[0].SectionPath) != 1 || headings[0].SectionPath[0] != "Architecture" {
		t.Errorf("heading[0] section_path = %v, 期望 [Architecture]", headings[0].SectionPath)
	}

	// 验证 "Components" heading
	if headings[1].Text != "Components" || headings[1].Level != 2 {
		t.Errorf("heading[1] = %+v, 期望 text=Components, level=2", headings[1])
	}
	if len(headings[1].SectionPath) != 2 || headings[1].SectionPath[0] != "Architecture" || headings[1].SectionPath[1] != "Components" {
		t.Errorf("heading[1] section_path = %v, 期望 [Architecture, Components]", headings[1].SectionPath)
	}

	// 验证 "Database" heading
	if headings[2].Text != "Database" || headings[2].Level != 3 {
		t.Errorf("heading[2] = %+v, 期望 text=Database, level=3", headings[2])
	}
	if len(headings[2].SectionPath) != 3 {
		t.Errorf("heading[2] section_path 长度 = %d, 期望 3", len(headings[2].SectionPath))
	}

	// 验证 "Security" heading 的 end_line（应为 "Development" heading 行号 - 1）
	// "Development" 在第 13 行（0-indexed line 12）
	securityIdx := -1
	devIdx := -1
	for i, h := range headings {
		if h.Text == "Security" {
			securityIdx = i
		}
		if h.Text == "Development" {
			devIdx = i
		}
	}
	if securityIdx >= 0 && devIdx >= 0 {
		if headings[securityIdx].EndLine != headings[devIdx].StartLine-1 {
			t.Errorf("Security end_line = %d, 期望 %d", headings[securityIdx].EndLine, headings[devIdx].StartLine-1)
		}
	}
}

func TestParseOutline_EmptyContent(t *testing.T) {
	headings := ParseOutline("")
	if len(headings) != 0 {
		t.Errorf("空内容应返回 0 个 heading，实际 %d", len(headings))
	}
}

func TestParseOutline_NoHeadings(t *testing.T) {
	content := "This is a paragraph.\n\nAnother paragraph.\n"
	headings := ParseOutline(content)
	if len(headings) != 0 {
		t.Errorf("无 heading 的内容应返回 0 个 heading，实际 %d", len(headings))
	}
}

func TestParseOutline_TrailingHashes(t *testing.T) {
	content := "## Heading ##\n"
	headings := ParseOutline(content)
	if len(headings) != 1 {
		t.Fatalf("应解析出 1 个 heading，实际 %d", len(headings))
	}
	if headings[0].Text != "Heading" {
		t.Errorf("text = %q, 期望 Heading（尾部 # 应去除）", headings[0].Text)
	}
}

func TestParseOutline_SetextNotRecognized(t *testing.T) {
	// Setext headings (underline style) should NOT be recognized
	content := "Heading\n=======\n\nSubheading\n----------\n"
	headings := ParseOutline(content)
	if len(headings) != 0 {
		t.Errorf("Setext headings 不应被识别，实际解析出 %d 个", len(headings))
	}
}

func TestParseOutline_AllSixLevels(t *testing.T) {
	content := "# H1\n## H2\n### H3\n#### H4\n##### H5\n###### H6\n"
	headings := ParseOutline(content)
	if len(headings) != 6 {
		t.Fatalf("应解析出 6 个 heading，实际 %d", len(headings))
	}
	for i, h := range headings {
		if h.Level != i+1 {
			t.Errorf("heading[%d].Level = %d, 期望 %d", i, h.Level, i+1)
		}
	}
}

func TestParseOutline_SevenHashesNotHeading(t *testing.T) {
	content := "####### Seven hashes\n"
	headings := ParseOutline(content)
	if len(headings) != 0 {
		t.Errorf("7 个 # 不应被识别为 heading，实际 %d 个", len(headings))
	}
}

func TestReadSection_ByPath(t *testing.T) {
	content := `# Architecture

## Components

### Database
PostgreSQL is used.

### API
REST API.

## Security
Auth middleware.
`
	// 读取 Architecture > Components > Database section
	sectionContent, ok := ReadSection(content, []string{"Architecture", "Components", "Database"})
	if !ok {
		t.Fatal("section 未找到")
	}
	if !strings.Contains(sectionContent, "### Database") {
		t.Errorf("section 内容应包含 '### Database' heading，实际: %s", sectionContent)
	}
	if !strings.Contains(sectionContent, "PostgreSQL is used.") {
		t.Errorf("section 内容应包含 'PostgreSQL is used.'，实际: %s", sectionContent)
	}
}

func TestReadSection_NotFound(t *testing.T) {
	content := "# Architecture\n\nContent here.\n"
	_, ok := ReadSection(content, []string{"Nonexistent"})
	if ok {
		t.Error("不存在的 section 应返回 false")
	}
}

func TestReadSection_EmptyPath(t *testing.T) {
	content := "# Architecture\n"
	_, ok := ReadSection(content, []string{})
	if ok {
		t.Error("空 section_path 应返回 false")
	}
}

func TestReadLines_PreciseRange(t *testing.T) {
	content := "line1\nline2\nline3\nline4\nline5\n"
	result, err := ReadLines(content, 2, 4, MaxLineReadCount)
	if err != nil {
		t.Fatalf("ReadLines 失败: %v", err)
	}
	if result != "line2\nline3\nline4" {
		t.Errorf("result = %q, 期望 'line2\\nline3\\nline4'", result)
	}
}

func TestReadLines_ExceedsMaxLines(t *testing.T) {
	content := "line1\n"
	_, err := ReadLines(content, 1, MaxLineReadCount+1, MaxLineReadCount)
	if err == nil {
		t.Error("超过最大行数限制应返回错误")
	}
}

func TestReadLines_StartLessThanOne(t *testing.T) {
	content := "line1\n"
	_, err := ReadLines(content, 0, 1, MaxLineReadCount)
	if err == nil {
		t.Error("start < 1 应返回错误")
	}
}

func TestReadLines_EndLessThanStart(t *testing.T) {
	content := "line1\n"
	_, err := ReadLines(content, 3, 2, MaxLineReadCount)
	if err == nil {
		t.Error("end < start 应返回错误")
	}
}

func TestReadLines_StartBeyondDocument(t *testing.T) {
	content := "line1\nline2\n"
	result, err := ReadLines(content, 10, 20, MaxLineReadCount)
	if err != nil {
		t.Fatalf("超出文档行数不应报错: %v", err)
	}
	if result != "" {
		t.Errorf("超出文档行数应返回空字符串，实际 %q", result)
	}
}

func TestReadLines_ClampedToEnd(t *testing.T) {
	content := "line1\nline2\nline3\n"
	result, err := ReadLines(content, 2, 100, MaxLineReadCount)
	if err != nil {
		t.Fatalf("ReadLines 失败: %v", err)
	}
	if result != "line2\nline3\n" {
		t.Errorf("result = %q, 期望 'line2\\nline3\\n'", result)
	}
}

func TestComputeContentHash_Deterministic(t *testing.T) {
	content := "# Hello World\n"
	hash1 := ComputeContentHash(content)
	hash2 := ComputeContentHash(content)
	if hash1 != hash2 {
		t.Error("相同内容的 hash 应相同")
	}
	if len(hash1) != 64 {
		t.Errorf("hash 长度 = %d, 期望 64", len(hash1))
	}
}

func TestComputeContentHash_DifferentContent(t *testing.T) {
	hash1 := ComputeContentHash("content A")
	hash2 := ComputeContentHash("content B")
	if hash1 == hash2 {
		t.Error("不同内容的 hash 应不同")
	}
}
