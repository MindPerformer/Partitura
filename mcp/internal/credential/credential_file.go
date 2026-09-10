// credential_file.go 实现跨平台的加密文件凭据存储。
//
// 引入动机：早期实现按平台分派（Windows 使用 Credential Manager，其他平台为桩），
// 导致非 Windows 既无法编译也无法运行，且三平台行为不一致。
// 现统一为「加密文件存储」：凭据文件与配置同放在运行目录（当前工作目录）下的
// .knowledge-mcp 中，三平台共用同一实现。
//
// 安全说明（重要）：
// 本实现的 AES-256-GCM 密钥是源码中硬编码的固定常量。
// 固定密钥仅提供**混淆级别**的保护：任何拿到二进制的攻击者都可以逆向提取该密钥，
// 进而解密凭据文件，因此它**不等同于强加密**，也无法抵御本地有权用户读取运行目录。
// 引入该设计是为了满足「三平台统一、无外部密钥管理依赖、凭据不以明文落盘」的需求，
// 用于提升 accidentally-shared 文件或随意翻看目录时的泄露门槛，
// 而不是作为强安全边界。
//
// 存储格式：
//   - 路径：<dir>/.knowledge-mcp/.credentials
//   - 目录权限 0700，文件权限 0600
//   - 内容为 JSON 信封：{"v":1,"nonce":"<base64 std>","data":"<base64 std>"}
//   - 明文为 map[serverURL]Tokens 的 JSON，经 AES-256-GCM 加密后放入 data
//
// 写入策略：先写临时文件，再 os.Rename 原子覆盖目标文件；失败清理临时文件并返回错误。
//
// 不使用反射。
package credential

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

// 凭据文件相关的固定名称与权限。
const (
	// credentialDirName 是运行目录下存放配置与凭据的子目录名。
	credentialDirName = ".knowledge-mcp"

	// credentialFileName 是加密凭据文件名。
	credentialFileName = ".credentials"

	// credentialTempPattern 是原子写使用的临时文件模式（CreateTemp 会替换 *）。
	credentialTempPattern = ".credentials-*.tmp"

	// credentialFormatVersion 是当前凭据信封格式版本。
	// 引入动机：为后续格式演进保留版本位；Load 遇到未知版本必须 fail-fast。
	credentialFormatVersion = 1

	// credentialDirPerm 是凭据目录权限（仅属主可访问）。
	credentialDirPerm os.FileMode = 0o700

	// credentialFilePerm 是凭据文件权限（仅属主可读写）。
	// 注意：Windows 不体现 POSIX 权限位，实际访问控制依赖 ACL。
	credentialFilePerm os.FileMode = 0o600
)

// credentialEncryptionKey 是 AES-256-GCM 使用的 32 字节固定密钥。
// 引入动机：三平台统一且不引入外部密钥管理依赖，需要在源码内固定密钥。
// 安全说明：见文件顶部——固定密钥仅提供混淆级保护，可被逆向提取，不等同于强加密。
// 说明：声明为包级变量以便包内测试注入错误密钥验证解密失败路径，生产路径不做任何分支。
var credentialEncryptionKey = []byte("Partitura/knowledge-mcp-cred-key")

// credentialEnvelope 是凭据文件的 JSON 信封。
// 引入动机：把格式版本与 GCM 所需的 nonce 同密文一起持久化，便于版本校验与解密。
type credentialEnvelope struct {
	// V 是信封格式版本，当前恒为 credentialFormatVersion。
	V int `json:"v"`

	// Nonce 是 AES-GCM 的 nonce，base64 标准编码。
	Nonce string `json:"nonce"`

	// Data 是密文（含 GCM tag），base64 标准编码。
	Data string `json:"data"`
}

// FileStore 是基于加密文件的凭据存储实现。
// 引入动机：三平台统一使用加密文件存储，凭据文件与配置同放在运行目录。
type FileStore struct {
	// dir 是运行目录（通常是当前工作目录），凭据文件位于其下的 .knowledge-mcp 中。
	dir string
}

// NewStore 创建加密文件凭据存储。
// 引入动机：调用方（CLI）以运行目录为根创建凭据存储。
// dir 为空或不可用（不存在/非目录）时返回错误（fail-fast），不得退化到其他目录。
func NewStore(dir string) (*FileStore, error) {
	if dir == "" {
		return nil, fmt.Errorf("credential store directory is empty")
	}

	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("credential store directory is unusable %s: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("credential store path is not a directory: %s", dir)
	}

	return &FileStore{dir: dir}, nil
}

// dirPath 返回凭据目录（<dir>/.knowledge-mcp）。
func (s *FileStore) dirPath() string {
	return filepath.Join(s.dir, credentialDirName)
}

// filePath 返回凭据文件路径（<dir>/.knowledge-mcp/.credentials）。
func (s *FileStore) filePath() string {
	return filepath.Join(s.dirPath(), credentialFileName)
}

// Save 将指定 server 的 token 加密写入凭据文件。
// 引入动机：login 成功后需要持久化 token。
// 语义：保留其他 server 的既有条目，仅覆盖当前 server。
// 失败（读写、序列化、加密、落盘）必须返回错误，不静默降级。
func (s *FileStore) Save(serverURL string, tokens *Tokens) error {
	if tokens == nil {
		return failFastLog("save credentials", serverURL, fmt.Errorf("tokens is nil"))
	}

	creds, exists, err := s.loadCredentials()
	if err != nil {
		return failFastLog("save credentials", serverURL, err)
	}
	if !exists {
		creds = make(map[string]Tokens)
	}

	creds[serverURL] = *tokens

	if err := s.storeCredentials(creds); err != nil {
		return failFastLog("save credentials", serverURL, err)
	}
	return nil
}

// Load 读取指定 server 的 token。
// 引入动机：MCP 进程启动时需要加载已保存的 token。
// 语义：凭据文件不存在或条目不存在时返回空 Tokens 与 nil error（表示未登录）；
// 文件损坏、版本不匹配、解密/认证失败必须返回错误（fail-fast）。
func (s *FileStore) Load(serverURL string) (*Tokens, error) {
	creds, exists, err := s.loadCredentials()
	if err != nil {
		return nil, failFastLog("read credentials", serverURL, err)
	}
	if !exists {
		return &Tokens{}, nil
	}

	tokens, ok := creds[serverURL]
	if !ok {
		return &Tokens{}, nil
	}
	return &tokens, nil
}

// Delete 删除指定 server 的 token。
// 引入动机：logout/revoke 需要清理凭据。
// 语义：凭据文件不存在或条目不存在时返回 nil（幂等操作）。
func (s *FileStore) Delete(serverURL string) error {
	creds, exists, err := s.loadCredentials()
	if err != nil {
		return failFastLog("delete credentials", serverURL, err)
	}
	if !exists {
		return nil
	}
	if _, ok := creds[serverURL]; !ok {
		return nil
	}

	delete(creds, serverURL)

	if err := s.storeCredentials(creds); err != nil {
		return failFastLog("delete credentials", serverURL, err)
	}
	return nil
}

// loadCredentials 读取并解密凭据文件。
// 引入动机：Save/Load/Delete 都需要先取得当前凭据映射，集中处理读、版本校验与解密。
// 返回 exists=false 表示凭据文件不存在（调用方按「无凭据」处理，而非错误）。
func (s *FileStore) loadCredentials() (creds map[string]Tokens, exists bool, err error) {
	data, err := os.ReadFile(s.filePath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("failed to read credential file %s: %w", s.filePath(), err)
	}

	var env credentialEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, false, fmt.Errorf("failed to parse credential envelope: %w", err)
	}
	if env.V != credentialFormatVersion {
		return nil, false, fmt.Errorf("unsupported credential file version %d (expected %d)", env.V, credentialFormatVersion)
	}

	nonce, err := base64.StdEncoding.DecodeString(env.Nonce)
	if err != nil {
		return nil, false, fmt.Errorf("failed to decode nonce: %w", err)
	}
	sealed, err := base64.StdEncoding.DecodeString(env.Data)
	if err != nil {
		return nil, false, fmt.Errorf("failed to decode ciphertext: %w", err)
	}

	block, err := aes.NewCipher(credentialEncryptionKey)
	if err != nil {
		return nil, false, fmt.Errorf("failed to create AES cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, false, fmt.Errorf("failed to create GCM: %w", err)
	}

	plaintext, err := gcm.Open(nil, nonce, sealed, nil)
	if err != nil {
		return nil, false, fmt.Errorf("failed to decrypt credentials: %w", err)
	}

	result := make(map[string]Tokens)
	if err := json.Unmarshal(plaintext, &result); err != nil {
		return nil, false, fmt.Errorf("failed to parse credential content: %w", err)
	}
	return result, true, nil
}

// storeCredentials 加密并原子写入凭据映射。
// 引入动机：Save/Delete 都需要一致的加密与原子落盘逻辑。
func (s *FileStore) storeCredentials(creds map[string]Tokens) error {
	plaintext, err := json.Marshal(creds)
	if err != nil {
		return fmt.Errorf("failed to serialize credential content: %w", err)
	}

	env, err := sealCredentials(plaintext)
	if err != nil {
		return err
	}

	return s.writeEnvelope(env)
}

// sealCredentials 使用 AES-256-GCM 加密明文并封装为信封。
// 引入动机：把加密与信封构造集中，便于 Save 与测试复用。
func sealCredentials(plaintext []byte) (*credentialEnvelope, error) {
	block, err := aes.NewCipher(credentialEncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create AES cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCM: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("failed to generate nonce: %w", err)
	}

	sealed := gcm.Seal(nil, nonce, plaintext, nil)

	return &credentialEnvelope{
		V:     credentialFormatVersion,
		Nonce: base64.StdEncoding.EncodeToString(nonce),
		Data:  base64.StdEncoding.EncodeToString(sealed),
	}, nil
}

// writeEnvelope 原子写入凭据信封。
// 引入动机：先写临时文件再 Rename 覆盖，避免进程中断导致凭据文件半写损坏。
// 失败路径必须清理临时文件并返回错误。
func (s *FileStore) writeEnvelope(env *credentialEnvelope) error {
	dir := s.dirPath()
	if err := os.MkdirAll(dir, credentialDirPerm); err != nil {
		return fmt.Errorf("failed to create credential directory %s: %w", dir, err)
	}

	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("failed to serialize credential envelope: %w", err)
	}

	tmp, err := os.CreateTemp(dir, credentialTempPattern)
	if err != nil {
		return fmt.Errorf("failed to create temporary credential file: %w", err)
	}
	tmpPath := tmp.Name()

	if err := writeTempCredentialFile(tmp, data); err != nil {
		removeTempCredentialFile(tmpPath)
		return err
	}

	if err := os.Rename(tmpPath, s.filePath()); err != nil {
		removeTempCredentialFile(tmpPath)
		return fmt.Errorf("failed to replace credential file %s: %w", s.filePath(), err)
	}
	return nil
}

// writeTempCredentialFile 写入临时凭据文件内容并设置权限，最后关闭。
// 引入动机：集中写入/权限/关闭的失败处理，保证原子写路径能统一清理临时文件。
// 关闭错误在无更早错误时上报，避免吞错。
func writeTempCredentialFile(f *os.File, data []byte) (retErr error) {
	defer func() {
		if closeErr := f.Close(); closeErr != nil && retErr == nil {
			retErr = fmt.Errorf("failed to close temporary credential file: %w", closeErr)
		}
	}()

	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("failed to write temporary credential file: %w", err)
	}
	if err := f.Chmod(credentialFilePerm); err != nil {
		return fmt.Errorf("failed to set credential file permissions: %w", err)
	}
	return nil
}

// removeTempCredentialFile 清理原子写失败后残留的临时文件。
// 引入动机：失败路径不得在运行目录留下游离临时文件；清理失败必须记录日志，不得静默吞错。
func removeTempCredentialFile(path string) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		slog.Error("failed to clean up temporary credential file", "path", path, "error", err.Error())
	}
}
