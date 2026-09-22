// 游荡下发（POST /api/random_walk[_stop]）：链载荷组装（目标图网格 + 跨图路由）+ 命令内容 + 失败说明。
//
// 契约（机器人端 random_walk.py:dispatch_cmd，2026-09-22 核实）：
//   - 命令**必须带 chain**（机器人端只认命令里的链数据：npcs/map_grids/dijkstra）；
//   - 命令**必须带 accounts**（不带 = 机器人端对所有号生效，接口层直接拦掉空账号）；
//   - 不在线 / 无状态记录的号逐个给"为什么没下发"（不静默）；
//   - 目标图必须在链数据里有网格、dijkstra 非空，否则硬失败（一条命令都不发）。
package api_test

import (
	"strings"
	"testing"
	"time"

	"zyctrlcenter/internal/services/accounts"
	"zyctrlcenter/test/testsupport"
)

// installWalkChain 装"游荡导航"链数据：基座链 newbie_full（图 6 半月岛 / 图 10 大唐东野林的
// 真实网格 + 跨图路由，从 data/chains/newbie_full.json 裁剪）。
func installWalkChain(t *testing.T, env *testEnv) {
	t.Helper()
	testsupport.InstallChainFixture(t, env.cfg.ChainDir, "newbie_full", "chains/walk_maps.mini.json")
}

// feedRobot 用一个心跳把账号写成在线（游荡/孵化都只下发给在线号）。
func feedRobot(t *testing.T, env *testEnv, acc string, extra map[string]any) {
	t.Helper()
	st := map[string]any{"account": acc, "online": true, "state": "IDLE", "level": 45}
	for k, v := range extra {
		st[k] = v
	}
	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{st}, "_zone": testsupportZone()})
}

// addUsableAccount 入池并标"已验证可用"（自动任务的候选只看池内号）。
func addUsableAccount(t *testing.T, env *testEnv, acc string, level int) {
	t.Helper()
	env.pool.Add([]string{acc}, "", testZoneAddr, "")
	env.pool.SetZoneState(acc, testZoneAddr, accounts.ZoneState{Verified: true, Usable: true, Level: level})
}

// 游荡命令：目标图网格 + 跨图路由必须随命令下发（机器人端不给就是原地不动）。
func TestRandomWalkDeliversChainWithTargetMap(t *testing.T) {
	env := newTestEnv(t, "")
	installWalkChain(t, env)
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	acc := "rw_ok@xy3.com"
	feedRobot(t, env, acc, nil)

	_, body := postJSON(t, env.srv.URL+"/api/random_walk", map[string]any{
		"accounts": []string{acc}, "mapid": 6, "minutes": 30}, nil)
	if body["ok"] != true || body["sent"] != float64(1) || body["failed"] != float64(0) {
		t.Fatalf("下发应成功且 sent=1/failed=0: %v", body)
	}

	cmd := rb.ReadCmd(t, 2*time.Second)
	if toStrAny(cmd["cmd"]) != "random_walk" {
		t.Fatalf("命令应为 random_walk: %v", cmd)
	}
	if cmd["mapid"] != float64(6) {
		t.Fatalf("目标图应为 6（半月岛）: %v", cmd["mapid"])
	}
	if cmd["minutes"] != float64(30) {
		t.Fatalf("minutes 应透传: %v", cmd["minutes"])
	}
	if got := asSlice(cmd["accounts"]); len(got) != 1 || toStrAny(got[0]) != acc {
		t.Fatalf("accounts 应只含在线号: %v", cmd["accounts"])
	}
	chain, _ := cmd["chain"].(map[string]any)
	if chain == nil {
		t.Fatalf("必须带链载荷（机器人端只认命令里的链数据）: %v", cmd)
	}
	grids, _ := chain["map_grids"].(map[string]any)
	if _, ok := grids["6"]; !ok {
		t.Fatalf("链载荷里必须有目标图 6 的寻路网格: %v", grids)
	}
	if dj, _ := chain["dijkstra"].(map[string]any); len(dj) == 0 {
		t.Fatalf("链载荷里必须有跨图路由（dijkstra）: %v", chain["dijkstra"])
	}
}

// 链数据缺失 / 目标图没有网格：明确报错，一条命令都不发（宁可失败也不发"走不过去"的载荷）。
func TestRandomWalkFailsLoudlyWhenChainUnusable(t *testing.T) {
	cases := []struct {
		name        string
		install     bool // 是否装链数据
		mapid       int
		wantMsgPart string
	}{
		{"链数据文件不存在", false, 6, "链数据文件不存在"},
		{"目标图没有网格", true, 25, "map_grids"}, // 2026-09-22：24 已成游荡排除图，换个非排除图测"没有网格"
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t, "")
			if tc.install {
				installWalkChain(t, env) // 只装了图 6/10（夹具）
			}
			rb := testsupport.ConnectFakeRobot(t, env.ctrl)
			defer rb.Close()
			acc := "rw_bad@xy3.com"
			feedRobot(t, env, acc, nil)

			_, body := postJSON(t, env.srv.URL+"/api/random_walk", map[string]any{
				"accounts": []string{acc}, "mapid": tc.mapid}, nil)
			if body["ok"] != false {
				t.Fatalf("载荷不可用必须失败: %v", body)
			}
			if !strings.Contains(toStrAny(body["msg"]), tc.wantMsgPart) {
				t.Fatalf("报错应说明原因（含 %q）: %v", tc.wantMsgPart, body["msg"])
			}
			if cmd := rb.TryReadCmd(200 * time.Millisecond); cmd != nil {
				t.Fatalf("不该下发任何命令: %v", cmd)
			}
		})
	}
}

// mapid / accounts 的入参校验（不猜不静默）：缺 mapid、空账号都要明确拒绝。
func TestRandomWalkValidatesParams(t *testing.T) {
	env := newTestEnv(t, "")
	installWalkChain(t, env)

	_, noMap := postJSON(t, env.srv.URL+"/api/random_walk",
		map[string]any{"accounts": []string{"rw_ok@xy3.com"}}, nil)
	if noMap["ok"] != false || !strings.Contains(toStrAny(noMap["msg"]), "mapid") {
		t.Fatalf("缺 mapid 应明确拒绝: %v", noMap)
	}
	_, noAcc := postJSON(t, env.srv.URL+"/api/random_walk", map[string]any{"mapid": 6}, nil)
	if noAcc["ok"] != false || !strings.Contains(toStrAny(noAcc["msg"]), "accounts") {
		t.Fatalf("空账号应明确拒绝（不带账号=机器人端对所有号生效）: %v", noAcc)
	}
}

// 不在线的号：逐个给"为什么没下发"，在线的那部分照发。
func TestRandomWalkReportsOfflineAccounts(t *testing.T) {
	env := newTestEnv(t, "")
	installWalkChain(t, env)
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	online, offline, unknown := "rw_on@xy3.com", "rw_off@xy3.com", "rw_unknown@xy3.com"
	feedRobot(t, env, online, nil)
	feedRobot(t, env, offline, map[string]any{"online": false})

	_, body := postJSON(t, env.srv.URL+"/api/random_walk", map[string]any{
		"accounts": []string{online, offline, unknown}, "mapid": 6}, nil)
	if body["ok"] != true || body["sent"] != float64(1) {
		t.Fatalf("只应下发给在线号: %v", body)
	}
	if body["failed"] != float64(2) {
		t.Fatalf("离线 + 无状态记录都应计入 failed（不静默）: %v", body)
	}
	fails := asSlice(body["failed_accounts"])
	msgs := map[string]string{}
	for _, it := range fails {
		m, _ := it.(map[string]any)
		msgs[toStrAny(m["account"])] = toStrAny(m["msg"])
	}
	if !strings.Contains(msgs[offline], "不在线") {
		t.Fatalf("离线号应说明「不在线」: %v", msgs)
	}
	if !strings.Contains(msgs[unknown], "没有状态记录") {
		t.Fatalf("无状态记录的号应说明原因: %v", msgs)
	}
	if !strings.Contains(toStrAny(body["msg"]), "未下发") {
		t.Fatalf("msg 里要带上未下发原因: %v", body["msg"])
	}

	cmd := rb.ReadCmd(t, 2*time.Second)
	if got := asSlice(cmd["accounts"]); len(got) != 1 || toStrAny(got[0]) != online {
		t.Fatalf("命令只该带在线号: %v", cmd["accounts"])
	}
}

// 停游荡：带账号只发这些号的；不带账号 = 全部（与 /api/stop 同口径）。
func TestRandomWalkStopDeliversAccounts(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	acc := "rw_stop@xy3.com"
	feedRobot(t, env, acc, nil)

	_, body := postJSON(t, env.srv.URL+"/api/random_walk/stop", map[string]any{
		"accounts": []string{acc}}, nil)
	if body["ok"] != true || body["sent"] != float64(1) {
		t.Fatalf("停止游荡应成功: %v", body)
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	if toStrAny(cmd["cmd"]) != "random_walk_stop" {
		t.Fatalf("命令应为 random_walk_stop: %v", cmd)
	}
	if got := asSlice(cmd["accounts"]); len(got) != 1 || toStrAny(got[0]) != acc {
		t.Fatalf("accounts 应透传: %v", cmd["accounts"])
	}

	// 不带账号 = 停全部（命令不带 accounts，机器人端按"所有号"处理）
	_, all := postJSON(t, env.srv.URL+"/api/random_walk/stop", map[string]any{}, nil)
	if all["ok"] != true {
		t.Fatalf("停全部应成功: %v", all)
	}
	cmd2 := rb.ReadCmd(t, 2*time.Second)
	if toStrAny(cmd2["cmd"]) != "random_walk_stop" {
		t.Fatalf("命令应为 random_walk_stop: %v", cmd2)
	}
	if cmd2["accounts"] != nil {
		t.Fatalf("停全部不该带 accounts: %v", cmd2["accounts"])
	}
}
