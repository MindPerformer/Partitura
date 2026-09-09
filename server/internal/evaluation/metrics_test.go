// metrics_test.go 测试评测指标计算和 regression gate。
package evaluation

import (
	"testing"

	types "partitura/server/internal/search/types"
)

func TestComputeNDCG_PerfectRanking(t *testing.T) {
	results := []types.SearchResult{
		{DocumentID: "d1"},
		{DocumentID: "d2"},
		{DocumentID: "d3"},
	}
	expected := map[string]int{"d1": 3, "d2": 2, "d3": 1}

	ndcg := computeNDCG(results, expected, 10)
	if ndcg != 1.0 {
		t.Errorf("完美排序的 NDCG 应为 1.0，得到 %f", ndcg)
	}
}

func TestComputeNDCG_NoRelevant(t *testing.T) {
	results := []types.SearchResult{
		{DocumentID: "d1"},
		{DocumentID: "d2"},
	}
	expected := map[string]int{"d3": 3}

	ndcg := computeNDCG(results, expected, 10)
	if ndcg != 0.0 {
		t.Errorf("无相关文档的 NDCG 应为 0.0，得到 %f", ndcg)
	}
}

func TestComputeRecall(t *testing.T) {
	results := []types.SearchResult{
		{DocumentID: "d1"},
		{DocumentID: "d2"},
		{DocumentID: "d3"},
		{DocumentID: "d4"},
		{DocumentID: "d5"},
	}
	expected := map[string]int{"d1": 3, "d2": 2, "d6": 1}

	recall5 := computeRecall(results, expected, 5)
	// relevant in top5: d1, d2 = 2; total relevant: d1, d2, d6 = 3
	if recall5 != 2.0/3.0 {
		t.Errorf("Recall@5 应为 %f，得到 %f", 2.0/3.0, recall5)
	}

	recall10 := computeRecall(results, expected, 10)
	// only 5 results, all relevant in top10: d1, d2 = 2; total relevant: 3
	if recall10 != 2.0/3.0 {
		t.Errorf("Recall@10 应为 %f，得到 %f", 2.0/3.0, recall10)
	}
}

func TestComputeMRR(t *testing.T) {
	results := []types.SearchResult{
		{DocumentID: "d1"},
		{DocumentID: "d2"},
		{DocumentID: "d3"},
	}
	expected := map[string]int{"d2": 2, "d3": 1}

	mrr := computeMRR(results, expected)
	// first relevant result is d2 at rank 2, MRR = 1/2
	if mrr != 0.5 {
		t.Errorf("MRR 应为 0.5，得到 %f", mrr)
	}
}

func TestComputePrecision(t *testing.T) {
	results := []types.SearchResult{
		{DocumentID: "d1"},
		{DocumentID: "d2"},
		{DocumentID: "d3"},
	}
	expected := map[string]int{"d1": 3, "d2": 2}

	precision := computePrecision(results, expected, 10)
	// relevant in top10: d1, d2 = 2; K=10 (but only 3 results)
	// Precision = 2/3 (using actual result count)
	if precision != 2.0/3.0 {
		t.Errorf("Precision 应为 %f，得到 %f", 2.0/3.0, precision)
	}
}

func TestComputePercentiles(t *testing.T) {
	values := []int{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}
	p50, p95 := computePercentiles(values)
	if p50 != 50 {
		t.Errorf("P50 应为 50，得到 %d", p50)
	}
	if p95 != 100 {
		t.Errorf("P95 应为 100，得到 %d", p95)
	}
}

func TestComputeMetrics_FullEvaluation(t *testing.T) {
	items := []EvaluationItem{
		{
			Query:      "database architecture",
			QueryClass: "architecture",
			ExpectedDocuments: []ExpectedDocument{
				{DocumentID: "d1", Grade: 3},
				{DocumentID: "d2", Grade: 2},
			},
		},
		{
			Query:      "error handling",
			QueryClass: "error_message",
			ExpectedDocuments: []ExpectedDocument{
				{DocumentID: "d3", Grade: 3},
			},
		},
	}

	queryResults := []QueryResult{
		{
			QueryClass: "architecture",
			LatencyMs:  100,
			Results: []types.SearchResult{
				{DocumentID: "d1"},
				{DocumentID: "d2"},
			},
		},
		{
			QueryClass: "error_message",
			LatencyMs:  200,
			Results: []types.SearchResult{
				{DocumentID: "d3"},
			},
		},
	}

	result := ComputeMetrics(items, queryResults)

	if result.NDCG10 != 1.0 {
		t.Errorf("完美排序的 NDCG@10 应为 1.0，得到 %f", result.NDCG10)
	}
	if result.Recall10 != 1.0 {
		t.Errorf("完美排序的 Recall@10 应为 1.0，得到 %f", result.Recall10)
	}
	if result.MRR != 1.0 {
		t.Errorf("完美排序的 MRR 应为 1.0，得到 %f", result.MRR)
	}

	// 验证 class metrics
	archMetric, ok := result.ClassMetrics["architecture"]
	if !ok {
		t.Fatal("缺少 architecture class metrics")
	}
	if archMetric.Count != 1 {
		t.Errorf("architecture class 应有 1 条 query")
	}
}

func TestRegressionGate_PassCase(t *testing.T) {
	baseline := EvaluationResult{
		NDCG10:      0.7,
		Recall10:    0.8,
		P95LatencyMs: 1000,
		RerankerCost: 0.005,
		ClassMetrics: map[string]ClassMetric{
			"architecture": {NDCG10: 0.7, Recall10: 0.8},
		},
	}
	candidate := EvaluationResult{
		NDCG10:      0.8,
		Recall10:    0.85,
		P95LatencyMs: 1200,
		RerankerCost: 0.008,
		ClassMetrics: map[string]ClassMetric{
			"architecture": {NDCG10: 0.75, Recall10: 0.82},
		},
	}

	gate := CheckRegressionGate(RegressionGateInput{
		Candidate:             candidate,
		Baseline:              baseline,
		MaxP95LatencyMs:       2000,
		MaxRerankerCostPerQuery: 0.01,
		IndexIntegrityOK:      true,
		CriticalQueryClasses:  []string{"architecture"},
	})

	if !gate.Passed {
		t.Errorf("应通过 regression gate，原因: %v", gate.Reasons)
	}
}

func TestRegressionGate_FailQualityNotImproved(t *testing.T) {
	baseline := EvaluationResult{NDCG10: 0.8, Recall10: 0.8}
	candidate := EvaluationResult{NDCG10: 0.7, Recall10: 0.85}

	gate := CheckRegressionGate(RegressionGateInput{
		Candidate:        candidate,
		Baseline:         baseline,
		MaxP95LatencyMs:  2000,
		IndexIntegrityOK: true,
	})

	if gate.Passed {
		t.Error("质量下降应不通过 gate")
	}
}

func TestRegressionGate_FailRecallDropped(t *testing.T) {
	baseline := EvaluationResult{NDCG10: 0.7, Recall10: 0.9}
	candidate := EvaluationResult{NDCG10: 0.8, Recall10: 0.8}

	gate := CheckRegressionGate(RegressionGateInput{
		Candidate:        candidate,
		Baseline:         baseline,
		MaxP95LatencyMs:  2000,
		IndexIntegrityOK: true,
	})

	if gate.Passed {
		t.Error("Recall@10 下降应不通过 gate")
	}
}

func TestRegressionGate_FailLatencyExceeded(t *testing.T) {
	baseline := EvaluationResult{NDCG10: 0.7, Recall10: 0.8, P95LatencyMs: 1000}
	candidate := EvaluationResult{NDCG10: 0.8, Recall10: 0.85, P95LatencyMs: 3000}

	gate := CheckRegressionGate(RegressionGateInput{
		Candidate:        candidate,
		Baseline:         baseline,
		MaxP95LatencyMs:  2000,
		IndexIntegrityOK: true,
	})

	if gate.Passed {
		t.Error("P95 延迟超阈值应不通过 gate")
	}
}

func TestRegressionGate_FailCostExceeded(t *testing.T) {
	baseline := EvaluationResult{NDCG10: 0.7, Recall10: 0.8, RerankerCost: 0.005}
	candidate := EvaluationResult{NDCG10: 0.8, Recall10: 0.85, RerankerCost: 0.02}

	gate := CheckRegressionGate(RegressionGateInput{
		Candidate:             candidate,
		Baseline:              baseline,
		MaxP95LatencyMs:       2000,
		MaxRerankerCostPerQuery: 0.01,
		IndexIntegrityOK:      true,
	})

	if gate.Passed {
		t.Error("成本超预算应不通过 gate")
	}
}

func TestRegressionGate_FailIntegrityBad(t *testing.T) {
	baseline := EvaluationResult{NDCG10: 0.7, Recall10: 0.8}
	candidate := EvaluationResult{NDCG10: 0.8, Recall10: 0.85}

	gate := CheckRegressionGate(RegressionGateInput{
		Candidate:        candidate,
		Baseline:         baseline,
		MaxP95LatencyMs:  2000,
		IndexIntegrityOK: false,
	})

	if gate.Passed {
		t.Error("索引完整性异常应不通过 gate")
	}
}

func TestRegressionGate_FailClassDegraded(t *testing.T) {
	baseline := EvaluationResult{
		NDCG10:   0.7,
		Recall10: 0.8,
		ClassMetrics: map[string]ClassMetric{
			"architecture": {NDCG10: 0.8, Recall10: 0.9},
		},
	}
	candidate := EvaluationResult{
		NDCG10:   0.75,
		Recall10: 0.82,
		ClassMetrics: map[string]ClassMetric{
			"architecture": {NDCG10: 0.6, Recall10: 0.85}, // NDCG 退化 > 5%
		},
	}

	gate := CheckRegressionGate(RegressionGateInput{
		Candidate:            candidate,
		Baseline:             baseline,
		MaxP95LatencyMs:      2000,
		IndexIntegrityOK:     true,
		CriticalQueryClasses: []string{"architecture"},
	})

	if gate.Passed {
		t.Error("关键 query class 退化应不通过 gate")
	}
}
