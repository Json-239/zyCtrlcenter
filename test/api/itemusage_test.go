// 物品使用配置（GET /api/item_usage）：面板「物品配置」页数据源（点击物品看实现）。
//
// 契约（三态，HTTP 一律 200，前端按 available/msg 渲染，不崩页）：
//   - 正常：available=true + 摘要计数（count/category_count/finding_count/unimplemented_count）+ catalog 原文透传；
//   - 缺文件：available=false + msg（面板提示"数据未就绪"）；
//   - 损坏：available=false + msg 含"解析失败"（不 panic）。
//
// 另钉住仓库配套数据文件 data/item_usage_catalog.json 的最小结构（文件在才校验，不在则跳过）。
package api_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// itemUsageCatalogJSON 一份最小合法物品清单（形状与生产 item_usage_catalog.json 一致）。
const itemUsageCatalogJSON = `{
  "version": "2026-09-29",
  "title": "测试物品清单",
  "categories": [{"key": "heal_hp", "label": "疗伤（回血）"}],
  "items": [
    {"item_id": 102007, "name": "金创药", "category": "heal_hp", "implemented": "yes",
     "sites": [{"module": "auto_summon", "function": "_try_role_heal", "file": "auto_summon.py", "line": 648,
                "protocol": "C2S_USE_ITEM", "scene": "idle", "trigger": "cur_hp<max_hp*0.5", "config": ["robot_auto_heal"]}]},
    {"item_id": 101009, "name": "宠物口粮", "category": "summon_food", "implemented": "partial", "sites": []}
  ],
  "unimplemented": [{"item_id": 101009, "name": "宠物口粮", "expected": "action_add_pet_stamina", "status": "partial(无效使用)"}],
  "findings": [{"id": "F1", "severity": "高", "title": "多实现", "detail": "…", "evidence": ["auto_summon.py:648"]}]
}`

func writeItemUsageCatalog(t *testing.T, env *testEnv, content string) string {
	t.Helper()
	p := filepath.Join(env.cfg.DataDir, "item_usage_catalog.json")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("写 %s 失败: %v", p, err)
	}
	return p
}

// 正常态：摘要计数正确 + catalog 原文带 items/sites（面板要拿它画列表与详情）。
func TestItemUsageEndpointOK(t *testing.T) {
	env := newTestEnv(t, "")
	writeItemUsageCatalog(t, env, itemUsageCatalogJSON)

	body := getJSON(t, env.srv.URL+"/api/item_usage")
	if body["ok"] != true || body["available"] != true {
		t.Fatalf("文件正常时应 ok/available=true: %v", body)
	}
	if n, _ := body["count"].(float64); n != 2 {
		t.Fatalf("count 应=2，实际 %v", body["count"])
	}
	if n, _ := body["category_count"].(float64); n != 1 {
		t.Fatalf("category_count 应=1，实际 %v", body["category_count"])
	}
	if n, _ := body["finding_count"].(float64); n != 1 {
		t.Fatalf("finding_count 应=1，实际 %v", body["finding_count"])
	}
	if n, _ := body["unimplemented_count"].(float64); n != 1 {
		t.Fatalf("unimplemented_count 应=1，实际 %v", body["unimplemented_count"])
	}
	if body["version"] != "2026-09-29" || body["title"] != "测试物品清单" {
		t.Fatalf("version/title 应透传: %v / %v", body["version"], body["title"])
	}
	cat, _ := body["catalog"].(map[string]any)
	if cat == nil {
		t.Fatalf("catalog 应为对象: %v", body["catalog"])
	}
	items, _ := cat["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("catalog.items 应=2: %v", cat["items"])
	}
	first, _ := items[0].(map[string]any)
	if first["name"] != "金创药" {
		t.Fatalf("条目原文应透传（含中文名）: %v", first)
	}
	// 数字不得被 float64 化截断（1013681 这类大编号）——用原文里的 item_id 验证是 int 形状
	if it, ok := first["item_id"].(float64); !ok || it != 102007 {
		t.Fatalf("item_id 应为数字 102007: %v(%T)", first["item_id"], first["item_id"])
	}
	sites, _ := first["sites"].([]any)
	if len(sites) != 1 {
		t.Fatalf("sites 应注入原文: %v", first["sites"])
	}
}

// 缺文件态：不 panic，available=false + msg 指向文件名；path 指向 DataDir。
func TestItemUsageEndpointMissingFile(t *testing.T) {
	env := newTestEnv(t, "") // DataDir 为空临时目录，不写文件

	body := getJSON(t, env.srv.URL+"/api/item_usage")
	if body["ok"] != true {
		t.Fatalf("缺文件也应 ok:true（只是数据未就绪）: %v", body)
	}
	if body["available"] != false {
		t.Fatalf("缺文件应 available=false: %v", body)
	}
	if n, _ := body["count"].(float64); n != 0 {
		t.Fatalf("缺文件 count 应=0: %v", body["count"])
	}
	if body["catalog"] != nil {
		t.Fatalf("缺文件不应返回 catalog: %v", body["catalog"])
	}
	msg, _ := body["msg"].(string)
	if !strings.Contains(msg, "item_usage_catalog.json") {
		t.Fatalf("msg 应说明缺哪个文件: %q", msg)
	}
	p, _ := body["path"].(string)
	if !strings.Contains(p, filepath.Base(env.cfg.DataDir)) {
		t.Fatalf("path 应指向 DataDir 下: %q (dataDir=%q)", p, env.cfg.DataDir)
	}
}

// 损坏态：非法 JSON 不 panic，available=false + msg 含"解析失败"。
func TestItemUsageEndpointCorrupted(t *testing.T) {
	env := newTestEnv(t, "")
	writeItemUsageCatalog(t, env, "{ this is not json ")

	body := getJSON(t, env.srv.URL+"/api/item_usage")
	if body["ok"] != true || body["available"] != false {
		t.Fatalf("损坏文件应 ok:true + available:false: %v", body)
	}
	msg, _ := body["msg"].(string)
	if !strings.Contains(msg, "解析失败") {
		t.Fatalf("msg 应含『解析失败』: %q", msg)
	}
	if body["catalog"] != nil {
		t.Fatalf("损坏文件不应返回 catalog: %v", body["catalog"])
	}
}

// 配套数据文件（仓库 data/item_usage_catalog.json）最小结构：
// 新页面直接消费这份数据，格式塌了要在测试里先炸（文件不在场则跳过——它属于运行期数据）。
func TestItemUsageCatalogRepoFileShape(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "data", "item_usage_catalog.json"))
	if err != nil {
		t.Skipf("仓库数据文件不在场（%v），跳过结构校验", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("data/item_usage_catalog.json 不是合法 JSON: %v", err)
	}
	items, _ := doc["items"].([]any)
	if len(items) == 0 {
		t.Fatalf("items 不应为空（面板列表要显示物品）: %v", doc["items"])
	}
	for i, it := range items {
		m, _ := it.(map[string]any)
		for _, f := range []string{"name", "category", "implemented"} {
			if s, _ := m[f].(string); s == "" {
				t.Fatalf("items[%d] 缺字段 %s: %v", i, f, m)
			}
		}
		if _, ok := m["item_id"].(float64); !ok {
			t.Fatalf("items[%d].item_id 应为数字: %v", i, m["item_id"])
		}
		if m["implemented"] != "yes" && m["implemented"] != "partial" && m["implemented"] != "no" {
			t.Fatalf("items[%d].implemented 应 ∈ yes/partial/no，实际 %v", i, m["implemented"])
		}
	}
	if findings, _ := doc["findings"].([]any); len(findings) == 0 {
		t.Fatalf("findings 不应为空（详情要标注 F1-F9 关联）")
	}
}
