// Package protocol 实现 MCP（Model Context Protocol）的 JSON-RPC stdio 协议。
//
// 引入动机：design/02-MCP.md §架构 要求 MCP binary 通过 stdio 暴露工具给 Agent。
// stdin 接受 JSON-RPC 请求，stdout 仅发送规范协议消息，日志仅输出到 stderr。
//
// 协议方法：
//   - initialize：握手，返回 server 能力和 MCP Guidance
//   - tools/list：返回所有已注册工具的名称、描述和输入 schema
//   - tools/call：执行指定工具，返回结构化结果
//
// 安全原则：
//   - stdout 只发送 JSON-RPC 响应，不混杂日志
//   - 日志只输出到 stderr
//   - 不使用反射生成 schema，手动定义每个工具的 inputSchema
package protocol

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
)

// JSONRPC 请求和响应类型

// Request 是 JSON-RPC 2.0 请求。
type Request struct {
	// JSONRPC 固定为 "2.0"。
	JSONRPC string `json:"jsonrpc"`

	// ID 是请求 ID（整数或字符串）。
	ID interface{} `json:"id"`

	// Method 是方法名。
	Method string `json:"method"`

	// Params 是方法参数。
	Params json.RawMessage `json:"params,omitempty"`
}

// Response 是 JSON-RPC 2.0 响应。
type Response struct {
	// JSONRPC 固定为 "2.0"。
	JSONRPC string `json:"jsonrpc"`

	// ID 是请求 ID（与请求对应）。
	ID interface{} `json:"id"`

	// Result 是成功结果（与 Error 互斥）。
	Result interface{} `json:"result,omitempty"`

	// Error 是错误信息（与 Result 互斥）。
	Error *RPCError `json:"error,omitempty"`
}

// RPCError 是 JSON-RPC 错误对象。
type RPCError struct {
	// Code 是错误代码。
	Code int `json:"code"`

	// Message 是错误消息。
	Message string `json:"message"`

	// Data 是附加错误数据。
	Data interface{} `json:"data,omitempty"`
}

// JSON-RPC 标准错误代码
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// MCP 协议方法名
const (
	MethodInitialize = "initialize"
	MethodToolsList  = "tools/list"
	MethodToolsCall  = "tools/call"
)

// InitializeResult 是 initialize 方法的返回结果。
type InitializeResult struct {
	// ProtocolVersion 是 MCP 协议版本。
	ProtocolVersion string `json:"protocolVersion"`

	// ServerInfo 是 server 信息。
	ServerInfo ServerInfo `json:"serverInfo"`

	// Capabilities 是 server 能力声明。
	Capabilities map[string]interface{} `json:"capabilities"`

	// Instructions 是 MCP Guidance 文本，向 Agent 描述通用工作约束。
	// 引入动机：design/02-MCP.md §MCP Guidance 要求 MCP 向 Agent 告知通用工作约束。
	Instructions string `json:"instructions,omitempty"`
}

// ServerInfo 是 server 基本信息。
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Tool 定义一个 MCP 工具。
// 引入动机：tools/list 返回工具清单，tools/call 按名称执行工具。
type Tool struct {
	// Name 是工具名称。
	Name string `json:"name"`

	// Description 是工具描述。
	Description string `json:"description"`

	// InputSchema 是工具输入参数的 JSON Schema。
	// 引入动机：design 要求协议/工具 schema 不得使用反射生成。
	// 每个工具手动定义 inputSchema。
	InputSchema map[string]interface{} `json:"inputSchema"`
}

// ToolResult 是 tools/call 的返回结果。
type ToolResult struct {
	// Content 是结果内容列表。
	Content []ContentItem `json:"content"`

	// IsError 标记是否为工具执行错误（区别于协议错误）。
	IsError bool `json:"isError,omitempty"`
}

// ContentItem 是工具结果中的一项内容。
type ContentItem struct {
	// Type 是内容类型（如 "text"）。
	Type string `json:"type"`

	// Text 是文本内容。
	Text string `json:"text"`
}

// ToolHandler 是工具执行函数签名。
// 引入动机：每个工具注册一个处理函数，接收原始参数 JSON，返回结构化结果或错误。
type ToolHandler func(params json.RawMessage) (*ToolResult, error)

// Server 是 MCP stdio JSON-RPC server。
// 引入动机：MCP binary 作为 stdio server，从 stdin 读取请求，向 stdout 写入响应。
type Server struct {
	// tools 是已注册的工具映射。
	tools map[string]*Tool

	// handlers 是工具名称到处理函数的映射。
	handlers map[string]ToolHandler

	// instructions 是 MCP Guidance 文本。
	instructions string

	// mu 保护 tools 和 handlers 的并发访问。
	mu sync.RWMutex

	// reader 是 stdin 读取器。
	reader io.Reader

	// writer 是 stdout 写入器。
	writer io.Writer

	// logger 输出到 stderr。
	logger *slog.Logger
}

// NewServer 创建 MCP stdio server。
// 引入动机：MCP binary 启动时创建 server，注册所有工具，开始 stdio 循环。
//
// 参数：
//   - instructions：MCP Guidance 文本
func NewServer(instructions string) *Server {
	return &Server{
		tools:        make(map[string]*Tool),
		handlers:     make(map[string]ToolHandler),
		instructions: instructions,
		reader:       os.Stdin,
		writer:       os.Stdout,
		logger:       slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
}

// RegisterTool 注册一个 MCP 工具。
// 引入动机：所有工具在 server 启动前注册。
//
// 参数：
//   - tool：工具定义（名称、描述、inputSchema）
//   - handler：工具处理函数
func (s *Server) RegisterTool(tool *Tool, handler ToolHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tools[tool.Name] = tool
	s.handlers[tool.Name] = handler
}

// Run 启动 stdio JSON-RPC 循环。
// 引入动机：MCP binary 主循环——从 stdin 读取请求，处理，向 stdout 写入响应。
// 日志输出到 stderr，不污染 stdout。
func (s *Server) Run() error {
	scanner := bufio.NewScanner(s.reader)
	// 增大 buffer 以支持长请求
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		s.handleLine(line)
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("读取 stdin: %w", err)
	}

	return nil
}

// handleLine 处理一行 JSON-RPC 请求。
func (s *Server) handleLine(line string) {
	var req Request
	if err := json.Unmarshal([]byte(line), &req); err != nil {
		s.sendResponse(&Response{
			JSONRPC: "2.0",
			ID:      nil,
			Error: &RPCError{
				Code:    CodeParseError,
				Message: "解析 JSON 失败",
			},
		})
		return
	}

	// 通知（无 ID）不需要响应
	if req.ID == nil {
		s.logger.Debug("收到通知，不回复", "method", req.Method)
		return
	}

	resp := s.handleRequest(&req)
	s.sendResponse(resp)
}

// handleRequest 处理 JSON-RPC 请求并返回响应。
func (s *Server) handleRequest(req *Request) *Response {
	switch req.Method {
	case MethodInitialize:
		return s.handleInitialize(req)
	case MethodToolsList:
		return s.handleToolsList(req)
	case MethodToolsCall:
		return s.handleToolsCall(req)
	default:
		return &Response{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error: &RPCError{
				Code:    CodeMethodNotFound,
				Message: fmt.Sprintf("未知方法: %s", req.Method),
			},
		}
	}
}

// handleInitialize 处理 initialize 方法。
func (s *Server) handleInitialize(req *Request) *Response {
	result := InitializeResult{
		ProtocolVersion: "2024-11-05",
		ServerInfo: ServerInfo{
			Name:    "knowledge-mcp",
			Version: "1.0.0",
		},
		Capabilities: map[string]interface{}{
			"tools": map[string]interface{}{
				"listChanged": false,
			},
		},
		Instructions: s.instructions,
	}

	return &Response{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result:  result,
	}
}

// handleToolsList 处理 tools/list 方法。
func (s *Server) handleToolsList(req *Request) *Response {
	s.mu.RLock()
	defer s.mu.RUnlock()

	tools := make([]*Tool, 0, len(s.tools))
	for _, t := range s.tools {
		tools = append(tools, t)
	}

	return &Response{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result: map[string]interface{}{
			"tools": tools,
		},
	}
}

// handleToolsCall 处理 tools/call 方法。
func (s *Server) handleToolsCall(req *Request) *Response {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return &Response{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error: &RPCError{
				Code:    CodeInvalidParams,
				Message: "解析 tools/call 参数失败",
			},
		}
	}

	s.mu.RLock()
	handler, ok := s.handlers[params.Name]
	_, toolExists := s.tools[params.Name]
	s.mu.RUnlock()

	if !ok || !toolExists {
		return &Response{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error: &RPCError{
				Code:    CodeMethodNotFound,
				Message: fmt.Sprintf("未知工具: %s", params.Name),
			},
		}
	}

	result, err := handler(params.Arguments)
	if err != nil {
		// 工具执行错误——返回为工具结果中的 isError，而非 RPC 错误
		// 引入动机：MCP 协议区分协议错误和工具执行错误
		errorResult := &ToolResult{
			Content: []ContentItem{
				{Type: "text", Text: err.Error()},
			},
			IsError: true,
		}
		return &Response{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  errorResult,
		}
	}

	return &Response{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result:  result,
	}
}

// sendResponse 向 stdout 写入 JSON-RPC 响应。
// 引入动机：stdout 只发送协议消息，不混杂日志。
func (s *Server) sendResponse(resp *Response) {
	data, err := json.Marshal(resp)
	if err != nil {
		s.logger.Error("序列化响应失败", "error", err)
		return
	}

	// 直接写入 stdout，每条消息一行
	if _, err := s.writer.Write(append(data, '\n')); err != nil {
		s.logger.Error("写入 stdout 失败", "error", err)
	}
}

// TextResult 创建纯文本工具结果。
// 引入动机：测试和简单工具需要快速创建文本结果。
func TextResult(text string) *ToolResult {
	return &ToolResult{
		Content: []ContentItem{
			{Type: "text", Text: text},
		},
	}
}
