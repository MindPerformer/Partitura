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
			fmt.Fprintln(os.Stderr, "用法: knowledge-mcp login <server-url>")
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
	case "-h", "--help", "help":
		printHelp()
	default:
		fmt.Fprintf(os.Stderr, "未知命令: %s\n", os.Args[1])
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
  help                 显示帮助信息`)
}

// runLogin 执行 device authorization 登录流程。
// 引入动机：design/02-MCP.md §登录 要求 knowledge-mcp login <server> 命令。
func runLogin(serverURL string) {
	// 加载或创建配置
	cfg, err := config.Load()
	if err != nil {
		slog.Error("加载配置失败", "error", err)
		os.Exit(1)
	}

	cfg.Server = serverURL
	cfg.AllowInsecureTLS = false

	// 创建 REST client（login 前不需要 token）
	cli := client.NewClient(serverURL, &credAdapter{store: credential.NewWindowsStore()}, cfg.AllowInsecureTLS)

	// 启动 device authorization 流程
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	fmt.Fprintf(os.Stderr, "正在连接 %s ...\n", serverURL)

	// MCP 不要求用户输入设备名，自动生成稳定非敏感名称（OS + hostname hash）。
	// 不包含用户名、路径、IP 或 token。
	deviceName := generateDeviceName()
	startResp, err := cli.DeviceAuthStart(ctx, deviceName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "启动 device authorization 失败: %v\n", err)
		os.Exit(1)
	}

	// 显示用户授权信息
	fmt.Fprintf(os.Stderr, "\n")
	fmt.Fprintf(os.Stderr, "========================================\n")
	fmt.Fprintf(os.Stderr, "  请在浏览器中打开以下 URL 并输入授权码:\n")
	fmt.Fprintf(os.Stderr, "  URL: %s\n", startResp.VerificationURL)
	fmt.Fprintf(os.Stderr, "  授权码: %s\n", startResp.UserCode)
	fmt.Fprintf(os.Stderr, "========================================\n")
	fmt.Fprintf(os.Stderr, "\n")
	fmt.Fprintf(os.Stderr, "等待授权... (有效期 %d 秒)\n", startResp.ExpiresIn)

	// 尝试按 OS 安全打开 URL；失败仅向 stderr 输出可复制链接，不阻断轮询。
	if err := openBrowser(startResp.VerificationURL); err != nil {
		fmt.Fprintf(os.Stderr, "无法自动打开浏览器: %v\n", err)
		fmt.Fprintf(os.Stderr, "请手动复制上方 URL 到浏览器打开。\n")
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
				fmt.Fprintf(os.Stderr, "\n授权被拒绝。\n")
				os.Exit(1)
			}
		if err == client.ErrDeviceCodeExpired {
			fmt.Fprintf(os.Stderr, "\n授权码已过期，请重新登录。\n")
			os.Exit(1)
		}
		if err == client.ErrAuthorizationCompleted {
			// 授权已被一次性交换完成，另一进程/轮询已领取 token。
			// 本进程无法再从 server 获取 token，必须重新登录。
			fmt.Fprintf(os.Stderr, "\n授权已完成（token 已由先前轮询领取），请重新登录。\n")
			os.Exit(1)
		}
			fmt.Fprintf(os.Stderr, "\n轮询失败: %v\n", err)
			os.Exit(1)
		}

		if pollResp.Status == "authorized" {
			fmt.Fprintf(os.Stderr, "\n授权成功！\n")

			// 保存 token 到 OS credential store
			tokens := &client.CredentialTokens{
				AccessToken:  pollResp.AccessToken,
				RefreshToken: pollResp.RefreshToken,
				ServerURL:    serverURL,
			}
			cli.SetTokens(tokens)
			if err := cli.SaveTokens(); err != nil {
				fmt.Fprintf(os.Stderr, "保存凭据失败: %v\n", err)
				os.Exit(1)
			}

			// 保存配置
			if err := config.Save(cfg); err != nil {
				fmt.Fprintf(os.Stderr, "保存配置失败: %v\n", err)
				os.Exit(1)
			}

			fmt.Fprintf(os.Stderr, "登录成功。token 已保存到操作系统凭据存储。\n")
			return
		}
	}

	fmt.Fprintf(os.Stderr, "\n授权超时，请重试。\n")
	os.Exit(1)
}

// runLogout 清除已保存的凭据。
// 引入动机：design/02-MCP.md §登录 要求 logout 命令清除凭据并撤销 session。
// 安全流程：
//   1. 先尝试调用 server 撤销当前 MCP/device session
//   2. 无论远端 revoke 是否成功，都删除本地凭据
//   3. 清除 active workspace 配置
//   4. 远端 revoke 失败时记录 stderr，但不阻止本地清理
//   5. 不得输出 token
func runLogout() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "加载配置失败: %v\n", err)
		os.Exit(1)
	}

	if cfg.Server == "" {
		fmt.Fprintln(os.Stderr, "未配置 server")
		os.Exit(1)
	}

	store := credential.NewWindowsStore()
	credAdapter := &credAdapter{store: store}

	// 创建 REST client 以调用 server revoke
	cli := client.NewClient(cfg.Server, credAdapter, cfg.AllowInsecureTLS)
	if err := cli.LoadTokens(); err != nil {
		// 凭据加载失败——记录 stderr 但继续清理本地
		fmt.Fprintf(os.Stderr, "警告: 加载凭据失败，跳过远端 revoke: %v\n", err)
	} else if cli.IsLoggedIn() {
		// 尝试调用 server 撤销 session
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		revokeErr := cli.RevokeSession(ctx)
		cancel()
		if revokeErr != nil {
			// 远端 revoke 失败——记录 stderr，但不阻止本地清理
			// 安全策略：远端失败可能是网络问题或 server 不可达，
			// 本地凭据仍应删除以防止用户以为仍处于登录状态
			fmt.Fprintf(os.Stderr, "警告: 远端 session 撤销失败: %v\n", revokeErr)
		}
	}

	// 删除本地凭据（无论远端 revoke 是否成功）
	if err := store.Delete(cfg.Server); err != nil {
		fmt.Fprintf(os.Stderr, "清除本地凭据失败: %v\n", err)
		os.Exit(1)
	}

	// 清除 active workspace
	cfg.ActiveWorkspace = ""
	if err := config.Save(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "清除 active workspace 配置失败: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintln(os.Stderr, "已退出登录，凭据已清除。")
}

// runServe 启动 stdio MCP server。
// 引入动机：MCP binary 作为 stdio server，从 stdin 读取 JSON-RPC 请求，向 stdout 写入响应。
func runServe() {
	// 加载配置
	cfg, err := config.Load()
	if err != nil {
		slog.Error("加载配置失败", "error", err)
		os.Exit(1)
	}

	if cfg.Server == "" {
		slog.Error("未配置 server URL，请先执行 knowledge-mcp login <server-url>")
		os.Exit(1)
	}

	// 创建凭据存储
	credStore := credential.NewWindowsStore()

	// 创建 REST client
	cli := client.NewClient(cfg.Server, &credAdapter{store: credStore}, cfg.AllowInsecureTLS)

	// 加载已保存的 token
	if err := cli.LoadTokens(); err != nil {
		slog.Error("加载凭据失败", "error", err)
		os.Exit(1)
	}

	if !cli.IsLoggedIn() {
		slog.Error("未登录，请先执行 knowledge-mcp login <server-url>")
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
		slog.Error("解析 cache TTL 失败", "error", err)
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
		slog.Error("MCP server 运行失败", "error", err)
		os.Exit(1)
	}
}

// runServerList 列出已配置的 server。
func runServerList() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "加载配置失败: %v\n", err)
		os.Exit(1)
	}
	if cfg.Server == "" {
		fmt.Println("未配置 server")
		return
	}
	fmt.Printf("Server: %s\n", cfg.Server)
}

// runServerCurrent 显示当前 server。
func runServerCurrent() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "加载配置失败: %v\n", err)
		os.Exit(1)
	}
	if cfg.Server == "" {
		fmt.Println("未配置 server")
		return
	}
	fmt.Println(cfg.Server)
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
		return fmt.Errorf("不支持的操作系统: %s", runtime.GOOS)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动浏览器: %w", err)
	}
	return nil
}

// credAdapter 将 credential.Store 适配为 client.CredentialStore。
// 引入动机：client 包定义了独立的 CredentialStore 接口，
// 需要适配器桥接 credential.Store 到 client.CredentialStore。
type credAdapter struct {
	store *credential.WindowsStore
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
