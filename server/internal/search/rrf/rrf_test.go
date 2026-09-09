// rrf_test.go 测试 RRF 融合逻辑。
package rrf

import (
	"testing"

	types "partitura/server/internal/search/types"
)

func TestFuse_BM25Only(t *testing.T) {
	bm25Results := []types.CandidateResult{
		{Chunk: types.Chunk{DocumentID: "d1", Path: "a.md", ChunkIndex: 0}, BM25Score: 10.0},
		{Chunk: types.Chunk{DocumentID: "d2", Path: "b.md", ChunkIndex: 0}, BM25Score: 8.0},
	}

	results := Fuse(bm25Results, nil, 60, 50)
	if len(results) != 2 {
		t.Fatalf("应有 2 个结果，得到 %d", len(results))
	}
	if results[0].Chunk.DocumentID != "d1" {
		t.Errorf("第一个结果应为 d1，得到 %s", results[0].Chunk.DocumentID)
	}
	if results[0].BM25Rank != 1 {
		t.Errorf("d1 的 BM25 rank 应为 1")
	}
}

func TestFuse_VectorOnly(t *testing.T) {
	vectorResults := []types.CandidateResult{
		{Chunk: types.Chunk{DocumentID: "d1", Path: "a.md", ChunkIndex: 0}, VectorScore: 0.9},
		{Chunk: types.Chunk{DocumentID: "d3", Path: "c.md", ChunkIndex: 0}, VectorScore: 0.8},
	}

	results := Fuse(nil, vectorResults, 60, 50)
	if len(results) != 2 {
		t.Fatalf("应有 2 个结果，得到 %d", len(results))
	}
	if results[0].Chunk.DocumentID != "d1" {
		t.Errorf("第一个结果应为 d1，得到 %s", results[0].Chunk.DocumentID)
	}
}

func TestFuse_BothSources(t *testing.T) {
	bm25Results := []types.CandidateResult{
		{Chunk: types.Chunk{DocumentID: "d1", Path: "a.md", ChunkIndex: 0}, BM25Score: 10.0},
		{Chunk: types.Chunk{DocumentID: "d2", Path: "b.md", ChunkIndex: 0}, BM25Score: 8.0},
	}
	vectorResults := []types.CandidateResult{
		{Chunk: types.Chunk{DocumentID: "d1", Path: "a.md", ChunkIndex: 0}, VectorScore: 0.9},
		{Chunk: types.Chunk{DocumentID: "d3", Path: "c.md", ChunkIndex: 0}, VectorScore: 0.8},
	}

	results := Fuse(bm25Results, vectorResults, 60, 50)
	if len(results) != 3 {
		t.Fatalf("应有 3 个唯一结果，得到 %d", len(results))
	}

	// d1 同时出现在 BM25 和 vector 中，RRF 分数应最高
	if results[0].Chunk.DocumentID != "d1" {
		t.Errorf("d1 应排第一（RRF 分数最高），得到 %s", results[0].Chunk.DocumentID)
	}
	if results[0].BM25Rank != 1 {
		t.Errorf("d1 的 BM25 rank 应为 1")
	}
	if results[0].VectorRank != 1 {
		t.Errorf("d1 的 vector rank 应为 1")
	}
	// d1 的 RRF 分数 = 1/(60+1) + 1/(60+1) = 2/61
	expectedScore := 2.0 / 61.0
	if abs(results[0].RRFScore-expectedScore) > 0.0001 {
		t.Errorf("d1 的 RRF 分数应为 %f，得到 %f", expectedScore, results[0].RRFScore)
	}
}

func TestFuse_TopKLimit(t *testing.T) {
	bm25Results := make([]types.CandidateResult, 10)
	for i := range bm25Results {
		bm25Results[i] = types.CandidateResult{
			Chunk: types.Chunk{DocumentID: "d" + itoa(i), Path: "a.md", ChunkIndex: i},
		}
	}

	results := Fuse(bm25Results, nil, 60, 5)
	if len(results) != 5 {
		t.Fatalf("topK=5 应返回 5 个结果，得到 %d", len(results))
	}
}

func TestFuse_EmptyInputs(t *testing.T) {
	results := Fuse(nil, nil, 60, 50)
	if len(results) != 0 {
		t.Errorf("空输入应返回 0 个结果，得到 %d", len(results))
	}
}

func TestFuse_SameChunkDifferentIndex(t *testing.T) {
	// 同一文档的不同 chunk 应被视为不同结果
	bm25Results := []types.CandidateResult{
		{Chunk: types.Chunk{DocumentID: "d1", Path: "a.md", ChunkIndex: 0}},
		{Chunk: types.Chunk{DocumentID: "d1", Path: "a.md", ChunkIndex: 1}},
	}

	results := Fuse(bm25Results, nil, 60, 50)
	if len(results) != 2 {
		t.Fatalf("同一文档的不同 chunk 应为 2 个结果，得到 %d", len(results))
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
