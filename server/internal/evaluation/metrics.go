// Package evaluation 实现搜索质量评测指标计算和 regression gate。
//
// 引入动机：design/01-SEARCH.md §Evaluation 要求：
//   - NDCG@10
//   - Recall@5
//   - Recall@10
//   - MRR
//   - Precision@10
//   - P50/P95 latency
//   - reranker cost
//
// §Regression Gate 要求 candidate 激活必须同时满足：
//   - overall quality 有明确提升
//   - Recall@10 不下降
//   - 任一关键 query class 不出现明显退化
//   - P95 latency 不超过阈值
//   - 成本不超过管理员预算
//   - index integrity 正常
package evaluation

import (
	"math"
	"sort"

	types "partitura/server/internal/search/types"
)

// ExpectedDocument 表示评测数据集中一个期望/相关的文档。
type ExpectedDocument struct {
	DocumentID string
	Path       string
	Grade      int // 0..3
}

// EvaluationItem 是一条评测数据。
type EvaluationItem struct {
	Query             string
	QueryClass        string
	ExpectedDocuments []ExpectedDocument
}

// EvaluationResult 是单次评测的指标结果。
type EvaluationResult struct {
	NDCG10       float64
	Recall5      float64
	Recall10     float64
	MRR          float64
	Precision10  float64
	P50LatencyMs float64
	P95LatencyMs float64
	RerankerCost  float64
	RerankerUsed  bool
	// 按 query class 分组的指标
	ClassMetrics map[string]ClassMetric
}

// ClassMetric 是单个 query class 的指标。
type ClassMetric struct {
	NDCG10   float64
	Recall10 float64
	MRR      float64
	Count    int
}

// QueryResult 是单条 query 的搜索结果和延迟。
type QueryResult struct {
	QueryClass  string
	LatencyMs   int
	Results     []types.SearchResult
	RerankerUsed bool
	RerankerCost float64
}

// ComputeMetrics 计算评测指标。
//
// 引入动机：design/01-SEARCH.md §Evaluation 要求计算 NDCG@10, Recall@5/10, MRR, Precision@10,
// P50/P95 latency, reranker cost。
//
// 参数：
//   - items：评测数据集
//   - queryResults：每条 query 的搜索结果（按 items 顺序对应）
//
// 返回 EvaluationResult。
func ComputeMetrics(items []EvaluationItem, queryResults []QueryResult) EvaluationResult {
	if len(items) == 0 || len(queryResults) == 0 {
		return EvaluationResult{ClassMetrics: make(map[string]ClassMetric)}
	}

	totalNDCG := 0.0
	totalRecall5 := 0.0
	totalRecall10 := 0.0
	totalMRR := 0.0
	totalPrecision10 := 0.0
	totalRerankerCost := 0.0
	rerankerUsedCount := 0

	latencies := make([]int, 0, len(queryResults))
	classMetrics := make(map[string]ClassMetric)
	classResults := make(map[string][]float64) // NDCG per class
	classRecall10 := make(map[string][]float64)
	classMRR := make(map[string][]float64)

	for i, item := range items {
		if i >= len(queryResults) {
			break
		}
		qr := queryResults[i]

		// 构建期望文档集合
		expectedMap := make(map[string]int) // document_id -> grade
		for _, ed := range item.ExpectedDocuments {
			expectedMap[ed.DocumentID] = ed.Grade
		}

		// 计算单个 query 的指标
		ndcg := computeNDCG(qr.Results, expectedMap, 10)
		recall5 := computeRecall(qr.Results, expectedMap, 5)
		recall10 := computeRecall(qr.Results, expectedMap, 10)
		mrr := computeMRR(qr.Results, expectedMap)
		precision10 := computePrecision(qr.Results, expectedMap, 10)

		totalNDCG += ndcg
		totalRecall5 += recall5
		totalRecall10 += recall10
		totalMRR += mrr
		totalPrecision10 += precision10
		totalRerankerCost += qr.RerankerCost
		if qr.RerankerUsed {
			rerankerUsedCount++
		}

		latencies = append(latencies, qr.LatencyMs)

		// 按 class 分组
		classResults[item.QueryClass] = append(classResults[item.QueryClass], ndcg)
		classRecall10[item.QueryClass] = append(classRecall10[item.QueryClass], recall10)
		classMRR[item.QueryClass] = append(classMRR[item.QueryClass], mrr)
	}

	n := float64(len(items))

	// 计算百分位延迟
	p50, p95 := computePercentiles(latencies)

	// 按 class 聚合
	for class, ndcgs := range classResults {
		recalls := classRecall10[class]
		mrrs := classMRR[class]
		cm := ClassMetric{Count: len(ndcgs)}
		cm.NDCG10 = avg(ndcgs)
		cm.Recall10 = avg(recalls)
		cm.MRR = avg(mrrs)
		classMetrics[class] = cm
	}

	return EvaluationResult{
		NDCG10:       totalNDCG / n,
		Recall5:      totalRecall5 / n,
		Recall10:     totalRecall10 / n,
		MRR:          totalMRR / n,
		Precision10:  totalPrecision10 / n,
		P50LatencyMs: float64(p50),
		P95LatencyMs: float64(p95),
		RerankerCost: totalRerankerCost / n,
		RerankerUsed: rerankerUsedCount > 0,
		ClassMetrics: classMetrics,
	}
}

// computeNDCG 计算 NDCG@K。
// 引入动机：design/01-SEARCH.md §Evaluation 要求 NDCG@10。
// DCG = Σ grade_i / log2(i+2), IDCG = 理想排序的 DCG, NDCG = DCG/IDCG。
func computeNDCG(results []types.SearchResult, expected map[string]int, k int) float64 {
	if len(results) == 0 || len(expected) == 0 {
		return 0.0
	}

	if k > len(results) {
		k = len(results)
	}

	// DCG
	dcg := 0.0
	for i := 0; i < k; i++ {
		grade, ok := expected[results[i].DocumentID]
		if !ok {
			continue
		}
		dcg += float64(grade) / math.Log2(float64(i+2))
	}

	// IDCG：理想排序
	grades := make([]int, 0, len(expected))
	for _, g := range expected {
		grades = append(grades, g)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(grades)))

	idcg := 0.0
	for i := 0; i < len(grades) && i < k; i++ {
		idcg += float64(grades[i]) / math.Log2(float64(i+2))
	}

	if idcg == 0 {
		return 0.0
	}

	return dcg / idcg
}

// computeRecall 计算 Recall@K。
// 引入动机：design/01-SEARCH.md §Evaluation 要求 Recall@5, Recall@10。
// Recall@K = |relevant ∩ topK| / |relevant|
func computeRecall(results []types.SearchResult, expected map[string]int, k int) float64 {
	if len(expected) == 0 {
		return 0.0
	}

	if k > len(results) {
		k = len(results)
	}

	relevantInTopK := 0
	for i := 0; i < k; i++ {
		if grade, ok := expected[results[i].DocumentID]; ok && grade > 0 {
			relevantInTopK++
		}
	}

	totalRelevant := 0
	for _, g := range expected {
		if g > 0 {
			totalRelevant++
		}
	}

	if totalRelevant == 0 {
		return 0.0
	}

	return float64(relevantInTopK) / float64(totalRelevant)
}

// computeMRR 计算 Mean Reciprocal Rank。
// 引入动机：design/01-SEARCH.md §Evaluation 要求 MRR。
// MRR = 1/rank of first relevant result
func computeMRR(results []types.SearchResult, expected map[string]int) float64 {
	for i, r := range results {
		if grade, ok := expected[r.DocumentID]; ok && grade > 0 {
			return 1.0 / float64(i+1)
		}
	}
	return 0.0
}

// computePrecision 计算 Precision@K。
// 引入动机：design/01-SEARCH.md §Evaluation 要求 Precision@10。
// Precision@K = |relevant ∩ topK| / K
func computePrecision(results []types.SearchResult, expected map[string]int, k int) float64 {
	if k == 0 {
		return 0.0
	}

	if k > len(results) {
		k = len(results)
	}
	if k == 0 {
		return 0.0
	}

	relevantInTopK := 0
	for i := 0; i < k; i++ {
		if grade, ok := expected[results[i].DocumentID]; ok && grade > 0 {
			relevantInTopK++
		}
	}

	return float64(relevantInTopK) / float64(k)
}

// computePercentiles 计算延迟的 P50 和 P95。
// 引入动机：design/01-SEARCH.md §Evaluation 要求 P50/P95 latency。
// 使用 nearest-rank 方法：百分位值 = sorted[ceil(percentile * N) - 1]。
func computePercentiles(values []int) (p50, p95 int) {
	if len(values) == 0 {
		return 0, 0
	}

	sorted := make([]int, len(values))
	copy(sorted, values)
	sort.Ints(sorted)

	p50Idx := int(math.Ceil(0.50*float64(len(sorted)))) - 1
	if p50Idx < 0 {
		p50Idx = 0
	}
	p95Idx := int(math.Ceil(0.95*float64(len(sorted)))) - 1
	if p95Idx < 0 {
		p95Idx = 0
	}
	if p95Idx >= len(sorted) {
		p95Idx = len(sorted) - 1
	}

	return sorted[p50Idx], sorted[p95Idx]
}

// avg 计算平均值。
func avg(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}

// RegressionGateInput 是 regression gate 的输入参数。
type RegressionGateInput struct {
	// Candidate 是候选 profile 的评测结果
	Candidate EvaluationResult
	// Baseline 是当前 active profile 的评测结果
	Baseline EvaluationResult
	// MaxP95LatencyMs 是 P95 延迟阈值
	MaxP95LatencyMs float64
	// MaxRerankerCostPerQuery 是每条查询的 reranker 成本预算
	MaxRerankerCostPerQuery float64
	// IndexIntegrityOK 索引完整性是否正常
	IndexIntegrityOK bool
	// CriticalQueryClasses 关键 query class 列表（不允许退化）
	CriticalQueryClasses []string
}

// RegressionGateResult 是 regression gate 的判定结果。
type RegressionGateResult struct {
	Passed  bool
	Reasons []string
}

// CheckRegressionGate 验证候选 profile 是否满足 regression gate 条件。
//
// 引入动机：design/01-SEARCH.md §Regression Gate 要求 candidate 激活必须同时满足：
//   - overall quality 有明确提升（NDCG@10 提升）
//   - Recall@10 不下降
//   - 任一关键 query class 不出现明显退化
//   - P95 latency 不超过阈值
//   - 成本不超过管理员预算
//   - index integrity 正常
func CheckRegressionGate(input RegressionGateInput) RegressionGateResult {
	var reasons []string
	passed := true

	// 1. overall quality 有明确提升
	if input.Candidate.NDCG10 <= input.Baseline.NDCG10 {
		passed = false
		reasons = append(reasons, "总体质量(NDCG@10)未提升")
	}

	// 2. Recall@10 不下降
	if input.Candidate.Recall10 < input.Baseline.Recall10 {
		passed = false
		reasons = append(reasons, "Recall@10 下降")
	}

	// 3. 关键 query class 不退化
	for _, class := range input.CriticalQueryClasses {
		candidateCM, ok := input.Candidate.ClassMetrics[class]
		if !ok {
			continue
		}
		baselineCM, ok := input.Baseline.ClassMetrics[class]
		if !ok {
			continue
		}
		// NDCG@10 退化超过 5% 视为明显退化
		if baselineCM.NDCG10 > 0 && candidateCM.NDCG10 < baselineCM.NDCG10*0.95 {
			passed = false
			reasons = append(reasons, "关键 query class "+class+" 退化")
		}
		// Recall@10 退化也检查
		if baselineCM.Recall10 > 0 && candidateCM.Recall10 < baselineCM.Recall10*0.95 {
			passed = false
			reasons = append(reasons, "关键 query class "+class+" Recall@10 退化")
		}
	}

	// 4. P95 latency 不超过阈值
	if input.Candidate.P95LatencyMs > input.MaxP95LatencyMs {
		passed = false
		reasons = append(reasons, "P95 延迟超过阈值")
	}

	// 5. 成本不超过管理员预算
	if input.Candidate.RerankerCost > input.MaxRerankerCostPerQuery {
		passed = false
		reasons = append(reasons, "reranker 成本超过预算")
	}

	// 6. index integrity 正常
	if !input.IndexIntegrityOK {
		passed = false
		reasons = append(reasons, "索引完整性异常")
	}

	return RegressionGateResult{Passed: passed, Reasons: reasons}
}
