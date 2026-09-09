// Package admin 实现业务配置（Settings）管理 HTTP handler。
//
// 引入动机：Phase6 WP3 要求安全业务配置管理：
//   - 严格 allowlist 和类型/range 校验
//   - 审计日志对旧/新值脱敏
//   - 返回明确的 restart_required 语义
//   - 提供 /api/admin/config/runtime 返回不敏感的运行状态
//
// 所有端点仅 system_admin 可访问。
package admin

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"partitura/server/internal/auth"
	"partitura/server/internal/config"
	"partitura/server/internal/health"
	httpmw "partitura/server/internal/http"
	"partitura/server/internal/settings"
	"partitura/server/internal/workspace"
)

// SettingsHandler 是业务配置管理 HTTP handler。
type SettingsHandler struct {
	repo            settings.Repository
	auditRepo       AuditRepository
	cfg             *config.Config
	healthCollector *health.SnapshotCollector
}

// NewSettingsHandler 创建业务配置管理 handler。
// 引入动机：将 settings 领域逻辑与 admin 路由结合，同时注入审计和运行状态依赖。
func NewSettingsHandler(repo settings.Repository, auditRepo AuditRepository, cfg *config.Config, healthCollector *health.SnapshotCollector) *SettingsHandler {
	return &SettingsHandler{
		repo:            repo,
		auditRepo:       auditRepo,
		cfg:             cfg,
		healthCollector: healthCollector,
	}
}

// RegisterSettingsRoutes 注册业务配置管理路由。
//
// 注册端点：
//   - GET  /api/admin/config/settings — system_admin，列出所有业务配置
//   - PUT  /api/admin/config/settings/{key} — system_admin + CSRF，更新指定业务配置
//   - GET  /api/admin/config/runtime — system_admin，查看不敏感的运行状态
func RegisterSettingsRoutes(
	mux *http.ServeMux,
	handler *SettingsHandler,
	authRepo auth.Repository,
	authCfg auth.AuthConfig,
) {
	// GET /api/admin/config/settings — system_admin
	mux.Handle("GET /api/admin/config/settings",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				workspace.RequireSystemAdmin(
					http.HandlerFunc(handler.ListSettings),
				),
			),
		),
	)

	// PUT /api/admin/config/settings/{key} — system_admin + CSRF
	mux.Handle("PUT /api/admin/config/settings/{key}",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				auth.RequireCSRF(authRepo, authCfg)(
					workspace.RequireSystemAdmin(
						http.HandlerFunc(handler.UpdateSetting),
					),
				),
			),
		),
	)

	// GET /api/admin/config/runtime — system_admin
	mux.Handle("GET /api/admin/config/runtime",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				workspace.RequireSystemAdmin(
					http.HandlerFunc(handler.GetRuntimeStatus),
				),
			),
		),
	)
}

// settingRecord 是返回给前端的业务配置项。
// 引入动机：与 repository 解耦，补充 allowlist 元信息。
type settingRecord struct {
	Key             string `json:"key"`
	Value           string `json:"value"`
	Type            string `json:"type"`
	Description     string `json:"description"`
	RestartRequired bool   `json:"restart_required"`
}

// ListSettings 处理 GET /api/admin/config/settings。
// 返回所有业务配置项，未设置时取 allowlist 默认值。
func (h *SettingsHandler) ListSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeAdminError(w, http.StatusMethodNotAllowed, "方法不允许")
		return
	}

	records, err := h.repo.ListNonSecret(r.Context())
	if err != nil {
		slog.Error("列出业务配置失败", "error", err)
		writeAdminError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	// 构建 key -> value 映射，便于填充未持久化的 allowlist 默认值。
	values := make(map[string]string, len(records))
	for _, rec := range records {
		values[rec.Key] = rec.Value
	}

	result := make([]settingRecord, 0, len(settings.AllowedBusinessSettings))
	for key, def := range settings.AllowedBusinessSettings {
		value := values[key]
		if value == "" {
			value = def.Default
		}
		result = append(result, settingRecord{
			Key:             key,
			Value:           value,
			Type:            def.Type,
			Description:     def.Description,
			RestartRequired: def.RestartRequired,
		})
	}

	writeAdminJSON(w, http.StatusOK, map[string]interface{}{
		"settings": result,
	})
}

// updateSettingRequest 是更新业务配置的请求体。
type updateSettingRequest struct {
	Value string `json:"value"`
}

// updateSettingResponse 是更新业务配置的响应体。
type updateSettingResponse struct {
	Key             string `json:"key"`
	Value           string `json:"value"`
	RestartRequired bool   `json:"restart_required"`
	Message         string `json:"message"`
}

// UpdateSetting 处理 PUT /api/admin/config/settings/{key}。
// 校验 allowlist、类型和范围后更新，并记录审计日志。
func (h *SettingsHandler) UpdateSetting(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", "PUT")
		writeAdminError(w, http.StatusMethodNotAllowed, "方法不允许")
		return
	}

	key := r.PathValue("key")
	if key == "" {
		writeAdminError(w, http.StatusBadRequest, "缺少 setting key")
		return
	}

	def, allowed := settings.GetAllowedSetting(key)
	if !allowed {
		writeAdminError(w, http.StatusForbidden, "该配置项不允许通过 Web 管理")
		return
	}

	var req updateSettingRequest
	if err := decodeJSONStrict(r, &req); err != nil {
		writeAdminError(w, http.StatusBadRequest, fmt.Sprintf("请求体非法: %v", err))
		return
	}

	normalized, err := settings.Validate(key, req.Value)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}

	// 查询旧值用于审计。
	identity := auth.IdentityFromContext(r.Context())
	oldValue := def.Default
	if oldRecord, oldErr := h.repo.Get(r.Context(), key); oldErr == nil && oldRecord != nil {
		oldValue = oldRecord.Value
	} else if oldErr != nil && oldErr != sql.ErrNoRows {
		slog.Warn("查询 setting 旧值失败，按默认值审计", "error", oldErr, "key", key)
	}

	updatedBy := ""
	if identity != nil {
		updatedBy = identity.UserID
	}
	if err := h.repo.Update(r.Context(), key, normalized, def.Type, updatedBy); err != nil {
		slog.Error("更新业务配置失败", "error", err, "key", key)
		writeAdminError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	// 记录审计：仅保留非敏感旧/新值摘要。
	if identity != nil {
		h.recordSettingsAudit(r, identity.UserID, key,
			settings.SanitizeForAudit(key, oldValue),
			settings.SanitizeForAudit(key, normalized),
		)
	}

	message := "配置已更新"
	if def.RestartRequired {
		message = "配置已保存，需要重启服务后才能生效"
	}

	writeAdminJSON(w, http.StatusOK, updateSettingResponse{
		Key:             key,
		Value:           normalized,
		RestartRequired: def.RestartRequired,
		Message:         message,
	})
}

// GetRuntimeStatus 处理 GET /api/admin/config/runtime。
// 返回不敏感的运行状态：健康快照、配置来源标志、当前 profile 等；绝不返回 DSN、key、password。
func (h *SettingsHandler) GetRuntimeStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeAdminError(w, http.StatusMethodNotAllowed, "方法不允许")
		return
	}

	var snapshot health.Snapshot
	if h.healthCollector != nil {
		snapshot = h.healthCollector.Collect(r.Context())
	}

	// 只返回不敏感的来源标志；真实值（URL、DSN、key）绝不暴露。
	// 引入动机：Provider 配置现在从数据库管理，不再从环境变量读取。
	runtime := map[string]interface{}{
		"log_level":             h.cfg.LogLevel,
		"job_worker_enabled":    h.cfg.JobWorkerEnabled,
		"scheduler_enabled":     h.cfg.SchedulerEnabled,
		"cookie_secure":         h.cfg.CookieSecure,
		"pg_configured":         h.cfg.DBHost != "" && h.cfg.DBName != "",
		"es_configured":         h.cfg.ESURL != "",
		"encryption_key_set":    h.cfg.MasterEncryptionKey != "",
		"health":                snapshot,
		"active_profile":        snapshot.ActiveProfile,
	}

	writeAdminJSON(w, http.StatusOK, map[string]interface{}{
		"status":  "ok",
		"runtime": runtime,
	})
}

// recordSettingsAudit 记录业务配置变更审计。
// 引入动机：H7 要求 admin 操作记录审计日志；对旧/新值脱敏。
func (h *SettingsHandler) recordSettingsAudit(r *http.Request, userID, key, oldValue, newValue string) {
	if h.auditRepo == nil {
		return
	}
	detail := map[string]string{
		"key":       key,
		"old_value": oldValue,
		"new_value": newValue,
	}
	detailJSON, err := json.Marshal(detail)
	if err != nil {
		slog.Error("序列化 settings 审计 detail 失败", "error", err, "key", key)
		return
	}
	requestID := httpmw.RequestIDFromContext(r.Context())
	if err := h.auditRepo.Record(r.Context(), userID, "", "update_setting", "setting", key, detailJSON, requestID); err != nil {
		slog.Error("写入 settings 审计日志失败", "error", err, "key", key)
	}
}
