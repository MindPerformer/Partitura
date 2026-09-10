// bulk_retry_test.go 验证 BulkIndex 对 429/503 的指数退避重试语义。
//
// 引入动机：生产上 ES 写入限流（429）会让 bulk 直接失败，导致 job 无谓失败甚至进入 dead。
// 这里用 httptest mock ES 驱动真实 HTTPClient，覆盖"重试后成功""重试耗尽""ctx 取消立即返回"
// 与"不可重试状态码立即失败"四条真实分支。
package es

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// bulkSuccessBody 是合法的 bulk 成功响应体（errors=false）。
const bulkSuccessBody = `{"took":1,"errors":false,"items":[{"index":{"_index":"knowledge_v1","_id":"doc-1","status":201}}]}`

// bulkTooManyRequestsBody 是 ES 写入限流时的标准错误响应体。
const bulkTooManyRequestsBody = `{"error":{"type":"rejected_execution_exception","reason":"bulk queue is full"},"status":429}`

// testBulkDocs 是重试测试使用的单文档批量请求。
func testBulkDocs() []IndexDoc {
	return []IndexDoc{{ID: "doc-1", Body: map[string]interface{}{"content": "hello"}}}
}

// TestBulkRetryDelayBounds 直接验证退避函数：指数翻倍、封顶与 ±jitter 区间。
// 引入动机：退避参数是线上调参的抓手，必须保证"翻倍 + 上限 + 抖动"三者同时成立。
func TestBulkRetryDelayBounds(t *testing.T) {
	tests := []struct {
		attempt  int
		wantBase time.Duration
	}{
		{attempt: 1, wantBase: bulkRetryBaseDelay},
		{attempt: 2, wantBase: 2 * bulkRetryBaseDelay},
		{attempt: 3, wantBase: 4 * bulkRetryBaseDelay},
		{attempt: 4, wantBase: 8 * bulkRetryBaseDelay},
		// 200ms * 2^5 = 6.4s 超过上限，必须封顶而不是继续翻倍。
		{attempt: 6, wantBase: bulkRetryMaxDelay},
		// 更远的次数同样封顶，且不会因位移溢出成负数。
		{attempt: 20, wantBase: bulkRetryMaxDelay},
	}

	for _, tc := range tests {
		minDelay := time.Duration(float64(tc.wantBase) * (1 - bulkRetryJitterRatio))
		maxDelay := time.Duration(float64(tc.wantBase) * (1 + bulkRetryJitterRatio))
		for i := 0; i < 50; i++ {
			got := bulkRetryDelay(tc.attempt)
			if got < minDelay || got > maxDelay {
				t.Fatalf("bulkRetryDelay(%d) = %s, 期望落在 [%s, %s]", tc.attempt, got, minDelay, maxDelay)
			}
		}
	}
}

// TestHTTPClient_BulkIndexRetriesRetryableStatus 验证 429/503 会被重试，
// 且重试请求携带完整的请求体（证明每次尝试都重建了 body reader）。
func TestHTTPClient_BulkIndexRetriesRetryableStatus(t *testing.T) {
	for _, retryStatus := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(retryStatus), func(t *testing.T) {
			var requests int32
			var mu sync.Mutex
			bodies := make([]string, 0, 2)

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempt := atomic.AddInt32(&requests, 1)
				if r.URL.Path != "/_bulk" {
					t.Errorf("请求路径 = %s, 期望 /_bulk", r.URL.Path)
				}
				payload, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("读取第 %d 次请求体失败: %v", attempt, err)
					return
				}
				mu.Lock()
				bodies = append(bodies, string(payload))
				mu.Unlock()

				w.Header().Set("Content-Type", "application/json")
				if attempt == 1 {
					w.WriteHeader(retryStatus)
					_, _ = w.Write([]byte(fmt.Sprintf(`{"error":{"type":"rejected_execution_exception","reason":"bulk queue is full"},"status":%d}`, retryStatus)))
					return
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(bulkSuccessBody))
			}))
			defer server.Close()

			client := NewHTTPClient(server.URL, 5*time.Second)
			started := time.Now()
			if err := client.BulkIndex(context.Background(), "knowledge_v1", testBulkDocs()); err != nil {
				t.Fatalf("%d 后重试成功，不应返回错误: %v", retryStatus, err)
			}
			elapsed := time.Since(started)

			if got := atomic.LoadInt32(&requests); got != 2 {
				t.Fatalf("请求次数 = %d, 期望 2（首次 + 一次重试）", got)
			}
			// 退避下限是 base*(1-jitter)，耗时应落在这之上，否则说明没有真正等待。
			if minWait := time.Duration(float64(bulkRetryBaseDelay) * (1 - bulkRetryJitterRatio)); elapsed < minWait {
				t.Errorf("耗时 %s 小于退避下限 %s，说明没有真正退避", elapsed, minWait)
			}
			// 第二次尝试若复用已消费的 reader，请求体将为空，这里必须仍是完整 ndjson。
			mu.Lock()
			recorded := append([]string(nil), bodies...)
			mu.Unlock()
			if len(recorded) != 2 {
				t.Fatalf("记录到的请求体数量 = %d, 期望 2", len(recorded))
			}
			for i, body := range recorded {
				if !strings.Contains(body, `"_id":"doc-1"`) || !strings.Contains(body, `"content":"hello"`) {
					t.Errorf("第 %d 次尝试的请求体不完整: %q", i+1, body)
				}
			}
		})
	}
}

// TestHTTPClient_BulkIndexExhaustsRetries 验证连续 429 会用尽尝试次数并返回错误。
// 注意：该用例真实等待完整退避序列（200ms+400ms+800ms+1600ms ≈ 3s）。
func TestHTTPClient_BulkIndexExhaustsRetries(t *testing.T) {
	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(bulkTooManyRequestsBody))
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, 10*time.Second)
	err := client.BulkIndex(context.Background(), "knowledge_v1", testBulkDocs())
	if err == nil {
		t.Fatal("连续 429 用尽重试次数后应返回错误")
	}
	if got := atomic.LoadInt32(&requests); got != bulkRetryMaxAttempts {
		t.Errorf("请求次数 = %d, 期望 %d（含首次尝试）", got, bulkRetryMaxAttempts)
	}
	for _, want := range []string{
		"bulk index 返回状态码 429",
		fmt.Sprintf("重试 %d 次后仍失败", bulkRetryMaxAttempts-1),
		"rejected_execution_exception",
		"bulk queue is full",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误 %q 应包含 %q", err.Error(), want)
		}
	}
}

// TestHTTPClient_BulkIndexStopsRetryingOnContextCancel 验证退避等待期间 ctx 被取消时
// 立即返回 ctx 错误，且不再发起新的请求。
func TestHTTPClient_BulkIndexStopsRetryingOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	responded := make(chan struct{}, 1)
	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(bulkTooManyRequestsBody))
		select {
		case responded <- struct{}{}:
		default:
		}
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, 10*time.Second)
	done := make(chan error, 1)
	started := time.Now()
	go func() {
		done <- client.BulkIndex(ctx, "knowledge_v1", testBulkDocs())
	}()

	// 等首个 429 写出后再等一小段（远小于退避下限 160ms）才取消 ctx，
	// 这样取消必然发生在退避等待中，从而覆盖 select 的 ctx.Done 分支。
	select {
	case <-responded:
	case <-time.After(5 * time.Second):
		t.Fatal("等待首个 429 响应超时")
	}
	time.Sleep(30 * time.Millisecond)
	cancel()

	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ctx 取消后 BulkIndex 未及时返回")
	}
	elapsed := time.Since(started)

	if err == nil {
		t.Fatal("ctx 取消后 BulkIndex 应返回错误")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("错误应包装 context.Canceled, 得到 %v", err)
	}
	if got := atomic.LoadInt32(&requests); got != 1 {
		t.Errorf("ctx 取消后不应再发起请求, 请求次数 = %d", got)
	}
	// 必须远小于首次退避的下限（base*(1-ratio)=160ms）：否则说明只是等满退避后
	// 由下一次请求的 ctx 报错兜住，而不是在退避等待中就立刻响应 ctx 取消。
	if maxWait := bulkRetryBaseDelay / 2; elapsed >= maxWait {
		t.Errorf("ctx 取消应立即返回, 实际耗时 %s（应小于 %s）", elapsed, maxWait)
	}
	if !strings.Contains(err.Error(), "重试等待被取消") {
		t.Errorf("错误应表明退避等待被 ctx 取消, 得到 %v", err)
	}
}

// TestHTTPClient_BulkIndexDoesNotRetryNonRetryableStatus 验证 400 等确定性失败立即返回，
// 不做任何重试，也不会退避等待。
func TestHTTPClient_BulkIndexDoesNotRetryNonRetryableStatus(t *testing.T) {
	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"type":"mapper_parsing_exception","reason":"failed to parse field [embedding]"},"status":400}`))
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, time.Second)
	started := time.Now()
	err := client.BulkIndex(context.Background(), "knowledge_v1", testBulkDocs())
	elapsed := time.Since(started)

	if err == nil {
		t.Fatal("400 应返回错误")
	}
	if got := atomic.LoadInt32(&requests); got != 1 {
		t.Errorf("不可重试状态码不应重试, 请求次数 = %d", got)
	}
	if elapsed >= bulkRetryBaseDelay {
		t.Errorf("不可重试状态码不应退避等待, 实际耗时 %s", elapsed)
	}
	for _, want := range []string{"bulk index 返回状态码 400", "mapper_parsing_exception"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误 %q 应包含 %q", err.Error(), want)
		}
	}
	if strings.Contains(err.Error(), "重试") {
		t.Errorf("不可重试状态码的错误不应提到重试: %q", err.Error())
	}
}
