// mock.go 提供凭据存储的测试替身。
//
// 引入动机：design/06-IMPLEMENTATION.md Phase 4 验收要求使用 fake HTTPS server
// 验证 MCP 实际调用 REST。测试替身需要跨包使用（tool 测试需要注入 credential mock），
// 因此放在非 _test.go 文件中。
//
// 安全原则：
//   - MockStore 不写入文件系统，仅在内存中保存
//   - 可注入错误以测试 fail-fast 行为
package credential

// MockStore 是测试用的内存凭据存储替身。
// 引入动机：测试需要内存替身而非真实加密文件存储。
type MockStore struct {
	data map[string]*Tokens
	err  error
}

// NewMockStore 创建测试用凭据存储。
func NewMockStore() *MockStore {
	return &MockStore{data: make(map[string]*Tokens)}
}

// NewMockStoreWithError 创建总是失败的凭据存储。
// 引入动机：测试存储失败 fail-fast。
func NewMockStoreWithError(err error) *MockStore {
	return &MockStore{data: make(map[string]*Tokens), err: err}
}

// Save 将 token 保存到内存。
func (m *MockStore) Save(serverURL string, tokens *Tokens) error {
	if m.err != nil {
		return failFastLog("保存凭据", serverURL, m.err)
	}
	m.data[serverURL] = tokens
	return nil
}

// Load 从内存读取 token。
func (m *MockStore) Load(serverURL string) (*Tokens, error) {
	if m.err != nil {
		return nil, failFastLog("读取凭据", serverURL, m.err)
	}
	if t, ok := m.data[serverURL]; ok {
		return t, nil
	}
	return &Tokens{}, nil
}

// Delete 从内存删除 token。
func (m *MockStore) Delete(serverURL string) error {
	if m.err != nil {
		return failFastLog("删除凭据", serverURL, m.err)
	}
	delete(m.data, serverURL)
	return nil
}
