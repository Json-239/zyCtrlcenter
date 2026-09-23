// 2026-09-23 R1/R2（操作健壮性审计修复）：派发在途记账扩展。
//
//   - R1：`start_chain`（新手链/捉鬼链）也有在途记账 —— 连点「启动(自动分配)」不再重复下发
//     （修复前新手链没有在途，第二次点击会把同一批号再派一遍）；
//   - R2：定时任务候选与派发排除在途号 —— 「手动派发 + 定时轮」在命令生效前的空窗里
//     不再重复派同一号（抓鬼重复下发=机器人端重启会话）。
//
// 口径：在途 TTL 120s（ghostInflightTTL）；"生效"判据与各自"在跑"同源
// （ghost=活跃会话；chain=在线且任务在推进）。
package api_test

import (
	"strings"
	"testing"
	"time"

	"zyctrlcenter/internal/services/autotask"
	"zyctrlcenter/test/testsupport"
)

// R1：配额不限（newbie 池没配目标 → poolQuota=-1）时，连点两次「启动(自动分配)」——
// 第二次必须被**在途去重**拦下。
func TestStartAutoChainInflightDedup(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	accs := []string{"n1@xy3.com", "n2@xy3.com", "n3@xy3.com"}
	robots := []any{}
	for _, acc := range accs {
		robots = append(robots, map[string]any{
			"account": acc, "level": 5, "online": true, "state": "IDLE", "task_index": 0})
	}
	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": robots, "_zone": testsupportZone()})

	_, body := postJSON(t, env.srv.URL+"/api/start", map[string]any{
		"auto": true, "accounts": accs,
	}, nil)
	groups, _ := body["groups"].(map[string]any)
	if got := len(asSlice(groups["start_chain"])); got != 3 {
		t.Fatalf("第一次应派 3 个 start_chain，实派 %d（msg=%v）", got, body["msg"])
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	if cmd["cmd"] != "start_chain" {
		t.Fatalf("应收 start_chain: %v", cmd)
	}
	if got := len(asSlice(cmd["accounts"])); got != 3 {
		t.Fatalf("首条命令应带 3 个号: %v", cmd["accounts"])
	}

	// 第二次（紧接着）：这批号还在途 → 一个都不派，且 assignments 回带原因（不静默）
	_, body2 := postJSON(t, env.srv.URL+"/api/start", map[string]any{
		"auto": true, "accounts": accs,
	}, nil)
	groups2, _ := body2["groups"].(map[string]any)
	if n := len(asSlice(groups2["start_chain"])); n != 0 {
		t.Fatalf("在途去重 → 第二次一个都不该派，实派 %d（msg=%v）", n, body2["msg"])
	}
	nBlocked := 0
	for _, v := range asSlice(body2["assignments"]) {
		m, _ := v.(map[string]any)
		if m["command"] == "" && containsText(m["reason"], "最近") {
			nBlocked++
		}
	}
	if nBlocked != 3 {
		t.Fatalf("3 个号都该被在途去重并回带原因，实得 %d（assignments=%v）", nBlocked, body2["assignments"])
	}
	if extra := rb.TryReadCmd(300 * time.Millisecond); extra != nil {
		t.Fatalf("第二次不应有下行命令，实收: %v", extra)
	}
}

// R1：有目标的新手池（target=5；3 个号在线且意图已登记 → "在跑"计 3）——第一次只派
// 差额 2 个（配额），第二次 0 个（在途 2 个占满目标 5）。
func TestStartAutoChainInflightCountedInQuota(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	if err := env.api.AutoTask.Start(autotask.KindNewbie, autotask.Config{
		Kind: autotask.KindNewbie, TargetOnline: 5, IntervalSec: 600, BatchMin: 1, BatchMax: 1,
	}); err != nil {
		t.Fatalf("启动新手池失败: %v", err)
	}

	accs := []string{"nq1@xy3.com", "nq2@xy3.com", "nq3@xy3.com"}
	robots := []any{}
	for _, acc := range accs {
		robots = append(robots, map[string]any{
			"account": acc, "level": 5, "online": true, "state": "IDLE", "task_index": 0})
	}
	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": robots, "_zone": testsupportZone()})

	_, body := postJSON(t, env.srv.URL+"/api/start", map[string]any{
		"auto": true, "accounts": accs,
	}, nil)
	groups, _ := body["groups"].(map[string]any)
	if got := len(asSlice(groups["start_chain"])); got != 2 {
		t.Fatalf("新手池目标 5、在跑 3 → 只该派差额 2 个，实派 %d（msg=%v）", got, body["msg"])
	}
	_ = rb.ReadCmd(t, 2*time.Second)

	// 第二次：2 个在途占满目标 2 → 0 个
	_, body2 := postJSON(t, env.srv.URL+"/api/start", map[string]any{
		"auto": true, "accounts": accs,
	}, nil)
	groups2, _ := body2["groups"].(map[string]any)
	if n := len(asSlice(groups2["start_chain"])); n != 0 {
		t.Fatalf("在途已占满新手池目标 → 第二次一个都不该派，实派 %d（msg=%v）", n, body2["msg"])
	}
	if extra := rb.TryReadCmd(300 * time.Millisecond); extra != nil {
		t.Fatalf("第二次不应有下行命令，实收: %v", extra)
	}
}

// R2：候选排除在途号 —— 手动派 1 个后，该号不再出现在 /api/autotask 的候选里
// （另有一个没派过的号保持候选，证明是"按号排除"而不是整体失效）。
func TestAutotaskCandidatesSkipGhostInflight(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	testsupport.InstallGhostNav(t, env.cfg.ChainDir)

	// autotask 候选来自**账号池** → 号必须入池（运行时状态由 status_reply 提供）
	env.pool.Add([]string{"ci1@xy3.com", "ci2@xy3.com"}, "pwd", "", "")
	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{
			map[string]any{"account": "ci1@xy3.com", "level": 45, "online": true, "state": "IDLE", "task_index": 0},
			map[string]any{"account": "ci2@xy3.com", "level": 45, "online": true, "state": "IDLE", "task_index": 0},
		}, "_zone": testsupportZone()})

	if got := candidatesOf(t, env, "ghost"); got != 2 {
		t.Fatalf("初始应有 2 个抓鬼候选，实得 %d", got)
	}

	_, body := postJSON(t, env.srv.URL+"/api/start", map[string]any{
		"auto": true, "accounts": []string{"ci1@xy3.com"},
	}, nil)
	groups, _ := body["groups"].(map[string]any)
	if got := len(asSlice(groups["ghost_start"])); got != 1 {
		t.Fatalf("手动应派 1 个，实派 %d（msg=%v）", got, body["msg"])
	}
	_ = rb.ReadCmd(t, 2*time.Second)

	if got := candidatesOf(t, env, "ghost"); got != 1 {
		t.Fatalf("在途号应被候选排除（只剩 ci2），实得 %d", got)
	}

	// ci1 已建立活跃会话（命令生效）→ 不再算在途 → 回到候选
	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{
			map[string]any{"account": "ci1@xy3.com", "level": 45, "online": true, "state": "FIGHT",
				"task_index": 0, "ghost": map[string]any{"enabled": true, "done": 1, "limit": 50}},
		}, "_zone": testsupportZone()})
	if got := candidatesOf(t, env, "ghost"); got != 1 {
		t.Fatalf("会话已建立（在跑）时 ci1 不算在途；候选应仍为 ci2 一个，实得 %d", got)
	}
}

// R2 强证明：手动派发后立刻让定时任务跑一轮 —— 在途号 q1 不被重复派，
// 未派过的 q2 正常被挑中（证明 tick 本身在工作，q1 是被单独排除的）。
func TestAutotaskTickNoRepeatAfterManualStart(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	testsupport.InstallGhostNav(t, env.cfg.ChainDir)

	if err := env.api.AutoTask.Start(autotask.KindGhost, autotask.Config{
		Kind: autotask.KindGhost, TargetOnline: 100, IntervalSec: 600, BatchMin: 1, BatchMax: 1,
	}); err != nil {
		t.Fatalf("启动抓鬼池失败: %v", err)
	}
	env.pool.Add([]string{"t1@xy3.com", "t2@xy3.com"}, "pwd", "", "")
	env.ev.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
		"robots": []any{
			map[string]any{"account": "t1@xy3.com", "level": 45, "online": true, "state": "IDLE", "task_index": 0},
			map[string]any{"account": "t2@xy3.com", "level": 45, "online": true, "state": "IDLE", "task_index": 0},
		}, "_zone": testsupportZone()})

	// 手动派 t1
	_, body := postJSON(t, env.srv.URL+"/api/start", map[string]any{
		"auto": true, "accounts": []string{"t1@xy3.com"},
	}, nil)
	groups, _ := body["groups"].(map[string]any)
	if got := len(asSlice(groups["ghost_start"])); got != 1 {
		t.Fatalf("手动应派 1 个，实派 %d（msg=%v）", got, body["msg"])
	}
	first := rb.ReadCmd(t, 2*time.Second)
	if !cmdHasAccount(first, "t1@xy3.com") {
		t.Fatalf("第一条命令应是 t1 的 ghost_start: %v", first)
	}

	// 定时任务立刻跑一轮：t1 在途被排除，只能挑 t2
	env.api.AutoTask.RunNow(autotask.KindGhost)
	env.api.AutoTask.Tick(time.Now())
	next := rb.ReadCmd(t, 2*time.Second)
	if next["cmd"] != "ghost_start" {
		t.Fatalf("定时任务应派 ghost_start: %v", next)
	}
	if cmdHasAccount(next, "t1@xy3.com") {
		t.Fatalf("在途号 t1 不该被定时任务重复派: %v", next)
	}
	if !cmdHasAccount(next, "t2@xy3.com") {
		t.Fatalf("未派过的 t2 应被挑中（证明 tick 在工作）: %v", next)
	}
	if extra := rb.TryReadCmd(300 * time.Millisecond); extra != nil {
		t.Fatalf("一轮只该派一条命令，实收第二条: %v", extra)
	}
}

// ---------------------------------------------------------------- 小工具

// candidatesOf 读 /api/autotask 的候选数（kind: newbie/ghost/hatch）。
func candidatesOf(t *testing.T, env *testEnv, kind string) int {
	t.Helper()
	body := getJSON(t, env.srv.URL+"/api/autotask")
	m, _ := body["candidates"].(map[string]any)
	n, _ := m[kind].(float64)
	return int(n)
}

// cmdHasAccount 下行命令的 accounts 里是否含某账号。
func cmdHasAccount(cmd map[string]any, acc string) bool {
	for _, v := range asSlice(cmd["accounts"]) {
		if toStrAny(v) == acc {
			return true
		}
	}
	return false
}

// containsText 字符串包含判断（JSON 解出的可能是 nil）。
func containsText(v any, sub string) bool {
	s, _ := v.(string)
	return strings.Contains(s, sub)
}
