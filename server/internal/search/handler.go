// Package search 实现搜索 HTTP handler 和路由注册。
//
// 引入动机：design/04-WEB-API.md §Search 要求：
//   - POST /api/workspaces/{wid}/search — viewer+，搜索当前 workspace
//   - POST /api/workspaces/{wid}/search/feedback — viewer+，记录搜索反馈
//
// design/04-WEB-API.md §Security 要求 ES query 只能由服务器生成，
// 客户端不能自定义 workspace filter 或原始 ES DSL 入口。
//
// 设计原则：
//   - handler 只负责 HTTP 解析和响应
//   - 搜索管线、provider、chunk、RRF/diversity 职责清楚
//   - 不出现 XxxService/XxxManager/XxxController
package search

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"partitura/server/internal/auth"
	httpmw "partitura/server/internal/http"
	"partitura/server/internal/job"
	"partitura/server/internal/profile"
	"partitura/server/internal/search/pipeline"
	types "partitura/server/internal/search/types"
	"partitura/server/internal/workspace"
)

// Handler 是搜索 HTTP handler。
// 引入动机：design/04-WEB-API.md §Search 要求搜索和反馈 API。
type Handler struct {
	pipeline    *pipeline.Pipeline
	profileRepo profile.Repository
	metricsRepo job.SearchMetricsRepo
}

// AuditRepo 定义审计记录接口（复用 audit.Repository 的子集）。
// 引入动机：搜索操作需要记录审计日志。
// 已移除：使用 audit.Repository 直接接口。

// NewHandler 创建搜索 handler。
func NewHandler(pipe *pipeline.Pipeline, profileRepo profile.Repository, metricsRepo job.SearchMetricsRepo) *Handler {
	return &Handler{
		pipeline:    pipe,
		profileRepo: profileRepo,
		metricsRepo: metricsRepo,
	}
}

// searchRequest 是搜索 API 的请求体。
// 引入动机：design/04-WEB-API.md §Search 要求搜索 API 接受 query, mode, limit。
// mode 控制搜索管线行为：
//   - "hybrid"（默认）：BM25 + vector → RRF → reranker
//   - "lexical"：仅 BM25 lexical 检索
//   - "semantic"：仅 dense vector 检索
type searchRequest struct {
	Query  string `json:"query"`
	Mode   string `json:"mode"`
	Limit  int    `json:"limit"`
	Offset int    `json:"offset"`
}

// 搜索模式常量。
const (
	SearchModeHybrid   = "hybrid"
	SearchModeLexical  = "lexical"
	SearchModeSemantic = "semantic"
)

// Search 处理 POST /api/workspaces/{wid}/search。
//
// 引入动机：design/04-WEB-API.md §Search 要求搜索当前 workspace。
// 权限：viewer+（RequireWorkspacePermission(PermSearch) 已在路由层验证）。
// 安全：服务端生成 workspace filter，客户端不能定义或绕过。
func (h *Handler) Search(w http.ResponseWriter, r *http.Request) {
	wsc := workspace.WorkspaceContextFromContext(r.Context())
	if wsc == nil {
		writeSearchError(w, http.StatusInternalServerError, "内部错误：缺少 workspace 上下文")
		return
	}

	var req searchRequest
	if err := decodeJSONStrict(r, &req); err != nil {
		writeSearchError(w, http.StatusBadRequest, fmt.Sprintf("请求体解析失败: %v", err))
		return
	}

	if req.Query == "" {
		writeSearchError(w, http.StatusBadRequest, "query 不能为空")
		return
	}

	// 验证 mode 参数（M3）
	switch req.Mode {
	case "", SearchModeHybrid, SearchModeLexical, SearchModeSemantic:
		// 合法：空值默认为 hybrid
		if req.Mode == "" {
			req.Mode = SearchModeHybrid
		}
	default:
		writeSearchError(w, http.StatusBadRequest, "无效的 mode，支持: hybrid, lexical, semantic")
		return
	}

	// limit/offset 边界校验：越界返回 400 而非静默重置。
	// 引入动机：design/00-MASTER.md 禁止 silent overwrite，design/04-WEB-API.md §Concurrency 要求
	// 不匹配时返回 HTTP 409/400。limit==0 表示客户端未设置，使用默认值 10。
	if req.Limit == 0 {
		req.Limit = 10
	}
	if req.Limit < 0 || req.Limit > 100 {
		slog.Warn("搜索请求 limit 越界",
			"workspace_id", wsc.WorkspaceID,
			"limit", req.Limit,
		)
		writeSearchError(w, http.StatusBadRequest,
			fmt.Sprintf("limit 越界: %d，允许范围 1-100（0 表示默认值 10）", req.Limit))
		return
	}
	if req.Offset < 0 {
		slog.Warn("搜索请求 offset 越界",
			"workspace_id", wsc.WorkspaceID,
			"offset", req.Offset,
		)
		writeSearchError(w, http.StatusBadRequest,
			fmt.Sprintf("offset 越界: %d，不允许负数", req.Offset))
		return
	}

	// 获取 active profile
	profileRecord, err := h.profileRepo.GetActiveProfile(r.Context())
	if err != nil {
		slog.Error("获取 active profile 失败", "error", err)
		// 无 active profile 时返回 degraded
		resp := types.SearchResponse{
			Results:           nil,
			Degraded:          true,
			DegradationReason: "no_active_profile",
		}
		writeSearchJSON(w, http.StatusOK, resp)
		return
	}

	profileConfig := profileRecord.ToConfig()

	// 执行搜索管线
	input := pipeline.SearchInput{
		WorkspaceID: wsc.WorkspaceID,
		Query:       req.Query,
		Profile:     profileConfig,
		Limit:       req.Limit,
		Offset:      req.Offset,
		Mode:        req.Mode,
	}

	output, err := h.pipeline.Search(r.Context(), input)
	if err != nil {
		slog.Error("搜索失败", "error", err, "workspace_id", wsc.WorkspaceID)
		writeSearchError(w, http.StatusInternalServerError, "搜索失败")
		return
	}

	// 构建响应
	resp := types.SearchResponse{
		Results:           output.Results,
		Total:             output.Total,
		Limit:             req.Limit,
		Offset:            req.Offset,
		Degraded:          output.Degraded,
		DegradationReason: output.DegradationReason,
		SearchID:          output.SearchID,
		RerankerUsed:      output.RerankerUsed,
	}

	// 记录搜索指标
	id := auth.IdentityFromContext(r.Context())
	userID := ""
	if id != nil {
		userID = id.UserID
	}
	requestID := httpmw.RequestIDFromContext(r.Context())

	_ = h.metricsRepo.RecordMetrics(r.Context(), job.RecordMetricsParams{
		WorkspaceID:       wsc.WorkspaceID,
		UserID:            userID,
		SearchID:          output.SearchID,
		ProfileID:         profileConfig.ID,
		LatencyMs:         output.LatencyMs,
		RerankerUsed:      output.RerankerUsed,
		ResultCount:       output.Total,
		Degraded:          output.Degraded,
		DegradationReason: output.DegradationReason,
	})

	slog.Info("搜索完成",
		"workspace_id", wsc.WorkspaceID,
		"search_id", output.SearchID,
		"results", output.Total,
		"degraded", output.Degraded,
		"reranker_used", output.RerankerUsed,
		"latency_ms", output.LatencyMs,
		"request_id", requestID,
	)

	writeSearchJSON(w, http.StatusOK, resp)
}

// feedbackRequest 是搜索反馈 API 的请求体。
type feedbackRequest struct {
	FeedbackType string `json:"feedback_type"`
	Query        string `json:"query"`
	DocumentID   string `json:"document_id"`
	Detail       json.RawMessage `json:"detail,omitempty"`
	SearchID     string `json:"search_id"`
}

// Feedback 处理 POST /api/workspaces/{wid}/search/feedback。
//
// 引入动机：design/04-WEB-API.md §Search 要求反馈 API。
// 权限：viewer+。
// 安全：不接受跨 workspace document。
func (h *Handler) Feedback(w http.ResponseWriter, r *http.Request) {
	wsc := workspace.WorkspaceContextFromContext(r.Context())
	if wsc == nil {
		writeSearchError(w, http.StatusInternalServerError, "内部错误：缺少 workspace 上下文")
		return
	}

	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeSearchError(w, http.StatusUnauthorized, "未认证")
		return
	}

	var req feedbackRequest
	if err := decodeJSONStrict(r, &req); err != nil {
		writeSearchError(w, http.StatusBadRequest, fmt.Sprintf("请求体解析失败: %v", err))
		return
	}

	// 验证 feedback_type
	switch req.FeedbackType {
	case types.FeedbackVerified, types.FeedbackExplicit, types.FeedbackImplicit:
		// 合法
	default:
		writeSearchError(w, http.StatusBadRequest, "无效的 feedback_type")
		return
	}

	// 记录反馈
	err := h.metricsRepo.RecordFeedback(r.Context(), job.RecordFeedbackParams{
		WorkspaceID:  wsc.WorkspaceID,
		UserID:       id.UserID,
		FeedbackType: req.FeedbackType,
		Query:        req.Query,
		DocumentID:   req.DocumentID,
		Detail:       req.Detail,
		SearchID:     req.SearchID,
	})
	if err != nil {
		slog.Error("记录搜索反馈失败", "error", err)
		writeSearchError(w, http.StatusInternalServerError, "记录反馈失败")
		return
	}

	writeSearchJSON(w, http.StatusCreated, map[string]string{"status": "recorded"})
}

// writeSearchError 写入统一 JSON 错误响应。
func writeSearchError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(map[string]string{"error": message}); err != nil {
		slog.Error("写入错误响应失败", "error", err)
	}
}

// writeSearchJSON 写入 JSON 响应。
func writeSearchJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	data, err := json.Marshal(body)
	if err != nil {
		slog.Error("序列化 JSON 响应失败", "error", err)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"内部错误"}`))
		return
	}
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

// decodeJSONStrict 严格解码 JSON 请求体。
// 引入动机：所有 handler 需要拒绝畸形 JSON、未知字段、多 JSON 值。
func decodeJSONStrict(r *http.Request, dst interface{}) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("请求体包含多个 JSON 值")
	}
	return nil
}

// RegisterRoutes 注册搜索模块的 HTTP 路由。
//
// 引入动机：main.go 调用此函数完成搜索路由注册。
//
// 路由清单：
//   - POST /api/workspaces/{wid}/search — viewer+，搜索当前 workspace
//   - POST /api/workspaces/{wid}/search/feedback — viewer+，记录搜索反馈
//
// middleware 链：AuthMiddleware → RequireAuth → RequireCSRF → RequireWorkspacePermission(PermSearch)
func RegisterRoutes(
	mux *http.ServeMux,
	handler *Handler,
	wsRepo workspace.Repository,
	authRepo auth.Repository,
	authCfg auth.AuthConfig,
) {
	// POST /api/workspaces/{wid}/search — viewer+（PermSearch）
	mux.Handle("POST /api/workspaces/{wid}/search",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				auth.RequireCSRF(authRepo, authCfg)(
					workspace.RequireWorkspacePermission(wsRepo, workspace.PermSearch, "wid")(
						http.HandlerFunc(handler.Search),
					),
				),
			),
		),
	)

	// POST /api/workspaces/{wid}/search/feedback — viewer+（PermSearch）
	mux.Handle("POST /api/workspaces/{wid}/search/feedback",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				auth.RequireCSRF(authRepo, authCfg)(
					workspace.RequireWorkspacePermission(wsRepo, workspace.PermSearch, "wid")(
						http.HandlerFunc(handler.Feedback),
					),
				),
			),
		),
	)
}
