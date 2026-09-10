// Package main 是 knowledge-mcp CLI 的入口点。
//
// 引入动机：design/02-MCP.md §架构 要求本地 Go MCP binary，
// 通过 stdio 暴露给 Agent，通过 HTTPS REST 与 Knowledge Server 通信。
//
// 命令：
//   - knowledge-mcp login <server>     — device authorization 登录流程
//   - knowledge-mcp logout             — 清除凭据
//   - knowledge-mcp serve              — 启动 stdio MCP server（默认命令）
//   - knowledge-mcp server-list        — 列出已配置的 server
//   - knowledge-mcp server-current     — 显示当前 server
//   - knowledge-mcp version            — 显示构建版本信息
//
// 配置与凭据路径：三平台统一使用**运行目录**（当前工作目录），
// 位于 <cwd>/.knowledge-mcp/ 下（config.toml 与加密凭据文件 .credentials）。
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"time"

	"partitura/mcp/internal/buildinfo"
	"partitura/mcp/internal/cache"
	"partitura/mcp/internal/client"
	"partitura/mcp/internal/config"
	"partitura/mcp/internal/credential"
	"partitura/mcp/internal/guidance"
	"partitura/mcp/internal/protocol"
	"partitura/mcp/internal/tool"
	"partitura/mcp/internal/workspace"
)

func main() {
	if len(os.Args) < 2 {
		// 默认启动 stdio MCP server
		runServe()
		return
	}

	switch os.Args[1] {
	case "login":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: knowledge-mcp login <server-url>")
			os.Exit(1)
		}
		runLogin(os.Args[2])
	case "logout":
		runLogout()
	case "serve":
		runServe()
	case "server-list":
		runServerList()
	case "server-current":
		runServerCurrent()
	case "version":
		runVersion()
	case "-h", "--help", "help":
		printHelp()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		printHelp()
		os.Exit(1)
	}
}

// printHelp 打印帮助信息。
func printHelp() {
	fmt.Fprintln(os.Stderr, `knowledge-mcp — Project Knowledge Workspace MCP Client

用法:
  knowledge-mcp [命令]

命令:
  login <server-url>   通过 device authorization 登录到 Knowledge Server
  logout               清除已保存的凭据
  serve                启动 stdio MCP server（默认）
  server-list          列出已配置的 server
  server-current       显示当前 server
  version              显示构建版本信息
  help                 显示帮助信息`)
}

// runLogin 执行 device authorization 登录流程。
// 引入动机：design/02-MCP.md §登录 要求 knowledge-mcp login <server> 命令。
func runLogin(serverURL string) {
	// 加载或创建配置
	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	cfg.Server = serverURL
	cfg.AllowInsecureTLS = false

	// 创建凭据存储（运行目录下的加密文件存储）
	store, err := newCredentialStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize credential store: %v\n", err)
		os.Exit(1)
	}

	// 创建 REST client（login 前不需要 token）
	cli := client.NewClient(serverURL, &credAdapter{store: store}, cfg.AllowInsecureTLS)

	// 启动 device authorization 流程
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	fmt.Fprintf(os.Stderr, "connecting to %s ...\n", serverURL)

	// MCP 不要求用户输入设备名，自动生成稳定非敏感名称（OS + hostname hash）。
	// 不包含用户名、路径、IP 或 token。
	deviceName := generateDeviceName()
	startResp, err := cli.DeviceAuthStart(ctx, deviceName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to start device authorization: %v\n", err)
		os.Exit(1)
	}

	// 显示用户授权信息
	fmt.Fprintf(os.Stderr, "\n")
	fmt.Fprintf(os.Stderr, "========================================\n")
	fmt.Fprintf(os.Stderr, "  Open the following URL in your browser and enter the code:\n")
	fmt.Fprintf(os.Stderr, "  URL: %s\n", startResp.VerificationURL)
	fmt.Fprintf(os.Stderr, "  Code: %s\n", startResp.UserCode)
	fmt.Fprintf(os.Stderr, "========================================\n")
	fmt.Fprintf(os.Stderr, "\n")
	fmt.Fprintf(os.Stderr, "Waiting for authorization... (expires in %d seconds)\n", startResp.ExpiresIn)

	// 尝试按 OS 安全打开 URL；失败仅向 stderr 输出可复制链接，不阻断轮询。
	if err := openBrowser(startResp.VerificationURL); err != nil {
		fmt.Fprintf(os.Stderr, "could not open the browser automatically: %v\n", err)
		fmt.Fprintf(os.Stderr, "Copy the URL above into your browser manually.\n")
	}

	// 轮询授权状态
	interval := time.Duration(startResp.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}

	deadline := time.Now().Add(time.Duration(startResp.ExpiresIn) * time.Second)

	for time.Now().Before(deadline) {
		time.Sleep(interval)

		pollResp, err := cli.DeviceAuthPoll(ctx, startResp.DeviceCode)
		if err != nil {
			if err == client.ErrPendingAuthorization {
				fmt.Fprintf(os.Stderr, ".")
				continue
			}
			if err == client.ErrAuthorizationDenied {
				fmt.Fprintf(os.Stderr, "\nAuthorization denied.\n")
				os.Exit(1)
			}
			if err == client.ErrDeviceCodeExpired {
				fmt.Fprintf(os.Stderr, "\nThe code has expired; log in again.\n")
				os.Exit(1)
			}
			if err == client.ErrAuthorizationCompleted {
				// 授权已被一次性交换完成，另一进程/轮询已领取 token。
				// 本进程无法再从 server 获取 token，必须重新登录。
				fmt.Fprintf(os.Stderr, "\nAuthorization already completed (the token was claimed by an earlier poll); log in again.\n")
				os.Exit(1)
			}
			fmt.Fprintf(os.Stderr, "\npolling failed: %v\n", err)
			os.Exit(1)
		}

		if pollResp.Status == "authorized" {
			fmt.Fprintf(os.Stderr, "\nAuthorization succeeded.\n")

			// 保存 token 到本地加密凭据文件
			tokens := &client.CredentialTokens{
				AccessToken:  pollResp.AccessToken,
				RefreshToken: pollResp.RefreshToken,
				ServerURL:    serverURL,
			}
			cli.SetTokens(tokens)
			if err := cli.SaveTokens(); err != nil {
				fmt.Fprintf(os.Stderr, "failed to save credentials: %v\n", err)
				os.Exit(1)
			}

			// 保存配置
			if err := config.Save(cfg); err != nil {
				fmt.Fprintf(os.Stderr, "failed to save config: %v\n", err)
				os.Exit(1)
			}

			fmt.Fprintf(os.Stderr, "Login succeeded. The token was saved to the local encrypted credential file.\n")
			return
		}
	}

	fmt.Fprintf(os.Stderr, "\nAuthorization timed out; try again.\n")
	os.Exit(1)
}

// runLogout 清除已保存的凭据。
// 引入动机：design/02-MCP.md §登录 要求 logout 命令清除凭据并撤销 session。
// 安全流程：
//  1. 先尝试调用 server 撤销当前 MCP/device session
//  2. 无论远端 revoke 是否成功，都删除本地凭据
//  3. 清除 active workspace 配置
//  4. 远端 revoke 失败时记录 stderr，但不阻止本地清理
//  5. 不得输出 token
func runLogout() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	if cfg.Server == "" {
		fmt.Fprintln(os.Stderr, "server is not configured")
		os.Exit(1)
	}

	store, err := newCredentialStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize credential store: %v\n", err)
		os.Exit(1)
	}
	credAdapter := &credAdapter{store: store}

	// 创建 REST client 以调用 server revoke
	cli := client.NewClient(cfg.Server, credAdapter, cfg.AllowInsecureTLS)
	if err := cli.LoadTokens(); err != nil {
		// 凭据加载失败——记录 stderr 但继续清理本地
		fmt.Fprintf(os.Stderr, "warning: failed to load credentials; skipping remote revoke: %v\n", err)
	} else if cli.IsLoggedIn() {
		// 尝试调用 server 撤销 session
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		revokeErr := cli.RevokeSession(ctx)
		cancel()
		if revokeErr != nil {
			// 远端 revoke 失败——记录 stderr，但不阻止本地清理
			// 安全策略：远端失败可能是网络问题或 server 不可达，
			// 本地凭据仍应删除以防止用户以为仍处于登录状态
			fmt.Fprintf(os.Stderr, "warning: failed to revoke the remote session: %v\n", revokeErr)
		}
	}

	// 删除本地凭据（无论远端 revoke 是否成功）
	if err := store.Delete(cfg.Server); err != nil {
		fmt.Fprintf(os.Stderr, "failed to clear local credentials: %v\n", err)
		os.Exit(1)
	}

	// 清除 active workspace
	cfg.ActiveWorkspace = ""
	if err := config.Save(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "failed to clear the active workspace setting: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintln(os.Stderr, "Logged out; credentials cleared.")
}

// runServe 启动 stdio MCP server。
// 引入动机：MCP binary 作为 stdio server，从 stdin 读取 JSON-RPC 请求，向 stdout 写入响应。
func runServe() {
	// 加载配置
	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	if cfg.Server == "" {
		slog.Error("server URL is not configured; run knowledge-mcp login <server-url> first")
		os.Exit(1)
	}

	// 创建凭据存储（运行目录下的加密文件存储）
	credStore, err := newCredentialStore()
	if err != nil {
		slog.Error("failed to initialize credential store", "error", err)
		os.Exit(1)
	}

	// 创建 REST client
	cli := client.NewClient(cfg.Server, &credAdapter{store: credStore}, cfg.AllowInsecureTLS)

	// 加载已保存的 token
	if err := cli.LoadTokens(); err != nil {
		slog.Error("failed to load credentials", "error", err)
		os.Exit(1)
	}

	if !cli.IsLoggedIn() {
		slog.Error("not logged in; run knowledge-mcp login <server-url> first")
		os.Exit(1)
	}

	// 创建 workspace 状态
	wsState := workspace.NewState()

	// 如果配置中有 active workspace，恢复状态
	if cfg.ActiveWorkspace != "" {
		// 验证 workspace 是否仍可访问
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		var ws struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		}
		if err := cli.Get(ctx, "/api/workspaces/"+cfg.ActiveWorkspace, &ws); err == nil {
			wsState.Switch(ws.ID, ws.DisplayName)
		}
		cancel()
	}

	// 创建缓存
	ttl, err := cfg.ParseCacheTTL()
	if err != nil {
		slog.Error("failed to parse cache TTL", "error", err)
		os.Exit(1)
	}
	memCache := cache.New(ttl, cfg.CacheMaxEntries)

	// 创建工具注册中心
	registry := tool.NewRegistry(cli, wsState, memCache, cfg.DefaultSearchMode, cfg.SearchResultLimit)

	// 创建 MCP server
	server := protocol.NewServer(guidance.MCPGuidance)

	// 注册所有工具
	registry.RegisterAll(server)

	// 启动 stdio 循环
	if err := server.Run(); err != nil {
		slog.Error("MCP server failed", "error", err)
		os.Exit(1)
	}
}

// runServerList 列出已配置的 server。
func runServerList() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}
	if cfg.Server == "" {
		fmt.Println("server is not configured")
		return
	}
	fmt.Printf("Server: %s\n", cfg.Server)
}

// runServerCurrent 显示当前 server。
func runServerCurrent() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}
	if cfg.Server == "" {
		fmt.Println("server is not configured")
		return
	}
	fmt.Println(cfg.Server)
}

// runVersion 打印构建版本信息。
// 引入动机：便于确认部署的 knowledge-mcp 二进制版本、提交号和构建时间。
// 版本由构建时的 -ldflags 注入（见 internal/buildinfo）。
func runVersion() {
	fmt.Printf("knowledge-mcp %s\n", buildinfo.Version)
	fmt.Printf("commit: %s\n", buildinfo.Commit)
	fmt.Printf("date: %s\n", buildinfo.Date)
}

// newCredentialStore 以运行目录（当前工作目录）为根创建加密文件凭据存储。
// 引入动机：三平台统一使用加密文件存储，凭据目录与配置同为运行目录。
// os.Getwd 或 credential.NewStore 失败必须 fail-fast，不得忽略错误或退化到其他目录。
func newCredentialStore() (*credential.FileStore, error) {
	dir, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("failed to get the current working directory: %w", err)
	}
	store, err := credential.NewStore(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to create credential store: %w", err)
	}
	return store, nil
}

// generateDeviceName 生成稳定且非敏感的 device name。
// 引入动机：MCP login 不要求用户输入设备名。
// 格式：mcp-{os}-{sha256(hostname)前8位}。
// 不包含用户名、路径、IP 或 token；hostname 被哈希以保护隐私。
func generateDeviceName() string {
	host := "unknown"
	if h, err := os.Hostname(); err == nil && h != "" {
		host = h
	}
	sum := sha256.Sum256([]byte(host))
	short := hex.EncodeToString(sum[:])[:8]
	return fmt.Sprintf("mcp-%s-%s", runtime.GOOS, short)
}

// openBrowser 尝试按 OS 打开授权 URL。
// 引入动机：design/02-MCP.md §登录 要求自动打开浏览器，失败时仍可手动复制链接。
// 返回值非 nil 表示打开失败，调用方应把 URL 输出到 stderr 让用户复制。
// 安全：不将 URL 写入日志文件，仅返回错误。
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux", "freebsd", "openbsd":
		cmd = exec.Command("xdg-open", url)
	default:
		return fmt.Errorf("unsupported operating system: %s", runtime.GOOS)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to launch browser: %w", err)
	}
	return nil
}

// credAdapter 将 credential.Store 适配为 client.CredentialStore。
// 引入动机：client 包定义了独立的 CredentialStore 接口，
// 需要适配器桥接 credential.Store 到 client.CredentialStore。
// store 使用 credential.Store 接口，使适配器不依赖具体实现（加密文件存储或测试替身）。
type credAdapter struct {
	store credential.Store
}

func (a *credAdapter) Load(serverURL string) (*client.CredentialTokens, error) {
	tokens, err := a.store.Load(serverURL)
	if err != nil {
		return nil, err
	}
	if tokens == nil {
		return &client.CredentialTokens{}, nil
	}
	return &client.CredentialTokens{
		AccessToken:  tokens.AccessToken,
		RefreshToken: tokens.RefreshToken,
		ServerURL:    tokens.ServerURL,
	}, nil
}

func (a *credAdapter) Save(serverURL string, tokens *client.CredentialTokens) error {
	return a.store.Save(serverURL, &credential.Tokens{
		AccessToken:  tokens.AccessToken,
		RefreshToken: tokens.RefreshToken,
		ServerURL:    serverURL,
	})
}

func (a *credAdapter) Delete(serverURL string) error {
	return a.store.Delete(serverURL)
}
