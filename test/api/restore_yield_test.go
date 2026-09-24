// 2026-09-24 RESTORE 误补发修复用例（现场：robot0001029 烽火 / robot0005278 神捕 各被熔断）：
//
// 根因链：机器人端分享日常无状态持久化 → 重登后心跳 daily 丢失 → 意图被判回 ghost →
// RESTORE/reghost 按 ghost 意图补发 ghost_start（机器人端日常与抓鬼互斥，把在跑的日常顶掉）
// → 点钟馗连续无对话卡死 → 重登→再补发循环 → 当日熔断。
//
// 本组用例钉住两层修复：
//
//	① event.decideIntent 台账兜底：重登（心跳丢失）时，当天"已派该日常且未见满额"的号
//	   维持该玩法意图（与 handlers.decideKind 的「续跑·台账」同款）—— 意图不再丢日常；
//	② reghost 重登补发的 kind 决策（ReghostDeps.Launch）：
//	   - 意图=shenbu/fenghuo → 按该玩法补发 share_daily_start；
//	   - 意图=ghost/未知 但当天有"已派/在跑未满"的日常 → 按该日常恢复（绝不补 ghost_start）。
package api_test

import (
	"testing"
	"time"

	"zyctrlcenter/internal/services/intent"
	"zyctrlcenter/test/testsupport"
)

// ① 重登（心跳 daily 丢失）时靠台账维持神捕意图；满额 = 自由号 → 判回抓鬼（名额让出来）。
func TestDecideIntentKeepsAssignedDailyOnRelogin(t *testing.T) {
	env := newTestEnv(t, "")
	acc := "ry_ledger_intent@xy3.com"
	env.st.MarkShareDailyAssigned(acc, ghostMutexShareKey)
	feedRobot(t, env, acc, nil) // 重登：心跳不带 daily（机器人端会话丢失）
	if it, ok := env.ev.Intents.Get(acc); !ok || it.Kind != intent.KindShenbu {
		t.Fatalf("重登应靠台账保持神捕意图（否则 RESTORE 会按 ghost 补发顶掉它）: %+v", it)
	}
	// 满额（独立满额表）→ 自由号 → 判回抓鬼
	env.st.MarkShareDailyFull(acc, ghostMutexShareKey)
	feedRobot(t, env, acc, nil)
	if it, _ := env.ev.Intents.Get(acc); it.Kind != intent.KindGhost {
		t.Fatalf("神捕满额 = 自由号，应判回抓鬼: %+v", it)
	}
	// 对照：无台账、无心跳的普通号照旧判抓鬼（兜底不误伤）
	ctl := "ry_ledger_none@xy3.com"
	feedRobot(t, env, ctl, nil)
	if it, _ := env.ev.Intents.Get(ctl); it.Kind != intent.KindGhost {
		t.Fatalf("无日常记录的号应判抓鬼（对照）: %+v", it)
	}
}

// ② reghost 重登补发：意图=ghost 但当天已派神捕未满 → 按神捕恢复（发 share_daily_start，
// 不发 ghost_start）；无台账的对照号照旧发 ghost_start。
func TestReghostLaunchPrefersAssignedDailyOverGhost(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	testsupport.InstallGhostNav(t, env.cfg.ChainDir)
	testsupport.InstallShareDailyNav(t, env.cfg.ChainDir)

	deps := env.api.ReghostDeps()

	acc, ctl := "ry_reghost_ledger@xy3.com", "ry_reghost_none@xy3.com"
	feedRobot(t, env, acc, nil) // 意图=ghost（无 daily）
	feedRobot(t, env, ctl, nil)
	env.st.MarkShareDailyAssigned(acc, ghostMutexShareKey) // 当天已派神捕未满

	if !deps.Launch(acc) {
		t.Fatal("重登补发应成功")
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	if cmd["cmd"] != "share_daily_start" || cmd["share_key"] != ghostMutexShareKey {
		t.Fatalf("有当天已派未满神捕 → 重登应按神捕恢复（不补 ghost_start 顶掉它）: %v", cmd)
	}
	if len(asSlice(cmd["accounts"])) != 1 || asSlice(cmd["accounts"])[0] != acc {
		t.Fatalf("补发应指定该号: %v", cmd["accounts"])
	}

	if !deps.Launch(ctl) {
		t.Fatal("对照号重登补发应成功")
	}
	cmd2 := rb.ReadCmd(t, 2*time.Second)
	if cmd2["cmd"] != "ghost_start" || asSlice(cmd2["accounts"])[0] != ctl {
		t.Fatalf("无日常记录的号应照旧补发 ghost_start（对照）: %v", cmd2)
	}
}

// ② 意图=fenghuo（开关开 + 心跳未满）→ 重登补发按烽火大唐恢复（share_key=share_daily_宫廷10）。
func TestReghostLaunchFollowsFenghuoIntent(t *testing.T) {
	t.Setenv("CTRL_FENGHUO", "1")
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	testsupport.InstallFenghuoNav(t, env.cfg.ChainDir)

	acc := "ry_reghost_fh@xy3.com"
	feedRobot(t, env, acc, map[string]any{"daily": map[string]any{
		"share_key": "share_daily_宫廷10", "done": 1, "limit": 20, "state": "RUNNING"}})
	if it, ok := env.ev.Intents.Get(acc); !ok || it.Kind != intent.KindFenghuo {
		t.Fatalf("开关开 + 烽火未满 应登记 fenghuo 意图: %+v", it)
	}

	if !env.api.ReghostDeps().Launch(acc) {
		t.Fatal("重登补发应成功")
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	if cmd["cmd"] != "share_daily_start" || cmd["share_key"] != "share_daily_宫廷10" {
		t.Fatalf("意图=fenghuo → 重登应按烽火大唐恢复: %v", cmd)
	}
}
