// password.go 实现 Argon2id 密码哈希与校验。
//
// 引入动机：design/04-WEB-API.md §Security 要求使用 Argon2id 进行密码哈希。
// Argon2id 是 OWASP 推荐的密码哈希算法，兼顾抗 GPU/ASIC 攻击和侧信道攻击。
// 哈希结果以标准编码格式存储（$argon2id$v=...$m=...,t=...,p=...$salt$hash），
// 可直接存入 users.password_hash 字段。
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// ErrPasswordVerificationFailed 表示密码校验失败。
// 引入动机：调用方需要区分"密码不匹配"和其他错误（如哈希格式损坏）。
var ErrPasswordVerificationFailed = errors.New("密码校验失败")

// HashPassword 使用 Argon2id 对明文密码进行哈希。
//
// 参数：
//   - plain：明文密码
//   - cfg：Argon2id 参数配置
//
// 返回值是标准 Argon2id 编码格式字符串，可直接存入数据库。
// 该函数使用 crypto/rand 生成随机盐，确保每次哈希结果不同。
func HashPassword(plain string, cfg AuthConfig) (string, error) {
	salt := make([]byte, cfg.Argon2SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("生成 Argon2id 盐: %w", err)
	}

	key := argon2.IDKey([]byte(plain), salt, cfg.Argon2Iterations, cfg.Argon2Memory, cfg.Argon2Parallelism, cfg.Argon2KeyLength)

	b64Salt := base64.RawStdEncoding.EncodeToString(salt)
	b64Key := base64.RawStdEncoding.EncodeToString(key)

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, cfg.Argon2Memory, cfg.Argon2Iterations, cfg.Argon2Parallelism,
		b64Salt, b64Key), nil
}

// VerifyPassword 校验明文密码是否与给定的 Argon2id 哈希匹配。
//
// 参数：
//   - encoded：HashPassword 返回的标准编码格式哈希字符串
//   - plain：待校验的明文密码
//
// 如果密码匹配返回 nil；不匹配返回 ErrPasswordVerificationFailed；
// 哈希格式损坏返回其他错误。
//
// 使用 subtle.ConstantTimeCompare 进行常量时间比较，防止时序攻击。
func VerifyPassword(encoded, plain string) error {
	params, salt, expectedKey, err := decodeArgon2idHash(encoded)
	if err != nil {
		return fmt.Errorf("解析 Argon2id 哈希: %w", err)
	}

	actualKey := argon2.IDKey([]byte(plain), salt, params.iterations, params.memory, params.parallelism, uint32(len(expectedKey)))

	if subtle.ConstantTimeCompare(actualKey, expectedKey) != 1 {
		return ErrPasswordVerificationFailed
	}
	return nil
}

// argon2Params 是从编码哈希中解析出的 Argon2id 参数。
type argon2Params struct {
	memory      uint32
	iterations  uint32
	parallelism uint8
}

// decodeArgon2idHash 解析标准 Argon2id 编码格式字符串。
// 格式：$argon2id$v=<version>$m=<memory>,t=<iterations>,p=<parallelism>$<base64-salt>$<base64-key>
func decodeArgon2idHash(encoded string) (argon2Params, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 {
		return argon2Params{}, nil, nil, errors.New("Argon2id 哈希格式不正确：字段数不为 6")
	}

	if parts[1] != "argon2id" {
		return argon2Params{}, nil, nil, fmt.Errorf("哈希算法不是 argon2id，实际为 %q", parts[1])
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return argon2Params{}, nil, nil, fmt.Errorf("解析 Argon2id 版本: %w", err)
	}
	if version != argon2.Version {
		return argon2Params{}, nil, nil, fmt.Errorf("不支持的 Argon2id 版本 %d，期望 %d", version, argon2.Version)
	}

	var params argon2Params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &params.memory, &params.iterations, &params.parallelism); err != nil {
		return argon2Params{}, nil, nil, fmt.Errorf("解析 Argon2id 参数: %w", err)
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return argon2Params{}, nil, nil, fmt.Errorf("解码 Argon2id 盐: %w", err)
	}

	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return argon2Params{}, nil, nil, fmt.Errorf("解码 Argon2id 密钥: %w", err)
	}

	return params, salt, key, nil
}
