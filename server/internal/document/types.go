// Package document 实现 Markdown 文档的全生命周期管理：创建、读取、修改、
// 归档、版本历史、来源管理和乐观并发控制。
//
// 引入动机：design/03-DOCUMENTS.md 定义了 Markdown 文档作为项目知识的核心载体，
// design/04-WEB-API.md §Document 规定了 REST API 和 RBAC 权限矩阵，
// design/00-MASTER.md §特殊文件 要求 PROJECT.md / AGENTS.md 在 workspace 创建时自动初始化。
//
// 设计原则：
//   - HTTP handler 仅做解析/响应，领域规则和 SQL 访问分离
//   - 所有 workspace-scoped 查询和变更在 SQL 级按 workspace_id 限定
//   - 路径严格安全校验，拒绝 path traversal
//   - 每次变更产生完整 Markdown snapshot revision
//   - 乐观并发：expected_revision + expected_hash 不匹配返回 409
//   - 不出现 XxxService / XxxManager / XxxController
package document

// DocumentStatus 是文档状态常量。
// 引入动机：design/03-DOCUMENTS.md §Metadata 定义 status: active / draft / archived。
const (
	StatusActive   = "active"
	StatusDraft    = "draft"
	StatusArchived = "archived"
)

// DocumentType 是文档类型常量。
// 引入动机：design/03-DOCUMENTS.md §Metadata 定义 type 可选值。
const (
	TypeArchitecture = "architecture"
	TypeCodebase     = "codebase"
	TypeDevelopment  = "development"
	TypeDecision     = "decision"
	TypeIssue        = "issue"
	TypeRoadmap      = "roadmap"
	TypeResearch     = "research"
	TypeReference    = "reference"
	TypeOperation    = "operation"
	TypeStandard     = "standard"
	TypeGuide        = "guide"
	TypeOther        = "other"
)

// SourceType 是来源类型常量。
// 引入动机：design/03-DOCUMENTS.md §Source 定义 source type。
const (
	SourceTypeWeb          = "web"
	SourceTypeCode         = "code"
	SourceTypeFile         = "file"
	SourceTypeIssue        = "issue"
	SourceTypeCommit       = "commit"
	SourceTypeConversation = "conversation"
	SourceTypeManual       = "manual"
	SourceTypeOther        = "other"
)

// MaxLineReadCount 是单次行读取的最大行数限制。
// 引入动机：design/03-DOCUMENTS.md §大文档 要求"限制最大行数"但未指定具体值。
// 选择 500 行作为合理上限，足够阅读中等长度的 section，同时防止一次性读取超大内容。
const MaxLineReadCount = 500

// SpecialFileProject 和 SpecialFileAgents 是 workspace 特殊文件路径常量。
// 引入动机：design/00-MASTER.md §特殊文件 要求每个 workspace 固定两个特殊文件。
// 这两个文件有 revision、可正常读写，但 Phase 3 搜索排除。
const (
	SpecialFileProject = "PROJECT.md"
	SpecialFileAgents  = "AGENTS.md"
)

// isValidDocumentType 判断给定字符串是否为合法的文档类型。
// 引入动机：创建/更新文档时需要验证 type 字段。
func isValidDocumentType(t string) bool {
	switch t {
	case TypeArchitecture, TypeCodebase, TypeDevelopment, TypeDecision,
		TypeIssue, TypeRoadmap, TypeResearch, TypeReference,
		TypeOperation, TypeStandard, TypeGuide, TypeOther:
		return true
	default:
		return false
	}
}

// isValidSourceType 判断给定字符串是否为合法的来源类型。
// 引入动机：添加 source 时需要验证 source_type 字段。
func isValidSourceType(t string) bool {
	switch t {
	case SourceTypeWeb, SourceTypeCode, SourceTypeFile, SourceTypeIssue,
		SourceTypeCommit, SourceTypeConversation, SourceTypeManual, SourceTypeOther:
		return true
	default:
		return false
	}
}

// isValidDocumentStatus 判断给定字符串是否为合法的文档状态。
func isValidDocumentStatus(s string) bool {
	return s == StatusActive || s == StatusDraft || s == StatusArchived
}

// Document 是从数据库读取的文档记录。
// 引入动机：文档 CRUD、revision、source 等 API 需要结构体映射数据库行。
type Document struct {
	ID              string
	WorkspaceID     string
	Path            string
	Title           string
	Type            string
	Status          string
	ContentMarkdown string
	ContentHash     string
	RevisionNumber  int
	IsSpecial       bool
	CreatedBy       string
	UpdatedBy       string
	CreatedAt       string
	UpdatedAt       string
}

// Revision 是从数据库读取的版本快照记录。
// 引入动机：history/revision API 需要返回版本列表和指定版本内容。
type Revision struct {
	ID              string
	DocumentID      string
	WorkspaceID     string
	RevisionNumber  int
	Path            string
	Title           string
	ContentMarkdown string
	ContentHash     string
	Status          string
	CreatedBy       string
	CreatedAt       string
}

// Source 是从数据库读取的文档来源记录。
// 引入动机：source list API 需要返回来源元数据列表。
type Source struct {
	ID                 string
	DocumentID         string
	WorkspaceID        string
	SourceType         string
	Value              string
	Title              string
	RetrievedAt        string
	ContentHash        string
	RefreshIntervalDays int
	SourceDocumentID   string
	CreatedBy          string
	CreatedAt          string
}

// ListDocumentsResult 是文档列表查询结果（分页）。
// 引入动机：list API 需要返回分页数据和总数。
type ListDocumentsResult struct {
	Documents []Document
	Total     int
}

// ListRevisionsResult 是版本列表查询结果（分页）。
// 引入动机：history API 需要返回分页数据和总数。
type ListRevisionsResult struct {
	Revisions []Revision
	Total     int
}

// ListSourcesResult 是来源列表查询结果（分页）。
// 引入动机：source list API 需要返回分页数据和总数。
type ListSourcesResult struct {
	Sources []Source
	Total   int
}
