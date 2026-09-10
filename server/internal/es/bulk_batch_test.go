// bulk_batch_test.go 验证 BulkIndex 按"字节数 + 条数"双限分批的真实行为。
//
// 引入动机：线上重建索引时把全部 chunk 拼成一个 54MB 的 bulk 请求，超过 ES 协调节点
// max_coordinating_bytes（默认堆的 10%）后被 es_rejected_execution_exception 确定性拒绝，
// 重试同一份载荷永远不可能成功。因此必须用 httptest 驱动真实 HTTPClient，断言
// 请求确实被拆成多批、每批都在上限内、单条超限文档不被丢弃，且任一批失败能定位到该批。
package es

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// batchTestDocSize 是分批测试中"普通大文档"的 content 字段字节数。
//
// 取值动机：单条文档的 ndjson 体积约为 content 字节数 + 65 字节（action 行 + JSON 包装），
// 1MB + 4KB 使得 5 条必然超过 5MB 上限（约 5.26MB），而 4 条（约 4.21MB）又安全落于上限内，
// 因此 7 条文档会稳定拆成 4 + 3 两批，不受 JSON 包装字节数微小波动影响。
const batchTestDocSize = (1 << 20) + 4096

// bigBulkDoc 构造 content 字段约为 size 字节的测试文档。
func bigBulkDoc(id string, size int) IndexDoc {
	return IndexDoc{ID: id, Body: map[string]interface{}{"content": strings.Repeat("x", size)}}
}

// recordedBulkRequest 记录一次 /_bulk 请求的诊断字段，供分批断言使用。
type recordedBulkRequest struct {
	Bytes int      // 请求体字节数
	IDs   []string // 该请求写入的文档 ID 顺序
}

// parseBulkRequestIDs 从 ndjson 请求体中按 action 行提取 _id。
// 引入动机：只统计字节数无法证明"文档没有被丢弃或重复"，必须逐条解析 action 行核对 ID。
func parseBulkRequestIDs(payload []byte) ([]string, error) {
	lines := bytes.Split(bytes.TrimRight(payload, "\n"), []byte("\n"))
	if len(lines)%2 != 0 {
		return nil, fmt.Errorf("bulk 请求体行数 = %d, 期望 action/source 成对出现", len(lines))
	}
	ids := make([]string, 0, len(lines)/2)
	for i := 0; i < len(lines); i += 2 {
		var action map[string]struct {
			ID string `json:"_id"`
		}
		if err := json.Unmarshal(lines[i], &action); err != nil {
			return nil, fmt.Errorf("解析第 %d 行 action 失败: %w (%s)", i, err, lines[i])
		}
		entry, ok := action["index"]
		if !ok {
			return nil, fmt.Errorf("第 %d 行 action 不是 index 操作: %s", i, lines[i])
		}
		ids = append(ids, entry.ID)
	}
	return ids, nil
}

// newBulkRecorder 返回一个记录每次 /_bulk 请求体的 httptest server 与读取记录的函数。
// 引入动机：分批断言需要看到"每次请求分别发了什么"，因此把记录逻辑集中复用，
// 各用例只需决定如何响应。
func newBulkRecorder(t *testing.T, respond func(requestIndex int, w http.ResponseWriter)) (*httptest.Server, func() []recordedBulkRequest) {
	t.Helper()

	var (
		mu       sync.Mutex
		recorded []recordedBulkRequest
		requests int32
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_bulk" {
			t.Errorf("请求路径 = %s, 期望 /_bulk", r.URL.Path)
			return
		}
		index := int(atomic.AddInt32(&requests, 1))

		payload, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("读取第 %d 次 bulk 请求体失败: %v", index, err)
			return
		}
		ids, parseErr := parseBulkRequestIDs(payload)
		if parseErr != nil {
			t.Errorf("解析第 %d 次 bulk 请求体失败: %v", index, parseErr)
			return
		}

		mu.Lock()
		recorded = append(recorded, recordedBulkRequest{Bytes: len(payload), IDs: ids})
		mu.Unlock()

		respond(index, w)
	}))

	return server, func() []recordedBulkRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]recordedBulkRequest(nil), recorded...)
	}
}

// respondBulkSuccess 是分批测试通用的成功响应。
func respondBulkSuccess(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(bulkSuccessBody))
}

// TestHTTPClient_BulkIndexSplitsBatchesBySize 验证超过单批大小上限的文档集合被拆成多次请求，
// 每次请求体都在字节与条数上限内，且所有文档恰好各写入一次。
func TestHTTPClient_BulkIndexSplitsBatchesBySize(t *testing.T) {
	docs := make([]IndexDoc, 0, 7)
	for i := 0; i < 7; i++ {
		docs = append(docs, bigBulkDoc(fmt.Sprintf("doc-%d", i), batchTestDocSize))
	}

	server, recordedRequests := newBulkRecorder(t, func(_ int, w http.ResponseWriter) {
		respondBulkSuccess(w)
	})
	defer server.Close()

	client := NewHTTPClient(server.URL, 30*time.Second)
	if err := client.BulkIndex(context.Background(), "knowledge_v1", docs); err != nil {
		t.Fatalf("分批写入不应失败: %v", err)
	}

	requests := recordedRequests()
	if len(requests) < 2 {
		t.Fatalf("请求次数 = %d, 期望按大小拆成多批", len(requests))
	}

	seen := make(map[string]int, len(docs))
	for i, req := range requests {
		if req.Bytes > bulkBatchMaxBytes {
			t.Errorf("第 %d 批请求体 %d 字节, 超过上限 %d", i+1, req.Bytes, bulkBatchMaxBytes)
		}
		if len(req.IDs) > bulkBatchMaxDocs {
			t.Errorf("第 %d 批文档数 %d, 超过条数上限 %d", i+1, len(req.IDs), bulkBatchMaxDocs)
		}
		if len(req.IDs) == 0 {
			t.Errorf("第 %d 批不含任何文档", i+1)
		}
		for _, id := range req.IDs {
			seen[id]++
		}
	}

	if len(seen) != len(docs) {
		t.Fatalf("被写入的文档种类 = %d, 期望 %d", len(seen), len(docs))
	}
	for id, count := range seen {
		if count != 1 {
			t.Errorf("文档 %s 被写入 %d 次, 期望恰好 1 次", id, count)
		}
	}
}

// TestHTTPClient_BulkIndexSendsOversizedDocAlone 验证单条自身超过 bulkBatchMaxBytes 的文档
// 被单独成批发送：既不与其它文档混合（否则会连带撑爆整批），也不会被静默丢弃，
// 同时留下带 doc_id 与字节数的告警日志。
func TestHTTPClient_BulkIndexSendsOversizedDocAlone(t *testing.T) {
	// 捕获 slog 输出，确认超大文档确实产生了可观测的告警，而不是静默放过。
	var logBuf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	defer slog.SetDefault(previous)

	const oversizedID = "doc-huge"
	docs := []IndexDoc{
		bigBulkDoc("doc-a", 1024),
		bigBulkDoc(oversizedID, 6<<20), // 单条约 6MB，超过 5MB 上限
		bigBulkDoc("doc-b", 1024),
	}

	server, recordedRequests := newBulkRecorder(t, func(_ int, w http.ResponseWriter) {
		respondBulkSuccess(w)
	})
	defer server.Close()

	client := NewHTTPClient(server.URL, 30*time.Second)
	if err := client.BulkIndex(context.Background(), "knowledge_v1", docs); err != nil {
		t.Fatalf("含超大文档的分批写入不应失败: %v", err)
	}

	requests := recordedRequests()
	if len(requests) != 3 {
		t.Fatalf("请求次数 = %d, 期望 3（小文档 / 超大文档 / 小文档，超大文档必须独占一批）", len(requests))
	}

	seen := make(map[string]int, len(docs))
	oversizedBatches := 0
	for i, req := range requests {
		for _, id := range req.IDs {
			seen[id]++
		}
		if len(req.IDs) == 1 && req.IDs[0] == oversizedID {
			oversizedBatches++
			if req.Bytes <= bulkBatchMaxBytes {
				t.Errorf("第 %d 批被判定为超大文档独占批, 但请求体仅 %d 字节, 未超过上限 %d", i+1, req.Bytes, bulkBatchMaxBytes)
			}
		}
	}

	if oversizedBatches != 1 {
		t.Errorf("超限文档独占批次数 = %d, 期望 1", oversizedBatches)
	}
	if len(seen) != len(docs) {
		t.Fatalf("被写入的文档种类 = %d, 期望 %d（超限文档不能被丢弃）", len(seen), len(docs))
	}
	for id, count := range seen {
		if count != 1 {
			t.Errorf("文档 %s 被写入 %d 次, 期望恰好 1 次", id, count)
		}
	}

	logged := logBuf.String()
	for _, want := range []string{"超过推荐请求体大小", oversizedID, "doc_bytes", "max_batch_bytes"} {
		if !strings.Contains(logged, want) {
			t.Errorf("告警日志 %q 应包含 %q", logged, want)
		}
	}
}

// TestHTTPClient_BulkIndexReturnsFailingBatchContext 验证分批后任一批失败都会立即返回错误，
// 且错误信息能定位到具体批次（batch=i/N）、文档数与字节数，同时不再写后续批次。
func TestHTTPClient_BulkIndexReturnsFailingBatchContext(t *testing.T) {
	docs := make([]IndexDoc, 0, 7)
	for i := 0; i < 7; i++ {
		docs = append(docs, bigBulkDoc(fmt.Sprintf("doc-%d", i), batchTestDocSize))
	}

	server, recordedRequests := newBulkRecorder(t, func(requestIndex int, w http.ResponseWriter) {
		if requestIndex != 2 {
			respondBulkSuccess(w)
			return
		}
		// 第二批确定性失败（400 mapping 错误）：不可重试，且必须立即中止后续批次。
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"type":"mapper_parsing_exception","reason":"failed to parse field [embedding]"},"status":400}`))
	})
	defer server.Close()

	client := NewHTTPClient(server.URL, 30*time.Second)
	err := client.BulkIndex(context.Background(), "knowledge_v1", docs)
	if err == nil {
		t.Fatal("第二批失败时 BulkIndex 应返回错误")
	}
	if got := len(recordedRequests()); got != 2 {
		t.Errorf("请求次数 = %d, 期望 2（失败后立即返回，不再写后续批次）", got)
	}

	for _, want := range []string{
		"bulk index 返回状态码 400",
		"batch=2/2",
		"documents=3",
		"mapper_parsing_exception",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误 %q 应包含批次定位信息 %q", err.Error(), want)
		}
	}
	if strings.Contains(err.Error(), "batch=1/2") {
		t.Errorf("错误 %q 不应把失败归到第一批", err.Error())
	}
}
