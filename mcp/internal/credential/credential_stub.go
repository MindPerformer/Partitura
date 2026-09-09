// credential_stub.go 为非 Windows 平台提供凭据存储的桩实现。
//
// 引入动机：当前运行环境为 Windows，但 Go 编译需要所有平台都有实现。
// 非 Windows 平台返回明确错误，不退化为文件明文存储。
// 生产环境应在对应平台实现 keychain/libsecret 等真实 credential store。

//go:build !windows

package credential

import (
	"fmt"
)

// StubStore 是非 Windows 平台的桩实现。
// 引入动机：保持编译通过，但不提供真实 credential store 功能。
type StubStore struct{}

// NewWindowsStore 在非 Windows 平台返回错误。
// 引入动机：统一构造函数名，非 Windows 平台调用时明确报错。
func NewWindowsStore() *StubStore {
	return &StubStore{}
}

func (s *StubStore) Save(serverURL string, tokens *Tokens) error {
	return failFastLog("保存凭据", serverURL, fmt.Errorf("当前平台不支持 Windows Credential Manager"))
}

func (s *StubStore) Load(serverURL string) (*Tokens, error) {
	return nil, failFastLog("读取凭据", serverURL, fmt.Errorf("当前平台不支持 Windows Credential Manager"))
}

func (s *StubStore) Delete(serverURL string) error {
	return failFastLog("删除凭据", serverURL, fmt.Errorf("当前平台不支持 Windows Credential Manager"))
}
