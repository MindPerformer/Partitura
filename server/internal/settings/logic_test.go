// Package settings 测试业务配置的 allowlist 与校验逻辑。
//
// 引入动机：Phase6 WP3 要求严格 allowlist 校验和 restart_required 语义，
// 需要不依赖 HTTP handler 直接测试领域规则。
package settings

import (
	"strings"
	"testing"
)

// TestIsAllowed 验证 allowlist 只包含预定义的业务配置键。
func TestIsAllowed(t *testing.T) {
	if !IsAllowed("default_embedding_model") {
		t.Error("default_embedding_model 应在 allowlist 中")
	}
	if IsAllowed("db_password") {
		t.Error("db_password 不应在 allowlist 中")
	}
	if IsAllowed("api_key") {
		t.Error("api_key 不应在 allowlist 中")
	}
}

// TestValidate 验证各类型 value 的校验和规范化。
func TestValidate(t *testing.T) {
	cases := []struct {
		key     string
		value   string
		wantErr bool
		want    string
	}{
		{key: "default_embedding_model", value: "qwen3-embedding", wantErr: false, want: "qwen3-embedding"},
		{key: "default_embedding_dimensions", value: "1024", wantErr: false, want: "1024"},
		{key: "default_embedding_dimensions", value: "0", wantErr: true, want: ""},
		{key: "job_worker_enabled", value: "true", wantErr: false, want: "true"},
		{key: "job_worker_enabled", value: "1", wantErr: false, want: "true"},
		{key: "default_revision_retention_days", value: "3", wantErr: false, want: "3"},
		{key: "default_revision_retention_days", value: "4000", wantErr: true, want: ""},
		{key: "default_embedding_timeout_seconds", value: "not-a-number", wantErr: true, want: ""},
		{key: "unknown_key", value: "foo", wantErr: true, want: ""},
		{key: "default_embedding_model", value: "", wantErr: true, want: ""},
	}

	for _, c := range cases {
		got, err := Validate(c.key, c.value)
		if (err != nil) != c.wantErr {
			t.Errorf("Validate(%q, %q) 错误状态不匹配: err=%v, wantErr=%v", c.key, c.value, err, c.wantErr)
			continue
		}
		if !c.wantErr && got != c.want {
			t.Errorf("Validate(%q, %q) = %q, want %q", c.key, c.value, got, c.want)
		}
	}
}

// TestValidate_UnknownKey 验证不在 allowlist 的 key 返回明确错误。
func TestValidate_UnknownKey(t *testing.T) {
	_, err := Validate("cookie_secret", "secret-value")
	if err == nil {
		t.Fatal("期望对 cookie_secret 返回错误")
	}
	if !strings.Contains(err.Error(), "不在 allowlist") {
		t.Errorf("错误信息应包含 '不在 allowlist'，实际: %v", err)
	}
}

// TestSanitizeForAudit 验证审计脱敏只截断超长值。
func TestSanitizeForAudit(t *testing.T) {
	short := "normal-value"
	if got := SanitizeForAudit("default_embedding_model", short); got != short {
		t.Errorf("短值不应被截断: got %q", got)
	}

	long := strings.Repeat("x", 600)
	got := SanitizeForAudit("default_embedding_model", long)
	if len(got) != 512+len("...") {
		t.Errorf("超长值应截断到 515 字节: got len %d", len(got))
	}
}
