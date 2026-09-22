// 生产回归（2026-09-21）：抓鬼号"重登/上线后卡 DIALOG 且没有活跃抓鬼会话"时，
// 恢复引擎（含手动补发）必须把它算作"没在跑"补发 ghost_start。
//
// 现场：机器人进程重启/中控重启后，号重新上线走登录流程，服务端在"领双引导对话"
// 点掉后推一个零选项空对话，机器人端从那一刻起 state=DIALOG 一动不动；且抓鬼会话
// 在重连时丢失（ghost 字段为空）。旧判据把 DIALOG 一律当"在跑" → 40+ 个号永久空转
// （实测 12+ 分钟），autotask 保持数又按"在线+意图"算出 127 ≥ 50 判定达标，全都不管。
package api_test

import (
	"testing"
	"time"

	"zyctrlcenter/test/testsupport"
)

func TestIntentsRestoreGhostStuckInDialog(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	testsupport.InstallGhostNav(t, env.cfg.ChainDir)

	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{map[string]any{
			"account": "stuck1@xy3.com", "level": 41, "online": true,
			"state": "DIALOG", "task_index": 0,
			// 不带 ghost 字段 = 抓鬼会话在重连时丢了（卡死形态）
		}},
		"_zone": testsupportZone(),
	})

	_, res := postJSON(t, env.srv.URL+"/api/intents/restore", map[string]any{}, nil)
	if res["sent"] != float64(1) {
		t.Fatalf("卡 DIALOG 且无抓鬼会话的号应补发: %v", res)
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	if cmd["cmd"] != "ghost_start" || cmd["chain_id"] != "zhongkui_nav" {
		t.Fatalf("应补发带导航数据的 ghost_start: %v", cmd)
	}
}

// 有活跃抓鬼会话的 DIALOG（正常跟钟馗对话）→ 不打扰（补发会把状态重置）。
func TestIntentsRestoreSkipsDialogWithGhostSession(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	testsupport.InstallGhostNav(t, env.cfg.ChainDir)

	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{map[string]any{
			"account": "busy1@xy3.com", "level": 41, "online": true,
			"state": "DIALOG", "task_index": 0,
			"ghost": map[string]any{"enabled": true, "state": "READY"},
		}},
		"_zone": testsupportZone(),
	})

	if _, res := postJSON(t, env.srv.URL+"/api/intents/restore", map[string]any{}, nil); res["sent"] != float64(0) {
		t.Fatalf("有抓鬼会话的 DIALOG 不该补发: %v", res)
	}
}

// DIALOG 里任务在手（task_index != 0，正推进交付/接取）→ 也不打扰。
func TestIntentsRestoreSkipsDialogWithTaskInHand(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	testsupport.InstallGhostNav(t, env.cfg.ChainDir)

	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{map[string]any{
			"account": "hand1@xy3.com", "level": 41, "online": true,
			"state": "DIALOG", "task_index": 2019508,
		}},
		"_zone": testsupportZone(),
	})

	if _, res := postJSON(t, env.srv.URL+"/api/intents/restore", map[string]any{}, nil); res["sent"] != float64(0) {
		t.Fatalf("DIALOG 且任务在手不该补发: %v", res)
	}
}
