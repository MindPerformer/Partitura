// token.go 实现安全随机 token 生成与 token 哈希。
//
// 引入动机：design/04-WEB-API.md §Security 要求 "token hashes only"——
// 数据库中只存储 token 的哈希，不存储明文。
// session token、CSRF token、device access token、refresh token 均使用此模块生成和哈希。
//
// 哈希算法选择 SHA-256：token 本身已有足够熵（32 字节随机），不需要慢哈希；
// SHA-256 足以防止彩虹表攻击（因为 token 空间远大于密码空间）。
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// tokenBytes 是生成的随机 token 的字节数长度。
// 32 字节 = 256 位熵，足以抵抗暴力枚举。
const tokenBytes = 32

// GenerateToken 生成一个密码学安全的随机 token。
// 返回十六进制编码的字符串（64 字符），可直接用于 cookie 值或 bearer token。
func GenerateToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("生成随机 token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// HashToken 对明文 token 计算 SHA-256 哈希。
// 返回十六进制编码的哈希字符串（64 字符），用于数据库存储。
//
// 引入动机：数据库只存储 token 哈希，即使数据库泄露攻击者也无法直接使用 token。
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
