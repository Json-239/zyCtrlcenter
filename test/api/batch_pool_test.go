// 2026-09-23 P2：面板「批量上线」的**池容量闸**（三池对齐）。
//
// 背景（docs/04-测试/分析-20260923-抓鬼分配逻辑.md）：批量上线原先没有任何池/容量约束，
// 一次可 add 100~200 个号；上线后被机器人端 auto_roam 兜底游荡、或按意图被各池补号通道
// 拉去干活，把三池目标（抓鬼/新手/游荡余量）冲垮。修复口径（batchOnlineBudgetOf）：
//
//	额度 allow = max(0, 容量 cap - 已占 online)
//	容量 cap   = 水位 target（水位启用且配了目标时；用户口径的"在线总数"）
//	           = 否则 Σ 三池 target（抓鬼/新手/游荡里 >0 的）
//	           = 都没配 → 0 → 不约束（保持旧行为）
//	已占 online = 当前区在线数 + 上线在途（已下发 add、还没上线的号；防连批叠加）
//
// 超出额度的号本次不上线（queued 回带，等池内收工/调大目标），force=true 跳过闸门
// （不误伤"人工强制上线"）；被拦的号**不改变状态**（不标记移除/暂停）。
package api_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zyctrlcenter/internal/services/autotask"
	"zyctrlcenter/internal/services/roampool"
	"zyctrlcenter/internal/services/waterline"
	"zyctrlcenter/internal/state"
	"zyctrlcenter/test/testsupport"
)

// addBatchAccounts 往账号池加 n 个可上线的测试号（密码 + 区与批量上线口径一致），返回名单。
func addBatchAccounts(t *testing.T, env *testEnv, n int, prefix string) []string {
	t.Helper()
	names := make([]string, 0, n)
	for i := 0; i < n; i++ {
		names = append(names, fmt.Sprintf("%s%02d@xy3.com", prefix, i))
	}
	_, add := postJSON(t, env.srv.URL+"/api/accounts/add", map[string]any{
		"accounts": names, "password": "pwdbatch", "zone": "47.96.8.240:2300",
	}, nil)
	if add["ok"] != true {
		t.Fatalf("加号失败: %v", add)
	}
	return names
}

// markOnline 把一批号置为在线（占用批量上线额度）。
func markOnline(env *testEnv, accounts ...string) {
	for _, acc := range accounts {
		env.st.Update(acc, func(r *state.Robot) { r.Online = true })
	}
}

// 超容量截断：容量 3、已有 2 个在线 → 一次勾选 4 个，只该上 1 个（其余 queued）。
func TestRobotsBatchPoolCapTrims(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	// 抓鬼池目标 3 → 容量 3（测试环境没有水位/游荡池）
	if err := env.api.AutoTask.Start(autotask.KindGhost, autotask.Config{
		Kind: autotask.KindGhost, TargetOnline: 3, IntervalSec: 600, BatchMin: 1, BatchMax: 1,
	}); err != nil {
		t.Fatalf("启动抓鬼池失败: %v", err)
	}
	markOnline(env, "robot0001000@xy3.com", "robot0001002@xy3.com")

	accs := addBatchAccounts(t, env, 4, "batchcap")
	_, body := postJSON(t, env.srv.URL+"/api/robots/batch", map[string]any{
		"action": "online", "accounts": accs, "zone": "47.96.8.240:2300",
		"interval_ms": 0, "chunk": 10,
	}, nil)
	if body["sent"] != float64(1) || body["queued_count"] != float64(3) {
		t.Fatalf("容量 3、在线 2 → 只该上 1 个（其余 queued），实际 %v", body)
	}
	if body["pool_cap"] != float64(3) || body["pool_online"] != float64(2) ||
		body["pool_allow"] != float64(1) || body["cap_source"] != "pools" {
		t.Fatalf("容量读数不符（应为 cap=3/online=2/allow=1/source=pools）: %v", body)
	}
	// 机器人实收：只有 1 条 add、只带第一个号（保序）
	cmd := rb.ReadCmd(t, 2*time.Second)
	got, _ := cmd["accounts"].([]any)
	if len(got) != 1 {
		t.Fatalf("应只下发 1 个号: %v", cmd)
	}
	pair, _ := got[0].([]any)
	if pair[0] != accs[0] {
		t.Fatalf("应下发勾选集合里的第一个: %v", got[0])
	}
	if extra := rb.TryReadCmd(300 * time.Millisecond); extra != nil {
		t.Fatalf("不该有多余命令: %v", extra)
	}
	// queued 名单 = 剩余 3 个（保序）
	q := asSlice(body["queued"])
	if len(q) != 3 || q[0] != accs[1] || q[2] != accs[3] {
		t.Fatalf("queued 名单不符（应保序）: %v", q)
	}
}

// 容量已满：一个都不上、不下发任何命令，且**不改变状态**（不标记移除/暂停）。
func TestRobotsBatchPoolCapFullBlocks(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	if err := env.api.AutoTask.Start(autotask.KindGhost, autotask.Config{
		Kind: autotask.KindGhost, TargetOnline: 2, IntervalSec: 600, BatchMin: 1, BatchMax: 1,
	}); err != nil {
		t.Fatalf("启动抓鬼池失败: %v", err)
	}
	markOnline(env, "robot0001000@xy3.com", "robot0001002@xy3.com")

	accs := addBatchAccounts(t, env, 2, "batchfull")
	_, body := postJSON(t, env.srv.URL+"/api/robots/batch", map[string]any{
		"action": "online", "accounts": accs, "zone": "47.96.8.240:2300", "interval_ms": 0,
	}, nil)
	if body["ok"] != false || body["sent"] != float64(0) || body["queued_count"] != float64(2) {
		t.Fatalf("容量已满（2/2）→ 一个都不该上: %v", body)
	}
	if msg, _ := body["msg"].(string); !strings.Contains(msg, "force") {
		t.Fatalf("容量满的提示应告诉用户怎么强制上线: %v", body["msg"])
	}
	if extra := rb.TryReadCmd(300 * time.Millisecond); extra != nil {
		t.Fatalf("容量已满时不该下发任何命令: %v", extra)
	}
	for _, acc := range accs {
		if env.st.IsRemoved(acc) {
			t.Fatalf("被拦的号不该被标记移除（只是本次不上线）: %s", acc)
		}
	}
}

// force=true 跳过容量闸（不误伤"人工强制上线"）。
func TestRobotsBatchForceBypassesPoolCap(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	if err := env.api.AutoTask.Start(autotask.KindGhost, autotask.Config{
		Kind: autotask.KindGhost, TargetOnline: 2, IntervalSec: 600, BatchMin: 1, BatchMax: 1,
	}); err != nil {
		t.Fatalf("启动抓鬼池失败: %v", err)
	}
	markOnline(env, "robot0001000@xy3.com", "robot0001002@xy3.com")

	accs := addBatchAccounts(t, env, 2, "batchforce")
	_, body := postJSON(t, env.srv.URL+"/api/robots/batch", map[string]any{
		"action": "online", "accounts": accs, "zone": "47.96.8.240:2300",
		"interval_ms": 0, "force": true,
	}, nil)
	if body["sent"] != float64(2) || body["queued_count"] != float64(0) || body["forced"] != true {
		t.Fatalf("force=true 应跳过容量闸全量上线: %v", body)
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	if got, _ := cmd["accounts"].([]any); len(got) != 2 {
		t.Fatalf("机器人应收到 2 个号: %v", cmd)
	}
}

// 缺口内全量放行（不延迟）：容量 3、在线 0 → 勾选 3 个一个不少。
func TestRobotsBatchPoolCapFillsDeficitFully(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	if err := env.api.AutoTask.Start(autotask.KindGhost, autotask.Config{
		Kind: autotask.KindGhost, TargetOnline: 3, IntervalSec: 600, BatchMin: 1, BatchMax: 1,
	}); err != nil {
		t.Fatalf("启动抓鬼池失败: %v", err)
	}
	accs := addBatchAccounts(t, env, 3, "batchfit")
	_, body := postJSON(t, env.srv.URL+"/api/robots/batch", map[string]any{
		"action": "online", "accounts": accs, "zone": "47.96.8.240:2300", "interval_ms": 0,
	}, nil)
	if body["sent"] != float64(3) || body["queued_count"] != float64(0) {
		t.Fatalf("缺口（3）内应全量放行、不排队: %v", body)
	}
	if body["pool_allow"] != float64(3) {
		t.Fatalf("额度应为 3: %v", body)
	}
}

// 三池都没配目标 → 不约束（保持旧行为；未配置环境/现有用例不受影响）。
func TestRobotsBatchNoPoolConfigUnlimited(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	accs := addBatchAccounts(t, env, 3, "batchnocap")
	_, body := postJSON(t, env.srv.URL+"/api/robots/batch", map[string]any{
		"action": "online", "accounts": accs, "zone": "47.96.8.240:2300", "interval_ms": 0,
	}, nil)
	if body["sent"] != float64(3) || body["queued_count"] != float64(0) {
		t.Fatalf("池都没配目标 → 不约束，应全量上线: %v", body)
	}
	if body["pool_cap"] != float64(0) || body["pool_online"] != float64(0) || body["cap_source"] != "none" {
		t.Fatalf("未配目标时容量读数应为 0/none: %v", body)
	}
}

// 上线在途也要占额度：第一次上线的号还没登录（快照未更新）时，第二次不许按旧在线数叠加放行。
func TestRobotsBatchOnlineInflightBlocksDoubleSend(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	if err := env.api.AutoTask.Start(autotask.KindGhost, autotask.Config{
		Kind: autotask.KindGhost, TargetOnline: 1, IntervalSec: 600, BatchMin: 1, BatchMax: 1,
	}); err != nil {
		t.Fatalf("启动抓鬼池失败: %v", err)
	}
	accs := addBatchAccounts(t, env, 3, "batchinflight")

	// 第一次：额度 1 → 上 accs[0]，accs[1] 被拦
	_, first := postJSON(t, env.srv.URL+"/api/robots/batch", map[string]any{
		"action": "online", "accounts": accs[:2], "zone": "47.96.8.240:2300", "interval_ms": 0,
	}, nil)
	if first["sent"] != float64(1) || first["queued_count"] != float64(1) {
		t.Fatalf("第一次应只上 1 个: %v", first)
	}
	_ = rb.ReadCmd(t, 2*time.Second) // accs[0] 的 add（号还没登录：快照里没有在线）

	// 第二次（紧接着）：accs[0] 仍在途 → 已占 1 → 额度 0 → accs[2] 也上不了
	_, second := postJSON(t, env.srv.URL+"/api/robots/batch", map[string]any{
		"action": "online", "accounts": []string{accs[2]}, "zone": "47.96.8.240:2300", "interval_ms": 0,
	}, nil)
	if second["sent"] != float64(0) || second["queued_count"] != float64(1) {
		t.Fatalf("在途应占满额度 → 第二次一个都不该上: %v", second)
	}
	if second["pool_online"] != float64(1) {
		t.Fatalf("在途号应计入已占（1）: %v", second)
	}
	if extra := rb.TryReadCmd(300 * time.Millisecond); extra != nil {
		t.Fatalf("不该有第二条上线命令: %v", extra)
	}
}

// 容量优先取水位 target（用户口径的"在线总数"）：抓鬼池目标 100 也不放宽。
func TestRobotsBatchBudgetPrefersWaterline(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	env.api.AutoTask.Start(autotask.KindGhost, autotask.Config{
		Kind: autotask.KindGhost, TargetOnline: 100, IntervalSec: 600, BatchMin: 1, BatchMax: 1,
	})
	env.api.Waterline = waterline.New(filepath.Join(t.TempDir(), "waterline.json"), env.api.WaterlineDeps())
	cfg := waterline.DefaultConfig()
	cfg.Enabled, cfg.Target, cfg.IntervalSec, cfg.MaxStep = true, 2, 60, 5
	if err := env.api.Waterline.SetConfig(cfg); err != nil {
		t.Fatalf("设置水位参数失败: %v", err)
	}

	accs := addBatchAccounts(t, env, 3, "batchwl")
	_, body := postJSON(t, env.srv.URL+"/api/robots/batch", map[string]any{
		"action": "online", "accounts": accs, "zone": "47.96.8.240:2300", "interval_ms": 0,
	}, nil)
	if body["cap_source"] != "waterline" || body["pool_cap"] != float64(2) {
		t.Fatalf("水位启用时容量应取水位 target=2: %v", body)
	}
	if body["sent"] != float64(2) || body["queued_count"] != float64(1) {
		t.Fatalf("水位 target=2 → 只该上 2 个: %v", body)
	}
}

// 游荡池 target 计入回退口径（未开水位器时：容量 = Σ 三池 target）。
func TestRobotsBatchBudgetCountsRoamTarget(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	env.api.Roampool = roampool.New(filepath.Join(t.TempDir(), "roampool.json"), env.api.RoampoolDeps())
	cfg := roampool.DefaultConfig()
	cfg.Target = 2
	if err := env.api.Roampool.SetConfig(cfg); err != nil {
		t.Fatalf("设置游荡池参数失败: %v", err)
	}

	accs := addBatchAccounts(t, env, 3, "batchroam")
	_, body := postJSON(t, env.srv.URL+"/api/robots/batch", map[string]any{
		"action": "online", "accounts": accs, "zone": "47.96.8.240:2300", "interval_ms": 0,
	}, nil)
	if body["cap_source"] != "pools" || body["pool_cap"] != float64(2) {
		t.Fatalf("游荡池 target=2 应计入容量: %v", body)
	}
	if body["sent"] != float64(2) || body["queued_count"] != float64(1) {
		t.Fatalf("容量 2 → 只该上 2 个: %v", body)
	}
}

// 自动选号（不给 accounts）同样受容量闸：夹具里该区可用 2 个，容量 1 → 只上 1 个。
func TestRobotsBatchAutoPickRespectsPoolCap(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	if err := env.api.AutoTask.Start(autotask.KindGhost, autotask.Config{
		Kind: autotask.KindGhost, TargetOnline: 1, IntervalSec: 600, BatchMin: 1, BatchMax: 1,
	}); err != nil {
		t.Fatalf("启动抓鬼池失败: %v", err)
	}
	_, body := postJSON(t, env.srv.URL+"/api/robots/batch", map[string]any{
		"action": "online", "zone": "47.96.8.240:2300", "limit": 5, "chunk": 10, "interval_ms": 0,
	}, nil)
	if body["sent"] != float64(1) || body["queued_count"] != float64(1) {
		t.Fatalf("自动选号也应受容量闸（容量 1）：%v", body)
	}
}
