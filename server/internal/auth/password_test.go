// password_test.go 测试 Argon2id 密码哈希与校验逻辑。
//
// 测试覆盖：
//   - HashPassword + VerifyPassword 正确匹配
//   - 错误密码校验失败（返回 ErrPasswordVerificationFailed）
//   - 不同密码生成不同哈希（盐随机）
//   - 相同密码生成不同哈希（盐随机）
//   - 损坏的哈希格式返回错误
//   - 错误凭据不泄露账户存在性（通过统一错误实现）
package auth

import (
	"errors"
	"strings"
	"testing"
)

// testAuthCfg 返回用于测试的 AuthConfig，使用较低的 Argon2id 参数以加速测试。
func testAuthCfg() AuthConfig {
	return AuthConfig{
		SessionDuration:            3600,
		DeviceAccessTokenDuration:  900,
		DeviceRefreshTokenDuration: 2592000,
		CookieSecure:               false,
		CookiePath:                 "/",
		CookieName:                 "session",
		CSRFCookieName:             "csrf",
		CSRFHeaderName:             "X-CSRF-Token",
		Argon2Memory:               32 * 1024, // 32 MiB，测试用较低值
		Argon2Iterations:           1,
		Argon2Parallelism:          1,
		Argon2SaltLength:           16,
		Argon2KeyLength:            32,
	}
}

func TestHashPassword_VerifyPassword_CorrectPassword(t *testing.T) {
	cfg := testAuthCfg()
	plain := "mySecretPassword123!"

	hash, err := HashPassword(plain, cfg)
	if err != nil {
		t.Fatalf("HashPassword 失败: %v", err)
	}

	if hash == "" {
		t.Fatal("哈希不应为空")
	}

	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Errorf("哈希应以 $argon2id$ 开头，实际: %s", hash[:min(len(hash), 20)])
	}

	if err := VerifyPassword(hash, plain); err != nil {
		t.Errorf("正确密码校验应通过，但返回错误: %v", err)
	}
}

func TestHashPassword_VerifyPassword_WrongPassword(t *testing.T) {
	cfg := testAuthCfg()
	plain := "correctPassword"
	wrong := "wrongPassword"

	hash, err := HashPassword(plain, cfg)
	if err != nil {
		t.Fatalf("HashPassword 失败: %v", err)
	}

	err = VerifyPassword(hash, wrong)
	if !errors.Is(err, ErrPasswordVerificationFailed) {
		t.Errorf("错误密码应返回 ErrPasswordVerificationFailed，实际: %v", err)
	}
}

func TestHashPassword_DifferentHashesForSamePassword(t *testing.T) {
	cfg := testAuthCfg()
	plain := "samePassword"

	hash1, err := HashPassword(plain, cfg)
	if err != nil {
		t.Fatalf("HashPassword 第一次失败: %v", err)
	}

	hash2, err := HashPassword(plain, cfg)
	if err != nil {
		t.Fatalf("HashPassword 第二次失败: %v", err)
	}

	if hash1 == hash2 {
		t.Error("相同密码应生成不同哈希（盐随机），但两次哈希相同")
	}

	// 两个哈希都应能验证同一密码
	if err := VerifyPassword(hash1, plain); err != nil {
		t.Errorf("第一个哈希验证失败: %v", err)
	}
	if err := VerifyPassword(hash2, plain); err != nil {
		t.Errorf("第二个哈希验证失败: %v", err)
	}
}

func TestHashPassword_DifferentPasswordsDifferentHashes(t *testing.T) {
	cfg := testAuthCfg()

	hash1, err := HashPassword("password1", cfg)
	if err != nil {
		t.Fatalf("HashPassword password1 失败: %v", err)
	}

	hash2, err := HashPassword("password2", cfg)
	if err != nil {
		t.Fatalf("HashPassword password2 失败: %v", err)
	}

	if hash1 == hash2 {
		t.Error("不同密码不应生成相同哈希")
	}
}

func TestVerifyPassword_CorruptedHash(t *testing.T) {
	corruptedHashes := []string{
		"",
		"not-a-hash",
		"$argon2id$",
		"$argon2id$v=19$m=32768,t=1,p=1$badbase64$badbase64",
		"$argon2i$v=19$m=32768,t=1,p=1$saltsalt$keykey", // argon2i 不是 argon2id
	}

	for _, hash := range corruptedHashes {
		err := VerifyPassword(hash, "anypassword")
		if err == nil {
			t.Errorf("损坏的哈希 %q 应返回错误，但验证通过", hash)
		}
		if errors.Is(err, ErrPasswordVerificationFailed) {
			t.Errorf("损坏的哈希 %q 不应返回 ErrPasswordVerificationFailed（应返回格式错误）", hash)
		}
	}
}

func TestVerifyPassword_EmptyPassword(t *testing.T) {
	cfg := testAuthCfg()

	hash, err := HashPassword("", cfg)
	if err != nil {
		t.Fatalf("HashPassword 空密码失败: %v", err)
	}

	// 空密码应能验证
	if err := VerifyPassword(hash, ""); err != nil {
		t.Errorf("空密码验证应通过: %v", err)
	}

	// 非空密码不应通过
	if err := VerifyPassword(hash, "nonempty"); !errors.Is(err, ErrPasswordVerificationFailed) {
		t.Errorf("非空密码验证空密码哈希应失败: %v", err)
	}
}
