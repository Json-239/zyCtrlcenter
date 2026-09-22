// 孵化（autotask kind=hatch）链路的中控侧：候选判据 / 下发 hatch_start / 心跳会话统计 / 到期收工。
//
// 口径（用户给定 + 服务端配置核实，证据见 docs/04-测试/改动-20260922-孵化链路Go侧.md）：
//   - 候选：**在线 && 抓鬼已满（done>=limit 或 state==DONE 或 done>=50）&& 有蛋**；
//   - 坐骑蛋（物品名在已知清单里，或已在装备栏五行珠位 pos=4108）→ 图 6 半月岛；
//   - 元气蛋（物品名"元气蛋"）→ 图 10 大唐东野林，且等级 ≥ 50（低等级必须被排除）；
//   - 下发：{"cmd":"hatch_start", mapid, kind, egg_item, chain(含 map_grids[mapid]), max_minutes, accounts}；
//   - 到期：中控下发 {"cmd":"hatch_stop"}；心跳 hatch.hatched=true → 从 running 移除 + 记日志。
package api_test

import (
	"strings"
	"testing"
	"time"

	"zyctrlcenter/internal/services/autotask"
	"zyctrlcenter/test/testsupport"
)

// bagItem 造一条机器人心跳里的背包摘要（真实形状：{"id","count","name","pos"}）。
func bagItem(name string, pos int) map[string]any {
	return map[string]any{"id": 1001, "count": 1, "name": name, "pos": pos}
}

// ghostFull 造一条"今日抓鬼已满"的会话字段（done>=limit）。
func ghostFull(done, limit int) map[string]any {
	return map[string]any{"enabled": false, "state": "IDLE", "done": done, "limit": limit}
}

// ghostDoneState 造一条"抓鬼会话自己报了 DONE"的字段（done 还没到 limit）。
func ghostDoneState() map[string]any {
	return map[string]any{"enabled": false, "state": "DONE", "done": 0, "limit": 100}
}

// 候选判据：ghost 未满 / 无蛋 / 元气蛋低等级 都要被排除；坐骑蛋+抓满 被选中。
func TestHatchCandidatesRequireFullGhostAndEgg(t *testing.T) {
	env := newTestEnv(t, "")
	installWalkChain(t, env)

	cases := []struct {
		acc   string
		level int
		ghost map[string]any
		bag   []any
		note  string
	}{
		{"h_mount@xy3.com", 45, ghostFull(50, 50), []any{bagItem("青牛", 8192)}, "坐骑蛋+抓满 → 候选"},
		{"h_notfull@xy3.com", 45, ghostFull(10, 50), []any{bagItem("青牛", 8192)}, "抓鬼没满 → 排除"},
		{"h_noegg@xy3.com", 45, ghostDoneState(), []any{bagItem("金创药", 8192)}, "没蛋 → 排除"},
		{"h_low@xy3.com", 40, ghostFull(50, 50), []any{bagItem("元气蛋", 8192)}, "元气蛋 40 级 → 排除"},
		{"h_guardian@xy3.com", 55, ghostFull(52, 100), []any{bagItem("元气蛋", 8192)}, "元气蛋 done>=50 → 候选"},
		{"h_equip@xy3.com", 45, ghostDoneState(), []any{bagItem("?", 4108)}, "蛋已在装备栏 4108 → 候选"},
		{"h_alias@xy3.com", 45, ghostFull(50, 50), []any{bagItem("白羊羊", 8192)}, "机器人端名(白羊羊)→ 候选"},
	}
	robots := []any{}
	for _, c := range cases {
		addUsableAccount(t, env, c.acc, c.level)
		robots = append(robots, map[string]any{
			"account": c.acc, "level": c.level, "online": true, "state": "IDLE",
			"ghost": c.ghost, "bag": c.bag,
		})
	}
	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": robots, "_zone": testsupportZone()})

	body := getJSON(t, env.srv.URL+"/api/autotask")
	cands, _ := body["candidates"].(map[string]any)
	if cands["hatch"] != float64(4) {
		t.Fatalf("孵化候选应为 4（坐骑蛋+抓满、元气蛋 55 级、装备栏 4108 有蛋、机器人端名白羊羊）: %v", cands)
	}
	pools, _ := body["pools"].(map[string]any)
	hp, _ := pools["hatch"].(map[string]any)
	if hp == nil || hp["usable"] != float64(4) || hp["running"] != float64(0) {
		t.Fatalf("孵化池统计：usable=候选数、running=活跃会话数: %v", pools["hatch"])
	}
}

// 坐骑蛋端到端：下发 hatch_start（图 6 / egg_item / max_minutes / 链载荷）→ 心跳孵化中 →
// /api/status 透传 hatch → 到期中控下发 hatch_stop 并清会话。
func TestHatchMountEndToEndThenTimeout(t *testing.T) {
	env := newTestEnv(t, "")
	installWalkChain(t, env)
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	acc := "h_mount@xy3.com"
	addUsableAccount(t, env, acc, 45)
	feedRobot(t, env, acc, map[string]any{
		"level": 45, "ghost": ghostFull(50, 50), "bag": []any{bagItem("青牛", 8192)}})

	// 启动策略（保持 1 个 / 单次限时 45 分钟）+ 立即跑一轮
	if _, st := postJSON(t, env.srv.URL+"/api/autotask/start", map[string]any{
		"kind": "hatch", "target_online": 1, "interval_sec": 300, "max_minutes": 45}, nil); st["ok"] != true {
		t.Fatalf("启动孵化定时任务应成功: %v", st)
	}
	if _, run := postJSON(t, env.srv.URL+"/api/autotask/run", map[string]any{"kind": "hatch"}, nil); run["ok"] != true {
		t.Fatalf("立即跑一轮应成功: %v", run)
	}

	cmd := rb.ReadCmd(t, 2*time.Second)
	if toStrAny(cmd["cmd"]) != "hatch_start" {
		t.Fatalf("命令应为 hatch_start: %v", cmd)
	}
	if cmd["mapid"] != float64(6) || toStrAny(cmd["kind"]) != "mount" || cmd["egg_item"] != float64(101316) {
		t.Fatalf("坐骑蛋应去图 6、kind=mount、egg_item=101316: %v", cmd)
	}
	if cmd["max_minutes"] != float64(45) {
		t.Fatalf("max_minutes 应随命令下发: %v", cmd["max_minutes"])
	}
	if got := asSlice(cmd["accounts"]); len(got) != 1 || toStrAny(got[0]) != acc {
		t.Fatalf("accounts 应只含该号: %v", cmd["accounts"])
	}
	chain, _ := cmd["chain"].(map[string]any)
	if chain == nil {
		t.Fatalf("必须带链载荷: %v", cmd)
	}
	grids, _ := chain["map_grids"].(map[string]any)
	if _, ok := grids["6"]; !ok {
		t.Fatalf("链载荷里必须有图 6 的寻路网格: %v", grids)
	}

	// 机器人上报"孵化中"：running 计入，候选不再含它（保持数已达标）
	feedRobot(t, env, acc, map[string]any{"hatch": map[string]any{
		"active": true, "kind": "mount", "egg_item": 101316, "mapid": 6,
		"battles": 3, "hatched": false, "reason": "", "since_ms": 1758500000000}})

	at := getJSON(t, env.srv.URL+"/api/autotask")
	pools, _ := at["pools"].(map[string]any)
	hp, _ := pools["hatch"].(map[string]any)
	if hp["running"] != float64(1) {
		t.Fatalf("活跃孵化会话应计入 running: %v", hp)
	}
	if hp["target"] != float64(1) || hp["deficit"] != float64(0) {
		t.Fatalf("保持数应回显（target=1 / deficit=0）: %v", hp)
	}
	if cands, _ := at["candidates"].(map[string]any); cands["hatch"] != float64(0) {
		t.Fatalf("已在孵化的号不该再算候选: %v", cands)
	}
	sess := asSlice(at["hatch_sessions"])
	if len(sess) != 1 || toStrAny(sess[0].(map[string]any)["account"]) != acc {
		t.Fatalf("应回带在孵化的会话（面板显示用）: %v", at["hatch_sessions"])
	}

	// C. status 透传：同一份 hatch 结构原样给面板
	st := getJSON(t, env.srv.URL+"/api/status")
	var row map[string]any
	for _, it := range asSlice(st["robots"]) {
		if m, _ := it.(map[string]any); toStrAny(m["account"]) == acc {
			row = m
		}
	}
	hatch, _ := row["hatch"].(map[string]any)
	if row == nil || hatch == nil || hatch["battles"] != float64(3) || hatch["mapid"] != float64(6) {
		t.Fatalf("status 应原样透传 hatch 字段: %v", row)
	}

	// 到期收工：把时钟推过 max_minutes（Engine.Tick 的 now 参数就是它），中控应下发 hatch_stop
	env.api.AutoTask.Tick(time.Now().Add(46 * time.Minute))
	stop := rb.ReadCmd(t, 2*time.Second)
	if toStrAny(stop["cmd"]) != "hatch_stop" {
		t.Fatalf("到期应下发 hatch_stop: %v", stop)
	}
	if got := asSlice(stop["accounts"]); len(got) != 1 || toStrAny(got[0]) != acc {
		t.Fatalf("hatch_stop 应带该号: %v", stop["accounts"])
	}
	if at2 := getJSON(t, env.srv.URL+"/api/autotask"); len(asSlice(at2["hatch_sessions"])) != 0 {
		t.Fatalf("收工后会话应清空: %v", at2["hatch_sessions"])
	}
}

// 孵出（hatch.hatched=true）：running 立刻归零 + 记一条"孵化完成"日志 + 会话收工。
func TestHatchHatchedClearsRunningAndLogs(t *testing.T) {
	env := newTestEnv(t, "")
	installWalkChain(t, env)
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	acc := "h_done@xy3.com"
	addUsableAccount(t, env, acc, 45)
	feedRobot(t, env, acc, map[string]any{
		"level": 45, "ghost": ghostFull(50, 50), "bag": []any{bagItem("青牛", 8192)}})

	if ok, msg := env.api.LaunchTask(autotask.KindHatch, []string{acc}); !ok {
		t.Fatalf("孵化下发应成功: %s", msg)
	}
	if cmd := rb.ReadCmd(t, 2*time.Second); toStrAny(cmd["cmd"]) != "hatch_start" {
		t.Fatalf("应先收到 hatch_start: %v", cmd)
	}
	feedRobot(t, env, acc, map[string]any{"hatch": map[string]any{
		"active": true, "kind": "mount", "egg_item": 101316, "mapid": 6, "battles": 12}})
	if hp := hatchPool(t, env); hp["running"] != float64(1) {
		t.Fatalf("孵化中 running 应为 1: %v", hp)
	}

	// 孵出（坐骑已入槽）
	feedRobot(t, env, acc, map[string]any{"hatch": map[string]any{
		"active": false, "kind": "mount", "egg_item": 101316, "mapid": 6,
		"battles": 38, "hatched": true, "reason": "坐骑已入槽"}})

	if hp := hatchPool(t, env); hp["running"] != float64(0) {
		t.Fatalf("孵出后 running 应归零（从 running 移除）: %v", hp)
	}
	if !storeHasMsg(env, "孵化完成") {
		t.Fatalf("孵出应记一条日志（运行历史）: %v", env.store.ReadTail(20))
	}
	// 会话收工：中控下发 hatch_stop（不依赖 5 秒心跳，Tick 里清理）
	env.api.AutoTask.Tick(time.Now())
	if cmd := rb.ReadCmd(t, 2*time.Second); toStrAny(cmd["cmd"]) != "hatch_stop" {
		t.Fatalf("孵出后应下发 hatch_stop 收工: %v", cmd)
	}
	if at := getJSON(t, env.srv.URL+"/api/autotask"); len(asSlice(at["hatch_sessions"])) != 0 {
		t.Fatalf("孵出后会话应清空: %v", at["hatch_sessions"])
	}
}

// 元气蛋：目标图 10（大唐东野林）、kind=guardian、egg_item=102603。
func TestHatchGuardianEggGoesToMap10(t *testing.T) {
	env := newTestEnv(t, "")
	installWalkChain(t, env)
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	acc := "h_guardian@xy3.com"
	addUsableAccount(t, env, acc, 55)
	feedRobot(t, env, acc, map[string]any{
		"level": 55, "ghost": ghostFull(50, 50), "bag": []any{bagItem("元气蛋", 8192)}})

	if ok, msg := env.api.LaunchTask(autotask.KindHatch, []string{acc}); !ok {
		t.Fatalf("元气蛋孵化下发应成功: %s", msg)
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	if toStrAny(cmd["cmd"]) != "hatch_start" || cmd["mapid"] != float64(10) ||
		toStrAny(cmd["kind"]) != "guardian" || cmd["egg_item"] != float64(102603) {
		t.Fatalf("元气蛋应去图 10、kind=guardian、egg_item=102603: %v", cmd)
	}
}

// 停策略 = 收工：给在孵化的号下发 hatch_stop（否则号会一直游荡没人管到期）。
func TestHatchStopStrategyStopsSessions(t *testing.T) {
	env := newTestEnv(t, "")
	installWalkChain(t, env)
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	acc := "h_stop@xy3.com"
	addUsableAccount(t, env, acc, 45)
	feedRobot(t, env, acc, map[string]any{
		"level": 45, "ghost": ghostFull(50, 50), "bag": []any{bagItem("青牛", 8192)}})

	if ok, msg := env.api.LaunchTask(autotask.KindHatch, []string{acc}); !ok {
		t.Fatalf("孵化下发应成功: %s", msg)
	}
	if cmd := rb.ReadCmd(t, 2*time.Second); toStrAny(cmd["cmd"]) != "hatch_start" {
		t.Fatalf("应先收到 hatch_start: %v", cmd)
	}

	_, stop := postJSON(t, env.srv.URL+"/api/autotask/stop", map[string]any{"kind": "hatch"}, nil)
	if stop["ok"] != true || !strings.Contains(toStrAny(stop["msg"]), "hatch_stop") {
		t.Fatalf("停孵化应说明已收工: %v", stop)
	}
	if cmd := rb.ReadCmd(t, 2*time.Second); toStrAny(cmd["cmd"]) != "hatch_stop" {
		t.Fatalf("停策略应给在孵化的号下发 hatch_stop: %v", cmd)
	}
	if at := getJSON(t, env.srv.URL+"/api/autotask"); len(asSlice(at["hatch_sessions"])) != 0 {
		t.Fatalf("停策略后会话应清空: %v", at["hatch_sessions"])
	}
}

// ---------------------------------------------------------------- 小工具

// hatchPool 取 /api/autotask 里孵化池的统计。
func hatchPool(t *testing.T, env *testEnv) map[string]any {
	t.Helper()
	body := getJSON(t, env.srv.URL+"/api/autotask")
	pools, _ := body["pools"].(map[string]any)
	hp, _ := pools["hatch"].(map[string]any)
	if hp == nil {
		t.Fatalf("应回带孵化池统计: %v", body["pools"])
	}
	return hp
}

// storeHasMsg 运行历史里是否有包含该文本的日志。
func storeHasMsg(env *testEnv, text string) bool {
	for _, e := range env.store.ReadTail(200) {
		if strings.Contains(toStrAny(e["msg"]), text) {
			return true
		}
	}
	return false
}
