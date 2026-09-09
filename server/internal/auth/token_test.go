// token_test.go 测试 token 生成与哈希逻辑。
//
// 测试覆盖：
//   - GenerateToken 返回非空、长度正确的十六进制字符串
//   - 多次调用 GenerateToken 返回不同值（随机性）
//   - HashToken 返回固定长度哈希
//   - 相同 token 的哈希一致（确定性）
//   - 不同 token 的哈希不同
//   - 哈希不等于原文（只存哈希原则）
package auth

import (
	"testing"
)

func TestGenerateToken_NotEmpty(t *testing.T) {
	token, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken 失败: %v", err)
	}
	if token == "" {
		t.Fatal("token 不应为空")
	}
	// 32 字节 = 64 十六进制字符
	if len(token) != 64 {
		t.Errorf("token 长度 = %d, 期望 64", len(token))
	}
}

func TestGenerateToken_Uniqueness(t *testing.T) {
	tokens := make(map[string]bool)
	for i := 0; i < 100; i++ {
		token, err := GenerateToken()
		if err != nil {
			t.Fatalf("GenerateToken 第 %d 次失败: %v", i, err)
		}
		if tokens[token] {
			t.Fatalf("第 %d 次生成的 token 与之前重复: %s", i, token)
		}
		tokens[token] = true
	}
}

func TestHashToken_Deterministic(t *testing.T) {
	token := "abc123testtoken"
	hash1 := HashToken(token)
	hash2 := HashToken(token)

	if hash1 != hash2 {
		t.Errorf("相同 token 的哈希应一致: %s vs %s", hash1, hash2)
	}
}

func TestHashToken_DifferentTokensDifferentHashes(t *testing.T) {
	hash1 := HashToken("token1")
	hash2 := HashToken("token2")

	if hash1 == hash2 {
		t.Error("不同 token 的哈希不应相同")
	}
}

func TestHashToken_NotEqualToOriginal(t *testing.T) {
	token := "mySecretToken123"
	hash := HashToken(token)

	if hash == token {
		t.Error("哈希不应等于原文 token")
	}
}

func TestHashToken_LengthConsistency(t *testing.T) {
	// SHA-256 输出 32 字节 = 64 十六进制字符
	token, _ := GenerateToken()
	hash := HashToken(token)

	if len(hash) != 64 {
		t.Errorf("哈希长度 = %d, 期望 64", len(hash))
	}
}
