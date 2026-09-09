// diversity_test.go 测试文档多样性逻辑。
package diversity

import (
	"testing"

	types "partitura/server/internal/search/types"
)

func TestApply_LimitChunksPerDocument(t *testing.T) {
	results := []types.SearchResult{
		{DocumentID: "d1", Score: 1.0, StartLine: 1, EndLine: 10},
		{DocumentID: "d1", Score: 0.9, StartLine: 11, EndLine: 20},
		{DocumentID: "d1", Score: 0.8, StartLine: 21, EndLine: 30},
		{DocumentID: "d1", Score: 0.7, StartLine: 31, EndLine: 40},
		{DocumentID: "d2", Score: 0.6, StartLine: 1, EndLine: 10},
	}

	filtered := Apply(results, 2, false)
	if len(filtered) != 3 {
		t.Fatalf("max=2 应保留 3 个结果（d1×2 + d2×1），得到 %d", len(filtered))
	}

	d1Count := 0
	d2Count := 0
	for _, r := range filtered {
		if r.DocumentID == "d1" {
			d1Count++
		}
		if r.DocumentID == "d2" {
			d2Count++
		}
	}
	if d1Count != 2 {
		t.Errorf("d1 应保留 2 个 chunk，得到 %d", d1Count)
	}
	if d2Count != 1 {
		t.Errorf("d2 应保留 1 个 chunk，得到 %d", d2Count)
	}
}

func TestApply_MergeAdjacentChunks(t *testing.T) {
	results := []types.SearchResult{
		{DocumentID: "d1", Score: 1.0, StartLine: 1, EndLine: 10, SectionPath: []string{"A"}},
		{DocumentID: "d1", Score: 0.9, StartLine: 11, EndLine: 20, SectionPath: []string{"A", "B"}},
		{DocumentID: "d2", Score: 0.8, StartLine: 1, EndLine: 10},
	}

	merged := Apply(results, 3, true)
	// d1 的两个相邻 chunk（1-10, 11-20）应合并为一个（1-20）
	d1Count := 0
	for _, r := range merged {
		if r.DocumentID == "d1" {
			d1Count++
			if r.StartLine != 1 || r.EndLine != 20 {
				t.Errorf("合并后的 d1 chunk 应为 1-20，得到 %d-%d", r.StartLine, r.EndLine)
			}
		}
	}
	if d1Count != 1 {
		t.Errorf("d1 合并后应有 1 个 chunk，得到 %d", d1Count)
	}
}

func TestApply_EmptyResults(t *testing.T) {
	filtered := Apply(nil, 3, true)
	if len(filtered) != 0 {
		t.Errorf("空输入应返回 0 个结果")
	}
}

func TestApply_RankReassignment(t *testing.T) {
	results := []types.SearchResult{
		{DocumentID: "d1", Score: 1.0, StartLine: 1, EndLine: 10},
		{DocumentID: "d2", Score: 0.9, StartLine: 1, EndLine: 10},
		{DocumentID: "d3", Score: 0.8, StartLine: 1, EndLine: 10},
	}

	filtered := Apply(results, 1, false)
	for i, r := range filtered {
		if r.Rank != i+1 {
			t.Errorf("结果 %d 的 rank 应为 %d，得到 %d", i, i+1, r.Rank)
		}
	}
}
