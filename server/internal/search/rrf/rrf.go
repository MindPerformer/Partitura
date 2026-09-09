// Package rrf 实现 Reciprocal Rank Fusion (RRF) 分数融合。
//
// 引入动机：design/01-SEARCH.md 搜索管线要求：
//   query → BM25 multi-field + dense vector → RRF → top candidates
//
// RRF 公式：score(d) = Σ 1/(k + rank_i(d))
// 其中 k 是 RRF 常数（默认 60），rank_i(d) 是文档 d 在第 i 个结果列表中的排名。
//
// 设计原则：
//   - 纯函数实现，不依赖外部状态
//   - 输入是多个已排序的结果列表，输出是融合后的排序结果
//   - 不写死 k 值，由调用方传入
package rrf

import (
	"sort"

	types "partitura/server/internal/search/types"
)

// Fuse 将 BM25 和向量检索结果按 RRF 公式融合。
//
// 引入动机：design/01-SEARCH.md 要求 BM25 + vector → RRF → top candidates。
//
// 参数：
//   - bm25Results：BM25 检索结果列表（按分数降序）
//   - vectorResults：向量检索结果列表（按分数降序）
//   - k：RRF 常数（通常为 60）
//   - topK：返回的候选数量上限
//
// 返回融合后的候选结果列表（按 RRF 分数降序），最多 topK 条。
// 同一个 chunk 在 BM25 和 vector 结果中都出现时，两个分数都计入 RRF。
func Fuse(bm25Results, vectorResults []types.CandidateResult, k int, topK int) []types.CandidateResult {
	if k <= 0 {
		k = 60
	}
	if topK <= 0 {
		topK = 50
	}

	// 用 chunk 的 document_id + chunk_index 作为唯一键
	// 同一个 chunk 可能同时出现在 BM25 和 vector 结果中
	scoreMap := make(map[string]*types.CandidateResult)

	// 处理 BM25 结果
	for i, cand := range bm25Results {
		key := chunkKey(cand.Chunk)
		rank := i + 1
		rrfScore := 1.0 / float64(k+rank)

		if existing, ok := scoreMap[key]; ok {
			existing.RRFScore += rrfScore
			existing.BM25Score = cand.BM25Score
			existing.BM25Rank = rank
		} else {
			cand.RRFScore = rrfScore
			cand.BM25Rank = rank
			scoreMap[key] = &cand
		}
	}

	// 处理 vector 结果
	for i, cand := range vectorResults {
		key := chunkKey(cand.Chunk)
		rank := i + 1
		rrfScore := 1.0 / float64(k+rank)

		if existing, ok := scoreMap[key]; ok {
			existing.RRFScore += rrfScore
			existing.VectorScore = cand.VectorScore
			existing.VectorRank = rank
		} else {
			cand.RRFScore = rrfScore
			cand.VectorRank = rank
			scoreMap[key] = &cand
		}
	}

	// 收集并按 RRF 分数降序排序
	results := make([]types.CandidateResult, 0, len(scoreMap))
	for _, cand := range scoreMap {
		results = append(results, *cand)
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].RRFScore > results[j].RRFScore
	})

	// 截取 topK
	if len(results) > topK {
		results = results[:topK]
	}

	return results
}

// chunkKey 生成 chunk 的唯一键。
// 引入动机：同一个 chunk 在 BM25 和 vector 结果中需要被识别为同一个文档。
func chunkKey(c types.Chunk) string {
	return c.DocumentID + ":" + c.Path + ":" + itoa(c.ChunkIndex)
}

// itoa 简单整数转字符串，避免引入 strconv。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
