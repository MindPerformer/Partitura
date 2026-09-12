// composables/useApi.ts — 类型化 API client
//
// 引入动机：design/04-WEB-API.md §Security 要求所有请求使用 secure cookie (credentials: include)
// 和 CSRF token。CSRF token 从 cookie 读取，在状态变更请求中通过 X-CSRF-Token header 发送。
//
// 设计原则：
// - 统一 fetch 封装，处理 JSON、错误、401/403/404/409
// - 不将 password/session/access/refresh token 存入 localStorage/sessionStorage/URL/日志
// - 请求错误不静默吞掉，通过受控 console.error 和返回错误信息处理
// - 分页支持
// - CSRF 自动注入

import { clearAuthState } from "~/composables/useAuth";
import type {
  ApiError,
  PaginationParams,
  LoginResponse,
  CurrentUserResponse,
  UpdateEmailRequest,
  UpdatePasswordRequest,
  ListWorkspacesResponse,
  Workspace,
  CreateWorkspaceRequest,
  UpdateWorkspaceRequest,
  ListMembersResponse,
  Member,
  MyMembershipResponse,
  AddMemberRequest,
  MemberCandidatesResponse,
  UpdateMemberRoleRequest,
  Document,
  DocumentListItem,
  ListDocumentsResponse,
  OutlineResponse,
  SectionResponse,
  LinesResponse,
  CreateDocumentRequest,
  ReplaceDocumentRequest,
  PatchDocumentRequest,
  MoveDocumentRequest,
  ArchiveDocumentRequest,
  Revision,
  ListRevisionsResponse,
  Source,
  ListSourcesResponse,
  AddSourceRequest,
  SearchRequest,
  SearchResponse,
  SearchFeedbackRequest,
  AdminUser,
  ListUsersResponse,
  UpdateUserRequest,
  CreateUserRequest,
  ListAuditResponse,
  SearchProfile,
  ListProfilesResponse,
  CreateProfileRequest,
  CreateProfileVersionRequest,
  EvaluationDataset,
  ListDatasetsResponse,
  CreateDatasetRequest,
  AddItemRequest,
  ListItemsResponse,
  RunEvaluationRequest,
  ListEvaluationResultsResponse,
  AdminAuditFilterParams,
  AdminUserFilterParams,
  AdminWorkspaceFilterParams,
  Job,
  ListJobsResponse,
  DeviceAuthStartResponse,
  DeviceAuthPollResponse,
  DeviceAuthInfoResponse,
  ListDeviceSessionsResponse,
  DeviceAuthorizeResponse,
  WorkspaceStatsResponse,
  SystemSetting,
  ListSettingsResponse,
  UpdateSettingRequest,
  UpdateSettingResponse,
  RuntimeStatusResponse,
  BootstrapStatusResponse,
  BootstrapRequest,
  BootstrapResponse,
  ListProvidersResponse,
  ProviderConfigDTO,
  PutProviderRequest,
  PutProviderResponse,
  TestProviderRequest,
  TestProviderResponse,
  TuningScope,
  TuningConfig,
  UpdateTuningRequest,
  ValidateTuningRequest,
  PublishTuningRequest,
  RollbackTuningRequest,
  TuningValidationResponse,
  TuningRecommendationsResponse,
} from "~/types/api";

/** CSRF cookie 名称 — 与 server config.CSRFCookieName 一致 */
const CSRF_COOKIE_NAME = "csrf";
/** CSRF header 名称 — 与 server config.CSRFHeaderName 一致 */
const CSRF_HEADER_NAME = "X-CSRF-Token";

/** 需要 CSRF 保护的状态变更方法 */
const MUTATION_METHODS = new Set(["POST", "PUT", "PATCH", "DELETE"]);

/**
 * 从 cookie 读取 CSRF token。
 *
 * 引入动机：server 在 login 时设置非 HttpOnly 的 csrf cookie，
 * 前端 JS 读取后以 header 回传，实现 double-submit CSRF 防护。
 *
 * 本项目 ssr:false 纯 CSR，只保留客户端 document.cookie 读取。
 * cookie 被禁用/环境异常时安全降级返回空串并 console.warn（不静默吞掉）。
 */
function getCsrfToken(): string {
  if (typeof document === "undefined") {
    console.warn("[useApi] document 不可用，无法读取 CSRF cookie");
    return "";
  }
  if (typeof navigator !== "undefined" && navigator.cookieEnabled === false) {
    console.warn(
      "[useApi] 浏览器 cookie 已禁用，CSRF token 不可用，状态变更请求可能被服务端拒绝",
    );
    return "";
  }
  const match = document.cookie.match(
    new RegExp(`(?:^|;\\s*)${CSRF_COOKIE_NAME}=([^;]+)`),
  );
  return match ? decodeURIComponent(match[1]!) : "";
}

/**
 * 模块级 401 处理：清理认证状态并跳转登录页。
 *
 * 设计动机：此函数运行在 apiFetch 的异步 catch 回调中，Nuxt 上下文已丢失，
 * 不能调用 useAuth()/useCookie()/useRouter()。因此：
 * - 认证清理委托给 useAuth.ts 的模块级 clearAuthState()（直接操作模块级 ref + document.cookie）
 * - 跳转直接使用 window.location.assign（CSR 全量刷新，顺带清空内存态）
 * - /auth/me 探测本身就是会话检查，由路由守卫决定跳转，这里跳过避免重复导航；
 *   已在 /login 或 /bootstrap 时也不再跳转，避免刷新循环。
 */
function handleUnauthorized(failedPath: string): void {
  clearAuthState();

  if (typeof window === "undefined") return;
  const pathname = window.location.pathname;
  if (
    failedPath === "/auth/me" ||
    pathname === "/login" ||
    pathname === "/bootstrap"
  ) {
    return;
  }
  const redirect = encodeURIComponent(pathname + window.location.search);
  window.location.assign(`/login?redirect=${redirect}`);
}

/**
 * 构建完整 API URL。
 * 开发环境通过 nitro devProxy 代理，生产环境通过 runtimeConfig.public.apiBase。
 */
function buildUrl(path: string, params?: Record<string, unknown>): string {
  const config = useRuntimeConfig();
  const base = config.public.apiBase as string;
  let url = `${base}${path}`;

  if (params) {
    const searchParams = new URLSearchParams();
    for (const [key, value] of Object.entries(params)) {
      if (value !== undefined && value !== null && value !== "") {
        searchParams.set(key, String(value));
      }
    }
    const qs = searchParams.toString();
    if (qs) {
      url += `?${qs}`;
    }
  }
  return url;
}

/**
 * 核心 fetch 函数 — 封装 $fetch，处理 CSRF、credentials、错误。
 *
 * 行为：
 * - 所有请求带 credentials: 'include'（发送 cookie）
 * - 状态变更方法自动注入 X-CSRF-Token header
 * - 响应非 2xx 时抛出 ApiError（含 status 和 message）
 * - 401 时触发认证状态清理
 *
 * @returns Promise<T> — 成功时返回解析后的 JSON
 * @throws ApiError — 失败时抛出带 status 和 error message 的对象
 */
export async function apiFetch<T>(
  path: string,
  options: {
    method?: "GET" | "POST" | "PUT" | "PATCH" | "DELETE";
    body?: unknown;
    query?: Record<string, unknown>;
  } = {},
): Promise<T> {
  const { method = "GET", body, query } = options;

  const headers: Record<string, string> = {
    Accept: "application/json",
  };

  // 状态变更请求注入 CSRF token
  if (MUTATION_METHODS.has(method)) {
    const csrfToken = getCsrfToken();
    if (csrfToken) {
      headers[CSRF_HEADER_NAME] = csrfToken;
    }
  }

  if (body !== undefined) {
    headers["Content-Type"] = "application/json";
  }

  const url = buildUrl(path, query);

  try {
    const response = await $fetch.raw<T>(url, {
      method,
      headers,
      body: body !== undefined ? JSON.stringify(body) : undefined,
      credentials: "include",
    });

    return response._data as T;
  } catch (err: unknown) {
    // $fetch 抛出 FetchError，包含 response 和 data
    const fetchErr = err as {
      response?: { status?: number; _data?: { error?: string } };
      message?: string;
    };
    const status = fetchErr.response?.status ?? 0;
    const message =
      fetchErr.response?._data?.error ?? fetchErr.message ?? "网络请求失败";

    // 受控日志：不记录敏感信息（password/token 不会出现在 API 响应 error message 中）
    if (import.meta.dev) {
      console.error(`[API] ${method} ${path} → ${status}: ${message}`);
    }

    // 401：清理认证态（含 pkw_user cookie）并跳转登录页；/auth/me 探测除外（守卫负责跳转）
    if (status === 401) {
      handleUnauthorized(path);
    }

    const apiError: ApiError = { error: message, status };
    throw apiError;
  }
}

// ============================================================
// Auth API
// ============================================================

export function useAuthApi() {
  return {
    login: (username: string, password: string) =>
      apiFetch<LoginResponse>("/auth/login", {
        method: "POST",
        body: { username, password },
      }),

    logout: () =>
      apiFetch<{ status: string }>("/auth/logout", {
        method: "POST",
      }),

    me: () => apiFetch<CurrentUserResponse>("/auth/me"),

    updateEmail: (req: UpdateEmailRequest) =>
      apiFetch<CurrentUserResponse>("/auth/me/email", {
        method: "PUT",
        body: req,
      }),

    updatePassword: (req: UpdatePasswordRequest) =>
      apiFetch<{ status: string }>("/auth/me/password", {
        method: "PUT",
        body: req,
      }),

    deviceAuthorize: (deviceName: string) =>
      apiFetch<DeviceAuthorizeResponse>("/auth/device/authorize", {
        method: "POST",
        body: { device_name: deviceName },
      }),

    revoke: (deviceSessionId?: string) =>
      apiFetch<{ status: string }>("/auth/revoke", {
        method: "POST",
        body: deviceSessionId ? { device_session_id: deviceSessionId } : {},
      }),

    deviceStart: (deviceName: string) =>
      apiFetch<DeviceAuthStartResponse>("/auth/device/start", {
        method: "POST",
        body: { device_name: deviceName },
      }),

    devicePoll: (deviceCode: string) =>
      apiFetch<DeviceAuthPollResponse>("/auth/device/poll", {
        method: "POST",
        body: { device_code: deviceCode },
      }),

    deviceApprove: (userCode: string) =>
      apiFetch<{ status: string }>("/auth/device/approve", {
        method: "POST",
        body: { user_code: userCode },
      }),

    deviceDeny: (userCode: string) =>
      apiFetch<{ status: string }>("/auth/device/deny", {
        method: "POST",
        body: { user_code: userCode },
      }),

    deviceInfo: (userCode: string) =>
      apiFetch<DeviceAuthInfoResponse>("/auth/device/info", {
        query: { code: userCode },
      }),

    listSessions: (params?: PaginationParams) =>
      apiFetch<ListDeviceSessionsResponse>("/auth/device/sessions", {
        query: params,
      }),
  };
}

// ============================================================
// Workspace API
// ============================================================

export function useWorkspaceApi() {
  return {
    list: (params?: PaginationParams) =>
      apiFetch<ListWorkspacesResponse>("/workspaces", { query: params }),

    create: (req: CreateWorkspaceRequest) =>
      apiFetch<Workspace>("/workspaces", { method: "POST", body: req }),

    get: (id: string) => apiFetch<Workspace>(`/workspaces/${id}`),

    update: (id: string, req: UpdateWorkspaceRequest) =>
      apiFetch<Workspace>(`/workspaces/${id}`, { method: "PUT", body: req }),

    archive: (id: string) =>
      apiFetch<Workspace>(`/workspaces/${id}/archive`, { method: "POST" }),

    stats: (id: string) =>
      apiFetch<WorkspaceStatsResponse>(`/workspaces/${id}/stats`),

    getMyMembership: (id: string) =>
      apiFetch<MyMembershipResponse>(`/workspaces/${id}/me/membership`),

    listMembers: (id: string, params?: PaginationParams) =>
      apiFetch<ListMembersResponse>(`/workspaces/${id}/members`, {
        query: params,
      }),

    listMemberCandidates: (id: string, query: string, limit = 10) =>
      apiFetch<MemberCandidatesResponse>(
        `/workspaces/${id}/members/candidates`,
        { query: { q: query, limit } },
      ),

    addMember: (id: string, req: AddMemberRequest) =>
      apiFetch<Member>(`/workspaces/${id}/members`, {
        method: "POST",
        body: req,
      }),

    updateMemberRole: (
      id: string,
      userId: string,
      req: UpdateMemberRoleRequest,
    ) =>
      apiFetch<Member>(`/workspaces/${id}/members/${userId}`, {
        method: "PUT",
        body: req,
      }),

    removeMember: (id: string, userId: string) =>
      apiFetch<{ status: string }>(`/workspaces/${id}/members/${userId}`, {
        method: "DELETE",
      }),
  };
}

// ============================================================
// Document API
// ============================================================

export function useDocumentApi() {
  return {
    list: (
      workspaceId: string,
      params?: PaginationParams & {
        status?: string;
        type?: string;
        include_archived?: boolean;
      },
    ) =>
      apiFetch<ListDocumentsResponse>(`/workspaces/${workspaceId}/documents`, {
        query: params,
      }),

    outline: (workspaceId: string, path: string) =>
      apiFetch<OutlineResponse>(
        `/workspaces/${workspaceId}/documents/outline`,
        { query: { path } },
      ),

    read: (workspaceId: string, path: string) =>
      apiFetch<Document>(`/workspaces/${workspaceId}/documents/read`, {
        query: { path },
      }),

    section: (workspaceId: string, path: string, sectionPath: string) =>
      apiFetch<SectionResponse>(
        `/workspaces/${workspaceId}/documents/section`,
        { query: { path, section_path: sectionPath } },
      ),

    lines: (workspaceId: string, path: string, start: number, end: number) =>
      apiFetch<LinesResponse>(`/workspaces/${workspaceId}/documents/lines`, {
        query: { path, start, end },
      }),

    create: (workspaceId: string, req: CreateDocumentRequest) =>
      apiFetch<Document>(`/workspaces/${workspaceId}/documents`, {
        method: "POST",
        body: req,
      }),

    replace: (workspaceId: string, path: string, req: ReplaceDocumentRequest) =>
      apiFetch<Document>(`/workspaces/${workspaceId}/documents`, {
        method: "PUT",
        query: { path },
        body: req,
      }),

    patch: (workspaceId: string, path: string, req: PatchDocumentRequest) =>
      apiFetch<Document>(`/workspaces/${workspaceId}/documents`, {
        method: "PATCH",
        query: { path },
        body: req,
      }),

    move: (workspaceId: string, path: string, req: MoveDocumentRequest) =>
      apiFetch<Document>(`/workspaces/${workspaceId}/documents/move`, {
        method: "POST",
        query: { path },
        body: req,
      }),

    archive: (workspaceId: string, path: string, req: ArchiveDocumentRequest) =>
      apiFetch<Document>(`/workspaces/${workspaceId}/documents/archive`, {
        method: "POST",
        query: { path },
        body: req,
      }),

    restore: (workspaceId: string, path: string, req: ArchiveDocumentRequest) =>
      apiFetch<Document>(`/workspaces/${workspaceId}/documents/restore`, {
        method: "POST",
        query: { path },
        body: req,
      }),

    purge: (workspaceId: string, path: string) =>
      apiFetch<{ status: string }>(
        `/workspaces/${workspaceId}/documents/purge`,
        { method: "POST", query: { path } },
      ),

    history: (workspaceId: string, path: string, params?: PaginationParams) =>
      apiFetch<ListRevisionsResponse>(
        `/workspaces/${workspaceId}/documents/history`,
        { query: { path, ...params } },
      ),

    revision: (workspaceId: string, path: string, revision: number) =>
      apiFetch<Revision>(`/workspaces/${workspaceId}/documents/revision`, {
        query: { path, revision },
      }),

    listSources: (
      workspaceId: string,
      path: string,
      params?: PaginationParams,
    ) =>
      apiFetch<ListSourcesResponse>(
        `/workspaces/${workspaceId}/documents/sources`,
        { query: { path, ...params } },
      ),

    addSource: (workspaceId: string, path: string, req: AddSourceRequest) =>
      apiFetch<Source>(`/workspaces/${workspaceId}/documents/sources`, {
        method: "POST",
        query: { path },
        body: req,
      }),

    deleteSource: (workspaceId: string, sourceId: string) =>
      apiFetch<{ status: string }>(
        `/workspaces/${workspaceId}/documents/sources/${sourceId}`,
        { method: "DELETE" },
      ),
  };
}

// ============================================================
// Search API
// ============================================================

export function useSearchApi() {
  return {
    search: (workspaceId: string, req: SearchRequest) =>
      apiFetch<SearchResponse>(`/workspaces/${workspaceId}/search`, {
        method: "POST",
        body: req,
      }),

    feedback: (workspaceId: string, req: SearchFeedbackRequest) =>
      apiFetch<{ status: string }>(
        `/workspaces/${workspaceId}/search/feedback`,
        { method: "POST", body: req },
      ),
  };
}

// ============================================================
// Admin API (workspace module)
// ============================================================

export function useAdminApi() {
  return {
    listUsers: (params?: AdminUserFilterParams) =>
      apiFetch<ListUsersResponse>("/admin/users", { query: params }),

    createUser: (req: CreateUserRequest) =>
      apiFetch<AdminUser>("/admin/users", { method: "POST", body: req }),

    updateUser: (id: string, req: UpdateUserRequest) =>
      apiFetch<AdminUser>(`/admin/users/${id}`, { method: "PUT", body: req }),

    listAllWorkspaces: (params?: AdminWorkspaceFilterParams) =>
      apiFetch<ListWorkspacesResponse>("/admin/workspaces", { query: params }),

    listAudit: (params?: AdminAuditFilterParams) =>
      apiFetch<ListAuditResponse>("/admin/audit", { query: params }),
  };
}

// ============================================================
// Admin API (search module)
// ============================================================

export function useTuningApi() {
  const path = (scope: TuningScope, workspaceId?: string) => {
    if (scope === "workspace") {
      if (!workspaceId)
        throw new Error("workspace tuning API requires workspaceId");
      return `/workspaces/${workspaceId}/tuning`;
    }
    return "/admin/tuning";
  };
  return {
    get: (scope: TuningScope, workspaceId?: string) =>
      apiFetch<TuningConfig>(path(scope, workspaceId)),
    updateDraft: (
      scope: TuningScope,
      req: UpdateTuningRequest,
      workspaceId?: string,
    ) =>
      apiFetch<TuningConfig>(`${path(scope, workspaceId)}/draft`, {
        method: "PUT",
        body: req,
      }),
    validate: (
      scope: TuningScope,
      req: ValidateTuningRequest,
      workspaceId?: string,
    ) =>
      apiFetch<TuningValidationResponse>(
        `${path(scope, workspaceId)}/validate`,
        { method: "POST", body: req },
      ),
    recommendations: (scope: TuningScope, workspaceId?: string) =>
      apiFetch<TuningRecommendationsResponse>(
        `${path(scope, workspaceId)}/recommendations`,
      ),
    publish: (
      scope: TuningScope,
      req: PublishTuningRequest,
      workspaceId?: string,
    ) =>
      apiFetch<TuningConfig>(`${path(scope, workspaceId)}/publish`, {
        method: "POST",
        body: req,
      }),
    rollback: (
      scope: TuningScope,
      req: RollbackTuningRequest,
      workspaceId?: string,
    ) =>
      apiFetch<TuningConfig>(`${path(scope, workspaceId)}/rollback`, {
        method: "POST",
        body: req,
      }),
    releaseOverride: (
      scope: TuningScope,
      req: PublishTuningRequest,
      workspaceId?: string,
    ) =>
      apiFetch<TuningConfig>(`${path(scope, workspaceId)}/override`, {
        method: "DELETE",
        body: req,
      }),
  };
}

export function useSearchAdminApi() {
  return {
    listProfiles: (params?: PaginationParams & { status?: string }) =>
      apiFetch<ListProfilesResponse>("/admin/search-profiles", {
        query: params,
      }),

    createProfile: (req: CreateProfileRequest) =>
      apiFetch<SearchProfile>("/admin/search-profiles", {
        method: "POST",
        body: req,
      }),

    // 新建版本：基于现有 profile 派生新 draft 版本，未提供的字段由服务端继承源 profile。
    createProfileVersion: (id: string, req: CreateProfileVersionRequest) =>
      apiFetch<SearchProfile>(`/admin/search-profiles/${id}/versions`, {
        method: "POST",
        body: req,
      }),

    // 归档：保留记录不物理删除，归档后可通过 rollback 重新激活。
    archiveProfile: (id: string) =>
      apiFetch<{ status: string }>(`/admin/search-profiles/${id}/archive`, {
        method: "POST",
      }),

    activateProfile: (id: string) =>
      apiFetch<{ status: string }>(`/admin/search-profiles/${id}/activate`, {
        method: "POST",
      }),

    rollbackProfile: (id: string) =>
      apiFetch<{ status: string }>(`/admin/search-profiles/${id}/rollback`, {
        method: "POST",
      }),

    listCandidates: (params?: PaginationParams & { status?: string }) =>
      apiFetch<{
        candidates: unknown[];
        total: number;
        limit: number;
        offset: number;
      }>("/admin/search-profiles/candidates", { query: params }),

    confirmCandidate: (id: string) =>
      apiFetch<{ status: string }>(
        `/admin/search-profiles/candidates/${id}/confirm`,
        { method: "POST" },
      ),

    listDatasets: (params?: PaginationParams & { status?: string }) =>
      apiFetch<ListDatasetsResponse>("/admin/evaluation/datasets", {
        query: params,
      }),

    createDataset: (req: CreateDatasetRequest) =>
      apiFetch<EvaluationDataset>("/admin/evaluation/datasets", {
        method: "POST",
        body: req,
      }),

    addItem: (datasetId: string, req: AddItemRequest) =>
      apiFetch<{ id: string }>(
        `/admin/evaluation/datasets/${datasetId}/items`,
        { method: "POST", body: req },
      ),

    /** GET /admin/evaluation/datasets/{id}/items — 评测条目分页列表 */
    listItems: (datasetId: string, params?: PaginationParams) =>
      apiFetch<ListItemsResponse>(
        `/admin/evaluation/datasets/${datasetId}/items`,
        { query: params },
      ),

    /** DELETE /admin/evaluation/datasets/{id}/items/{itemId} — 删除单条评测条目（CSRF + system_admin） */
    deleteItem: (datasetId: string, itemId: string) =>
      apiFetch<{ status: string }>(
        `/admin/evaluation/datasets/${datasetId}/items/${itemId}`,
        { method: "DELETE" },
      ),

    /**
     * DELETE /admin/evaluation/datasets/{id} — 软删（归档）数据集。
     * 后端将 status 置为 'archived'，保留全部 items 和 evaluation_results。
     * 归档为 API 层终态，无 restore 端点；归档后 RunEvaluation/AddItem 返回 409。
     */
    deleteDataset: (datasetId: string) =>
      apiFetch<{ status: string }>(`/admin/evaluation/datasets/${datasetId}`, {
        method: "DELETE",
      }),

    runEvaluation: (req: RunEvaluationRequest) =>
      apiFetch<{
        status: string;
        job_id: string;
        dataset_id: string;
        profile_id: string;
      }>("/admin/evaluation/run", { method: "POST", body: req }),

    listEvaluationResults: (datasetId: string, params?: PaginationParams) =>
      apiFetch<ListEvaluationResultsResponse>("/admin/evaluation/results", {
        query: { dataset_id: datasetId, ...params },
      }),

    listJobs: (params?: PaginationParams & { status?: string }) =>
      apiFetch<ListJobsResponse>("/admin/jobs", { query: params }),

    retryJob: (id: string) =>
      apiFetch<{ status: string }>(`/admin/jobs/${id}/retry`, {
        method: "POST",
      }),

    rebuildIndex: (req: { workspace_id?: string; index_name?: string }) =>
      apiFetch<{ status: string }>("/admin/jobs/rebuild", {
        method: "POST",
        body: req,
      }),

    // Settings 管理（Phase6 WP3）
    listSettings: () =>
      apiFetch<ListSettingsResponse>("/admin/config/settings"),

    updateSetting: (key: string, req: UpdateSettingRequest) =>
      apiFetch<UpdateSettingResponse>(`/admin/config/settings/${key}`, {
        method: "PUT",
        body: req,
      }),

    getRuntimeStatus: () =>
      apiFetch<RuntimeStatusResponse>("/admin/config/runtime"),
  };
}

// ============================================================
// Bootstrap API — 首次管理员创建（公开，不需要认证）
// ============================================================

/**
 * useBootstrapApi 提供 Bootstrap 相关 API 调用。
 *
 * 引入动机：计划要求空数据库启动时提供一次性 Bootstrap API 创建第一个 system_admin。
 * Bootstrap 端点不需要认证（首次使用时无用户），但创建后永久关闭。
 */
export function useBootstrapApi() {
  return {
    /** GET /api/bootstrap — 查询 Bootstrap 是否可用 */
    getStatus: () => apiFetch<BootstrapStatusResponse>("/bootstrap"),

    /** POST /api/bootstrap — 创建首个 system_admin（仅 users 表为空时可用） */
    createAdmin: (req: BootstrapRequest) =>
      apiFetch<BootstrapResponse>("/bootstrap", { method: "POST", body: req }),
  };
}

// ============================================================
// Provider API — 管理员 Provider 配置管理（system_admin + CSRF）
// ============================================================

/**
 * useProviderApi 提供 Provider 配置管理 API 调用。
 *
 * 引入动机：计划要求 system_admin 通过受 CSRF 保护的 Web API 管理 Provider 配置。
 * 安全契约：
 *   - GET 响应绝不包含 API Key 明文或密文
 *   - PUT 请求的 api_key 字段不进入 GET 响应、不进入缓存
 *   - POST test 不持久化任何数据
 *
 * 调用方职责：
 *   - PUT 成功后立即清空 api_key 输入框，不缓存、不回显
 *   - GET 仅显示是否已设置（api_key_set 布尔值）
 */
export function useProviderApi() {
  return {
    /** GET /api/admin/providers — 列出所有 Provider 状态和非敏感配置 */
    list: () => apiFetch<ListProvidersResponse>("/admin/providers"),

    /** GET /api/admin/providers/{type} — 获取单个 Provider 状态和非敏感配置 */
    get: (type: "embedding" | "reranker") =>
      apiFetch<ProviderConfigDTO>(`/admin/providers/${type}`),

    /** PUT /api/admin/providers/{type} — 加密写入 Provider 配置并热更新 */
    put: (type: "embedding" | "reranker", req: PutProviderRequest) =>
      apiFetch<PutProviderResponse>(`/admin/providers/${type}`, {
        method: "PUT",
        body: req,
      }),

    /** POST /api/admin/providers/{type}/test — 临时测试 Provider 连通性，不持久化 */
    test: (type: "embedding" | "reranker", req: TestProviderRequest) =>
      apiFetch<TestProviderResponse>(`/admin/providers/${type}/test`, {
        method: "POST",
        body: req,
      }),
  };
}
