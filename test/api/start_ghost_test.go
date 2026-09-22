// 抓鬼链路（ghost_start）的载荷与防误选：
//
//   - 抓鬼必须带**导航数据**（chainlib.Build 的 zhongkui_nav）：机器人端只认 cmd["chain"]，
//     不给就是"原地不动"（npcs/map_grids/dijkstra/ghost_maps/ghost_map_pos 全在载荷里）；
//   - 导航数据缺失/解析失败 → 硬失败：ok:false + 明确 msg，且**一条命令都不发**
//     （不能出现"新手链那组发出去了、抓鬼这组失败"的半成功）；
//   - 导航数据不是链：手选它启动会被接口层直接拒绝（面板同时禁用按钮）；
//   - 选了剧情链但该号条件已判抓鬼 → 回带 warnings（面板提示改用「自动分配」）。
package api_test

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"zyctrlcenter/internal/api"
	"zyctrlcenter/test/testsupport"
)

// 抓鬼意图的号：实收 ghost_start 必须带 role=solo + 完整导航载荷。
func TestStartAutoGhostCarriesNavPayload(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	testsupport.InstallGhostNav(t, env.cfg.ChainDir)

	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{map[string]any{
			"account": "ghost_payload@xy3.com", "level": 45, "online": true,
			"state": "IDLE", "task_index": 0,
		}},
		"_zone": testsupportZone()})

	_, body := postJSON(t, env.srv.URL+"/api/start", map[string]any{
		"auto": true, "accounts": []string{"ghost_payload@xy3.com"},
	}, nil)
	if body["ok"] != true || body["mode"] != "auto" {
		t.Fatalf("应走自动分配: %v", body)
	}

	cmd := rb.ReadCmd(t, 2*time.Second)
	if cmd["cmd"] != "ghost_start" {
		t.Fatalf("抓鬼意图应收 ghost_start: %v", cmd)
	}
	// 夹具文件里没有 chain_id → chainlib 按文件名补成 zhongkui_nav（与线上真实文件一致）
	if cmd["chain_id"] != "zhongkui_nav" {
		t.Fatalf("应带导航数据文件名（顶层没有 chain_id，按文件名补）: %v", cmd["chain_id"])
	}
	if cmd["role"] != "solo" {
		t.Fatalf("应显式带 role=solo: %v", cmd["role"])
	}
	if cmd["daily_limit"] != float64(50) {
		t.Fatalf("应带 daily_limit=50（与机器人默认/参考实现同口径）: %v", cmd["daily_limit"])
	}
	chain, _ := cmd["chain"].(map[string]any)
	if len(chain) == 0 {
		t.Fatalf("抓鬼必须带 chain 载荷（不带=机器人原地不动）: %v", cmd)
	}
	// 地址齐不齐：这是「借鉴参考实现 ghost_nav_payload」的核心 —— 坐标/网格/路由来自基座链，
	// 刷鬼图与落点来自抓鬼专属文件，缺一不可（缺了机器人到不了刷鬼图）。
	npcs, _ := chain["npcs"].(map[string]any)
	if _, ok := npcs["10146"]; !ok {
		t.Fatalf("chain.npcs 缺钟馗 10146（来自基座链 newbie_full）: %v", npcs)
	}
	grids, _ := chain["map_grids"].(map[string]any)
	if len(grids) == 0 {
		t.Fatalf("chain.map_grids 不能为空（寻路网格在它里面）: %v", chain["map_grids"])
	}
	if dj, _ := chain["dijkstra"].(map[string]any); len(dj) == 0 {
		t.Fatalf("chain.dijkstra 不能为空（跨图路由在它里面）: %v", chain["dijkstra"])
	}
	ghostMaps := asSlice(chain["ghost_maps"])
	if len(ghostMaps) == 0 {
		t.Fatalf("chain.ghost_maps 不能为空（刷鬼图列表在它里面）: %v", chain["ghost_maps"])
	}
	pos, _ := chain["ghost_map_pos"].(map[string]any)
	if len(pos) == 0 {
		t.Fatalf("chain.ghost_map_pos 不能为空（每图筋斗云落点在它里面）: %v", chain["ghost_map_pos"])
	}
	for _, m := range ghostMaps {
		key := mapKey(m)
		if _, ok := grids[key]; !ok {
			t.Fatalf("刷鬼图 %s 必须有寻路网格（否则到图后只能直线走）: %v", key, grids)
		}
		if _, ok := pos[key]; !ok {
			t.Fatalf("刷鬼图 %s 必须有落点: %v", key, pos)
		}
	}
	// 没装导航数据时不会有第二条命令
	if extra := rb.TryReadCmd(200 * time.Millisecond); extra != nil {
		t.Fatalf("只该收到一条 ghost_start: %v", extra)
	}
}

// 载荷是「基座链 + 抓鬼专属」**组装**出来的（对齐参考实现 services/chain.py:ghost_nav_payload）：
// 专属文件只放 ghost_maps/ghost_map_pos，坐标与网格必须自动从基座链补齐。
func TestGhostNavAssemblesBaseAndGhostFields(t *testing.T) {
	env := newTestEnv(t, "")
	testsupport.InstallGhostNav(t, env.cfg.ChainDir)

	nav, err := api.NewPayloads(env.cfg).Ghost()
	if err != nil {
		t.Fatalf("组装抓鬼导航载荷失败: %v", err)
	}
	// 基座提供：NPC 坐标（钟馗）、寻路网格、跨图路由
	if _, ok := nav.NPCs["10146"]; !ok {
		t.Fatalf("应复用基座链的 npcs（含钟馗 10146）: %v", nav.NPCs)
	}
	for _, m := range []string{"9", "10"} {
		if _, ok := nav.MapGrids[m]; !ok {
			t.Fatalf("应复用基座链的 map_grids（缺图 %s）", m)
		}
		if _, ok := nav.NPCs["44"]; !ok && m == "9" {
			t.Fatalf("应复用基座链的 npcs（缺图9 的 NPC 44）")
		}
	}
	if len(nav.Dijkstra) == 0 {
		t.Fatal("应复用基座链的 dijkstra")
	}
	// 专属提供：刷鬼图 / 落点 / 地图名
	if !strings.Contains(string(nav.Extra["ghost_maps"]), "9") {
		t.Fatalf("ghost_maps 应来自抓鬼专属文件: %s", nav.Extra["ghost_maps"])
	}
	if !strings.Contains(string(nav.Extra["ghost_map_pos"]), "787") {
		t.Fatalf("ghost_map_pos 应来自抓鬼专属文件: %s", nav.Extra["ghost_map_pos"])
	}
	if nav.Maps["9"] != "东海湾" {
		t.Fatalf("maps 应来自抓鬼专属文件: %v", nav.Maps)
	}
	if nav.GridCell != 16 {
		t.Fatalf("grid_cell 应沿用基座: %d", nav.GridCell)
	}
	// 载荷里不该混进任务链字段（抓鬼不是任务链）
	if len(nav.TaskOrder) != 0 || len(nav.TaskHints) != 0 {
		t.Fatalf("抓鬼载荷不该带 task_order/task_hints: %v / %v", nav.TaskOrder, nav.TaskHints)
	}
}

// 地址不全（刷鬼图没有网格/没有落点）→ 硬失败，且错误要说清缺什么。
func TestGhostNavRejectsIncompleteAddresses(t *testing.T) {
	env := newTestEnv(t, "")
	testsupport.InstallGhostNav(t, env.cfg.ChainDir)
	// 造一份"刷鬼图 11 既没网格也没落点"的专属文件（基座里只有 9/10 两张图）
	testsupport.WriteChainFile(t, env.cfg.ChainDir, "zhongkui_nav", map[string]any{
		"ghost_maps":    []int{9, 11},
		"ghost_map_pos": map[string]any{"9": []int{787, 780}, "11": []int{1, 2}},
	})
	if _, err := api.NewPayloads(env.cfg).Ghost(); err == nil {
		t.Fatal("刷鬼图缺网格时应报错（否则机器人到图后直线走/穿墙）")
	} else if !strings.Contains(err.Error(), "11") || !strings.Contains(err.Error(), "map_grids") {
		t.Fatalf("错误要说清缺哪张图的网格: %v", err)
	}

	// 落点缺失同理
	testsupport.WriteChainFile(t, env.cfg.ChainDir, "zhongkui_nav", map[string]any{
		"ghost_maps":    []int{9},
		"ghost_map_pos": map[string]any{},
	})
	if _, err := api.NewPayloads(env.cfg).Ghost(); err == nil {
		t.Fatal("刷鬼图缺落点时应报错（筋斗云传送要用）")
	} else if !strings.Contains(err.Error(), "ghost_map_pos") {
		t.Fatalf("错误要说清缺落点: %v", err)
	}
}

// 导航数据缺失：硬失败，且**一条命令都不发**（连同一批的新手链那组也不发）。
func TestStartGhostMissingNavFailsLoudly(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	// 故意不装 zhongkui_nav.json：链目录是空的

	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{
			map[string]any{"account": "mix_low@xy3.com", "level": 20, "online": true, "state": "IDLE", "task_index": 0},
			map[string]any{"account": "mix_high@xy3.com", "level": 45, "online": true, "state": "IDLE", "task_index": 0},
		},
		"_zone": testsupportZone()})

	_, body := postJSON(t, env.srv.URL+"/api/start", map[string]any{
		"auto": true, "accounts": []string{"mix_low@xy3.com", "mix_high@xy3.com"},
	}, nil)
	if body["ok"] != false {
		t.Fatalf("导航数据缺失应硬失败: %v", body)
	}
	if msg, _ := body["msg"].(string); !strings.Contains(msg, "导航数据不可用") {
		t.Fatalf("要说清是导航数据缺失（不能静默）: %v", body["msg"])
	}
	if body["sent"] != float64(0) {
		t.Fatalf("不该发任何命令: %v", body["sent"])
	}
	// 新手链那组也不能发出去（避免"半成功"）
	if cmd := rb.TryReadCmd(300 * time.Millisecond); cmd != nil {
		t.Fatalf("硬失败时机器人不该收到任何命令: %v", cmd)
	}
}

// 导航数据不是链：手选它启动 → 接口层直接拒绝（不发 start_chain 让机器人干等）。
func TestStartRejectsNavOnlyChain(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	testsupport.InstallChainFixture(t, env.cfg.ChainDir, "zhongkui_nav", "chains/zhongkui_nav.mini.json")

	_, body := postJSON(t, env.srv.URL+"/api/start",
		map[string]any{"chain_id": "zhongkui_nav", "accounts": []string{"someone@xy3.com"}}, nil)
	if body["ok"] != false || body["nav_only"] != true {
		t.Fatalf("导航数据应被拒绝并标注 nav_only: %v", body)
	}
	if msg, _ := body["msg"].(string); !strings.Contains(msg, "导航数据") {
		t.Fatalf("拒绝理由要说清: %v", body["msg"])
	}
	if cmd := rb.TryReadCmd(300 * time.Millisecond); cmd != nil {
		t.Fatalf("拒绝时不该下发命令: %v", cmd)
	}
}

// 选了剧情链但该号已判抓鬼 → 回带 warnings（面板提示），并把话拼进 msg（走既有 toast 通道）。
func TestStartWarnsWhenConditionMismatch(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{map[string]any{
			"account": "should_ghost@xy3.com", "level": 45, "online": true,
			"state": "IDLE", "task_index": 0,
		}},
		"_zone": testsupportZone()})

	_, body := postJSON(t, env.srv.URL+"/api/start",
		map[string]any{"chain_id": "newbie_full", "accounts": []string{"should_ghost@xy3.com"}}, nil)
	if body["ok"] != true {
		t.Fatalf("提示不阻断下发: %v", body)
	}
	warns, _ := body["warnings"].([]any)
	if len(warns) != 1 {
		t.Fatalf("应回带 1 条提示: %v", body["warnings"])
	}
	w, _ := warns[0].(map[string]any)
	if w["account"] != "should_ghost@xy3.com" || !strings.Contains(toStrAny(w["msg"]), "抓鬼") {
		t.Fatalf("提示要指名道姓 + 说清应分配抓鬼: %v", w)
	}
	if msg, _ := body["msg"].(string); !strings.Contains(msg, "抓鬼") {
		t.Fatalf("提示要拼进 msg（面板 toast 直接显示）: %v", body["msg"])
	}
	// 命令照发（用户可能就是要强制跑新手链）
	cmd := rb.ReadCmd(t, 2*time.Second)
	if cmd["cmd"] != "start_chain" || cmd["chain_id"] != "newbie_full" {
		t.Fatalf("仍按用户选择的链下发: %v", cmd)
	}
}

func toStrAny(v any) string {
	s, _ := v.(string)
	return s
}

// mapKey 把 JSON 里的 mapid（数字或字符串）统一成 map 的 key 形式。
func mapKey(v any) string {
	switch t := v.(type) {
	case float64:
		return strconv.Itoa(int(t))
	case string:
		return t
	}
	return ""
}
