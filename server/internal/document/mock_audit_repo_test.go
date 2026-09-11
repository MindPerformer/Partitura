// mock_audit_repo_test.go 提供 audit.Repository 接口的本地 mock 实现，供 document 测试使用。
//
// 引入动机：audit 包没有在非 test 文件中提供 mock 实现。
// workspace 测试中的 MockAuditRepository 在 _test.go 中不可跨包访问。
// document 测试需要 audit.Repository 的 mock 来构建完整 handler。
package document

import (
	"context"
	"encoding/json"
	"sync"

	"partitura/server/internal/audit"
)

// LocalAuditMockRepository 是 audit.Repository 接口的本地 mock 实现。
type LocalAuditMockRepository struct {
	mu      sync.Mutex
	entries []localAuditEntry
}

type localAuditEntry struct {
	userID       string
	workspaceID  string
	action       string
	resourceType string
	resourceID   string
	detail       json.RawMessage
	requestID    string
}

// NewLocalAuditMockRepository 创建空 mock audit repository。
func NewLocalAuditMockRepository() *LocalAuditMockRepository {
	return &LocalAuditMockRepository{}
}

// Record 实现 audit.Repository 接口。
func (m *LocalAuditMockRepository) Record(ctx context.Context, userID, workspaceID, action, resourceType, resourceID string, detail json.RawMessage, requestID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = append(m.entries, localAuditEntry{
		userID:       userID,
		workspaceID:  workspaceID,
		action:       action,
		resourceType: resourceType,
		resourceID:   resourceID,
		detail:       detail,
		requestID:    requestID,
	})
	return nil
}

// List 实现 audit.Repository 接口。
// 引入动机：接口签名含可选筛选条件，mock 复现同一过滤语义以适配签名。
// From/To 在本 mock 中不参与过滤（entry 未持久化时间戳），仅字段级条件生效。
func (m *LocalAuditMockRepository) List(ctx context.Context, filter audit.ListFilter, limit, offset int) (*audit.ListResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var filtered []localAuditEntry
	for _, e := range m.entries {
		if filter.UserID != "" && e.userID != filter.UserID {
			continue
		}
		if filter.Action != "" && e.action != filter.Action {
			continue
		}
		if filter.ResourceType != "" && e.resourceType != filter.ResourceType {
			continue
		}
		if filter.WorkspaceID != "" && e.workspaceID != filter.WorkspaceID {
			continue
		}
		filtered = append(filtered, e)
	}

	total := len(filtered)
	if offset >= total {
		return &audit.ListResult{Total: total}, nil
	}
	end := offset + limit
	if end > total {
		end = total
	}
	var result []audit.Entry
	for i := total - 1 - offset; i >= total-end; i-- {
		e := filtered[i]
		result = append(result, audit.Entry{
			UserID:       e.userID,
			WorkspaceID:  e.workspaceID,
			Action:       e.action,
			ResourceType: e.resourceType,
			ResourceID:   e.resourceID,
			Detail:       e.detail,
			RequestID:    e.requestID,
		})
	}
	return &audit.ListResult{Entries: result, Total: total}, nil
}
