// 2026-09-24 用户口径：「启动」= 恢复当前任务、「停止」= 停当前任务。
//
// "派什么任务"交给定时任务策略 + 后续日常轮转，本组用例锁定四处行为：
//
//	① 行内「启动」：心跳 daily 有该玩法且未满 → 续跑神捕（不分中控开关）；满额 → 回落意图（把名额让出来）；
//	② 策略候选：当天有 daily 记录且未满的号不再让路（意图=抓鬼也会被定时任务捡回）；
//	③ 在线数：shenbu 按心跳 daily 在跑计（开关关时意图恒 ghost，旧口径 online=0 → 池空转）；
//	④ 「停止」：补发 share_daily_stop 收工（既有 stop 只停任务链+抓鬼，神捕会话不停）。
package api_test

import (
	"strings"
	"testing"
	"time"

	"zyctrlcenter/test/testsupport"
)

// shenbuDaily 构造一条心跳 daily 条目（大唐神捕）。
func shenbuDaily(done, limit int, phase string) map[string]any {
	return map[string]any{"share_key": "share_daily_大唐神捕", "done": done, "limit": limit, "state": phase}
}

// ① 行内「启动」= 恢复当前任务：daily 未满 → 续跑神捕（开关默认关也生效 ——
// 心跳 daily 条目 = 机器人当前确实在跑神捕，与意图/开关无关）。
func TestStartResumesRunningShareDaily(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	testsupport.InstallShareDailyNav(t, env.cfg.ChainDir)
	testsupport.InstallGhostNav(t, env.cfg.ChainDir) // 若误判成抓鬼这里会真发（更严格）

	acc := "resume1@xy3.com"
	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{map[string]any{"account": acc, "level": 45, "online": true,
			"state": "IDLE", "task_index": 0, "daily": shenbuDaily(3, 10, "RUNNING")}},
		"_zone": testsupportZone()})

	_, body := postJSON(t, env.srv.URL+"/api/start", map[string]any{
		"auto": true, "accounts": []string{acc}}, nil)
	if body["ok"] != true || body["mode"] != "auto" {
		t.Fatalf("应走自动分配: %v", body)
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	if cmd["cmd"] != "share_daily_start" || cmd["share_key"] != "share_daily_大唐神捕" {
		t.Fatalf("daily 未满的号点「启动」应续跑神捕（不分开关）: %v", cmd)
	}
	if done, _ := cmd["done"].(map[string]any); done[acc] != float64(3) {
		t.Fatalf("续跑应带已做次数（重新下发不丢进度）: %v", cmd["done"])
	}
	groups, _ := body["groups"].(map[string]any)
	if g := asSlice(groups["share_daily_start"]); len(g) != 1 || g[0] != acc {
		t.Fatalf("groups 应把该号分给 share_daily_start: %v", groups)
	}
	reason := ""
	for _, it := range asSlice(body["assignments"]) {
		if m, _ := it.(map[string]any); m["account"] == acc {
			reason, _ = m["reason"].(string)
		}
	}
	if !strings.Contains(reason, "续跑") {
		t.Fatalf("分配理由应写明续跑（面板可解释）: %q", reason)
	}
}

// ① 满额 → 不走续跑，回落意图（45 级 → 抓鬼），把神捕名额让出来。
func TestStartFallsBackWhenShareDailyFull(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	testsupport.InstallShareDailyNav(t, env.cfg.ChainDir)
	testsupport.InstallGhostNav(t, env.cfg.ChainDir)

	acc := "resume2@xy3.com"
	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{map[string]any{"account": acc, "level": 45, "online": true,
			"state": "IDLE", "task_index": 0, "daily": shenbuDaily(10, 10, "SUBMIT")}},
		"_zone": testsupportZone()})

	_, body := postJSON(t, env.srv.URL+"/api/start", map[string]any{
		"auto": true, "accounts": []string{acc}}, nil)
	if body["ok"] != true {
		t.Fatalf("应接受: %v", body)
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	if cmd["cmd"] != "ghost_start" {
		t.Fatalf("神捕已满应回落意图（抓鬼），把名额让出来: %v", cmd)
	}
	groups, _ := body["groups"].(map[string]any)
	if len(asSlice(groups["share_daily_start"])) != 0 {
		t.Fatalf("满额号不该分给 share_daily_start: %v", groups)
	}
}

// ② 策略候选：当天有 daily 记录且未满的号**不再让路** —— 意图仍是 ghost（开关关）、
// 抓鬼未满，旧规则会让路；新规则由定时任务自动捡回（对齐轮转模型）。
func TestShenbuCandidateKeepsRunningDailyAccount(t *testing.T) {
	env := newTestEnv(t, "")
	acc := "robot0001002@xy3.com" // 夹具账号：池内该区可用（心跳等级覆盖池内 36）
	feed := func(daily any) {
		rb := map[string]any{"account": acc, "level": 45, "online": true, "state": "IDLE", "task_index": 0}
		if daily != nil {
			rb["daily"] = daily
		}
		env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
			"robots": []any{rb}, "_zone": testsupportZone()})
	}
	candN := func() float64 {
		res := getJSON(t, env.srv.URL+"/api/autotask")
		cands, _ := res["candidates"].(map[string]any)
		n, _ := cands["shenbu"].(float64)
		return n
	}
	feed(shenbuDaily(1, 10, "RUNNING"))
	if n := candN(); n != 1 {
		t.Fatalf("有 daily 在跑（未满）的 ghost 意图号应进神捕候选（不再让路）: %v", n)
	}
	feed(shenbuDaily(10, 10, "SUBMIT"))
	if n := candN(); n != 0 {
		t.Fatalf("满额号不该进神捕候选: %v", n)
	}
}

// ③ 在线数：shenbu 按心跳 daily 在跑计 —— 开关关（意图恒 ghost）时旧口径永远 0，
// 池会空转"没号可拉"；在跑=1，满额/STOPPED=0。
func TestShenbuOnlineCountByDailyHeartbeat(t *testing.T) {
	env := newTestEnv(t, "")
	acc := "robot0001002@xy3.com"
	feed := func(daily any) {
		rb := map[string]any{"account": acc, "level": 45, "online": true, "state": "IDLE", "task_index": 0}
		if daily != nil {
			rb["daily"] = daily
		}
		env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
			"robots": []any{rb}, "_zone": testsupportZone()})
	}
	running := func() float64 {
		res := getJSON(t, env.srv.URL+"/api/autotask")
		pools, _ := res["pools"].(map[string]any)
		p, _ := pools["shenbu"].(map[string]any)
		n, _ := p["running"].(float64)
		return n
	}
	feed(shenbuDaily(2, 10, "RUNNING"))
	if n := running(); n != 1 {
		t.Fatalf("daily 在跑的号应计入神捕 online（不依赖意图）: %v", n)
	}
	feed(shenbuDaily(10, 10, "SUBMIT"))
	if n := running(); n != 0 {
		t.Fatalf("满额不算在跑: %v", n)
	}
	feed(shenbuDaily(2, 10, "STOPPED"))
	if n := running(); n != 0 {
		t.Fatalf("STOPPED 相位不算在跑: %v", n)
	}
}

// ④ 「停止」= 停当前任务：原有 stop 之外，给心跳 daily 在跑的号补发 share_daily_stop；
// 无 daily / 已 STOPPED 的号不发。停全部（不给账号）也一样覆盖活跃 daily 号。
func TestStopSendsShareDailyStopForRunningAccounts(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	run, idle, done := "stop1@xy3.com", "stop2@xy3.com", "stop3@xy3.com"
	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{
			map[string]any{"account": run, "level": 45, "online": true, "state": "IDLE", "task_index": 0,
				"daily": shenbuDaily(3, 10, "RUNNING")},
			map[string]any{"account": idle, "level": 45, "online": true, "state": "IDLE", "task_index": 0},
			map[string]any{"account": done, "level": 45, "online": true, "state": "IDLE", "task_index": 0,
				"daily": shenbuDaily(10, 10, "STOPPED")},
		},
		"_zone": testsupportZone()})

	// 指定账号（含一个无 daily 的号）：只给活跃 daily 号补发
	_, res := postJSON(t, env.srv.URL+"/api/stop", map[string]any{"accounts": []string{run, idle}}, nil)
	if res["ok"] != true || res["daily_stop"] != float64(1) {
		t.Fatalf("应只给有活跃 daily 的号补发收工: %v", res)
	}
	cmds := map[string]map[string]any{}
	for i := 0; i < 2; i++ {
		cmd := rb.ReadCmd(t, 2*time.Second)
		name, _ := cmd["cmd"].(string)
		cmds[name] = cmd
	}
	if _, ok := cmds["stop"]; !ok {
		t.Fatalf("原有 stop 仍要下发: %v", cmds)
	}
	sd, ok := cmds["share_daily_stop"]
	if !ok {
		t.Fatalf("应补发 share_daily_stop（既有 stop 不停神捕会话）: %v", cmds)
	}
	if accs := asSlice(sd["accounts"]); len(accs) != 1 || accs[0] != run {
		t.Fatalf("收工只该带活跃 daily 的号: %v", sd["accounts"])
	}
	if extra := rb.TryReadCmd(200 * time.Millisecond); extra != nil {
		t.Fatalf("不该有多余命令: %v", extra)
	}

	// 停全部（不给账号）：同样覆盖活跃 daily 号（含上面已 STOPPED 的号不发）
	_, res2 := postJSON(t, env.srv.URL+"/api/stop", map[string]any{}, nil)
	if res2["daily_stop"] != float64(1) {
		t.Fatalf("停全部也要给活跃 daily 号下发收工: %v", res2)
	}
	names := map[string]bool{}
	for i := 0; i < 2; i++ {
		cmd := rb.ReadCmd(t, 2*time.Second)
		name, _ := cmd["cmd"].(string)
		names[name] = true
		if name == "share_daily_stop" {
			if accs := asSlice(cmd["accounts"]); len(accs) != 1 || accs[0] != run {
				t.Fatalf("停全部的收工名单只该有活跃 daily 号: %v", cmd["accounts"])
			}
		}
	}
	if !names["share_daily_stop"] || !names["stop"] {
		t.Fatalf("停全部应同时下发 stop 与 share_daily_stop: %v", names)
	}
}
