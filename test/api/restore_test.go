// 恢复引擎接口：GET /api/intents 带恢复状态；POST /api/intents/restore 手动补发一次。
// （自动补发由 restorer 的 Run 循环负责；这里验"手动触发 + 命令真的到了机器人"）
package api_test

import (
	"testing"
	"time"

	"zyctrlcenter/test/testsupport"
)

func TestIntentsRestoreManualSendsCommand(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	// 心跳把账号置为"在线 + 空闲 + 20 级"：既登记意图（newbie），又满足补发条件
	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{map[string]any{
			"account": "robot0007100@xy3.com", "level": 20, "online": true,
			"state": "IDLE", "task_index": 0,
		}},
		"_zone": testsupportZone(),
	})

	// 列表要能看到意图 + 恢复状态 + 总开关（默认关）
	body := getJSON(t, env.srv.URL+"/api/intents")
	if body["auto_restore"] != false {
		t.Fatalf("默认应关闭自动补发（避免误发命令）: %v", body["auto_restore"])
	}
	if body["recover"] == nil {
		t.Fatalf("应回带每账号恢复状态: %v", body)
	}

	// 手动补发一次 → 机器人应收到 start_chain(newbie_full)
	_, res := postJSON(t, env.srv.URL+"/api/intents/restore", map[string]any{}, nil)
	if res["ok"] != true || res["sent"] != float64(1) {
		t.Fatalf("应补发 1 条: %v", res)
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	if cmd["cmd"] != "start_chain" || cmd["chain_id"] != "newbie_full" {
		t.Fatalf("机器人应收到 start_chain(newbie_full): %v", cmd)
	}
	accounts, _ := cmd["accounts"].([]any)
	if len(accounts) != 1 || accounts[0] != "robot0007100@xy3.com" {
		t.Fatalf("应指定该账号: %v", cmd)
	}

	// 紧接着再来一次：冷却中，不该重复发
	_, res2 := postJSON(t, env.srv.URL+"/api/intents/restore", map[string]any{}, nil)
	if res2["sent"] != float64(0) {
		t.Fatalf("冷却期内不该重复补发: %v", res2)
	}
	// 只补指定账号
	if _, res3 := postJSON(t, env.srv.URL+"/api/intents/restore",
		map[string]any{"account": "other@x.com"}, nil); res3["sent"] != float64(0) {
		t.Fatalf("指定别的账号不该补发: %v", res3)
	}
}

// 同命令 + 同链的号**合并成一条**命令：两个抓鬼号 → 机器人只收 1 条 ghost_start
// （带一份导航载荷，而不是一人一份 2MB）；sent=账号数、commands=命令数。
func TestIntentsRestoreMergesGhostAccountsIntoOneCommand(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	testsupport.InstallGhostNav(t, env.cfg.ChainDir)

	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{
			map[string]any{"account": "merge1@xy3.com", "level": 45, "online": true, "state": "IDLE", "task_index": 0},
			map[string]any{"account": "merge2@xy3.com", "level": 46, "online": true, "state": "IDLE", "task_index": 0},
		},
		"_zone": testsupportZone(),
	})

	_, res := postJSON(t, env.srv.URL+"/api/intents/restore", map[string]any{}, nil)
	if res["ok"] != true || res["sent"] != float64(2) || res["commands"] != float64(1) {
		t.Fatalf("两个抓鬼号应合并成 1 条命令（sent=2 账号 / commands=1 命令）: %v", res)
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	if cmd["cmd"] != "ghost_start" || cmd["role"] != "solo" || cmd["chain_id"] != "zhongkui_nav" {
		t.Fatalf("补发也必须是带导航数据的 ghost_start（否则掉线恢复后原地不动）: %v", cmd)
	}
	if cmd["daily_limit"] != float64(50) {
		t.Fatalf("补发同样要带 daily_limit（与「启动」同口径/参考实现 intent_restore 一致）: %v", cmd["daily_limit"])
	}
	accs := asSlice(cmd["accounts"])
	if len(accs) != 2 {
		t.Fatalf("一条命令应覆盖两个账号: %v", cmd["accounts"])
	}
	chain, _ := cmd["chain"].(map[string]any)
	if len(asSlice(chain["ghost_maps"])) == 0 {
		t.Fatalf("补发必须带 chain.ghost_maps: %v", cmd["chain"])
	}
	if extra := rb.TryReadCmd(200 * time.Millisecond); extra != nil {
		t.Fatalf("合并后机器人只该收到一条命令: %v", extra)
	}
}

// 正在跑的号不该被打扰（补发会把链重置，属于破坏性操作）。
func TestIntentsRestoreSkipsRunningRobot(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{map[string]any{
			"account": "robot0007101@xy3.com", "level": 42, "online": true,
			"state": "NAV", "task_index": 7001003,
		}},
		"_zone": testsupportZone(),
	})

	_, res := postJSON(t, env.srv.URL+"/api/intents/restore", map[string]any{}, nil)
	if res["sent"] != float64(0) {
		t.Fatalf("正在导航的号不该补发: %v", res)
	}
	// 意图本身仍然登记着（面板能看到"该跑抓鬼"）
	body := getJSON(t, env.srv.URL+"/api/intents")
	if body["count"] != float64(1) {
		t.Fatalf("意图应仍在: %v", body)
	}
}
