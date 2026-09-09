// Package diversity 实现搜索结果的文档多样性处理。
//
// 引入动机：design/01-SEARCH.md §Document Diversity 要求：
//   - 避免 Top 10 都来自同一文档
//   - Rerank 后：group by document, 限制单文档 chunk 数, 合并相邻 chunk, 保留多个不同文档
//
// 设计原则：
//   - 纯函数实现，不依赖外部状态
//   - 输入是已排序的候选结果列表，输出是去重后的结果列表
package diversity

import (
	"sort"

	types "partitura/server/internal/search/types"
)

// Apply 对搜索结果应用文档多样性限制。
//
// 引入动机：design/01-SEARCH.md §Document Diversity 要求避免 Top N 都来自同一文档。
//
// 参数：
//   - results：已按分数降序排列的搜索结果
//   - maxChunksPerDocument：同一文档最多保留的 chunk 数
//   - mergeAdjacent：是否合并来自同一文档的相邻 chunk
//
// 返回经过多样性处理后的结果列表。
func Apply(results []types.SearchResult, maxChunksPerDocument int, mergeAdjacent bool) []types.SearchResult {
	if len(results) == 0 {
		return results
	}
	if maxChunksPerDocument <= 0 {
		maxChunksPerDocument = 3
	}

	// 按文档分组计数
	docCount := make(map[string]int)
	var filtered []types.SearchResult

	for _, r := range results {
		if docCount[r.DocumentID] < maxChunksPerDocument {
			filtered = append(filtered, r)
			docCount[r.DocumentID]++
		}
	}

	if mergeAdjacent {
		filtered = mergeAdjacentChunks(filtered)
	}

	// 重新分配 rank
	for i := range filtered {
		filtered[i].Rank = i + 1
	}

	return filtered
}

// mergeAdjacentChunks 合并来自同一文档且行号相邻的 chunk。
// 引入动机：design/01-SEARCH.md §Document Diversity 要求合并相邻 chunk。
// 合并后 snippet 取第一个 chunk 的 snippet，start_line 取最小值，end_line 取最大值。
func mergeAdjacentChunks(results []types.SearchResult) []types.SearchResult {
	if len(results) <= 1 {
		return results
	}

	// 先按文档分组，再按行号排序
	docChunks := make(map[string][]types.SearchResult)
	docOrder := []string{} // 保持文档首次出现的顺序

	for _, r := range results {
		if _, exists := docChunks[r.DocumentID]; !exists {
			docOrder = append(docOrder, r.DocumentID)
		}
		docChunks[r.DocumentID] = append(docChunks[r.DocumentID], r)
	}

	var merged []types.SearchResult

	for _, docID := range docOrder {
		chunks := docChunks[docID]
		// 按 start_line 排序
		sort.Slice(chunks, func(i, j int) bool {
			return chunks[i].StartLine < chunks[j].StartLine
		})

		// 合并相邻的 chunk（end_line + 1 >= next start_line）
		i := 0
		for i < len(chunks) {
			current := chunks[i]
			j := i + 1
			for j < len(chunks) && chunks[j].StartLine <= current.EndLine+1 {
				// 合并 j 到 current
				if chunks[j].EndLine > current.EndLine {
					current.EndLine = chunks[j].EndLine
				}
				// 合并 section_path（取较长的）
				if len(chunks[j].SectionPath) > len(current.SectionPath) {
					current.SectionPath = chunks[j].SectionPath
				}
				j++
			}
			merged = append(merged, current)
			i = j
		}
	}

	// 按原始分数降序排序
	sort.Slice(merged, func(i, j int) bool {
		return merged[i].Score > merged[j].Score
	})

	return merged
}
