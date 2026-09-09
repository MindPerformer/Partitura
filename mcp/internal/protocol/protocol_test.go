// protocol_test.go 测试 MCP stdio JSON-RPC 协议实现。
//
// 引入动机：design/06-IMPLEMENTATION.md Phase 4 验收要求：
//   - stdio initialize、tools/list、tools/call 验证
//   - stdout 仅协议 JSON、日志不污染 stdout
//   - 工具 schema 含明确输入
package protocol

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// runServerWithInput 使用给定输入运行 server 并捕获 stdout 输出。
func runServerWithInput(t *testing.T, input string, server *Server) string {
	t.Helper()

	var stdout bytes.Buffer
	server.reader = strings.NewReader(input)
	server.writer = &stdout

	if err := server.Run(); err != nil {
		t.Fatalf("server.Run 失败: %v", err)
	}

	return stdout.String()
}

// parseResponses 解析 stdout 中的多行 JSON-RPC 响应。
func parseResponses(t *testing.T, output string) []Response {
	t.Helper()

	var responses []Response
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var resp Response
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			t.Fatalf("解析响应失败: %v, line: %s", err, line)
		}
		responses = append(responses, resp)
	}
	return responses
}

func TestInitialize(t *testing.T) {
	server := NewServer("test guidance")

	input := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`
	output := runServerWithInput(t, input, server)

	responses := parseResponses(t, output)
	if len(responses) != 1 {
		t.Fatalf("期望 1 个响应，得到 %d", len(responses))
	}

	resp := responses[0]
	if resp.JSONRPC != "2.0" {
		t.Errorf("JSONRPC = %s, 期望 2.0", resp.JSONRPC)
	}
	if resp.ID != float64(1) {
		t.Errorf("ID = %v, 期望 1", resp.ID)
	}
	if resp.Error != nil {
		t.Fatalf("不期望错误: %v", resp.Error)
	}

	// 验证 initialize 结果
	result, ok := resp.Result.(map[string]interface{})
	if !ok {
		t.Fatalf("result 类型错误: %T", resp.Result)
	}
	if result["protocolVersion"] != "2024-11-05" {
		t.Errorf("protocolVersion = %v", result["protocolVersion"])
	}
	if result["instructions"] != "test guidance" {
		t.Errorf("instructions = %v, 期望 test guidance", result["instructions"])
	}

	serverInfo, ok := result["serverInfo"].(map[string]interface{})
	if !ok {
		t.Fatalf("serverInfo 类型错误")
	}
	if serverInfo["name"] != "knowledge-mcp" {
		t.Errorf("serverInfo.name = %v", serverInfo["name"])
	}
}

func TestToolsList(t *testing.T) {
	server := NewServer("")

	// 注册测试工具
	server.RegisterTool(&Tool{
		Name:        "test_tool",
		Description: "测试工具",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"param1": map[string]interface{}{"type": "string"},
			},
			"required": []string{"param1"},
		},
	}, func(args json.RawMessage) (*ToolResult, error) {
		return TextResult("ok"), nil
	})

	input := `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`
	output := runServerWithInput(t, input, server)

	responses := parseResponses(t, output)
	if len(responses) != 1 {
		t.Fatalf("期望 1 个响应，得到 %d", len(responses))
	}

	resp := responses[0]
	if resp.Error != nil {
		t.Fatalf("不期望错误: %v", resp.Error)
	}

	result, ok := resp.Result.(map[string]interface{})
	if !ok {
		t.Fatalf("result 类型错误")
	}

	tools, ok := result["tools"].([]interface{})
	if !ok {
		t.Fatalf("tools 类型错误")
	}
	if len(tools) != 1 {
		t.Fatalf("期望 1 个工具，得到 %d", len(tools))
	}

	tool, ok := tools[0].(map[string]interface{})
	if !ok {
		t.Fatalf("tool 类型错误")
	}
	if tool["name"] != "test_tool" {
		t.Errorf("tool.name = %v", tool["name"])
	}
	if tool["description"] != "测试工具" {
		t.Errorf("tool.description = %v", tool["description"])
	}

	// 验证 inputSchema 包含明确输入
	schema, ok := tool["inputSchema"].(map[string]interface{})
	if !ok {
		t.Fatalf("inputSchema 类型错误")
	}
	if schema["type"] != "object" {
		t.Errorf("schema.type = %v", schema["type"])
	}
}

func TestToolsCall(t *testing.T) {
	server := NewServer("")

	server.RegisterTool(&Tool{
		Name:        "echo",
		Description: "回显工具",
		InputSchema: map[string]interface{}{"type": "object"},
	}, func(args json.RawMessage) (*ToolResult, error) {
		var params struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(args, &params)
		return TextResult("echo: " + params.Message), nil
	})

	input := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"message":"hello"}}}`
	output := runServerWithInput(t, input, server)

	responses := parseResponses(t, output)
	if len(responses) != 1 {
		t.Fatalf("期望 1 个响应，得到 %d", len(responses))
	}

	resp := responses[0]
	if resp.Error != nil {
		t.Fatalf("不期望错误: %v", resp.Error)
	}

	result, ok := resp.Result.(map[string]interface{})
	if !ok {
		t.Fatalf("result 类型错误")
	}

	content, ok := result["content"].([]interface{})
	if !ok {
		t.Fatalf("content 类型错误")
	}
	if len(content) != 1 {
		t.Fatalf("期望 1 个 content item，得到 %d", len(content))
	}

	item, ok := content[0].(map[string]interface{})
	if !ok {
		t.Fatalf("content item 类型错误")
	}
	if item["type"] != "text" {
		t.Errorf("type = %v", item["type"])
	}
	if item["text"] != "echo: hello" {
		t.Errorf("text = %v, 期望 echo: hello", item["text"])
	}
}

func TestToolsCallUnknownTool(t *testing.T) {
	server := NewServer("")

	input := `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"nonexistent","arguments":{}}}`
	output := runServerWithInput(t, input, server)

	responses := parseResponses(t, output)
	if len(responses) != 1 {
		t.Fatalf("期望 1 个响应，得到 %d", len(responses))
	}

	resp := responses[0]
	if resp.Error == nil {
		t.Fatal("期望错误，得到 nil")
	}
	if resp.Error.Code != CodeMethodNotFound {
		t.Errorf("error.code = %d, 期望 %d", resp.Error.Code, CodeMethodNotFound)
	}
}

func TestStdoutOnlyProtocolJSON(t *testing.T) {
	// 验证 stdout 仅包含协议 JSON，不混杂日志
	server := NewServer("")

	server.RegisterTool(&Tool{
		Name:        "test",
		Description: "test",
		InputSchema: map[string]interface{}{"type": "object"},
	}, func(args json.RawMessage) (*ToolResult, error) {
		return TextResult("ok"), nil
	})

	// 发送多个请求
	input := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}
{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"test","arguments":{}}}`

	output := runServerWithInput(t, input, server)

	// 每行应该是合法 JSON
	lines := strings.Split(strings.TrimSpace(output), "\n")
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var resp map[string]interface{}
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			t.Errorf("第 %d 行不是合法 JSON: %v, line: %s", i+1, err, line)
		}
		// 验证不包含日志前缀
		if strings.Contains(line, "level=") || strings.Contains(line, "time=") {
			t.Errorf("第 %d 行包含日志输出: %s", i+1, line)
		}
	}
}

func TestMethodNotFound(t *testing.T) {
	server := NewServer("")

	input := `{"jsonrpc":"2.0","id":5,"method":"unknown/method","params":{}}`
	output := runServerWithInput(t, input, server)

	responses := parseResponses(t, output)
	if len(responses) != 1 {
		t.Fatalf("期望 1 个响应，得到 %d", len(responses))
	}

	resp := responses[0]
	if resp.Error == nil {
		t.Fatal("期望错误")
	}
	if resp.Error.Code != CodeMethodNotFound {
		t.Errorf("error.code = %d, 期望 %d", resp.Error.Code, CodeMethodNotFound)
	}
}

func TestParseError(t *testing.T) {
	server := NewServer("")

	// 发送非法 JSON
	input := `{invalid json}`
	output := runServerWithInput(t, input, server)

	responses := parseResponses(t, output)
	if len(responses) != 1 {
		t.Fatalf("期望 1 个响应，得到 %d", len(responses))
	}

	resp := responses[0]
	if resp.Error == nil {
		t.Fatal("期望错误")
	}
	if resp.Error.Code != CodeParseError {
		t.Errorf("error.code = %d, 期望 %d", resp.Error.Code, CodeParseError)
	}
}
