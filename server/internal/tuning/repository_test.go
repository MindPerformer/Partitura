package tuning

import (
	"testing"

	types "partitura/server/internal/search/types"
)

func TestValidateAcceptsQueryTimeParameters(t *testing.T) {
	if err := Validate(Parameters{"lexical_top_k": 50.0, "title_boost": 2.5, "merge_adjacent_chunks": true}); err != nil {
		t.Fatalf("合法参数被拒绝: %v", err)
	}
}

func TestValidateRejectsIndexAffectingParameters(t *testing.T) {
	if err := Validate(Parameters{"chunk_target_size": 512}); err == nil {
		t.Fatal("索引相关参数应拒绝在线覆盖")
	}
}

func TestValidateRejectsUnknownAndOutOfRangeParameters(t *testing.T) {
	for name, params := range map[string]Parameters{
		"unknown": {"not_allowed": 1},
		"range":   {"rrf_k": 201},
		"type":    {"merge_adjacent_chunks": "true"},
	} {
		if err := Validate(params); err == nil {
			t.Fatalf("%s 参数应被拒绝", name)
		}
	}
}

func TestApplyRuntimeOverrideUpdatesTypedConfig(t *testing.T) {
	cfg := types.SearchProfileConfig{LexicalTopK: 50, TitleBoost: 2}
	if err := applyRuntimeOverride(&cfg, "lexical_top_k", 100.0); err != nil {
		t.Fatalf("应用整数覆盖失败: %v", err)
	}
	if err := applyRuntimeOverride(&cfg, "title_boost", 3.5); err != nil {
		t.Fatalf("应用浮点覆盖失败: %v", err)
	}
	if cfg.LexicalTopK != 100 || cfg.TitleBoost != 3.5 {
		t.Fatalf("typed config 未更新: %+v", cfg)
	}
}
