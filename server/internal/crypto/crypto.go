// Package crypto 实现 Provider API Key 的 AES-256-GCM 加解密。
//
// 引入动机：计划要求 Provider API Key 不得以明文存储于 PostgreSQL，
// 部署方通过 MASTER_ENCRYPTION_KEY 环境变量提供 base64 编码的 32 字节根密钥，
// 系统使用 AES-256-GCM 对 Provider API Key 进行加密后持久化。
//
// 安全原则：
//   - 根密钥严格校验：base64 解码后必须恰好 32 字节，否则 fail-fast
//   - 每次加密生成随机 12 字节 nonce，绝不复用
//   - GCM 认证失败（密文篡改/截断）必须返回错误，不得静默返回空值
//   - 错误信息中不包含任何密钥材料、nonce 或密文内容
//   - 不出现空 catch / 兜底 / 占位
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// ErrInvalidKey 表示根密钥格式非法或长度不正确。
// 引入动机：调用方需要区分根密钥问题和密文篡改问题，给出不同的 fail-fast 行为。
var ErrInvalidKey = errors.New("根密钥格式非法：必须为 base64 编码的 32 字节密钥")

// ErrDecryptionFailed 表示解密失败，通常由密文篡改、截断或根密钥不匹配导致。
// 引入动机：GCM 认证失败时必须明确返回错误，调用方据此 fail-fast 拒绝启动。
var ErrDecryptionFailed = errors.New("解密失败：密文可能已被篡改或根密钥不匹配")

// KeyLength 是 AES-256 所需的密钥字节长度。
// 引入动机：AES-256-GCM 要求密钥恰好 32 字节，常量化避免魔法数字。
const KeyLength = 32

// NonceLength 是 GCM 标准推荐的 12 字节 nonce 长度。
// 引入动机：GCM nonce 长度固定为 12 字节时性能最优且安全性充分。
const NonceLength = 12

// ParseRootKey 严格解析 MASTER_ENCRYPTION_KEY 环境变量值。
//
// 引入动机：部署方通过环境变量提供 base64 编码的 32 字节根密钥，
// 此函数严格校验格式——base64 解码后必须恰好 32 字节，否则返回 ErrInvalidKey。
//
// 参数：
//   - raw：环境变量的原始字符串值
//
// 返回 32 字节密钥或 ErrInvalidKey。错误信息不含密钥材料。
func ParseRootKey(raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, ErrInvalidKey
	}

	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: base64 解码失败", ErrInvalidKey)
	}

	if len(decoded) != KeyLength {
		return nil, fmt.Errorf("%w: 解码后长度为 %d 字节，期望 %d 字节", ErrInvalidKey, len(decoded), KeyLength)
	}

	return decoded, nil
}

// Encrypt 使用 AES-256-GCM 加密明文。
//
// 引入动机：Provider API Key 写入数据库前必须加密，使用随机 nonce 保证语义安全。
//
// 参数：
//   - key：32 字节根密钥（由 ParseRootKey 解析得到）
//   - plaintext：待加密的明文（如 Provider API Key）
//
// 返回 base64 编码的密文（nonce + ciphertext 拼接后 base64 编码）。
// 错误信息不含密钥材料或明文内容。
func Encrypt(key, plaintext []byte) (string, error) {
	if len(key) != KeyLength {
		return "", fmt.Errorf("密钥长度非法: %d 字节, 期望 %d 字节", len(key), KeyLength)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("创建 AES cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("创建 GCM: %w", err)
	}

	nonce := make([]byte, NonceLength)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("生成随机 nonce: %w", err)
	}

	// nonce 作为前缀拼接到密文之前，解密时按长度切分
	ciphertext := gcm.Seal(nonce, nonce, plaintext, nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// Decrypt 使用 AES-256-GCM 解密密文。
//
// 引入动机：从数据库读取 Provider API Key 密文后需要解密才能构造 Provider 实例。
// GCM 认证失败（密文篡改/截断/根密钥不匹配）时返回 ErrDecryptionFailed。
//
// 参数：
//   - key：32 字节根密钥（由 ParseRootKey 解析得到）
//   - encoded：base64 编码的密文（nonce + ciphertext 拼接）
//
// 返回解密后的明文。错误信息不含密钥材料或密文内容。
func Decrypt(key []byte, encoded string) ([]byte, error) {
	if len(key) != KeyLength {
		return nil, fmt.Errorf("密钥长度非法: %d 字节, 期望 %d 字节", len(key), KeyLength)
	}

	ciphertext, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("%w: base64 解码失败", ErrDecryptionFailed)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("创建 AES cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("创建 GCM: %w", err)
	}

	nonceSize := gcm.NonceSize()
	if nonceSize != NonceLength {
		return nil, fmt.Errorf("GCM nonce 长度异常: %d, 期望 %d", nonceSize, NonceLength)
	}

	if len(ciphertext) < nonceSize {
		return nil, fmt.Errorf("%w: 密文长度不足", ErrDecryptionFailed)
	}

	nonce, ciphertextBody := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertextBody, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: GCM 认证失败", ErrDecryptionFailed)
	}

	return plaintext, nil
}
