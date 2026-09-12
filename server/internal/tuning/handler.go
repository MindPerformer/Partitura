package tuning

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"partitura/server/internal/auth"
	"partitura/server/internal/es"
	httpmw "partitura/server/internal/http"
	"partitura/server/internal/profile"
	"partitura/server/internal/search/pipeline"
	"partitura/server/internal/workspace"
)

type MetricsReader interface {
	AggregateMetrics(context.Context, string) (*MetricsSummary, error)
}
type Handler struct {
	repo     Repository
	profiles profile.Repository
	audit    AuditRepository
	metrics  MetricsReader
	pipe     *pipeline.Pipeline
}
type AuditRepository interface {
	Record(context.Context, string, string, string, string, string, json.RawMessage, string) error
}

func NewHandler(repo Repository, profiles profile.Repository, audit AuditRepository) *Handler {
	return &Handler{repo: repo, profiles: profiles, audit: audit}
}
func NewHandlerWithMetrics(repo Repository, profiles profile.Repository, audit AuditRepository, metrics MetricsReader, pipe *pipeline.Pipeline) *Handler {
	return &Handler{repo: repo, profiles: profiles, audit: audit, metrics: metrics, pipe: pipe}
}

type draftRequest struct {
	ExpectedRevision int64      `json:"expected_revision"`
	Parameters       Parameters `json:"parameters"`
}
type publishRequest struct {
	ExpectedRevision int64 `json:"expected_revision"`
}
type rollbackRequest struct {
	ExpectedRevision int64 `json:"expected_revision"`
	TargetRevision   int64 `json:"target_revision"`
}
type validateRequest struct {
	Parameters  Parameters `json:"parameters"`
	WorkspaceID string     `json:"workspace_id"`
	Query       string     `json:"query"`
	Queries     []string   `json:"queries"`
}

func (h *Handler) state(w http.ResponseWriter, r *http.Request) {
	wid := r.PathValue("id")
	base, p, s, err := h.repo.Effective(r.Context(), wid)
	if err != nil {
		h.fail(w, err, "读取 effective tuning")
		return
	}
	cfg, err := profileMap(base, p)
	if err != nil {
		h.fail(w, err, "编码 effective tuning")
		return
	}
	global, err := h.repo.GetState(r.Context(), "")
	if err != nil {
		h.fail(w, err, "读取 global tuning")
		return
	}
	capabilities := map[string]any{"can_edit": true, "can_publish": true, "can_rollback": s.PublishedRevision > 0, "can_clear": s.PublishedRevision > 0 || len(s.Draft) > 0}
	writeJSON(w, http.StatusOK, map[string]any{
		"scope": s.Scope, "workspace_id": s.WorkspaceID, "revision": s.Revision,
		"global_revision": global.Revision, "published_revision": s.PublishedRevision,
		"base_profile": base, "effective_config": cfg, "published": nonNilParameters(s.Published),
		"draft": nonNilParameters(s.Draft), "history": nonNilReleases(s.History),
		"parameters": ParameterDefinitions(), "capabilities": capabilities,
	})
}
func profileMap(base *profile.ProfileRecord, overrides Parameters) (map[string]any, error) {
	b, err := json.Marshal(base)
	if err != nil {
		return nil, err
	}
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}
	for k, v := range overrides {
		cfg[k] = v
	}
	return cfg, nil
}
func nonNilParameters(p Parameters) Parameters {
	if p == nil {
		return Parameters{}
	}
	return p
}
func nonNilReleases(r []Release) []Release {
	if r == nil {
		return []Release{}
	}
	return r
}
func (h *Handler) Draft(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeJSON(w, 401, map[string]string{"error": "未认证"})
		return
	}
	var q draftRequest
	if e := decode(r, &q); e != nil {
		writeJSON(w, 400, map[string]string{"error": e.Error()})
		return
	}
	wid := r.PathValue("id")
	s, e := h.repo.SaveDraft(r.Context(), wid, q.ExpectedRevision, q.Parameters, id.UserID)
	if e != nil {
		h.fail(w, e, "保存 draft")
		return
	}
	h.auditLog(r, id.UserID, wid, "tuning.draft", q.Parameters)
	h.state(w, r)
	_ = s
}
func (h *Handler) Publish(w http.ResponseWriter, r *http.Request) {
	var q publishRequest
	if e := decode(r, &q); e != nil {
		writeJSON(w, 400, map[string]string{"error": e.Error()})
		return
	}
	h.mutate(w, r, "tuning.publish", func(ctx context.Context, wid, uid string) (*State, error) {
		return h.repo.Publish(ctx, wid, q.ExpectedRevision, uid)
	})
}
func (h *Handler) Rollback(w http.ResponseWriter, r *http.Request) {
	var q rollbackRequest
	if e := decode(r, &q); e != nil {
		writeJSON(w, 400, map[string]string{"error": e.Error()})
		return
	}
	h.mutate(w, r, "tuning.rollback", func(ctx context.Context, wid, uid string) (*State, error) {
		return h.repo.Rollback(ctx, wid, q.ExpectedRevision, q.TargetRevision, uid)
	})
}
func (h *Handler) Clear(w http.ResponseWriter, r *http.Request) {
	var q publishRequest
	if e := decode(r, &q); e != nil {
		writeJSON(w, 400, map[string]string{"error": e.Error()})
		return
	}
	h.mutate(w, r, "tuning.clear", func(ctx context.Context, wid, uid string) (*State, error) {
		return h.repo.Clear(ctx, wid, q.ExpectedRevision, uid)
	})
}
func (h *Handler) mutate(w http.ResponseWriter, r *http.Request, action string, fn func(context.Context, string, string) (*State, error)) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeJSON(w, 401, map[string]string{"error": "未认证"})
		return
	}
	s, e := fn(r.Context(), r.PathValue("id"), id.UserID)
	if e != nil {
		h.fail(w, e, action)
		return
	}
	h.auditLog(r, id.UserID, r.PathValue("id"), action, nil)
	writeJSON(w, 200, s)
}
func (h *Handler) Validate(w http.ResponseWriter, r *http.Request) {
	var q validateRequest
	if err := decode(r, &q); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := Validate(q.Parameters); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	wid := r.PathValue("id")
	if wid == "" {
		wid = q.WorkspaceID
	}
	if len(q.Queries) == 0 && strings.TrimSpace(q.Query) != "" {
		q.Queries = []string{q.Query}
	}
	if len(q.Queries) > 5 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "queries 最多支持 5 条"})
		return
	}
	base, current, _, err := h.repo.Effective(r.Context(), wid)
	if err != nil {
		h.fail(w, err, "验证 tuning")
		return
	}
	baselineConfig := base.ToConfig()
	candidate := base.ToConfig()
	for k, v := range current {
		if err := applyRuntimeOverride(&baselineConfig, k, v); err != nil {
			h.fail(w, err, "应用当前 tuning")
			return
		}
		if err := applyRuntimeOverride(&candidate, k, v); err != nil {
			h.fail(w, err, "应用当前 tuning")
			return
		}
	}
	for k, v := range q.Parameters {
		if err := applyRuntimeOverride(&candidate, k, v); err != nil {
			h.fail(w, fmt.Errorf("%w: %v", ErrInvalidParameter, err), "应用候选 tuning")
			return
		}
	}
	overrides := Parameters{}
	for k, v := range current {
		overrides[k] = v
	}
	for k, v := range q.Parameters {
		overrides[k] = v
	}
	cfg, err := profileMap(base, overrides)
	if err != nil {
		h.fail(w, err, "编码 candidate tuning")
		return
	}
	checks := []map[string]any{{"code": "parameter_range", "passed": true, "message": "参数范围合法"}}
	results := []any{}
	if len(q.Queries) > 0 {
		if wid == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "提供 query/queries 时必须指定 workspace_id"})
			return
		}
		if h.pipe == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"valid": false, "checks": checks, "effective_config": cfg, "results": []any{}, "publish_gate": false, "message": "快速验证链路未配置"})
			return
		}
		for _, raw := range q.Queries {
			query := strings.TrimSpace(raw)
			if query == "" || len([]rune(query)) > 4096 {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "query 不能为空且最多 4096 个字符"})
				return
			}
			baselineOut, baselineErr := h.pipe.Search(r.Context(), pipeline.SearchInput{WorkspaceID: wid, Query: query, Profile: baselineConfig, IndexName: es.AliasName, Limit: 10, Mode: "hybrid"})
			candidateOut, candidateErr := h.pipe.Search(r.Context(), pipeline.SearchInput{WorkspaceID: wid, Query: query, Profile: candidate, IndexName: es.AliasName, Limit: 10, Mode: "hybrid"})
			if baselineErr != nil || candidateErr != nil {
				writeJSON(w, http.StatusServiceUnavailable, map[string]any{"valid": false, "checks": checks, "effective_config": cfg, "results": results, "publish_gate": false, "message": "快速验证无法执行搜索链路"})
				return
			}
			results = append(results, validationResult(query, baselineOut, candidateOut))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"valid": true, "checks": checks, "effective_config": cfg, "results": results, "publish_gate": false})
}
func validationResult(query string, baseline, candidate *pipeline.SearchOutput) map[string]any {
	return map[string]any{"query": query, "baseline": map[string]any{"results": baseline.Results, "total": baseline.Total, "latency_ms": baseline.LatencyMs, "degraded": baseline.Degraded, "degradation_reason": baseline.DegradationReason}, "candidate": map[string]any{"results": candidate.Results, "total": candidate.Total, "latency_ms": candidate.LatencyMs, "degraded": candidate.Degraded, "degradation_reason": candidate.DegradationReason}}
}
func (h *Handler) Recommendations(w http.ResponseWriter, r *http.Request) {
	wid := r.PathValue("id")
	_, p, s, err := h.repo.Effective(r.Context(), wid)
	if err != nil {
		h.fail(w, err, "读取 tuning 推荐")
		return
	}
	metrics := &MetricsSummary{}
	if h.metrics != nil {
		metrics, err = h.metrics.AggregateMetrics(r.Context(), wid)
		if err != nil {
			h.fail(w, err, "聚合 tuning metrics")
			return
		}
	}
	recs := []map[string]any{}
	latencyParams := Parameters{"reranker_candidate_count": 20}
	latencyReason := "暂无近 7 天 search_metrics，建议从保守候选数开始验证"
	if metrics.Samples > 0 {
		latencyReason = fmt.Sprintf("近 7 天 %d 次搜索 P95 延迟 %.0fms，降低 reranker 候选数可优先控制延迟", metrics.Samples, metrics.P95LatencyMs)
		if metrics.P95LatencyMs <= 500 {
			latencyReason = fmt.Sprintf("近 7 天 P95 延迟 %.0fms，当前延迟约束已较好，建议保持候选数并做小步验证", metrics.P95LatencyMs)
		}
	}
	recs = append(recs, map[string]any{"id": "latency-budget", "title": "控制 reranker 延迟预算", "reason": latencyReason, "parameters": latencyParams, "evidence": []string{fmt.Sprintf("samples=%d", metrics.Samples), fmt.Sprintf("p50_latency_ms=%.0f", metrics.P50LatencyMs), fmt.Sprintf("p95_latency_ms=%.0f", metrics.P95LatencyMs)}, "tradeoff": "候选数下降可能降低重排质量"})
	qualityParams := Parameters{"lexical_top_k": 75, "vector_top_k": 75}
	qualityReason := "暂无近 7 天退化指标，建议扩大检索候选集并用快速验证比较结果摘要"
	if metrics.Samples > 0 {
		qualityReason = fmt.Sprintf("近 7 天平均结果数 %.1f、降级率 %.1f%%，扩大候选集可改善低结果/降级场景", metrics.AvgResultCount, metrics.DegradedRate*100)
	}
	recs = append(recs, map[string]any{"id": "retrieval-coverage", "title": "改善检索覆盖度", "reason": qualityReason, "parameters": qualityParams, "evidence": []string{fmt.Sprintf("avg_result_count=%.1f", metrics.AvgResultCount), fmt.Sprintf("degraded_rate=%.3f", metrics.DegradedRate), fmt.Sprintf("avg_reranker_cost=%.4f", metrics.AvgRerankerCost)}, "tradeoff": "增加候选数可能提高延迟和成本"})
	_ = p
	writeJSON(w, http.StatusOK, map[string]any{"scope": s.Scope, "recommendations": recs, "metrics": metrics, "automatic_publish": false})
}
func (h *Handler) fail(w http.ResponseWriter, e error, op string) {
	if errors.Is(e, ErrConflict) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "版本已变化，请重新读取后提交"})
		return
	}
	if errors.Is(e, ErrInvalidParameter) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": e.Error()})
		return
	}
	if errors.Is(e, sql.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "资源不存在"})
		return
	}
	slog.Error(op, "error", e)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "操作失败"})
}
func (h *Handler) auditLog(r *http.Request, uid, wid, action string, d any) {
	if h.audit == nil {
		return
	}
	b, _ := json.Marshal(d)
	if e := h.audit.Record(r.Context(), uid, wid, action, "search_tuning", wid, b, httpmw.RequestIDFromContext(r.Context())); e != nil {
		slog.Error("写入 tuning 审计失败", "error", e)
	}
}
func decode(r *http.Request, v any) error {
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		return fmt.Errorf("请求体非法: %w", e)
	}
	return nil
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if e := json.NewEncoder(w).Encode(v); e != nil {
		slog.Error("写入 tuning 响应失败", "error", e)
	}
}
func ParameterDefinitions() []map[string]any {
	return []map[string]any{{"key": "title_boost", "type": "float", "min": 0, "max": 20}, {"key": "heading_boost", "type": "float", "min": 0, "max": 20}, {"key": "path_boost", "type": "float", "min": 0, "max": 20}, {"key": "tags_boost", "type": "float", "min": 0, "max": 20}, {"key": "body_boost", "type": "float", "min": 0, "max": 20}, {"key": "lexical_top_k", "type": "int", "min": 1, "max": 500}, {"key": "vector_top_k", "type": "int", "min": 1, "max": 500}, {"key": "rrf_k", "type": "int", "min": 1, "max": 200}, {"key": "reranker_candidate_count", "type": "int", "min": 1, "max": 200}, {"key": "reranker_final_count", "type": "int", "min": 1, "max": 100}, {"key": "max_chunks_per_document", "type": "int", "min": 1, "max": 20}, {"key": "merge_adjacent_chunks", "type": "bool"}, {"key": "max_p95_latency_ms", "type": "int", "min": 1, "max": 60000}, {"key": "max_reranker_cost_per_query", "type": "float", "min": 0, "max": 20}}
}

var _ = workspace.PermSettings
