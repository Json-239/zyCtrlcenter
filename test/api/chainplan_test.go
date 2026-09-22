// 链数据的模块化视图：GET /api/chains?id= 要带 plan（模块序列）与校验结果。
// 面板用它显示"这条链由哪些模块组成"，排障时一眼看出悬空 next / 未知执行器。
package api_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePlanChainFile(t *testing.T, dir, id, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestChainsEndpointIncludesModulePlan(t *testing.T) {
	env := newTestEnv(t, "")
	writePlanChainFile(t, env.cfg.ChainDir, "demo_plan", `{
	  "chain_id": "demo_plan", "start_task": 7001001, "end_task": 7001002, "grid_cell": 16,
	  "npcs": {"10103": [[5, 1600, 1200]], "13520": [[5, 900, 900]]},
	  "task_order": [
	    {"task_index": 7001001, "name": "天命所归", "catcher_npc": "10103", "thrower_npc": "10103", "next": [7001002]},
	    {"task_index": 7001002, "name": "采购", "catcher_npc": "10103", "thrower_npc": "10103", "next": [7001003]}
	  ],
	  "task_hints": {"7001002": {"executor": "shop_purchase", "shop": {"npc": 13520, "item_index": 101403, "count": 1}}}
	}`)

	body := getJSON(t, env.srv.URL+"/api/chains?id=demo_plan")
	if body["chain_found"] != true {
		t.Fatalf("链应能找到: %v", body)
	}
	plan, _ := body["plan"].(map[string]any)
	if plan == nil {
		t.Fatalf("应带模块化视图 plan: %v", body)
	}
	if plan["chain_id"] != "demo_plan" {
		t.Fatalf("plan 链 id 不符: %v", plan["chain_id"])
	}
	tasks, _ := plan["tasks"].([]any)
	if len(tasks) != 2 {
		t.Fatalf("应展开 2 个任务: %v", tasks)
	}
	t0, _ := tasks[0].(map[string]any)
	steps, _ := t0["steps"].([]any)
	mods := []string{}
	for _, s := range steps {
		sm, _ := s.(map[string]any)
		mods = append(mods, sm["module"].(string))
	}
	if strings.Join(mods, ",") != "pathfind,move,talk,wait" {
		t.Fatalf("首个任务的模块序列不符: %v", mods)
	}
	// 第二个任务要能看到购买模块
	t1, _ := tasks[1].(map[string]any)
	mods1 := []string{}
	for _, s := range t1["steps"].([]any) {
		sm, _ := s.(map[string]any)
		mods1 = append(mods1, sm["module"].(string))
	}
	if !strings.Contains(strings.Join(mods1, ","), "buy") {
		t.Fatalf("第二个任务应含购买模块: %v", mods1)
	}
	// 模块计数（面板展示"用了哪些模块"）
	modCnt, _ := plan["modules"].(map[string]any)
	if modCnt["wait"] != float64(2) || modCnt["talk"] != float64(2) {
		t.Fatalf("模块计数不符: %v", modCnt)
	}
}

// 列表要标出"导航数据"（没有任务节点的文件，如钟馗抓鬼日常的 zhongkui_nav.json）：
// 它不是可启动的链，面板据此把它从「启动链路」里排除，避免误选。
func TestChainsEndpointMarksNavOnly(t *testing.T) {
	env := newTestEnv(t, "")
	writePlanChainFile(t, env.cfg.ChainDir, "nav_demo", `{"chain_id":"nav_demo","npcs":{},"map_grids":{},"dijkstra":{}}`)
	writePlanChainFile(t, env.cfg.ChainDir, "chain_demo", `{
	  "chain_id":"chain_demo","start_task":7001001,"end_task":7001001,
	  "npcs":{"10103":[[5,100,100]]},
	  "task_order":[{"task_index":7001001,"catcher_npc":"10103","next":[]}]
	}`)

	body := getJSON(t, env.srv.URL+"/api/chains")
	items, _ := body["chains"].([]any)
	got := map[string]bool{}
	for _, it := range items {
		m, _ := it.(map[string]any)
		id, _ := m["id"].(string)
		nav, _ := m["nav_only"].(bool)
		got[id] = nav
	}
	if v, ok := got["nav_demo"]; !ok || !v {
		t.Fatalf("没有任务节点的文件应标 nav_only=true: %v", got)
	}
	if v, ok := got["chain_demo"]; !ok || v {
		t.Fatalf("有任务节点的链不该标 nav_only: %v", got)
	}
}

// 坏链：结构问题要能提前看到（plan_error），而不是等到机器人端卡住。
func TestChainsEndpointReportsPlanError(t *testing.T) {
	env := newTestEnv(t, "")
	writePlanChainFile(t, env.cfg.ChainDir, "demo_bad", `{
	  "chain_id": "demo_bad", "start_task": 7001001, "end_task": 7001005, "grid_cell": 16,
	  "npcs": {"10103": [[5, 100, 100]]},
	  "task_order": [
	    {"task_index": 7001001, "catcher_npc": "10103", "next": [7001002]},
	    {"task_index": 7001002, "catcher_npc": "10103", "next": [7001004]}
	  ],
	  "task_hints": {"7001002": {"executor": "fly_away"}}
	}`)

	body := getJSON(t, env.srv.URL+"/api/chains?id=demo_bad")
	if body["chain_found"] != true {
		t.Fatalf("链应能找到: %v", body)
	}
	errMsg, _ := body["plan_error"].(string)
	if errMsg == "" {
		t.Fatalf("坏链应给出 plan_error: %v", body)
	}
	if !strings.Contains(errMsg, "fly_away") || !strings.Contains(errMsg, "7001004") {
		t.Fatalf("两类问题都要报出来（未知执行器 + 残缺后继）: %s", errMsg)
	}
	// 组装结果仍然返回（面板能显示"哪一步有问题"）
	if body["plan"] == nil {
		t.Fatal("即使有错也要返回 plan（供面板定位）")
	}
}
