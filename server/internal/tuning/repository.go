// Package tuning 提供 workspace-scoped search profile override/binding 数据访问与解析。
package tuning

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"partitura/server/internal/profile"
	types "partitura/server/internal/search/types"
)

var ErrConflict = errors.New("tuning revision conflict")
var ErrInvalidParameter = errors.New("invalid tuning parameter")

type Parameters map[string]any
type Release struct {
	Revision      int64      `json:"revision"`
	BaseProfileID string     `json:"base_profile_id"`
	Parameters    Parameters `json:"parameters"`
	Action        string     `json:"action"`
	CreatedBy     string     `json:"created_by"`
	CreatedAt     string     `json:"created_at"`
}
type State struct {
	Scope             string     `json:"scope"`
	WorkspaceID       string     `json:"workspace_id,omitempty"`
	Revision          int64      `json:"revision"`
	PublishedRevision int64      `json:"published_revision"`
	Draft             Parameters `json:"draft"`
	Published         Parameters `json:"published"`
	BaseProfileID     string     `json:"base_profile_id"`
	History           []Release  `json:"history"`
}
type Repository interface {
	GetState(context.Context, string) (*State, error)
	SaveDraft(context.Context, string, int64, Parameters, string) (*State, error)
	Publish(context.Context, string, int64, string) (*State, error)
	Rollback(context.Context, string, int64, int64, string) (*State, error)
	Clear(context.Context, string, int64, string) (*State, error)
	Effective(context.Context, string) (*profile.ProfileRecord, Parameters, *State, error)
}
type MetricsSummary struct {
	Samples         int     `json:"samples"`
	P50LatencyMs    float64 `json:"p50_latency_ms"`
	P95LatencyMs    float64 `json:"p95_latency_ms"`
	AvgResultCount  float64 `json:"avg_result_count"`
	DegradedRate    float64 `json:"degraded_rate"`
	AvgRerankerCost float64 `json:"avg_reranker_cost"`
}
type PGRepository struct {
	db       *sql.DB
	profiles profile.Repository
}

func NewPGRepository(db *sql.DB, p profile.Repository) *PGRepository {
	return &PGRepository{db: db, profiles: p}
}

func (r *PGRepository) AggregateMetrics(ctx context.Context, workspaceID string) (*MetricsSummary, error) {
	var s MetricsSummary
	query := `SELECT COUNT(*), COALESCE(percentile_cont(0.50) WITHIN GROUP (ORDER BY latency_ms),0), COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms),0), COALESCE(AVG(result_count),0), COALESCE(AVG(CASE WHEN degraded THEN 1.0 ELSE 0.0 END),0), COALESCE(AVG(reranker_cost),0) FROM search_metrics WHERE created_at >= now() - interval '7 days'`
	args := []any{}
	if workspaceID != "" {
		query += " AND workspace_id = $1"
		args = append(args, workspaceID)
	}
	if err := r.db.QueryRowContext(ctx, query, args...).Scan(&s.Samples, &s.P50LatencyMs, &s.P95LatencyMs, &s.AvgResultCount, &s.DegradedRate, &s.AvgRerankerCost); err != nil {
		return nil, fmt.Errorf("聚合 search metrics: %w", err)
	}
	return &s, nil
}
func scopeKey(wid string) string {
	if wid == "" {
		return "global"
	}
	return wid
}
func (r *PGRepository) ensure(ctx context.Context, key string) error {
	_, e := r.db.ExecContext(ctx, `INSERT INTO search_profile_bindings(scope_key,workspace_id) VALUES($1,NULLIF($2,'' )::uuid) ON CONFLICT(scope_key) DO NOTHING`, key, func() string {
		if key == "global" {
			return ""
		}
		return key
	}())
	return e
}
func decodeParams(b []byte) (Parameters, error) {
	if len(b) == 0 {
		return nil, nil
	}
	var p Parameters
	if e := json.Unmarshal(b, &p); e != nil {
		return nil, e
	}
	return p, nil
}
func (r *PGRepository) GetState(ctx context.Context, wid string) (*State, error) {
	key := scopeKey(wid)
	if e := r.ensure(ctx, key); e != nil {
		return nil, fmt.Errorf("初始化 tuning scope: %w", e)
	}
	var s State
	var draft []byte
	var pubID sql.NullInt64
	var ws sql.NullString
	e := r.db.QueryRowContext(ctx, `SELECT scope_key,workspace_id,revision,published_revision,draft_overrides FROM search_profile_bindings WHERE scope_key=$1`, key).Scan(&s.Scope, &ws, &s.Revision, &pubID, &draft)
	if e != nil {
		return nil, fmt.Errorf("读取 tuning scope: %w", e)
	}
	s.WorkspaceID = ws.String
	if pubID.Valid {
		s.PublishedRevision = pubID.Int64
	}
	s.Draft, e = decodeParams(draft)
	if e != nil {
		return nil, e
	}
	rows, e := r.db.QueryContext(ctx, `SELECT revision,base_profile_id::text,parameters,action,created_by::text,to_char(created_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS"Z"') FROM search_profile_overrides WHERE scope_key=$1 ORDER BY revision DESC LIMIT 20`, key)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	for rows.Next() {
		var x Release
		var b []byte
		if e = rows.Scan(&x.Revision, &x.BaseProfileID, &b, &x.Action, &x.CreatedBy, &x.CreatedAt); e != nil {
			return nil, e
		}
		x.Parameters, e = decodeParams(b)
		if e != nil {
			return nil, e
		}
		s.History = append(s.History, x)
		if x.Revision == s.PublishedRevision {
			s.Published = x.Parameters
			s.BaseProfileID = x.BaseProfileID
		}
	}
	if s.Draft == nil {
		s.Draft = Parameters{}
	}
	if s.Published == nil {
		s.Published = Parameters{}
	}
	if s.History == nil {
		s.History = []Release{}
	}
	return &s, rows.Err()
}
func (r *PGRepository) SaveDraft(ctx context.Context, wid string, expected int64, p Parameters, user string) (*State, error) {
	if e := Validate(p); e != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidParameter, e)
	}
	b, _ := json.Marshal(p)
	key := scopeKey(wid)
	res, e := r.db.ExecContext(ctx, `UPDATE search_profile_bindings SET draft_overrides=$1,revision=revision+1,updated_by=$2,updated_at=now() WHERE scope_key=$3 AND revision=$4`, b, user, key, expected)
	if e != nil {
		return nil, e
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return nil, ErrConflict
	}
	return r.GetState(ctx, wid)
}
func (r *PGRepository) Publish(ctx context.Context, wid string, expected int64, user string) (*State, error) {
	key := scopeKey(wid)
	tx, e := r.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	var rev int64
	var draft []byte
	e = tx.QueryRowContext(ctx, `SELECT revision,draft_overrides FROM search_profile_bindings WHERE scope_key=$1 AND revision=$2 FOR UPDATE`, key, expected).Scan(&rev, &draft)
	if e != nil {
		return nil, func() error {
			if e == sql.ErrNoRows {
				return ErrConflict
			}
			return e
		}()
	}
	if len(draft) == 0 {
		draft = []byte("{}")
	}
	var base string
	e = tx.QueryRowContext(ctx, `SELECT id::text FROM search_profiles WHERE status='active' ORDER BY activated_at DESC NULLS LAST LIMIT 1`).Scan(&base)
	if e != nil {
		return nil, e
	}
	var newRev int64
	e = tx.QueryRowContext(ctx, `INSERT INTO search_profile_overrides(scope_key,revision,base_profile_id,parameters,action,created_by) VALUES($1,$2,$3,$4,'publish',$5) RETURNING revision`, key, rev+1, base, draft, user).Scan(&newRev)
	if e != nil {
		return nil, e
	}
	if _, e = tx.ExecContext(ctx, `UPDATE search_profile_bindings SET published_revision=$1,draft_overrides=NULL,revision=revision+1,updated_by=$2,updated_at=now() WHERE scope_key=$3`, newRev, user, key); e != nil {
		return nil, e
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	return r.GetState(ctx, wid)
}
func (r *PGRepository) Rollback(ctx context.Context, wid string, expected, target int64, user string) (*State, error) {
	return r.restore(ctx, wid, expected, target, user, "rollback")
}
func (r *PGRepository) Clear(ctx context.Context, wid string, expected int64, user string) (*State, error) {
	return r.restore(ctx, wid, expected, 0, user, "clear")
}
func (r *PGRepository) restore(ctx context.Context, wid string, expected, target int64, user, action string) (*State, error) {
	key := scopeKey(wid)
	tx, e := r.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	var current int64
	e = tx.QueryRowContext(ctx, `SELECT revision FROM search_profile_bindings WHERE scope_key=$1 AND revision=$2 FOR UPDATE`, key, expected).Scan(&current)
	if e != nil {
		return nil, ErrConflict
	}
	var base string
	params := []byte("{}")
	if target > 0 {
		e = tx.QueryRowContext(ctx, `SELECT base_profile_id::text,parameters FROM search_profile_overrides WHERE scope_key=$1 AND revision=$2`, key, target).Scan(&base, &params)
		if e != nil {
			return nil, e
		}
	} else {
		e = tx.QueryRowContext(ctx, `SELECT id::text FROM search_profiles WHERE status='active' ORDER BY activated_at DESC NULLS LAST LIMIT 1`).Scan(&base)
		if e != nil {
			return nil, e
		}
	}
	var nr int64
	e = tx.QueryRowContext(ctx, `INSERT INTO search_profile_overrides(scope_key,revision,base_profile_id,parameters,action,created_by) VALUES($1,$2,$3,$4,$5,$6) RETURNING revision`, key, current+1, base, params, action, user).Scan(&nr)
	if e != nil {
		return nil, e
	}
	_, e = tx.ExecContext(ctx, `UPDATE search_profile_bindings SET published_revision=$1,draft_overrides=NULL,revision=revision+1,updated_by=$2,updated_at=now() WHERE scope_key=$3`, nr, user, key)
	if e != nil {
		return nil, e
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	return r.GetState(ctx, wid)
}
func (r *PGRepository) ResolveProfile(ctx context.Context, wid string) (*profile.ProfileRecord, types.SearchProfileConfig, error) {
	base, overrides, _, err := r.Effective(ctx, wid)
	if err != nil {
		return nil, types.SearchProfileConfig{}, err
	}
	cfg := base.ToConfig()
	// ProfileRecord JSON keys and SearchProfileConfig field names differ, so apply
	// only the explicitly supported override keys to the typed runtime config.
	for k, v := range overrides {
		if err := applyRuntimeOverride(&cfg, k, v); err != nil {
			return nil, types.SearchProfileConfig{}, err
		}
	}
	return base, cfg, nil
}

func applyRuntimeOverride(c *types.SearchProfileConfig, key string, v any) error {
	n, ok := number(v)
	switch key {
	case "title_boost":
		if !ok {
			return fmt.Errorf("%s 必须为数字", key)
		}
		c.TitleBoost = float32(n)
	case "heading_boost":
		if !ok {
			return fmt.Errorf("%s 必须为数字", key)
		}
		c.HeadingBoost = float32(n)
	case "path_boost":
		if !ok {
			return fmt.Errorf("%s 必须为数字", key)
		}
		c.PathBoost = float32(n)
	case "tags_boost":
		if !ok {
			return fmt.Errorf("%s 必须为数字", key)
		}
		c.TagsBoost = float32(n)
	case "body_boost":
		if !ok {
			return fmt.Errorf("%s 必须为数字", key)
		}
		c.BodyBoost = float32(n)
	case "lexical_top_k":
		if !ok {
			return fmt.Errorf("%s 必须为数字", key)
		}
		c.LexicalTopK = int(n)
	case "vector_top_k":
		if !ok {
			return fmt.Errorf("%s 必须为数字", key)
		}
		c.VectorTopK = int(n)
	case "rrf_k":
		if !ok {
			return fmt.Errorf("%s 必须为数字", key)
		}
		c.RRFK = int(n)
	case "reranker_candidate_count":
		if !ok {
			return fmt.Errorf("%s 必须为数字", key)
		}
		c.RerankerCandidateCount = int(n)
	case "reranker_final_count":
		if !ok {
			return fmt.Errorf("%s 必须为数字", key)
		}
		c.RerankerFinalCount = int(n)
	case "max_chunks_per_document":
		if !ok {
			return fmt.Errorf("%s 必须为数字", key)
		}
		c.MaxChunksPerDocument = int(n)
	case "merge_adjacent_chunks":
		b, ok := v.(bool)
		if !ok {
			return fmt.Errorf("%s 必须为 bool", key)
		}
		c.MergeAdjacentChunks = b
	case "max_p95_latency_ms":
		if !ok {
			return fmt.Errorf("%s 必须为数字", key)
		}
		c.MaxP95LatencyMs = int(n)
	case "max_reranker_cost_per_query":
		if !ok {
			return fmt.Errorf("%s 必须为数字", key)
		}
		c.MaxRerankerCostPerQuery = float32(n)
	default:
		return fmt.Errorf("不允许覆盖字段 %s", key)
	}
	return nil
}

func (r *PGRepository) Effective(ctx context.Context, wid string) (*profile.ProfileRecord, Parameters, *State, error) {
	base, e := r.profiles.GetActiveProfile(ctx)
	if e != nil {
		return nil, nil, nil, e
	}
	g, e := r.GetState(ctx, "")
	if e != nil {
		return nil, nil, nil, e
	}
	merged := Parameters{}
	for k, v := range g.Published {
		merged[k] = v
	}
	if wid != "" {
		s, e := r.GetState(ctx, wid)
		if e != nil {
			return nil, nil, nil, e
		}
		if s.BaseProfileID != "" {
			base, e = r.profiles.GetProfileByID(ctx, s.BaseProfileID)
			if e != nil {
				return nil, nil, nil, e
			}
		}
		for k, v := range s.Published {
			merged[k] = v
		}
		return base, merged, s, nil
	}
	return base, merged, g, nil
}
func Validate(p Parameters) error {
	for k, v := range p {
		switch k {
		case "merge_adjacent_chunks":
			if _, ok := v.(bool); !ok {
				return fmt.Errorf("%s 必须为 bool", k)
			}
		case "title_boost", "heading_boost", "path_boost", "tags_boost", "body_boost", "max_reranker_cost_per_query":
			n, ok := number(v)
			if !ok || n < 0 || n > 20 {
				return fmt.Errorf("%s 必须在 0..20", k)
			}
		case "lexical_top_k", "vector_top_k":
			n, ok := number(v)
			if !ok || n < 1 || n > 500 {
				return fmt.Errorf("%s 必须在 1..500", k)
			}
		case "rrf_k":
			n, ok := number(v)
			if !ok || n < 1 || n > 200 {
				return fmt.Errorf("%s 必须在 1..200", k)
			}
		case "reranker_candidate_count":
			n, ok := number(v)
			if !ok || n < 1 || n > 200 {
				return fmt.Errorf("%s 必须在 1..200", k)
			}
		case "reranker_final_count":
			n, ok := number(v)
			if !ok || n < 1 || n > 100 {
				return fmt.Errorf("%s 必须在 1..100", k)
			}
		case "max_chunks_per_document":
			n, ok := number(v)
			if !ok || n < 1 || n > 20 {
				return fmt.Errorf("%s 必须在 1..20", k)
			}
		case "chunk_target_size", "chunk_overlap", "max_p95_latency_ms":
			return fmt.Errorf("%s 属于索引/重建参数，不能在线覆盖", k)
		default:
			return fmt.Errorf("不允许覆盖字段 %s", k)
		}
	}
	return nil
}
func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case json.Number:
		f, e := n.Float64()
		return f, e == nil
	}
	return 0, false
}

var _ = time.Now
