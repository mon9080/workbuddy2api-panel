// tool_names_test.go tools[] 条目校验单测（对齐 hub PR #726 的实测形态：
// 上游 400 code 11133，param 路径 tools[i].function.name）。
package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

func toolsOf(t *testing.T, body string) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	normalizeToolsEntries(obj)
	return obj
}

func TestNormalizeToolsEntries(t *testing.T) {
	t.Run("服务端声明无function载荷剔除", func(t *testing.T) {
		body := `{"model":"m","tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}},{"type":"web_search"}]}`
		obj := toolsOf(t, body)
		tools, _ := obj["tools"].([]any)
		if len(tools) != 1 {
			t.Fatalf("应只留 1 条合法工具，剩 %v", tools)
		}
	})
	t.Run("非法名剔除-空缺失非法字符超长", func(t *testing.T) {
		mk := func(name any) string {
			fn, _ := json.Marshal(map[string]any{"name": name, "parameters": map[string]any{"type": "object"}})
			entry, _ := json.Marshal(map[string]any{"type": "function", "function": json.RawMessage(fn)})
			return `{"model":"m","tools":[{"type":"function","function":{"name":"ok_good-1","parameters":{"type":"object"}}},` + string(entry) + `]}`
		}
		for _, bad := range []any{"", "has space", "dot.name", "中文", strings.Repeat("a", 65), nil} {
			obj := toolsOf(t, mk(bad))
			tools, _ := obj["tools"].([]any)
			if len(tools) != 1 {
				t.Errorf("非法名 %v 应被剔除", bad)
			}
		}
		// 恰好 64 字符合法。
		obj := toolsOf(t, mk(strings.Repeat("a", 64)))
		tools, _ := obj["tools"].([]any)
		if len(tools) != 2 {
			t.Errorf("64 字符名应保留")
		}
	})
	t.Run("裸name形态同口径", func(t *testing.T) {
		body := `{"model":"m","tools":[{"type":"function","name":"bare_ok"},{"type":"function","name":"bad.name"}]}`
		obj := toolsOf(t, body)
		tools, _ := obj["tools"].([]any)
		if len(tools) != 1 {
			t.Fatalf("裸 name 形态应按同口径过滤，剩 %v", tools)
		}
	})
	t.Run("同名去重", func(t *testing.T) {
		body := `{"model":"m","tools":[{"type":"function","function":{"name":"dup"}},{"type":"function","function":{"name":"dup"}},{"type":"function","function":{"name":"other"}}]}`
		obj := toolsOf(t, body)
		tools, _ := obj["tools"].([]any)
		if len(tools) != 2 {
			t.Fatalf("同名应去重剩 2 条，剩 %v", tools)
		}
	})
	t.Run("全剔净删tools键", func(t *testing.T) {
		body := `{"model":"m","tools":[{"type":"web_search"},{"type":"function","function":{"name":""}}]}`
		obj := toolsOf(t, body)
		if _, has := obj["tools"]; has {
			t.Fatalf("全部剔净应删除 tools 键: %v", obj["tools"])
		}
	})
	t.Run("合法条目原样保留不改名", func(t *testing.T) {
		body := `{"model":"m","tools":[{"type":"function","function":{"name":"get_weather-01","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}}]}`
		obj := toolsOf(t, body)
		tools, _ := obj["tools"].([]any)
		if len(tools) != 1 {
			t.Fatalf("合法条目应保留")
		}
		fn := tools[0].(map[string]any)["function"].(map[string]any)
		if fn["name"] != "get_weather-01" {
			t.Errorf("不得改写合法名: %v", fn["name"])
		}
	})
}
