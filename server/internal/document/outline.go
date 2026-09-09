// outline.go 实现 Markdown ATX heading 解析，生成层级树和 section path。
//
// 引入动机：design/03-DOCUMENTS.md §大文档 要求 outline、section read、lines read
// 优先于全文读取。design/04-WEB-API.md §Document 要求 outline endpoint 返回 heading tree。
//
// 解析规则：
//   - 仅识别 ATX headings（`#`~`######`），不识别 Setext headings
//   - `#` 前不允许非空白字符（行首可有最多 3 个空格，遵循 CommonMark）
//   - `#` 后必须至少有一个空格或行尾
//   - heading text 去除首尾空白和尾部 `#` 序列
//   - section_path 是从根到当前 heading 的 heading text 序列
//   - start_line / end_line 定义 section 内容范围（含 heading 行本身）
//   - end_line 为下一个同级或更高级 heading 的行号 - 1，或文档末尾
package document

import (
	"fmt"
	"strings"
)

// Heading 表示一个 Markdown ATX heading 的解析结果。
// 引入动机：outline API 需要返回每个 heading 的层级、文本、行号和 section path。
type Heading struct {
	// Level 是 heading 级别（1~6，对应 #~######）。
	Level int `json:"level"`

	// Text 是 heading 文本内容（去除首尾空白和尾部 # 序列）。
	Text string `json:"text"`

	// StartLine 是 heading 在文档中的行号（从 1 开始）。
	StartLine int `json:"start_line"`

	// EndLine 是该 section 内容的最后一行号（含）。
	// 为下一个同级或更高级 heading 的行号 - 1，或文档末尾。
	EndLine int `json:"end_line"`

	// SectionPath 是从根到当前 heading 的 heading text 路径序列。
	// 例如 ["Architecture", "Components", "Database"]。
	SectionPath []string `json:"section_path"`
}

// ParseOutline 解析 Markdown 文本，返回 ATX heading 列表（含 section path 和行号）。
// 引入动机：outline endpoint 需要将 Markdown 解析为结构化 heading tree。
// 核心逻辑受控不崩溃——畸形输入只产生空或部分结果，不 panic。
//
// 参数：
//   - content：Markdown 文本
//
// 返回按出现顺序排列的 Heading 列表。
func ParseOutline(content string) []Heading {
	lines := strings.Split(content, "\n")
	headings := make([]Heading, 0)

	// 第一遍：识别所有 heading 及其行号
	type rawHeading struct {
		level     int
		text      string
		startLine int
	}
	var raws []rawHeading

	for i, line := range lines {
		level, text, ok := parseATXHeading(line)
		if !ok {
			continue
		}
		raws = append(raws, rawHeading{
			level:     level,
			text:      text,
			startLine: i + 1, // 行号从 1 开始
		})
	}

	if len(raws) == 0 {
		return headings
	}

	// 第二遍：计算每个 heading 的 end_line 和 section_path
	// section_path 使用栈维护：遇到同级或更高级 heading 时弹出栈顶
	type stackEntry struct {
		level int
		text  string
		index int // 在 raws 中的索引
	}
	var stack []stackEntry

	for i, rh := range raws {
		// 弹出栈中级别 >= 当前的条目
		for len(stack) > 0 && stack[len(stack)-1].level >= rh.level {
			stack = stack[:len(stack)-1]
		}

		// 构建 section_path
		sectionPath := make([]string, 0, len(stack)+1)
		for _, entry := range stack {
			sectionPath = append(sectionPath, entry.text)
		}
		sectionPath = append(sectionPath, rh.text)

		// 计算 end_line：下一个同级或更高级 heading 的行号 - 1，或文档末尾
		endLine := len(lines)
		for j := i + 1; j < len(raws); j++ {
			if raws[j].level <= rh.level {
				endLine = raws[j].startLine - 1
				break
			}
		}

		headings = append(headings, Heading{
			Level:       rh.level,
			Text:        rh.text,
			StartLine:   rh.startLine,
			EndLine:     endLine,
			SectionPath: sectionPath,
		})

		stack = append(stack, stackEntry{
			level: rh.level,
			text:  rh.text,
			index: i,
		})
	}

	return headings
}

// ReadSection 根据 section_path 从 Markdown 内容中提取目标 section 的文本。
// 引入动机：design/04-WEB-API.md §Document 要求 read section endpoint。
//
// 参数：
//   - content：Markdown 文本
//   - sectionPath：section 路径序列（如 ["Architecture", "Components"]）
//
// 返回匹配 section 的 Markdown 文本（含 heading 行本身）。
// 如果 sectionPath 为空或未找到匹配，返回空字符串和 false。
func ReadSection(content string, sectionPath []string) (string, bool) {
	if len(sectionPath) == 0 {
		return "", false
	}

	headings := ParseOutline(content)

	for _, h := range headings {
		if pathEqual(h.SectionPath, sectionPath) {
			lines := strings.Split(content, "\n")
			if h.StartLine > len(lines) {
				return "", false
			}
			endLine := h.EndLine
			if endLine > len(lines) {
				endLine = len(lines)
			}
			return strings.Join(lines[h.StartLine-1:endLine], "\n"), true
		}
	}

	return "", false
}

// ReadLines 从 Markdown 内容中提取指定行范围的内容。
// 引入动机：design/04-WEB-API.md §Document 要求 read lines endpoint。
//
// 参数：
//   - content：Markdown 文本
//   - startLine：起始行号（从 1 开始，含）
//   - endLine：结束行号（含）
//   - maxLines：最大允许行数限制
//
// 返回行范围内容和可能的错误：
//   - startLine < 1：错误
//   - endLine < startLine：错误
//   - endLine - startLine + 1 > maxLines：错误
//   - startLine 超出文档行数：空字符串
func ReadLines(content string, startLine, endLine, maxLines int) (string, error) {
	if startLine < 1 {
		return "", errLineRange("起始行号不能小于 1")
	}
	if endLine < startLine {
		return "", errLineRange("结束行号不能小于起始行号")
	}
	requested := endLine - startLine + 1
	if requested > maxLines {
		return "", errLineRange("请求行数 %d 超过最大限制 %d", requested, maxLines)
	}

	lines := strings.Split(content, "\n")
	if startLine > len(lines) {
		return "", nil
	}
	endLine = min(endLine, len(lines))
	return strings.Join(lines[startLine-1:endLine], "\n"), nil
}

// parseATXHeading 解析单行是否为 ATX heading。
// 引入动机：CommonMark 规范定义 ATX heading 由 1~6 个 # 开头。
// 返回 level（1~6）、text 和是否为 heading。
//
// 解析规则：
//   - 行首最多 3 个空格
//   - 1~6 个 # 字符
//   - # 后必须至少有一个空格或为行尾
//   - heading text 去除首尾空白
//   - 尾部 # 序列被去除（CommonMark §4.2）
func parseATXHeading(line string) (int, string, bool) {
	// 去除行首最多 3 个空格
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 {
		return 0, "", false
	}

	// 计算 # 的数量
	hashCount := 0
	for hashCount < len(trimmed) && trimmed[hashCount] == '#' {
		hashCount++
	}
	if hashCount == 0 || hashCount > 6 {
		return 0, "", false
	}

	// # 后必须为空格或行尾
	rest := trimmed[hashCount:]
	if len(rest) == 0 {
		// 空标题（如 `##`）
		return hashCount, "", true
	}
	if rest[0] != ' ' && rest[0] != '\t' {
		return 0, "", false
	}

	// 去除首尾空白
	text := strings.TrimSpace(rest)
	// 去除尾部 # 序列（CommonMark: trailing # sequence must be preceded by space）
	text = removeTrailingHashes(text)

	return hashCount, text, true
}

// removeTrailingHashes 去除 heading text 尾部的 # 序列。
// 引入动机：CommonMark §4.2 规定 ATX heading 的尾部 # 序列不是内容。
// 尾部 # 序列前必须有空格才被去除；如果 # 序列前无空格则保留。
func removeTrailingHashes(text string) string {
	if text == "" {
		return text
	}
	// 从尾部向前扫描 #
	end := len(text)
	for end > 0 && text[end-1] == '#' {
		end--
	}
	if end == len(text) {
		// 尾部没有 #
		return text
	}
	// # 序列前必须有空格或 tab
	if end > 0 && (text[end-1] == ' ' || text[end-1] == '\t') {
		return strings.TrimRight(text[:end], " \t")
	}
	// # 序列前无空格，保留原文
	return text
}

// pathEqual 比较两个 string slice 是否相等。
func pathEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// errLineRange 创建行范围错误。
type lineRangeError struct {
	msg string
}

func (e *lineRangeError) Error() string { return e.msg }

func errLineRange(format string, args ...interface{}) error {
	return &lineRangeError{msg: fmt.Sprintf(format, args...)}
}
