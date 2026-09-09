// Package searchtypes 定义搜索系统共用的核心类型。
//
// 引入动机：design/01-SEARCH.md 定义了搜索管线的完整流程，
// 各子模块（chunking、embedding、reranker、pipeline、profile、evaluation）
// 需要共享统一的类型定义，避免循环依赖。
//
// 设计原则：
//   - 类型定义集中在此包，供所有搜索子模块引用
//   - 不包含业务逻辑，仅定义数据结构
//   - 所有公开类型和字段必须有中文动机/作用注释
package searchtypes

// Chunk 表示一个 Markdown 文档的分片结果。
// 引入动机：design/01-SEARCH.md §Chunking 要求 Markdown-aware chunking，
// Document → Section → Child Chunk，每个 chunk 保存检索和完整性字段。
type Chunk struct {
	// DocumentID 是文档的 UUID。
	DocumentID string
	// WorkspaceID 是文档所属 workspace 的 UUID。
	WorkspaceID string
	// Path 是文档路径（如 "architecture/overview.md"）。
	Path string
	// Title 是文档标题。
	Title string
	// SectionPath 是从根到当前 section 的 heading 路径序列。
	// 例如 ["Architecture", "Components", "Database"]。
	SectionPath []string
	// Heading 是 section 的 heading 文本（可能为空表示文档头部无 heading 的内容）。
	Heading string
	// Content 是 chunk 的 Markdown 文本内容。
	Content string
	// StartLine 是 chunk 在文档中的起始行号（从 1 开始，含）。
	StartLine int
	// EndLine 是 chunk 在文档中的结束行号（含）。
	EndLine int
	// ChunkIndex 是同一文档内 chunk 的序号（从 0 开始）。
	ChunkIndex int
	// ContentHash 是 chunk 内容的 SHA-256 十六进制摘要，用于完整性校验。
	ContentHash string
	// Revision 是文档当前的 revision 号。
	Revision int
	// Status 是文档当前状态（active/draft/archived）。
	Status string
	// IsSpecial 标记是否为特殊文件（PROJECT.md/AGENTS.md），特殊文件不入索引。
	IsSpecial bool
}

// SearchResult 表示一条搜索结果。
// 引入动机：design/01-SEARCH.md §Search Result 要求返回 document_id, path, title,
// section_path, start_line, end_line, snippet, score, revision, source freshness。
type SearchResult struct {
	// DocumentID 是文档的 UUID。
	DocumentID string `json:"document_id"`
	// Path 是文档路径。
	Path string `json:"path"`
	// Title 是文档标题。
	Title string `json:"title"`
	// SectionPath 是 chunk 所在 section 的路径序列。
	SectionPath []string `json:"section_path"`
	// StartLine 是 chunk 起始行号。
	StartLine int `json:"start_line"`
	// EndLine 是 chunk 结束行号。
	EndLine int `json:"end_line"`
	// Snippet 是搜索结果摘要（不返回整篇长文）。
	Snippet string `json:"snippet"`
	// Score 是最终排序分数。
	Score float64 `json:"score"`
	// Revision 是文档当前的 revision 号。
	Revision int `json:"revision"`
	// Rank 是结果在最终列表中的排名（从 1 开始）。
	Rank int `json:"rank"`
}

// SearchResponse 是搜索 API 的响应结构。
// 引入动机：design/01-SEARCH.md §Search Result 和 design/04-WEB-API.md §Search
// 要求搜索 API 返回结果列表和分页信息。
type SearchResponse struct {
	// Results 是搜索结果列表。
	Results []SearchResult `json:"results"`
	// Total 是匹配的总结果数（可能大于返回数量）。
	Total int `json:"total"`
	// Limit 是本次返回的最大结果数。
	Limit int `json:"limit"`
	// Offset 是分页偏移量。
	Offset int `json:"offset"`
	// Degraded 标记搜索是否处于降级状态。
	// 引入动机：design/05-OPERATIONS.md §Fault Degradation 要求 ES 不可用时返回 degraded。
	Degraded bool `json:"degraded"`
	// DegradationReason 描述降级原因（如 "elasticsearch_unavailable", "embedding_unavailable"）。
	DegradationReason string `json:"degradation_reason,omitempty"`
	// SearchID 是本次搜索的唯一标识，用于关联 metrics 和 feedback。
	SearchID string `json:"search_id"`
	// RerankerUsed 标记是否使用了 reranker。
	RerankerUsed bool `json:"reranker_used"`
}

// CandidateResult 表示 RRF 融合后、reranker 之前的候选结果。
// 引入动机：design/01-SEARCH.md 搜索管线要求 BM25+vector → RRF → top candidates → reranker。
type CandidateResult struct {
	// Chunk 是对应的 chunk 信息。
	Chunk Chunk
	// BM25Score 是 BM25 检索分数（0 表示未参与 BM25 检索）。
	BM25Score float64
	// VectorScore 是向量检索分数（0 表示未参与向量检索）。
	VectorScore float64
	// RRFScore 是 RRF 融合后的分数。
	RRFScore float64
	// BM25Rank 是在 BM25 结果中的排名（从 1 开始，0 表示未出现在 BM25 结果中）。
	BM25Rank int
	// VectorRank 是在向量结果中的排名（从 1 开始，0 表示未出现在向量结果中）。
	VectorRank int
}

// SearchProfileConfig 是 Search Profile 的完整配置，用于搜索管线运行时。
// 引入动机：design/01-SEARCH.md §Search Profile 定义了全部搜索参数，
// 搜索管线需要从 profile 读取 BM25 boost、topK、RRF 参数、reranker 配置等。
type SearchProfileConfig struct {
	// ID 是 profile 的 UUID。
	ID string
	// Name 是 profile 名称。
	Name string
	// Version 是 profile 版本号。
	Version int
	// Status 是 profile 状态。
	Status string

	// --- Embedding 配置 ---
	EmbeddingProvider         string
	EmbeddingModel            string
	EmbeddingDimensions       int
	EmbeddingQueryInstruction string
	EmbeddingDocInstruction   string

	// --- Chunk 配置 ---
	ChunkAlgorithmVersion string
	ChunkTargetSize       int
	ChunkOverlap          int

	// --- Lexical 配置 ---
	TitleBoost   float32
	HeadingBoost float32
	PathBoost    float32
	TagsBoost    float32
	BodyBoost    float32
	Analyzer     string

	// --- Retrieval 配置 ---
	LexicalTopK int
	VectorTopK  int
	RRFK        int

	// --- Reranker 配置 ---
	RerankerProvider      string
	RerankerModel         string
	RerankerCandidateCount int
	RerankerFinalCount     int

	// --- Diversification 配置 ---
	MaxChunksPerDocument int
	MergeAdjacentChunks  bool

	// --- ES 索引 ---
	ESIndexName string

	// --- 预算与阈值 ---
	MaxP95LatencyMs      int
	MaxRerankerCostPerQuery float32
}

// EmbeddingConfig 是 Embedding Provider 的运行时配置。
// 引入动机：design/01-SEARCH.md §Embedding Provider 要求配置 base_url, api_key, model,
// dimensions, timeout, batch_size, query_instruction, document_instruction。
type EmbeddingConfig struct {
	BaseURL              string
	APIKey               string
	Model                string
	Dimensions           int
	TimeoutSeconds       int
	BatchSize            int
	QueryInstruction     string
	DocumentInstruction  string
}

// RerankerConfig 是 Reranker Provider 的运行时配置。
// 引入动机：design/01-SEARCH.md §Reranker Provider 要求配置 base_url, api_key, model,
// timeout, max_candidates。
type RerankerConfig struct {
	BaseURL       string
	APIKey        string
	Model         string
	TimeoutSeconds int
	MaxCandidates  int
}

// RerankerCandidate 是传给 reranker 的候选项。
// 引入动机：reranker 需要查询文本和候选文本来进行重排序。
type RerankerCandidate struct {
	// Text 是候选文本内容。
	Text string
	// Index 是在候选列表中的原始位置。
	Index int
}

// RerankerResult 是 reranker 返回的单条排序结果。
type RerankerResult struct {
	// Index 是在候选列表中的原始位置。
	Index int
	// Score 是 reranker 给出的相关性分数。
	Score float64
}

// FeedbackType 是搜索反馈类型常量。
// 引入动机：design/01-SEARCH.md §Feedback 定义三种反馈类型。
const (
	FeedbackVerified  = "verified"
	FeedbackExplicit  = "explicit"
	FeedbackImplicit  = "implicit"
)

// JobType 是后台任务类型常量。
// 引入动机：design/05-OPERATIONS.md §Background Jobs 定义任务类型。
//           §Revision Cleanup 要求 cleanup_revisions 任务。
const (
	JobIndexDocument      = "index_document"
	JobRebuildIndex       = "rebuild_index"
	JobRepairIndex        = "repair_index"
	JobEvaluateProfile    = "evaluate_profile"
	JobOptimizeProfile    = "optimize_profile"
	JobCleanupOldIndexes  = "cleanup_old_indexes"
	JobCleanupRevisions   = "cleanup_revisions"
)

// JobStatus 是后台任务状态常量。
const (
	JobPending   = "pending"
	JobRunning   = "running"
	JobCompleted = "completed"
	JobFailed    = "failed"
	JobDead      = "dead"
)

// TuningLevel 是自动调优级别常量。
// 引入动机：design/01-SEARCH.md §Auto Tuning 定义三级调优。
const (
	TuningLevel1 = 1 // 自动激活：field boost, top_k, RRF, candidate count, diversity
	TuningLevel2 = 2 // 自动测试+管理员确认：chunk size, overlap, dimensions, instructions, reranker config
	TuningLevel3 = 3 // 人工触发：embedding model, provider, analyzer
)

// ProfileStatus 是 Search Profile 状态常量。
const (
	ProfileDraft    = "draft"
	ProfileActive   = "active"
	ProfileInactive = "inactive"
	ProfileArchived = "archived"
)

// CandidateStatus 是调优候选状态常量。
const (
	CandidatePending  = "pending"
	CandidateTesting  = "testing"
	CandidatePassed   = "passed"
	CandidateFailed   = "failed"
	CandidateActivated = "activated"
	CandidateRejected  = "rejected"
)
