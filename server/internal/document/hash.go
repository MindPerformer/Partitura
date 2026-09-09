// hash.go 实现内容哈希计算。
//
// 引入动机：design/04-WEB-API.md §Concurrency 要求 expected_hash 用于乐观并发控制。
// design/03-DOCUMENTS.md §Metadata 定义 content_hash 字段。
// 使用 SHA-256 计算content_markdown 的十六进制摘要，作为内容指纹。
//
// 设计原则：
//   - 哈希计算稳定、确定性（相同输入永远产生相同输出）
//   - 使用 SHA-256，足够安全且性能可接受
//   - 输出为 64 字符十六进制字符串
package document

import (
	"crypto/sha256"
	"encoding/hex"
)

// ComputeContentHash 计算 Markdown 内容的 SHA-256 十六进制摘要。
// 引入动机：文档创建和更新时需要计算 content_hash，用于乐观并发控制和 revision 存储。
//
// 参数：
//   - content：Markdown 文本
//
// 返回 64 字符十六进制字符串。
func ComputeContentHash(content string) string {
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:])
}
