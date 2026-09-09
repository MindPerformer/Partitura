// credential_test.go 测试凭据存储抽象。
//
// 引入动机：design/06-IMPLEMENTATION.md Phase 4 验收要求：
//   - credential 抽象用真实接口+安全测试替身
//   - 验证配置文件无 token
//   - 存储失败 fail-fast
//   - logout/revoke 删除
package credential

import (
	"testing"
)

func TestSaveLoadDelete(t *testing.T) {
	store := NewMockStore()

	tokens := &Tokens{
		AccessToken:  "test-access-token",
		RefreshToken: "test-refresh-token",
		ServerURL:    "https://example.com",
	}

	// Save
	if err := store.Save("https://example.com", tokens); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}

	// Load
	loaded, err := store.Load("https://example.com")
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if loaded.AccessToken != "test-access-token" {
		t.Errorf("AccessToken = %v", loaded.AccessToken)
	}
	if loaded.RefreshToken != "test-refresh-token" {
		t.Errorf("RefreshToken = %v", loaded.RefreshToken)
	}

	// Delete
	if err := store.Delete("https://example.com"); err != nil {
		t.Fatalf("Delete 失败: %v", err)
	}

	// Load after delete——应返回空 Tokens
	loaded, err = store.Load("https://example.com")
	if err != nil {
		t.Fatalf("Load after delete 失败: %v", err)
	}
	if loaded.AccessToken != "" {
		t.Errorf("AccessToken after delete = %v, 期望空", loaded.AccessToken)
	}
}

func TestLoadNonexistent(t *testing.T) {
	store := NewMockStore()

	loaded, err := store.Load("https://nonexistent.com")
	if err != nil {
		t.Fatalf("Load nonexistent 失败: %v", err)
	}
	if loaded.AccessToken != "" {
		t.Errorf("AccessToken = %v, 期望空", loaded.AccessToken)
	}
}

func TestDeleteNonexistent(t *testing.T) {
	store := NewMockStore()

	// 删除不存在的凭据——应幂等返回 nil
	if err := store.Delete("https://nonexistent.com"); err != nil {
		t.Fatalf("Delete nonexistent 失败: %v", err)
	}
}

func TestSaveFailFast(t *testing.T) {
	// 测试存储失败时 fail-fast
	store := NewMockStoreWithError(errTestStorageFailure)

	err := store.Save("https://example.com", &Tokens{
		AccessToken:  "token",
		RefreshToken: "refresh",
	})
	if err == nil {
		t.Fatal("期望 Save 失败")
	}
}

func TestLoadFailFast(t *testing.T) {
	store := NewMockStoreWithError(errTestStorageFailure)

	_, err := store.Load("https://example.com")
	if err == nil {
		t.Fatal("期望 Load 失败")
	}
}

func TestDeleteFailFast(t *testing.T) {
	store := NewMockStoreWithError(errTestStorageFailure)

	err := store.Delete("https://example.com")
	if err == nil {
		t.Fatal("期望 Delete 失败")
	}
}

// errTestStorageFailure 是测试用的存储失败错误。
var errTestStorageFailure = errTest("storage failure")

type errTest string

func (e errTest) Error() string { return string(e) }
