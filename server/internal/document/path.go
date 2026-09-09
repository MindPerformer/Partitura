// path.go 实现文档路径安全校验。
//
// 引入动机：design/03-DOCUMENTS.md §默认目录 禁止 absolute path、`..`、path traversal。
// design/04-WEB-API.md §Security 要求 strict path validation。
// 所有文档路径必须通过此校验才能写入或读取数据库，防止目录遍历攻击。
//
// 安全规则：
//   - 拒绝空路径
//   - 拒绝绝对路径（以 `/` 或 `\` 开头，或 Windows 盘符如 `C:`）
//   - 拒绝包含 `..` 路径段
//   - 拒绝包含反斜杠 `\`（防止 Windows 路径绕过）
//   - 拒绝重复分隔符 `//`
//   - 拒绝包含控制字符（ASCII 0x00-0x1F）
//   - 拒绝路径段为空（如 `a//b`）
//   - 路径必须以 `.md` 结尾
//   - 路径总长度不超过 512 字符
//   - 路径段不允许以 `.` 开头（防止隐藏文件或 `..` 变体）
package document

import (
	"fmt"
	"strings"
)

// MaxPathLength 是路径最大长度限制，与数据库 VARCHAR(512) 一致。
const MaxPathLength = 512

// ValidatePath 校验文档路径安全性。
// 引入动机：所有文档创建、移动、读取操作必须先调用此函数验证路径。
// 返回 nil 表示路径合法，返回 error 描述具体拒绝原因。
//
// 参数：
//   - path：待校验的文档路径
//
// 安全规则见文件注释。
func ValidatePath(path string) error {
	if path == "" {
		return fmt.Errorf("路径不能为空")
	}

	if len(path) > MaxPathLength {
		return fmt.Errorf("路径长度超过 %d 字符", MaxPathLength)
	}

	// 拒绝绝对路径：以 / 或 \ 开头
	if path[0] == '/' || path[0] == '\\' {
		return fmt.Errorf("路径不能是绝对路径")
	}

	// 拒绝 Windows 盘符前缀（如 C:、D:）
	if len(path) >= 2 && path[1] == ':' {
		return fmt.Errorf("路径不能包含盘符前缀")
	}

	// 拒绝反斜杠
	if strings.Contains(path, "\\") {
		return fmt.Errorf("路径不能包含反斜杠")
	}

	// 拒绝控制字符
	for _, c := range path {
		if c < 0x20 || c == 0x7F {
			return fmt.Errorf("路径不能包含控制字符")
		}
	}

	// 必须以 .md 结尾
	if !strings.HasSuffix(path, ".md") {
		return fmt.Errorf("路径必须以 .md 结尾")
	}

	// 分割路径段，检查每段
	segments := strings.Split(path, "/")
	for i, seg := range segments {
		if seg == "" {
			if i == 0 {
				return fmt.Errorf("路径不能以分隔符开头")
			}
			return fmt.Errorf("路径不能包含重复分隔符或空段")
		}

		// 拒绝 .. 路径遍历
		if seg == ".." {
			return fmt.Errorf("路径不能包含 ..")
		}

		// 拒绝以 . 开头的路径段（防止隐藏文件或 . 变体）
		// 例外：允许 .md 扩展名中的 .（这是文件名的一部分，不是路径段开头）
		if strings.HasPrefix(seg, ".") && seg != ".md" {
			return fmt.Errorf("路径段不能以 . 开头")
		}
	}

	return nil
}

// IsSpecialFilePath 判断路径是否为特殊文件路径。
// 引入动机：design/00-MASTER.md §特殊文件 定义 PROJECT.md 和 AGENTS.md 为特殊文件。
// 用于 workspace 初始化和文档创建时的特殊处理。
func IsSpecialFilePath(path string) bool {
	return path == SpecialFileProject || path == SpecialFileAgents
}
