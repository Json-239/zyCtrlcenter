// 大屏「启动」自动分配链路：按账号**意图**决定发什么命令
// （新手链意图 → start_chain(newbie_full)；抓鬼意图 → ghost_start；没有意图 → 回落到请求给的 chain_id）。
// 这样点"启动"不用先手选链；想强制某条链仍然可以手动指定（auto=false 走原来的行为）。
package api_test

import (
	"testing"
	"time"

	"zyctrlcenter/internal/services/accounts"
	"zyctrlcenter/test/testsupport"
)

func TestStartAutoAssignsByIntent(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	// 抓鬼必须带导航数据下发：装一份小夹具（真实 2MB 会让 handler 写阻塞到超时）
	testsupport.InstallGhostNav(t, env.cfg.ChainDir)

	// 两个号：低等级（新手链意图）、高等级（抓鬼意图）
	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{
			map[string]any{"account": "auto_low@xy3.com", "level": 20, "online": true, "state": "IDLE", "task_index": 0},
			map[string]any{"account": "auto_high@xy3.com", "level": 45, "online": true, "state": "IDLE", "task_index": 0},
		},
		"_zone": testsupportZone()})

	_, body := postJSON(t, env.srv.URL+"/api/start", map[string]any{
		"auto":     true,
		"accounts": []string{"auto_low@xy3.com", "auto_high@xy3.com"},
	}, nil)
	if body["ok"] != true || body["mode"] != "auto" {
		t.Fatalf("应走自动分配: %v", body)
	}
	groups, _ := body["groups"].(map[string]any)
	if len(asSlice(groups["ghost_start"])) != 1 || len(asSlice(groups["start_chain"])) != 1 {
		t.Fatalf("应按意图分成两组（新手链 1 / 抓鬼 1）: %v", groups)
	}

	// 机器人应收到两条命令：一条 start_chain(newbie_full) 一条 ghost_start，各带对账号
	got := map[string][]string{}
	chains := map[string]string{}
	for i := 0; i < 2; i++ {
		cmd := rb.ReadCmd(t, 2*time.Second)
		name, _ := cmd["cmd"].(string)
		accs := []string{}
		for _, v := range asSlice(cmd["accounts"]) {
			if s, ok := v.(string); ok {
				accs = append(accs, s)
			}
		}
		got[name] = accs
		if id, _ := cmd["chain_id"].(string); id != "" {
			chains[name] = id
		}
	}
	if len(got["start_chain"]) != 1 || got["start_chain"][0] != "auto_low@xy3.com" {
		t.Fatalf("新手链意图的号应收到 start_chain: %v", got)
	}
	if chains["start_chain"] != "newbie_full" {
		t.Fatalf("新手链应带 chain_id=newbie_full: %v", chains)
	}
	if len(got["ghost_start"]) != 1 || got["ghost_start"][0] != "auto_high@xy3.com" {
		t.Fatalf("抓鬼意图的号应收到 ghost_start: %v", got)
	}

	// 分配结果要能解释（面板显示"为什么给它这条链"）
	items, _ := body["assignments"].([]any)
	if len(items) != 2 {
		t.Fatalf("应逐号回带分配结果: %v", body["assignments"])
	}
	for _, it := range items {
		m, _ := it.(map[string]any)
		if m["reason"] == "" || m["command"] == "" {
			t.Fatalf("分配结果要带命令与理由: %v", m)
		}
	}
}

// 意图表里还没有这个号时，用**账号池里的当前条件**判（≥31 或已完成 → 抓鬼；<31 → 新手链）：
// 这正是"按当前条件决定分配哪一条链"，而不是盲目发默认链被机器人端跳过。
func TestStartAutoUsesPoolConditionWhenNoIntent(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	testsupport.InstallGhostNav(t, env.cfg.ChainDir)

	zone := "47.96.8.240:2300"
	high, low := "pool_high@xy3.com", "pool_low@xy3.com"
	env.pool.Add([]string{high, low}, "", zone, "")
	env.pool.SetZoneState(high, zone, accounts.ZoneState{Verified: true, Level: 33, ChainDone: true})
	env.pool.SetZoneState(low, zone, accounts.ZoneState{Verified: true, Level: 20})

	_, body := postJSON(t, env.srv.URL+"/api/start", map[string]any{
		"auto": true, "chain_id": "newbie_full", "accounts": []string{high, low},
	}, nil)
	if body["ok"] != true || body["mode"] != "auto" {
		t.Fatalf("应走自动分配: %v", body)
	}
	got := map[string]string{}
	for i := 0; i < 2; i++ {
		cmd := rb.ReadCmd(t, 2*time.Second)
		name, _ := cmd["cmd"].(string)
		for _, v := range asSlice(cmd["accounts"]) {
			if s, ok := v.(string); ok {
				got[s] = name
			}
		}
	}
	if got[high] != "ghost_start" {
		t.Fatalf("≥31 且已完成的号应分到抓鬼（当前条件满足）: %v", got)
	}
	if got[low] != "start_chain" {
		t.Fatalf("<31 的号应分到新手链: %v", got)
	}
	// 每个号只分到一条链（不并行）
	if len(got) != 2 {
		t.Fatalf("不应有号被分到多条链: %v", got)
	}
}

// 没有意图（等级未知/没登记）→ 回落到请求给的 chain_id（保持老行为，不乱猜）。
func TestStartAutoFallsBackToGivenChain(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	_, body := postJSON(t, env.srv.URL+"/api/start", map[string]any{
		"auto": true, "chain_id": "some_chain",
		"accounts": []string{"no_intent@xy3.com"},
	}, nil)
	if body["ok"] != true {
		t.Fatalf("应接受: %v", body)
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	if cmd["cmd"] != "start_chain" || cmd["chain_id"] != "some_chain" {
		t.Fatalf("无意图应回落指定链: %v", cmd)
	}
}

// 手动指定链（auto 缺省）行为不变：一条 start_chain 带全部账号。
func TestStartManualKeepsOldBehavior(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{map[string]any{"account": "m@xy3.com", "level": 45, "online": true, "state": "IDLE"}},
		"_zone":  testsupportZone()})

	_, body := postJSON(t, env.srv.URL+"/api/start",
		map[string]any{"chain_id": "newbie_full", "accounts": []string{"m@xy3.com"}}, nil)
	if body["mode"] == "auto" {
		t.Fatalf("没传 auto 不该走自动分配: %v", body)
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	if cmd["cmd"] != "start_chain" || cmd["chain_id"] != "newbie_full" {
		t.Fatalf("手动指定应原样下发 start_chain: %v", cmd)
	}
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}
