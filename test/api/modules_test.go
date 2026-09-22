// 模块地图（GET /api/modules）：注册表口径 + 最近一次测试报告。
//
// 口径由这条用例钉住：**动作层 9 个模块的键集与顺序必须与 chainplan.ModuleNames() 完全一致**
// （防止"注册表漏登记一个模块"这种静默缺卡）；总数 12 = 动作 9 + 链数据 2 + 编排 1。
package api_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"zyctrlcenter/internal/chainplan"
)

func TestModulesEndpointRegistry(t *testing.T) {
	env := newTestEnv(t, "")
	body := getJSON(t, env.srv.URL+"/api/modules")

	if body["ok"] != true {
		t.Fatalf("应返回 ok:true: %v", body)
	}
	mods, _ := body["modules"].([]any)
	if len(mods) != 12 {
		t.Fatalf("模块总数应为 12（动作 9 + 链数据 2 + 编排 1），实际 %d", len(mods))
	}
	layers, _ := body["layers"].([]any)
	if len(layers) != 3 {
		t.Fatalf("应分 3 层（动作/链数据/编排）: %v", layers)
	}

	// 动作层：键集 + 顺序 === chainplan.ModuleNames()
	want := make([]string, 0, 9)
	for _, m := range chainplan.ModuleNames() {
		want = append(want, string(m))
	}
	got := []string{}
	for _, it := range mods {
		m, _ := it.(map[string]any)
		if m["layer"] != "actions" {
			continue
		}
		got = append(got, toStrAny(m["key"]))
	}
	if len(got) != len(want) {
		t.Fatalf("动作层模块数应与 chainplan.ModuleNames() 一致（%d）: %v", len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("动作层顺序/键名应与 ModuleNames() 一致，第 %d 个：%q != %q", i, got[i], want[i])
		}
	}

	// 每张卡片都要有职责/数据来源/参考实现/关联用例（面板点开就有东西看）
	for _, it := range mods {
		m, _ := it.(map[string]any)
		for _, field := range []string{"key", "name", "layer", "desc", "data", "py"} {
			if toStrAny(m[field]) == "" {
				t.Fatalf("模块 %v 缺字段 %s: %v", m["key"], field, m)
			}
		}
		tests, _ := m["tests"].([]any)
		if len(tests) == 0 {
			t.Fatalf("模块 %v 应有关联用例（面板要显示「这些用例在保它」）: %v", m["key"], m)
		}
	}

	// 还没跑过报告 → 不是错误，页面显示"—"
	if body["report"] != nil {
		t.Fatalf("没有 data/test_report.json 时 report 应为空: %v", body["report"])
	}
}

// 跑过 tools/test_report 之后：页面能显示"最近一次测试结果"。
func TestModulesEndpointReadsLastReport(t *testing.T) {
	env := newTestEnv(t, "")
	rep := map[string]any{
		"date": "2026-09-21", "ok": true, "vet": "ok", "race": "skipped",
		"totals": map[string]any{"cases": 236, "passed": 236, "failed": 0},
	}
	raw, _ := json.Marshal(rep)
	if err := os.WriteFile(filepath.Join(env.cfg.DataDir, "test_report.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	body := getJSON(t, env.srv.URL+"/api/modules")
	got, _ := body["report"].(map[string]any)
	if got == nil || got["date"] != "2026-09-21" {
		t.Fatalf("应把最近一次报告回带: %v", body["report"])
	}
	totals, _ := got["totals"].(map[string]any)
	if totals["cases"] != float64(236) {
		t.Fatalf("报告内容应原样透传: %v", got)
	}
	// 路径也要给出来，排障时知道读的哪个文件
	if toStrAny(body["report_path"]) == "" {
		t.Fatalf("应回带报告文件路径: %v", body)
	}
}

// 报告文件坏了 → 明确报错（不静默当成"没跑过"）。
func TestModulesEndpointBadReportDoesNotPanic(t *testing.T) {
	env := newTestEnv(t, "")
	if err := os.WriteFile(filepath.Join(env.cfg.DataDir, "test_report.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	body := getJSON(t, env.srv.URL+"/api/modules")
	if body["ok"] != true {
		t.Fatalf("报告坏了也不该整体失败（模块注册表仍要能看）: %v", body)
	}
	if toStrAny(body["report_error"]) == "" {
		t.Fatalf("应把「报告读不出来」说出来: %v", body)
	}
}
