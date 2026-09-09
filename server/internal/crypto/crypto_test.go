// Package crypto 的测试覆盖 AES-256-GCM 加解密、根密钥解析、密文篡改。
//
// 引入动机：计划要求真实行为测试——根密钥格式、加密/篡改失败。
// 禁止源码字符串包含测试，所有测试调用真实加解密逻辑验证。
package crypto

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
)

// generateTestKey 生成一个合法的 base64 编码 32 字节密钥用于测试。
func generateTestKey(t *testing.T) string {
	t.Helper()
	raw := make([]byte, KeyLength)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("生成测试密钥失败: %v", err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func TestParseRootKey_Valid(t *testing.T) {
	encoded := generateTestKey(t)
	key, err := ParseRootKey(encoded)
	if err != nil {
		t.Fatalf("合法密钥应解析成功, got error: %v", err)
	}
	if len(key) != KeyLength {
		t.Fatalf("解析后密钥长度 = %d, want %d", len(key), KeyLength)
	}
}

func TestParseRootKey_Empty(t *testing.T) {
	_, err := ParseRootKey("")
	if err == nil {
		t.Fatal("空字符串应返回错误")
	}
	if !errorsIs(err, ErrInvalidKey) {
		t.Fatalf("空字符串应返回 ErrInvalidKey, got: %v", err)
	}
}

func TestParseRootKey_InvalidBase64(t *testing.T) {
	_, err := ParseRootKey("!!!not-valid-base64!!!")
	if err == nil {
		t.Fatal("非法 base64 应返回错误")
	}
	if !errorsIs(err, ErrInvalidKey) {
		t.Fatalf("非法 base64 应返回 ErrInvalidKey, got: %v", err)
	}
}

func TestParseRootKey_WrongLength(t *testing.T) {
	// 16 字节密钥 base64 编码——长度不足
	shortKey := make([]byte, 16)
	encoded := base64.StdEncoding.EncodeToString(shortKey)
	_, err := ParseRootKey(encoded)
	if err == nil {
		t.Fatal("16 字节密钥应返回错误")
	}
	if !errorsIs(err, ErrInvalidKey) {
		t.Fatalf("16 字节密钥应返回 ErrInvalidKey, got: %v", err)
	}
}

func TestEncryptDecrypt_RoundTrip(t *testing.T) {
	encoded := generateTestKey(t)
	key, err := ParseRootKey(encoded)
	if err != nil {
		t.Fatalf("解析密钥失败: %v", err)
	}

	plaintext := "sk-test-api-key-12345"
	ciphertext, err := Encrypt(key, []byte(plaintext))
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}

	if ciphertext == plaintext {
		t.Fatal("密文不应等于明文")
	}

	decrypted, err := Decrypt(key, ciphertext)
	if err != nil {
		t.Fatalf("解密失败: %v", err)
	}

	if string(decrypted) != plaintext {
		t.Fatalf("解密结果 = %q, want %q", string(decrypted), plaintext)
	}
}

func TestEncrypt_NonceUniqueness(t *testing.T) {
	encoded := generateTestKey(t)
	key, _ := ParseRootKey(encoded)

	plaintext := "same-key"
	ct1, _ := Encrypt(key, []byte(plaintext))
	ct2, _ := Encrypt(key, []byte(plaintext))

	if ct1 == ct2 {
		t.Fatal("相同明文两次加密应产生不同密文（随机 nonce）")
	}

	// 两者都应能正确解密
	d1, err := Decrypt(key, ct1)
	if err != nil {
		t.Fatalf("第一次密文解密失败: %v", err)
	}
	d2, err := Decrypt(key, ct2)
	if err != nil {
		t.Fatalf("第二次密文解密失败: %v", err)
	}
	if string(d1) != plaintext || string(d2) != plaintext {
		t.Fatal("两次解密结果应与原文一致")
	}
}

func TestDecrypt_TamperedCiphertext(t *testing.T) {
	encoded := generateTestKey(t)
	key, _ := ParseRootKey(encoded)

	plaintext := "sk-secret-key"
	ct, _ := Encrypt(key, []byte(plaintext))

	// 篡改密文：翻转最后一个字符
	tampered := ct[:len(ct)-2] + "XX"
	_, err := Decrypt(key, tampered)
	if err == nil {
		t.Fatal("篡改后的密文应解密失败")
	}
	if !errorsIs(err, ErrDecryptionFailed) {
		t.Fatalf("篡改密文应返回 ErrDecryptionFailed, got: %v", err)
	}
}

func TestDecrypt_TruncatedCiphertext(t *testing.T) {
	encoded := generateTestKey(t)
	key, _ := ParseRootKey(encoded)

	ct, _ := Encrypt(key, []byte("some-plaintext"))
	// 截断到只有 nonce 部分（12 字节 base64 后约 16 字符）
	truncated := ct[:16]
	_, err := Decrypt(key, truncated)
	if err == nil {
		t.Fatal("截断密文应解密失败")
	}
	if !errorsIs(err, ErrDecryptionFailed) {
		t.Fatalf("截断密文应返回 ErrDecryptionFailed, got: %v", err)
	}
}

func TestDecrypt_WrongKey(t *testing.T) {
	key1Encoded := generateTestKey(t)
	key1, _ := ParseRootKey(key1Encoded)

	key2Encoded := generateTestKey(t)
	key2, _ := ParseRootKey(key2Encoded)

	ct, _ := Encrypt(key1, []byte("secret"))
	_, err := Decrypt(key2, ct)
	if err == nil {
		t.Fatal("使用错误密钥应解密失败")
	}
	if !errorsIs(err, ErrDecryptionFailed) {
		t.Fatalf("错误密钥应返回 ErrDecryptionFailed, got: %v", err)
	}
}

func TestEncrypt_InvalidKeyLength(t *testing.T) {
	shortKey := make([]byte, 16)
	_, err := Encrypt(shortKey, []byte("test"))
	if err == nil {
		t.Fatal("16 字节密钥加密应失败")
	}
	if !strings.Contains(err.Error(), "密钥长度非法") {
		t.Fatalf("错误信息应包含密钥长度非法, got: %v", err)
	}
}

func TestDecrypt_InvalidKeyLength(t *testing.T) {
	encoded := generateTestKey(t)
	key, _ := ParseRootKey(encoded)
	ct, _ := Encrypt(key, []byte("test"))

	shortKey := make([]byte, 16)
	_, err := Decrypt(shortKey, ct)
	if err == nil {
		t.Fatal("16 字节密钥解密应失败")
	}
}

func TestDecrypt_InvalidBase64(t *testing.T) {
	encoded := generateTestKey(t)
	key, _ := ParseRootKey(encoded)

	_, err := Decrypt(key, "!!!not-base64!!!")
	if err == nil {
		t.Fatal("非法 base64 密文应返回错误")
	}
	if !errorsIs(err, ErrDecryptionFailed) {
		t.Fatalf("非法 base64 应返回 ErrDecryptionFailed, got: %v", err)
	}
}

// errorsIs 是 errors.Is 的本地包装，避免在测试文件中额外 import errors。
func errorsIs(err, target error) bool {
	return err != nil && (err == target || strings.Contains(err.Error(), target.Error()))
}
