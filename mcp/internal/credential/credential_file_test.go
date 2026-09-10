// credential_file_test.go 对加密文件凭据存储做真实文件读写的逻辑验证。
//
// 引入动机：FileStore 是三平台唯一的凭据落盘实现，必须通过真实文件读写验证：
// 往返一致性与多 server 隔离、加密完整性（错误密钥/篡改密文/损坏文件）、
// 幂等语义、落盘权限、信封版本校验，而不是对源码做字符串包含检查。
//
// 说明：错误密钥场景通过在测试内替换包级 credentialEncryptionKey 实现，
// 生产代码不为测试暴露后门，也不引入任何分支。
package credential

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// newTestFileStore 在临时目录创建真实的加密文件凭据存储。
func newTestFileStore(t *testing.T) *FileStore {
	t.Helper()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore 失败: %v", err)
	}
	return store
}

// readRawEnvelope 读取并解析落盘的凭据信封（测试直接操作文件以构造异常场景）。
func readRawEnvelope(t *testing.T, store *FileStore) credentialEnvelope {
	t.Helper()
	data, err := os.ReadFile(store.filePath())
	if err != nil {
		t.Fatalf("读取凭据文件失败: %v", err)
	}
	var env credentialEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("解析凭据信封失败: %v", err)
	}
	return env
}

// writeRawEnvelope 将修改后的信封写回凭据文件。
func writeRawEnvelope(t *testing.T, store *FileStore, env credentialEnvelope) {
	t.Helper()
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("序列化凭据信封失败: %v", err)
	}
	if err := os.WriteFile(store.filePath(), data, credentialFilePerm); err != nil {
		t.Fatalf("写入凭据文件失败: %v", err)
	}
}

// TestFileStoreSaveLoadRoundTrip 验证保存后读取往返一致，且多个 server URL 相互隔离。
func TestFileStoreSaveLoadRoundTrip(t *testing.T) {
	store := newTestFileStore(t)

	tokensA := &Tokens{AccessToken: "access-a", RefreshToken: "refresh-a", ServerURL: "https://a.example.com"}
	tokensB := &Tokens{AccessToken: "access-b", RefreshToken: "refresh-b", ServerURL: "https://b.example.com"}

	if err := store.Save("https://a.example.com", tokensA); err != nil {
		t.Fatalf("Save A 失败: %v", err)
	}
	if err := store.Save("https://b.example.com", tokensB); err != nil {
		t.Fatalf("Save B 失败: %v", err)
	}

	// 使用同一目录上的新实例读取，证明数据真正落盘而非停留在内存。
	reopened, err := NewStore(store.dir)
	if err != nil {
		t.Fatalf("重新打开存储失败: %v", err)
	}

	gotA, err := reopened.Load("https://a.example.com")
	if err != nil {
		t.Fatalf("Load A 失败: %v", err)
	}
	if gotA.AccessToken != tokensA.AccessToken || gotA.RefreshToken != tokensA.RefreshToken || gotA.ServerURL != tokensA.ServerURL {
		t.Errorf("A 往返不一致: got %+v, want %+v", gotA, tokensA)
	}

	gotB, err := reopened.Load("https://b.example.com")
	if err != nil {
		t.Fatalf("Load B 失败: %v", err)
	}
	if gotB.AccessToken != tokensB.AccessToken || gotB.RefreshToken != tokensB.RefreshToken || gotB.ServerURL != tokensB.ServerURL {
		t.Errorf("B 往返不一致: got %+v, want %+v", gotB, tokensB)
	}

	// 隔离性：A 与 B 不串数据。
	if gotA.AccessToken == gotB.AccessToken {
		t.Errorf("不同 server 的凭据发生串用: %v", gotA.AccessToken)
	}

	// 覆盖同一 server 后再读取应得到新值。
	updated := &Tokens{AccessToken: "access-a2", RefreshToken: "refresh-a2", ServerURL: "https://a.example.com"}
	if err := reopened.Save("https://a.example.com", updated); err != nil {
		t.Fatalf("覆盖保存失败: %v", err)
	}
	gotA2, err := reopened.Load("https://a.example.com")
	if err != nil {
		t.Fatalf("覆盖后 Load 失败: %v", err)
	}
	if gotA2.AccessToken != updated.AccessToken {
		t.Errorf("覆盖后 AccessToken = %v, 期望 %v", gotA2.AccessToken, updated.AccessToken)
	}
	// 覆盖 A 不应影响 B。
	gotB2, err := reopened.Load("https://b.example.com")
	if err != nil {
		t.Fatalf("覆盖 A 后 Load B 失败: %v", err)
	}
	if gotB2.AccessToken != tokensB.AccessToken {
		t.Errorf("覆盖 A 后 B 被破坏: %v", gotB2.AccessToken)
	}
}

// TestFileStoreLoadNonexistent 验证凭据文件不存在时返回空 Tokens 与 nil error。
func TestFileStoreLoadNonexistent(t *testing.T) {
	store := newTestFileStore(t)

	tokens, err := store.Load("https://example.com")
	if err != nil {
		t.Fatalf("文件不存在时 Load 不应报错: %v", err)
	}
	if tokens == nil {
		t.Fatal("文件不存在时应返回非 nil 的空 Tokens")
	}
	if tokens.AccessToken != "" || tokens.RefreshToken != "" || tokens.ServerURL != "" {
		t.Errorf("文件不存在时应返回空 Tokens, got %+v", tokens)
	}
}

// TestFileStoreDeleteIdempotent 验证 Delete 对存在/不存在的条目都幂等。
func TestFileStoreDeleteIdempotent(t *testing.T) {
	store := newTestFileStore(t)

	tokensA := &Tokens{AccessToken: "access-a", RefreshToken: "refresh-a", ServerURL: "https://a.example.com"}
	tokensB := &Tokens{AccessToken: "access-b", RefreshToken: "refresh-b", ServerURL: "https://b.example.com"}
	if err := store.Save("https://a.example.com", tokensA); err != nil {
		t.Fatalf("Save A 失败: %v", err)
	}
	if err := store.Save("https://b.example.com", tokensB); err != nil {
		t.Fatalf("Save B 失败: %v", err)
	}

	// 删除存在的条目。
	if err := store.Delete("https://a.example.com"); err != nil {
		t.Fatalf("Delete 已存在条目失败: %v", err)
	}
	gotA, err := store.Load("https://a.example.com")
	if err != nil {
		t.Fatalf("删除后 Load 失败: %v", err)
	}
	if gotA.AccessToken != "" {
		t.Errorf("删除后仍读到 A: %v", gotA.AccessToken)
	}
	// 删除 A 不应影响 B。
	gotB, err := store.Load("https://b.example.com")
	if err != nil {
		t.Fatalf("删除 A 后 Load B 失败: %v", err)
	}
	if gotB.AccessToken != tokensB.AccessToken {
		t.Errorf("删除 A 后 B 被破坏: %v", gotB.AccessToken)
	}

	// 重复删除同一条目——幂等。
	if err := store.Delete("https://a.example.com"); err != nil {
		t.Fatalf("重复 Delete 应幂等: %v", err)
	}

	// 删除文件中不存在的条目——幂等。
	if err := store.Delete("https://never.example.com"); err != nil {
		t.Fatalf("Delete 不存在条目应幂等: %v", err)
	}

	// 删除文件不存在时的条目——幂等。
	empty := newTestFileStore(t)
	if err := empty.Delete("https://example.com"); err != nil {
		t.Fatalf("无凭据文件时 Delete 应幂等: %v", err)
	}
}

// TestFileStoreDeleteLastEntryThenLoad 验证删除最后一条后 Load 为空且 Delete 仍幂等。
func TestFileStoreDeleteLastEntryThenLoad(t *testing.T) {
	store := newTestFileStore(t)

	if err := store.Save("https://only.example.com", &Tokens{AccessToken: "only", RefreshToken: "r", ServerURL: "https://only.example.com"}); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}
	if err := store.Delete("https://only.example.com"); err != nil {
		t.Fatalf("Delete 失败: %v", err)
	}

	got, err := store.Load("https://only.example.com")
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if got.AccessToken != "" {
		t.Errorf("删除最后一条后应读到空 Tokens, got %+v", got)
	}
	if err := store.Delete("https://only.example.com"); err != nil {
		t.Fatalf("再次 Delete 应幂等: %v", err)
	}
}

// TestFileStoreWrongKeyFails 验证使用错误密钥时 Load 必须报错（fail-fast，不静默降级）。
func TestFileStoreWrongKeyFails(t *testing.T) {
	store := newTestFileStore(t)

	if err := store.Save("https://example.com", &Tokens{AccessToken: "secret", RefreshToken: "refresh", ServerURL: "https://example.com"}); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}

	original := credentialEncryptionKey
	// 使用等长的不同密钥，保证失败源于 GCM 认证而非密钥长度错误。
	replacement := make([]byte, len(original))
	for i := range replacement {
		replacement[i] = original[i] ^ 0xFF
	}
	credentialEncryptionKey = replacement
	defer func() { credentialEncryptionKey = original }()

	if _, err := store.Load("https://example.com"); err == nil {
		t.Fatal("错误密钥下 Load 应报错")
	}
}

// TestFileStoreTamperedCiphertextFails 验证篡改密文后 Load 必须报错。
func TestFileStoreTamperedCiphertextFails(t *testing.T) {
	store := newTestFileStore(t)

	if err := store.Save("https://example.com", &Tokens{AccessToken: "secret", RefreshToken: "refresh", ServerURL: "https://example.com"}); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}

	env := readRawEnvelope(t, store)
	sealed, err := base64.StdEncoding.DecodeString(env.Data)
	if err != nil {
		t.Fatalf("解码密文失败: %v", err)
	}
	sealed[len(sealed)-1] ^= 0x01
	env.Data = base64.StdEncoding.EncodeToString(sealed)
	writeRawEnvelope(t, store, env)

	if _, err := store.Load("https://example.com"); err == nil {
		t.Fatal("密文被篡改后 Load 应报错（GCM 认证失败）")
	}
}

// TestFileStoreVersionMismatchFails 验证信封版本不匹配时 Load 必须报错。
func TestFileStoreVersionMismatchFails(t *testing.T) {
	store := newTestFileStore(t)

	if err := store.Save("https://example.com", &Tokens{AccessToken: "secret", RefreshToken: "refresh", ServerURL: "https://example.com"}); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}

	env := readRawEnvelope(t, store)
	env.V = credentialFormatVersion + 1
	writeRawEnvelope(t, store, env)

	if _, err := store.Load("https://example.com"); err == nil {
		t.Fatal("信封版本不匹配时 Load 应报错")
	}
}

// TestFileStoreCorruptedFileFails 验证凭据文件内容损坏时 Load 必须报错。
func TestFileStoreCorruptedFileFails(t *testing.T) {
	store := newTestFileStore(t)

	if err := store.Save("https://example.com", &Tokens{AccessToken: "secret", RefreshToken: "refresh", ServerURL: "https://example.com"}); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}

	if err := os.WriteFile(store.filePath(), []byte("not-a-json-envelope"), credentialFilePerm); err != nil {
		t.Fatalf("写入损坏文件失败: %v", err)
	}

	if _, err := store.Load("https://example.com"); err == nil {
		t.Fatal("凭据文件损坏时 Load 应报错")
	}
}

// TestFileStoreEnvelopeFormat 验证落盘格式为 v=1 的 JSON 信封且密文非明文。
func TestFileStoreEnvelopeFormat(t *testing.T) {
	store := newTestFileStore(t)

	const accessMarker = "PLAINTEXT-ACCESS-MARKER"
	const refreshMarker = "PLAINTEXT-REFRESH-MARKER"
	if err := store.Save("https://example.com", &Tokens{
		AccessToken:  accessMarker,
		RefreshToken: refreshMarker,
		ServerURL:    "https://example.com",
	}); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}

	raw, err := os.ReadFile(store.filePath())
	if err != nil {
		t.Fatalf("读取凭据文件失败: %v", err)
	}
	if strings.Contains(string(raw), accessMarker) {
		t.Fatalf("凭据文件包含明文 access token: %s", string(raw))
	}
	if strings.Contains(string(raw), refreshMarker) {
		t.Fatalf("凭据文件包含明文 refresh token: %s", string(raw))
	}

	env := readRawEnvelope(t, store)
	if env.V != credentialFormatVersion {
		t.Errorf("信封 v = %d, 期望 %d", env.V, credentialFormatVersion)
	}
	if env.Nonce == "" {
		t.Error("信封 nonce 不应为空")
	}
	if env.Data == "" {
		t.Error("信封 data 不应为空")
	}
}

// TestFileStoreFilePermissions 验证落盘文件与目录权限。
func TestFileStoreFilePermissions(t *testing.T) {
	store := newTestFileStore(t)

	if err := store.Save("https://example.com", &Tokens{AccessToken: "secret", RefreshToken: "refresh", ServerURL: "https://example.com"}); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}

	info, err := os.Stat(store.filePath())
	if err != nil {
		t.Fatalf("Stat 凭据文件失败: %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("凭据文件不是常规文件: %v", info.Mode())
	}

	if runtime.GOOS == "windows" {
		// Windows 不使用 POSIX 权限位：Go 对普通文件统一报告 0666（只读为 0444），
		// 实际访问控制由 ACL 决定，无法在此断言 0600。创建时已显式指定 0600。
		t.Logf("Windows 平台不体现 POSIX 权限位，os.Stat 报告 %#o（创建时已指定 0600）", info.Mode().Perm())
		return
	}

	if perm := info.Mode().Perm(); perm != credentialFilePerm {
		t.Errorf("凭据文件权限 = %#o, 期望 %#o", perm, credentialFilePerm)
	}

	dirInfo, err := os.Stat(store.dirPath())
	if err != nil {
		t.Fatalf("Stat 凭据目录失败: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != credentialDirPerm {
		t.Errorf("凭据目录权限 = %#o, 期望 %#o", perm, credentialDirPerm)
	}
}

// TestFileStorePathLayout 验证凭据文件位于 <dir>/.knowledge-mcp/.credentials。
func TestFileStorePathLayout(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore 失败: %v", err)
	}

	want := filepath.Join(dir, ".knowledge-mcp", ".credentials")
	if store.filePath() != want {
		t.Errorf("凭据文件路径 = %s, 期望 %s", store.filePath(), want)
	}
}

// TestFileStoreNewStoreFailFast 验证 dir 为空或不可用时构造函数 fail-fast。
func TestFileStoreNewStoreFailFast(t *testing.T) {
	if _, err := NewStore(""); err == nil {
		t.Fatal("空目录应返回错误")
	}

	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := NewStore(missing); err == nil {
		t.Fatal("不存在的目录应返回错误")
	}

	file := filepath.Join(t.TempDir(), "regular-file")
	if err := os.WriteFile(file, []byte("x"), credentialFilePerm); err != nil {
		t.Fatalf("准备测试文件失败: %v", err)
	}
	if _, err := NewStore(file); err == nil {
		t.Fatal("非目录路径应返回错误")
	}
}

// TestEncryptionKeyLength 验证硬编码密钥为 32 字节（AES-256）。
func TestEncryptionKeyLength(t *testing.T) {
	if len(credentialEncryptionKey) != 32 {
		t.Fatalf("AES-256 密钥长度 = %d, 期望 32", len(credentialEncryptionKey))
	}
}
