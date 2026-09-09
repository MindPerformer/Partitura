// credential_windows.go 实现 Windows Credential Manager 的凭据存储。
//
// 引入动机：design/02-MCP.md §登录 要求 token 保存到 OS Credential Store。
// 当前运行环境为 Windows，使用 Windows Credential Manager（advapi32 CredWrite/CredRead/CredDelete）。
//
// 实现方式：
//   - 调用 advapi32.dll 的 CredWriteW、CredReadW、CredDeleteW 系统 API
//   - 使用 CRED_TYPE_GENERIC 通用凭据类型
//   - 不使用反射，直接通过 syscall 调用
//   - token 以 UTF-16 编码存储在 CredentialBlob 中
//
// 安全：
//   - token 不出现在日志中
//   - 存储失败 fail-fast
//   - 不退化为文件明文存储

//go:build windows

package credential

import (
	"encoding/json"
	"fmt"
	"syscall"
	"unsafe"
)

// Windows API 常量
const (
	credTypeGeneric      = 1
	credPersistLocal     = 2
	credMaxCredentialLen = 512
)

// advapi32 DLL 和函数指针
var (
	advapi32           = syscall.NewLazyDLL("advapi32.dll")
	procCredWrite      = advapi32.NewProc("CredWriteW")
	procCredRead       = advapi32.NewProc("CredReadW")
	procCredDelete     = advapi32.NewProc("CredDeleteW")
	procCredFree       = advapi32.NewProc("CredFree")
)

// CREDENTIAL 结构体映射 Windows CREDENTIALW
// 引入动机：CredWrite/CredRead 需要此结构体。
type credentialStruct struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16 // LPCWSTR
	Comment            *uint16 // LPCWSTR
	LastWritten        syscall.Filetime
	CredentialBlobSize uint32
	CredentialBlob     *byte   // LPBYTE
	Persist            uint32
	AttributeCount     uint32
	Attributes         uintptr // PCREDENTIAL_ATTRIBUTEW
	TargetAlias        *uint16 // LPCWSTR
	UserName           *uint16 // LPCWSTR
}

// WindowsStore 是 Windows Credential Manager 的凭据存储实现。
// 引入动机：当前运行环境为 Windows，需要真实 OS credential store。
type WindowsStore struct{}

// NewWindowsStore 创建 Windows Credential Manager 凭据存储实例。
func NewWindowsStore() *WindowsStore {
	return &WindowsStore{}
}

// Save 将 token 保存到 Windows Credential Manager。
func (s *WindowsStore) Save(serverURL string, tokens *Tokens) error {
	// 序列化 token 为 JSON
	data, err := json.Marshal(tokens)
	if err != nil {
		return failFastLog("保存凭据", serverURL, fmt.Errorf("序列化 token: %w", err))
	}

	target, err := syscall.UTF16PtrFromString(targetName(serverURL))
	if err != nil {
		return failFastLog("保存凭据", serverURL, fmt.Errorf("编码 target name: %w", err))
	}

	// CredentialBlob 需要通过 unsafe.Pointer 传递
	blobBytes := data
	var cred credentialStruct
	cred.Type = credTypeGeneric
	cred.TargetName = target
	cred.CredentialBlobSize = uint32(len(blobBytes))
	cred.CredentialBlob = &blobBytes[0]
	cred.Persist = credPersistLocal

	// 设置 Comment 为 server URL（不含 token）
	comment, err := syscall.UTF16PtrFromString("knowledge-mcp token for " + serverURL)
	if err != nil {
		return failFastLog("保存凭据", serverURL, fmt.Errorf("编码 comment: %w", err))
	}
	cred.Comment = comment

	r1, _, err := procCredWrite.Call(
		uintptr(unsafe.Pointer(&cred)),
		0,
	)
	if r1 == 0 {
		return failFastLog("保存凭据", serverURL, fmt.Errorf("CredWrite 失败: %w", err))
	}

	return nil
}

// Load 从 Windows Credential Manager 读取 token。
func (s *WindowsStore) Load(serverURL string) (*Tokens, error) {
	target, err := syscall.UTF16PtrFromString(targetName(serverURL))
	if err != nil {
		return nil, failFastLog("读取凭据", serverURL, fmt.Errorf("编码 target name: %w", err))
	}

	var pcred unsafe.Pointer
	r1, _, err := procCredRead.Call(
		uintptr(unsafe.Pointer(target)),
		credTypeGeneric,
		0,
		uintptr(unsafe.Pointer(&pcred)),
	)
	if r1 == 0 {
		// 凭据不存在——返回空 Tokens 和 nil error（表示未登录）
		if err == syscall.ERROR_NOT_FOUND {
			return &Tokens{}, nil
		}
		return nil, failFastLog("读取凭据", serverURL, fmt.Errorf("CredRead 失败: %w", err))
	}
	defer procCredFree.Call(uintptr(pcred))

	cred := (*credentialStruct)(pcred)

	// 读取 CredentialBlob
	if cred.CredentialBlobSize == 0 {
		return &Tokens{}, nil
	}

	blob := make([]byte, cred.CredentialBlobSize)
	copy(blob, (*[1 << 20]byte)(unsafe.Pointer(cred.CredentialBlob))[:cred.CredentialBlobSize])

	var tokens Tokens
	if err := json.Unmarshal(blob, &tokens); err != nil {
		return nil, failFastLog("读取凭据", serverURL, fmt.Errorf("反序列化 token: %w", err))
	}

	return &tokens, nil
}

// Delete 从 Windows Credential Manager 删除 token。
func (s *WindowsStore) Delete(serverURL string) error {
	target, err := syscall.UTF16PtrFromString(targetName(serverURL))
	if err != nil {
		return failFastLog("删除凭据", serverURL, fmt.Errorf("编码 target name: %w", err))
	}

	r1, _, err := procCredDelete.Call(
		uintptr(unsafe.Pointer(target)),
		credTypeGeneric,
		0,
	)
	if r1 == 0 {
		// 凭据不存在——幂等处理
		if err == syscall.ERROR_NOT_FOUND {
			return nil
		}
		return failFastLog("删除凭据", serverURL, fmt.Errorf("CredDelete 失败: %w", err))
	}

	return nil
}
