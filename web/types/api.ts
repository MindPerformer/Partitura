// types/api.ts — 全部 API 类型定义
//
// 引入动机：design/04-WEB-API.md 定义了 REST API 契约。
// 此文件提供与 Go 后端 JSON 结构精确对应的 TypeScript 类型，
// 供 composables 和 pages 使用，确保类型安全。
//
// 设计原则：
// - 类型名与 server JSON tag 保持一致
// - 分页响应统一结构
// - 错误响应统一结构

// ============================================================
// Auth
// ============================================================

/** 登录响应 — POST /api/auth/login */
export interface LoginResponse {
  user: {
    id: string
    username: string
    system_role: string
  }
  csrf_token: string
  expires_at: string
}

/** GET /api/auth/me 响应 — 当前用户非敏感资料。 */
export interface CurrentUserResponse {
  id: string
  username: string
  email: string
  system_role: string
  workspace_create_perm: boolean
}

/** PUT /api/auth/me/email 请求。 */
export interface UpdateEmailRequest {
  current_password: string
  email: string
}

/** PUT /api/auth/me/password 请求。 */
export interface UpdatePasswordRequest {
  current_password: string
  new_password: string
}

/** Device authorize 响应 — POST /api/auth/device/authorize */
export interface DeviceAuthorizeResponse {
  access_token: string
  refresh_token: string
  token_type: string
  expires_in: number
  refresh_expires_in: number
}

/** Device auth start 响应 — POST /api/auth/device/start */
export interface DeviceAuthStartResponse {
  device_code: string
  user_code: string
  verification_url: string
  expires_in: number
  interval: number
}

/** Device auth poll 响应 — POST /api/auth/device/poll */
export interface DeviceAuthPollResponse {
  status: 'pending' | 'authorized' | 'denied' | 'expired' | 'completed'
  access_token?: string
  refresh_token?: string
  token_type?: string
  expires_in?: number
}

/** Device auth info 响应 — GET /api/auth/device/info（最小攻击面：不含 token/user/device_code） */
export interface DeviceAuthInfoResponse {
  status: 'pending' | 'authorized' | 'completed' | 'denied' | 'expired'
  device_name: string
  expires_in: number
}

/** GET /api/auth/device/sessions 响应中的设备会话元数据。 */
export interface DeviceSession {
  id: string
  device_name: string
  expires_at: string
  refresh_expires_at: string
  created_at: string
  revoked_at?: string
}

/** GET /api/auth/device/sessions 响应。 */
export interface ListDeviceSessionsResponse {
  sessions: DeviceSession[]
  total: number
  limit: number
  offset: number
}

// ============================================================
// Workspace
// ============================================================

export interface Workspace {
  id: string
  name: string
  display_name: string
  description: string
  status: WorkspaceStatus
  revision_retention_days: number
  revision_max_count: number
  max_document_size_bytes: number
  created_by: string
  /** 创建者用户名（后端 LEFT JOIN users COALESCE，无值时为空串） */
  created_by_username?: string
}

export interface ListWorkspacesResponse {
  workspaces: Workspace[]
  total: number
  limit: number
  offset: number
}

export interface CreateWorkspaceRequest {
  name: string
  display_name: string
  description: string
}

export interface UpdateWorkspaceRequest {
  display_name?: string
  description?: string
  revision_retention_days?: number
  revision_max_count?: number
  max_document_size_bytes?: number
}

/** GET /api/workspaces/{id}/me/membership 响应 */
export interface MyMembershipResponse {
  workspace_id: string
  role: WorkspaceRole
}

// ============================================================
// Workspace Members
// ============================================================

export interface Member {
  id: string
  user_id: string
  username: string
  email: string
  role: WorkspaceRole
}

export interface ListMembersResponse {
  members: Member[]
  total: number
  limit: number
  offset: number
}

export interface AddMemberRequest {
  username: string
  /** 保持 string：成员表单为普通字符串控件，后端词汇见 WORKSPACE_ROLES */
  role: string
}

/** GET /api/workspaces/{id}/members/candidates 响应。 */
export interface MemberCandidatesResponse {
  users: Member[]
}

export interface UpdateMemberRoleRequest {
  /** 保持 string：成员表单为普通字符串控件，后端词汇见 WORKSPACE_ROLES */
  role: string
}

// ============================================================
// Document
// ============================================================

export interface Document {
  id: string
  workspace_id: string
  path: string
  title: string
  /** 文档类型词汇；空串表示后端未设置（"未分类"），与 DocumentListItem 一致 */
  type?: DocumentType | ''
  status: DocumentStatus
  content_markdown: string
  content_hash: string
  revision_number: number
  is_special: boolean
  created_by: string
  updated_by: string
  /** 创建者用户名（后端 LEFT JOIN users COALESCE，无值时为空串） */
  created_by_username?: string
  /** 最后编辑者用户名（同上） */
  updated_by_username?: string
  created_at: string
  updated_at: string
}

export interface DocumentListItem {
  id: string
  path: string
  title: string
  /** 文档类型词汇；空串表示后端未设置（"未分类"） */
  type?: DocumentType | ''
  status: DocumentStatus
  content_hash: string
  revision_number: number
  is_special: boolean
  updated_by: string
  /** 最后编辑者用户名（后端 LEFT JOIN users COALESCE，无值时为空串） */
  updated_by_username?: string
  updated_at: string
}

export interface ListDocumentsResponse {
  documents: DocumentListItem[]
  total: number
  limit: number
  offset: number
}

export interface Heading {
  level: number
  text: string
  start_line: number
  end_line: number
  section_path: string[]
}

export interface OutlineResponse {
  path: string
  outline: Heading[]
}

export interface SectionResponse {
  path: string
  section_path: string[]
  content: string
  start_line: number
  end_line: number
}

export interface LinesResponse {
  path: string
  start_line: number
  end_line: number
  content: string
}

export interface CreateDocumentRequest {
  path: string
  title: string
  /** 保持 string：编辑器表单为普通字符串，后端词汇见 DOCUMENT_TYPES */
  type: string
  content_markdown: string
}

export interface ReplaceDocumentRequest {
  title: string
  type: string
  content_markdown: string
  expected_revision: number
  expected_hash: string
}

export interface PatchDocumentRequest {
  content_markdown: string
  candidate_hash: string
  expected_revision: number
  expected_hash: string
}

export interface MoveDocumentRequest {
  new_path: string
  expected_revision: number
  expected_hash: string
}

export interface ArchiveDocumentRequest {
  expected_revision: number
  expected_hash: string
}

// ============================================================
// Revision
// ============================================================

export interface Revision {
  id: string
  document_id: string
  revision_number: number
  path: string
  title: string
  content_markdown: string
  content_hash: string
  status: DocumentStatus
  created_by: string
  /** 创建者用户名（后端 LEFT JOIN users COALESCE，无值时为空串） */
  created_by_username?: string
  created_at: string
}

export interface ListRevisionsResponse {
  revisions: Revision[]
  total: number
  limit: number
  offset: number
}

// ============================================================
// Source
// ============================================================

export interface Source {
  id: string
  document_id: string
  source_type: SourceType
  value: string
  title?: string
  retrieved_at?: string
  content_hash?: string
  refresh_interval_days?: number
  source_document_id?: string
  created_by: string
  /** 创建者用户名（后端 LEFT JOIN users COALESCE，无值时为空串） */
  created_by_username?: string
  created_at: string
}

export interface ListSourcesResponse {
  sources: Source[]
  total: number
  limit:  number
  offset: number
}

export interface AddSourceRequest {
  /** 保持 string：来源表单为普通字符串控件，后端词汇见 SOURCE_TYPES */
  source_type: string
  value: string
  title: string
  retrieved_at: string
  content_hash: string
  refresh_interval_days: number
  source_document_id: string
}

// ============================================================
// Search
// ============================================================

export interface SearchResult {
  document_id: string
  path: string
  title: string
  section_path: string[]
  start_line: number
  end_line: number
  snippet: string
  score: number
  revision: number
  rank: number
}

export interface SearchResponse {
  results: SearchResult[]
  total: number
  limit: number
  offset: number
  degraded: boolean
  degradation_reason?: string
  search_id: string
  reranker_used: boolean
}

export interface SearchRequest {
  query: string
  mode: 'hybrid' | 'lexical' | 'semantic'
  limit: number
  offset: number
}

export interface SearchFeedbackRequest {
  feedback_type: 'verified' | 'explicit' | 'implicit'
  query: string
  document_id: string
  detail?: unknown
  search_id: string
}

// ============================================================
// Admin — Users
// ============================================================

export interface AdminUser {
  id: string
  username: string
  email: string
  system_role: SystemRole
  workspace_create_perm: boolean
}

export interface ListUsersResponse {
  users: AdminUser[]
  total: number
  limit: number
  offset: number
}

export interface UpdateUserRequest {
  /** 保持 string：管理端编辑表单使用普通字符串控件，后端返回时由 AdminUser.system_role 收窄 */
  system_role?: string
  workspace_create_perm?: boolean
}

/**
 * CreateUserRequest — system_admin 创建普通用户的请求体。
 * 引入动机：计划要求仅 system_admin 可创建普通用户，前端需要类型化请求体。
 * 默认 system_role=user、workspace_create_perm=false。
 */
export interface CreateUserRequest {
  username: string
  email: string
  password: string
  /** 保持 string：创建表单不暴露角色选择，后端默认 system_role=user */
  system_role?: string
  workspace_create_perm?: boolean
}

// ============================================================
// Admin — Audit
// ============================================================

export interface AuditEntry {
  id: number
  user_id: string
  workspace_id: string
  action: string
  resource_type: string
  resource_id: string
  detail: unknown
  request_id: string
  created_at: string
}

export interface ListAuditResponse {
  entries: AuditEntry[]
  total: number
  limit: number
  offset: number
}

// ============================================================
// Admin — Search Profiles
// ============================================================

export interface SearchProfile {
  id: string
  name: string
  version: number
  status: ProfileStatus
  embedding_provider: string
  embedding_model: string
  embedding_dimensions: number
  embedding_query_instruction: string
  embedding_document_instruction: string
  chunk_target_size: number
  chunk_overlap: number
  title_boost: number
  heading_boost: number
  path_boost: number
  tags_boost: number
  body_boost: number
  analyzer: string
  lexical_top_k: number
  vector_top_k: number
  rrf_k: number
  reranker_provider: string
  reranker_model: string
  reranker_candidate_count: number
  reranker_final_count: number
  max_chunks_per_document: number
  merge_adjacent_chunks: boolean
  max_p95_latency_ms: number
  max_reranker_cost_per_query: number
  es_index_name: string
  created_by: string
  created_at: string
  activated_at?: string
}

export interface ListProfilesResponse {
  profiles: SearchProfile[]
  total: number
  limit: number
  offset: number
}

export interface CreateProfileRequest {
  name: string
  embedding_provider: string
  embedding_model: string
  embedding_dimensions: number
  embedding_query_instruction: string
  embedding_document_instruction: string
  chunk_target_size: number
  chunk_overlap: number
  title_boost: number
  heading_boost: number
  path_boost: number
  tags_boost: number
  body_boost: number
  analyzer: string
  lexical_top_k: number
  vector_top_k: number
  rrf_k: number
  reranker_provider: string
  reranker_model: string
  reranker_candidate_count: number
  reranker_final_count: number
  max_chunks_per_document: number
  merge_adjacent_chunks: boolean
  max_p95_latency_ms: number
  max_reranker_cost_per_query: number
}

/**
 * 新建配置版本（POST /admin/search-profiles/{id}/versions）的请求体。
 *
 * 引入动机：搜索配置不支持原地修改，"修改"的语义是基于现有配置派生新版本。
 * 服务端会继承源 profile 中本请求体未提供的字段，因此这里的每个可覆盖字段都是可选的：
 * 前端只发送界面上真正暴露给用户的字段，其余字段留在服务端继承。
 * 该类型不改变 CreateProfileRequest / SearchProfile 的既有字段语义。
 */
export type CreateProfileVersionRequest = Partial<CreateProfileRequest>

// ============================================================
// Admin — Evaluation
// ============================================================

export interface EvaluationDataset {
  id: string
  name: string
  description: string
  /** 数据集状态；归档后为只读终态，不可再评测或添加条目 */
  status: 'active' | 'archived'
  created_by: string
  created_at: string
}

export interface EvaluationItem {
  id: string
  dataset_id: string
  query: string
  expected_documents: ExpectedDocument[]
  relevance_grade: number
  query_class: string
  created_at: string
}

export interface ExpectedDocument {
  document_id: string
  path?: string
}

export interface EvaluationResult {
  id: string
  dataset_id: string
  profile_id: string
  job_id: string
  metrics: unknown
  created_at: string
}

export interface ListDatasetsResponse {
  datasets: EvaluationDataset[]
  total: number
  limit: number
  offset: number
}

/** GET /api/admin/evaluation/datasets/{id}/items 响应。 */
export interface ListItemsResponse {
  items: EvaluationItem[]
  total: number
  limit: number
  offset: number
}

export interface ListEvaluationResultsResponse {
  results: EvaluationResult[]
  total: number
  limit: number
  offset: number
}

export interface CreateDatasetRequest {
  name: string
  description: string
}

export interface AddItemRequest {
  query: string
  expected_documents: ExpectedDocument[]
  relevance_grade: number
  query_class: string
}

export interface RunEvaluationRequest {
  dataset_id: string
  profile_id: string
}

// ============================================================
// Admin — Jobs
// ============================================================

export interface Job {
  id: string
  type: JobType
  status: JobStatus
  payload: unknown
  attempts: number
  max_attempts: number
  error: string
  created_at: string
  updated_at: string
}

export interface ListJobsResponse {
  jobs: Job[]
  total: number
  limit: number
  offset: number
}

// ============================================================
// Workspace Stats
// ============================================================

/** Workspace 工作台近期 revision 摘要 */
export interface RecentRevision {
  path: string
  title: string
  revision_number: number
  created_at: string
}

/** Workspace 工作台聚合统计 */
export interface WorkspaceStats {
  total_documents: number
  active_documents: number
  draft_documents: number
  archived_documents: number
  member_count: number
  recent_revisions: RecentRevision[]
}

/** GET /api/workspaces/{id}/stats 响应 */
export interface WorkspaceStatsResponse {
  stats: WorkspaceStats
}

// ============================================================
// Admin — Settings
// ============================================================

/** 系统业务配置项 */
export interface SystemSetting {
  key: string
  value: string
  type: 'string' | 'int' | 'bool' | 'float' | 'duration'
  description: string
  restart_required: boolean
}

/** GET /api/admin/config/settings 响应 */
export interface ListSettingsResponse {
  settings: SystemSetting[]
}

/** PUT /api/admin/config/settings/{key} 请求 */
export interface UpdateSettingRequest {
  value: string
}

/** PUT /api/admin/config/settings/{key} 响应 */
export interface UpdateSettingResponse {
  key: string
  value: string
  restart_required: boolean
  message: string
}

/** GET /api/admin/config/runtime 响应 */
export interface RuntimeStatusResponse {
  status: string
  runtime: Record<string, unknown>
}

// ============================================================
// Bootstrap — 首次管理员创建
// ============================================================

/** GET /api/bootstrap 响应 — 查询 Bootstrap 是否可用 */
export interface BootstrapStatusResponse {
  bootstrap_available: boolean
}

/** POST /api/bootstrap 请求 — 创建首个 system_admin */
export interface BootstrapRequest {
  username: string
  email: string
  password: string
}

/** POST /api/bootstrap 响应 — 创建成功 */
export interface BootstrapResponse {
  status: string
  user_id: string
}

// ============================================================
// Admin — Provider 配置
// ============================================================

/** Embedding Provider 非敏感 JSON 配置 */
export interface EmbeddingProviderConfig {
  base_url: string
  model: string
  dimensions: number
  timeout_seconds: number
  batch_size: number
  query_instruction: string
  document_instruction: string
}

/** Reranker Provider 非敏感 JSON 配置 */
export interface RerankerProviderConfig {
  base_url: string
  model: string
  timeout_seconds: number
  max_candidates: number
}

/** Provider 配置 DTO — GET 响应，绝不包含 API Key 明文或密文 */
export interface ProviderConfigDTO {
  configured: boolean
  api_key_set: boolean
  config?: EmbeddingProviderConfig | RerankerProviderConfig | Record<string, unknown>
  updated_at?: string
}

/** GET /api/admin/providers 响应 — 所有 Provider 状态 */
export interface ListProvidersResponse {
  providers: Record<string, ProviderConfigDTO>
}

/** PUT /api/admin/providers/{type} 请求 — 严格 JSON，api_key 仅用于写入 */
export interface PutProviderRequest {
  api_key: string
  config: EmbeddingProviderConfig | RerankerProviderConfig
}

/** PUT /api/admin/providers/{type} 响应 */
export interface PutProviderResponse {
  status: string
  message: string
}

/** POST /api/admin/providers/{type}/test 请求 — 临时测试，不持久化 */
export interface TestProviderRequest {
  api_key: string
  config: EmbeddingProviderConfig | RerankerProviderConfig
}

/** POST /api/admin/providers/{type}/test 响应 */
export interface TestProviderResponse {
  status: 'ok' | 'failed' | 'unavailable'
  message?: string
}

// ============================================================
// 通用错误响应
// ============================================================

export interface ApiError {
  error: string
  status: number
}

// ============================================================
// 分页参数
// ============================================================

export interface PaginationParams {
  limit?: number
  offset?: number
  [key: string]: unknown
}

/**
 * GET /api/admin/audit 服务端筛选参数。
 * 均为可选；from/to 为 RFC3339 时间字符串，非法值后端返回 400。
 */
export interface AdminAuditFilterParams extends PaginationParams {
  user_id?: string
  action?: string
  resource_type?: string
  workspace_id?: string
  /** RFC3339 时间下界 */
  from?: string
  /** RFC3339 时间上界 */
  to?: string
}

/**
 * GET /api/admin/users 服务端筛选参数。
 * system_role 后端仅接受 user / system_admin，非法值返回 400。
 */
export interface AdminUserFilterParams extends PaginationParams {
  system_role?: string
  /** username / email 模糊匹配 */
  q?: string
}

/**
 * GET /api/admin/workspaces 服务端筛选参数。
 * status 后端仅接受 active / archived，非法值返回 400。
 */
export interface AdminWorkspaceFilterParams extends PaginationParams {
  status?: string
  /** name / display_name 模糊匹配 */
  q?: string
}

// ============================================================
// RBAC 角色常量
// ============================================================

export const SYSTEM_ROLES = {
  ADMIN: 'system_admin',
  USER: 'user'
} as const

export const WORKSPACE_ROLES = {
  OWNER: 'owner',
  ADMIN: 'admin',
  EDITOR: 'editor',
  VIEWER: 'viewer'
} as const

/** workspace 角色权限层级：owner > admin > editor > viewer */
export const ROLE_RANK: Record<string, number> = {
  owner: 4,
  admin: 3,
  editor: 2,
  viewer: 1
}

/** 判断当前角色是否满足最低权限要求 */
export function hasMinRole(currentRole: string, requiredRole: string): boolean {
  return (ROLE_RANK[currentRole] ?? 0) >= (ROLE_RANK[requiredRole] ?? 0)
}

// ============================================================
// 文档类型/状态常量
// ============================================================

export const DOCUMENT_TYPES = [
  'architecture', 'codebase', 'development', 'decision',
  'issue', 'roadmap', 'research', 'reference',
  'operation', 'standard', 'guide', 'other'
] as const

export const DOCUMENT_STATUSES = ['active', 'draft', 'archived'] as const

export const SOURCE_TYPES = [
  'web', 'code', 'file', 'issue',
  'commit', 'conversation', 'manual', 'other'
] as const

export const SEARCH_MODES = ['hybrid', 'lexical', 'semantic'] as const

// ============================================================
// 枚举联合类型（与 useEnumLabels 的键集一一对应）
// ============================================================

/** 系统角色（对应 admin.roleUser / admin.roleSystemAdmin） */
export type SystemRole = (typeof SYSTEM_ROLES)[keyof typeof SYSTEM_ROLES]

/** workspace 角色（对应 workspace.roleViewer/Editor/Admin/Owner） */
export type WorkspaceRole = (typeof WORKSPACE_ROLES)[keyof typeof WORKSPACE_ROLES]

/** workspace 状态（对应 workspace.statusActive / statusArchived） */
export type WorkspaceStatus = 'active' | 'archived'

/** 文档类型（对应 document.typeXxx） */
export type DocumentType = (typeof DOCUMENT_TYPES)[number]

/** 文档状态（对应 document.statusActive/statusArchived；draft 为后端词汇，列表/历史可出现） */
export type DocumentStatus = (typeof DOCUMENT_STATUSES)[number]

/** 来源类型（对应 document.sourceTypeXxx） */
export type SourceType = (typeof SOURCE_TYPES)[number]

/** 搜索模式（对应 search.modeXxx） */
export type SearchMode = (typeof SEARCH_MODES)[number]

/** 搜索配置状态（对应 admin.statusActive/Draft/Inactive/Archived） */
export type ProfileStatus = 'active' | 'draft' | 'inactive' | 'archived'

/** 后台任务状态（对应 admin.statusPending/Running/Completed/Failed/Dead） */
export type JobStatus = 'pending' | 'running' | 'completed' | 'failed' | 'dead'

/** 后台任务类型（对应 admin.jobTypeXxx） */
export type JobType =
  | 'index_document'
  | 'rebuild_index'
  | 'repair_index'
  | 'evaluate_profile'
  | 'optimize_profile'
  | 'cleanup_old_indexes'
  | 'cleanup_revisions'
