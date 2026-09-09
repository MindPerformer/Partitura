// repository_test.go 测试 Search Profile repository 的 JSON 契约与空列表行为。
//
// 引入动机：Phase6 WP1 修复 search-profiles 页面崩溃。
// 直接测试 JSON 序列化（不依赖数据库），确保：
//   - ListProfilesResult 空列表序列化为 profiles: []
//   - ProfileRecord 字段为 snake_case
//   - activated_at 未激活时省略
package profile

import (
	"encoding/json"
	"testing"
)

// TestListProfilesResult_EmptyProfilesIsArray 验证空结果 JSON 编码为 profiles: [] 而非 null。
func TestListProfilesResult_EmptyProfilesIsArray(t *testing.T) {
	result := ListProfilesResult{
		Profiles: []ProfileRecord{},
		Total:    0,
	}

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("JSON 序列化失败: %v", err)
	}

	body := string(data)
	if !containsSubseq(body, `"profiles":[]`) && !containsSubseq(body, `"profiles": []`) {
		t.Errorf("空 profiles 应编码为数组，实际 body: %s", body)
	}
}

// TestProfileRecord_SnakeCaseJSON 验证 ProfileRecord 字段名按 snake_case 输出。
func TestProfileRecord_SnakeCaseJSON(t *testing.T) {
	p := ProfileRecord{
		ID:                       "p1",
		Name:                     "default",
		Version:                  1,
		Status:                   "draft",
		EmbeddingProvider:        "openai-compatible",
		EmbeddingModel:           "qwen3-embedding",
		EmbeddingDimensions:      1024,
		EmbeddingQueryInstruction: "query",
		EmbeddingDocInstruction:   "doc",
		ChunkAlgorithmVersion:    "v1",
		ChunkTargetSize:          512,
		ChunkOverlap:             64,
		ChunkParentSectionBehavior: "include",
		TitleBoost:               2.0,
		HeadingBoost:             1.5,
		PathBoost:                1.0,
		TagsBoost:                0.5,
		BodyBoost:                1.0,
		Analyzer:                 "standard",
		LexicalTopK:              50,
		VectorTopK:               50,
		RRFK:                     60,
		RerankerProvider:         "openai-compatible",
		RerankerModel:            "qwen3-reranker",
		RerankerCandidateCount:   20,
		RerankerFinalCount:       10,
		MaxChunksPerDocument:     3,
		MergeAdjacentChunks:      true,
		ESIndexName:              "idx_v1",
		MaxP95LatencyMs:          2000,
		MaxRerankerCostPerQuery:  0.01,
		CreatedBy:                "u1",
		CreatedAt:                "2025-01-01T00:00:00+00:00",
		// activated_at 为空，omitempty 应省略
	}

	data, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("JSON 序列化失败: %v", err)
	}

	body := string(data)

	required := []string{
		"\"embedding_model\"",
		"\"embedding_dimensions\"",
		"\"embedding_query_instruction\"",
		"\"embedding_document_instruction\"",
		"\"chunk_algorithm_version\"",
		"\"chunk_target_size\"",
		"\"chunk_overlap\"",
		"\"chunk_parent_section_behavior\"",
		"\"title_boost\"",
		"\"heading_boost\"",
		"\"path_boost\"",
		"\"tags_boost\"",
		"\"body_boost\"",
		"\"analyzer\"",
		"\"lexical_top_k\"",
		"\"vector_top_k\"",
		"\"rrf_k\"",
		"\"reranker_provider\"",
		"\"reranker_model\"",
		"\"reranker_candidate_count\"",
		"\"reranker_final_count\"",
		"\"max_chunks_per_document\"",
		"\"merge_adjacent_chunks\"",
		"\"es_index_name\"",
		"\"max_p95_latency_ms\"",
		"\"max_reranker_cost_per_query\"",
		"\"created_by\"",
		"\"created_at\"",
	}

	for _, key := range required {
		if !containsSubseq(body, key) {
			t.Errorf("响应中缺少字段 %s，body: %s", key, body)
		}
	}

	// 不应出现 PascalCase
	if containsSubseq(body, "\"EmbeddingModel\"") || containsSubseq(body, "\"LexicalTopK\"") {
		t.Errorf("响应中不应出现 PascalCase 字段，body: %s", body)
	}

	// activated_at 为空应省略
	if containsSubseq(body, "\"activated_at\"") {
		t.Errorf("未激活 profile 不应输出 activated_at，body: %s", body)
	}
}

// TestProfileRecord_ActivatedAtNotEmpty 验证已激活 profile 输出 activated_at。
func TestProfileRecord_ActivatedAtNotEmpty(t *testing.T) {
	p := ProfileRecord{
		ID:          "p1",
		Status:      "active",
		ActivatedAt: "2025-01-02T00:00:00+00:00",
	}

	data, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("JSON 序列化失败: %v", err)
	}

	body := string(data)
	if !containsSubseq(body, "\"activated_at\"") {
		t.Errorf("已激活 profile 应输出 activated_at，body: %s", body)
	}
}

func containsSubseq(s, sub string) bool {
	return len(s) >= len(sub) && jsonContains(s, sub)
}

func jsonContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
