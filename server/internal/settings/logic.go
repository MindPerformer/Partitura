// Package settings 实现业务配置的领域校验和 allowlist 管理。
//
// 引入动机：Phase6 WP3 要求严格 allowlist、类型/range 校验、
// 审计脱敏和 restart_required 语义，避免"万能 secret PUT"。
package settings

import (
	"fmt"
	"strconv"
)

// AllowedSetting 描述一个允许 Web 管理的业务配置项。
type AllowedSetting struct {
	// Type 是 value 的数据类型：string, int, bool, float, duration。
	Type string

	// Category 为 business 表示可读写业务配置；runtime_status 表示只读状态。
	Category string

	// RestartRequired 表示修改后是否需要重启服务才能生效。
	// 引入动机：design 要求不能热应用的设置明确返回 restart_required。
	RestartRequired bool

	// Min/Max 是 int/float 的合法范围（仅对数值类型生效）。
	Min float64
	Max float64

	// Description 是供 UI 展示的描述。
	Description string

	// Default 是默认值，用于初始化或校验回退。
	Default string
}

// AllowedBusinessSettings 是允许通过 Web 管理的业务配置 allowlist。
// 只应包含可安全持久化、非 secret 的业务参数。
// 基础设施/secret 配置不在此列，仍由部署环境或专用 secret store 管理。
var AllowedBusinessSettings = map[string]AllowedSetting{
	"default_revision_retention_days": {Type: "int", Category: "business", RestartRequired: true, Min: 1, Max: 3650, Description: "workspace 默认 revision 保留天数", Default: "7"},
	"default_revision_max_count":      {Type: "int", Category: "business", RestartRequired: true, Min: 1, Max: 10000, Description: "workspace 默认 revision 最大数量", Default: "30"},
	"default_max_document_size_bytes": {Type: "int", Category: "business", RestartRequired: true, Min: 1024, Max: 104857600, Description: "workspace 默认单文档最大字节数", Default: "2097152"},
	"default_embedding_provider":      {Type: "string", Category: "business", RestartRequired: true, Description: "默认 embedding provider 标识", Default: "openai-compatible"},
	"default_embedding_model":         {Type: "string", Category: "business", RestartRequired: true, Description: "默认 embedding 模型名", Default: "qwen3-embedding"},
	"default_embedding_dimensions":    {Type: "int", Category: "business", RestartRequired: true, Min: 1, Max: 8192, Description: "默认 embedding 维度", Default: "1024"},
	"default_embedding_timeout_seconds": {Type: "int", Category: "business", RestartRequired: true, Min: 1, Max: 300, Description: "默认 embedding 请求超时（秒）", Default: "30"},
	"default_embedding_batch_size":    {Type: "int", Category: "business", RestartRequired: true, Min: 1, Max: 512, Description: "默认 embedding 批量大小", Default: "32"},
	"default_reranker_provider":       {Type: "string", Category: "business", RestartRequired: true, Description: "默认 reranker provider 标识", Default: "openai-compatible"},
	"default_reranker_model":          {Type: "string", Category: "business", RestartRequired: true, Description: "默认 reranker 模型名", Default: "qwen3-reranker"},
	"default_reranker_timeout_seconds": {Type: "int", Category: "business", RestartRequired: true, Min: 1, Max: 300, Description: "默认 reranker 请求超时（秒）", Default: "10"},
	"default_reranker_max_candidates": {Type: "int", Category: "business", RestartRequired: true, Min: 1, Max: 200, Description: "默认 reranker 候选数", Default: "20"},
	"job_worker_enabled":              {Type: "bool", Category: "business", RestartRequired: true, Description: "后台 job worker 是否默认启用", Default: "false"},
	"job_worker_poll_interval":        {Type: "int", Category: "business", RestartRequired: true, Min: 1, Max: 3600, Description: "后台 job worker 轮询间隔（秒）", Default: "5"},
	"scheduler_enabled":               {Type: "bool", Category: "business", RestartRequired: true, Description: "后台 scheduler 是否默认启用", Default: "false"},
	"scheduler_tick_interval":         {Type: "int", Category: "business", RestartRequired: true, Min: 1, Max: 86400, Description: "后台 scheduler tick 间隔（秒）", Default: "60"},
}

// IsAllowed 判断 key 是否在业务配置 allowlist 中。
func IsAllowed(key string) bool {
	_, ok := AllowedBusinessSettings[key]
	return ok
}

// GetAllowedSetting 返回 allowlist 中的配置定义。
func GetAllowedSetting(key string) (AllowedSetting, bool) {
	s, ok := AllowedBusinessSettings[key]
	return s, ok
}

// Validate 按 allowlist 的类型和范围校验 value。
// 返回规范化后的 value（去除非法空白等）或错误。
func Validate(key, value string) (string, error) {
	s, ok := AllowedBusinessSettings[key]
	if !ok {
		return "", fmt.Errorf("setting %s 不在 allowlist 中", key)
	}

	switch s.Type {
	case "string":
		if value == "" {
			return "", fmt.Errorf("%s 不能为空", key)
		}
		return value, nil

	case "int":
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return "", fmt.Errorf("%s 必须为整数: %w", key, err)
		}
		if n < int64(s.Min) || n > int64(s.Max) {
			return "", fmt.Errorf("%s 必须在 %v..%v 范围内，得到 %d", key, int64(s.Min), int64(s.Max), n)
		}
		return strconv.FormatInt(n, 10), nil

	case "float":
		f, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return "", fmt.Errorf("%s 必须为数字: %w", key, err)
		}
		if f < s.Min || f > s.Max {
			return "", fmt.Errorf("%s 必须在 %v..%v 范围内，得到 %f", key, s.Min, s.Max, f)
		}
		return strconv.FormatFloat(f, 'f', -1, 64), nil

	case "bool":
		b, err := strconv.ParseBool(value)
		if err != nil {
			return "", fmt.Errorf("%s 必须为布尔值: %w", key, err)
		}
		return strconv.FormatBool(b), nil

	case "duration":
		// duration 统一以秒为单位存储，允许整数或 duration 字符串（简单起见仅接受整数秒）。
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return "", fmt.Errorf("%s 必须为整数秒: %w", key, err)
		}
		if n < int64(s.Min) || n > int64(s.Max) {
			return "", fmt.Errorf("%s 必须在 %v..%v 秒范围内，得到 %d", key, int64(s.Min), int64(s.Max), n)
		}
		return strconv.FormatInt(n, 10), nil

	default:
		return "", fmt.Errorf("%s 未知类型 %s", key, s.Type)
	}
}

// SanitizeForAudit 将审计 detail 中的敏感值脱敏。
// 当前 system_settings 不存储 secret，仅对极长值截断；
// 保留非 secret 业务配置的旧/新值供审计展示。
func SanitizeForAudit(key, value string) string {
	if len(value) > 512 {
		return value[:512] + "..."
	}
	return value
}
