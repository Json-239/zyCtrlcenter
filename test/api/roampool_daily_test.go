// 2026-09-29 P0-2 修法 B（接口层闭环）：游荡池"回收→日常池"通道 ——
// keeper 缺员回收游荡号时直发 share_daily_start（神捕/烽火），把"抓鬼满额转游荡"的
// 富余号（现场游荡池 running 124 > target 100）变成日常池号源（治"候选恒 0"）。
//
// 资格口径（差分断言）：
//   - 抓鬼已满 + 等级达标 + 未满额 → 可回收转投；
//   - 抓鬼未满 → 留给抓鬼（本通路不抢）；
//   - 背包心跳近似满（≥50 条）→ 不回收（派下去必死在采购/交付，P0-2 背包预检）。
package api_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zyctrlcenter/internal/services/autotask"
	"zyctrlcenter/internal/services/roampool"
	"zyctrlcenter/test/testsupport"
)

// roamingGhostFull 造一个"抓鬼满额转游荡"的心跳（等级 45、walk.enabled、ghost 50/50）。
func roamingGhostFull(t *testing.T, env *testEnv, acc string, extra map[string]any) {
	t.Helper()
	st := map[string]any{
		"walk": map[string]any{"enabled": true, "mapid": 6, "state": "walk"},
		"ghost": map[string]any{"done": 50, "limit": 50, "enabled": false,
			"count_date": time.Now().Format("20060102")},
	}
	for k, v := range extra {
		st[k] = v
	}
	feedRobot(t, env, acc, st)
}

// newDailyReclaimKeeper 造一个启用的游荡池 keeper（直连环境 API 的依赖）。
func newDailyReclaimKeeper(t *testing.T, env *testEnv, patch func(*roampool.Config)) *roampool.Keeper {
	t.Helper()
	k := roampool.New(filepath.Join(t.TempDir(), "roampool.json"), env.api.RoampoolDeps())
	cfg := roampool.DefaultConfig()
	cfg.Enabled = true
	cfg.IntervalSec = 60
	cfg.Target = 5
	if patch != nil {
		patch(&cfg)
	}
	if err := k.SetConfig(cfg); err != nil {
		t.Fatalf("设置游荡池参数失败: %v", err)
	}
	env.api.Roampool = k
	return k
}

// 主链路：神捕池缺员 + 抓鬼满额的游荡号 → 回收转投 share_daily_start（带玩法键/载荷）。
func TestRoampoolReclaimsRoamerToShenbu(t *testing.T) {
	env := newTestEnv(t, "")
	testsupport.InstallShareDailyNav(t, env.cfg.ChainDir)
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	if err := env.api.AutoTask.Start(autotask.KindShenbu, autotask.Config{
		Kind: autotask.KindShenbu, IntervalSec: 600, TargetOnline: 3, BatchMin: 1, BatchMax: 1,
	}); err != nil {
		t.Fatalf("启动大唐神捕池失败: %v", err)
	}
	acc := "dr1@xy3.com"
	roamingGhostFull(t, env, acc, nil)

	k := newDailyReclaimKeeper(t, env, nil)
	if !k.Tick(time.Now()) {
		t.Fatal("神捕池缺员 + 有可回收游荡号 → 应下发回收转投")
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	if cmd["cmd"] != "share_daily_start" {
		t.Fatalf("应直发 share_daily_start（机器人端收到即停游荡）: %v", cmd)
	}
	if cmd["share_key"] != "share_daily_大唐神捕" {
		t.Fatalf("应带神捕玩法键: %v", cmd["share_key"])
	}
	accs := asSlice(cmd["accounts"])
	if len(accs) != 1 || accs[0] != acc {
		t.Fatalf("应只派回收选中的号 %s，实际 %v", acc, accs)
	}
	if _, ok := cmd["chain"].(map[string]any); !ok {
		t.Fatalf("必须带链载荷（不带机器人拿不到导航）: %v", cmd)
	}
	if !strings.Contains(k.Status().LastAction, "回收 1 个游荡号给日常池") {
		t.Fatalf("最近动作应说明回收转投日常，实际 %q", k.Status().LastAction)
	}
}

// 抓鬼未满的游荡号不回收给日常（留给抓鬼）→ 无动作 + noop 说明。
func TestRoampoolDailyReclaimSkipsNonGhostFull(t *testing.T) {
	env := newTestEnv(t, "")
	testsupport.InstallShareDailyNav(t, env.cfg.ChainDir)
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	if err := env.api.AutoTask.Start(autotask.KindShenbu, autotask.Config{
		Kind: autotask.KindShenbu, IntervalSec: 600, TargetOnline: 3, BatchMin: 1, BatchMax: 1,
	}); err != nil {
		t.Fatalf("启动大唐神捕池失败: %v", err)
	}
	acc := "dr2@xy3.com"
	feedRobot(t, env, acc, map[string]any{ // 抓鬼未满（done=0）
		"walk": map[string]any{"enabled": true, "mapid": 6, "state": "walk"},
		"ghost": map[string]any{"done": 0, "limit": 50, "enabled": false,
			"count_date": time.Now().Format("20060102")},
	})

	k := newDailyReclaimKeeper(t, env, nil)
	if k.Tick(time.Now()) {
		t.Fatal("抓鬼未满的游荡号不该被回收转投日常")
	}
	if cmd := rb.TryReadCmd(300 * time.Millisecond); cmd != nil {
		t.Fatalf("不该下发任何命令，实际 %v", cmd)
	}
	if !strings.Contains(k.Status().LastAction, "没有可回收的游荡号") {
		t.Fatalf("应说明为什么没动作，实际 %q", k.Status().LastAction)
	}
}

// 背包预检（P0-2）：同是抓鬼满额的游荡号，心跳 bag 达阈值（50 条）→ 不回收（换号）。
func TestRoampoolDailyReclaimSkipsNearFullBag(t *testing.T) {
	env := newTestEnv(t, "")
	testsupport.InstallShareDailyNav(t, env.cfg.ChainDir)
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	if err := env.api.AutoTask.Start(autotask.KindShenbu, autotask.Config{
		Kind: autotask.KindShenbu, IntervalSec: 600, TargetOnline: 3, BatchMin: 1, BatchMax: 1,
	}); err != nil {
		t.Fatalf("启动大唐神捕池失败: %v", err)
	}
	acc := "dr3@xy3.com"
	bag := make([]any, 50) // 达 dailyBagFullSlots 阈值
	for i := range bag {
		bag[i] = map[string]any{"id": float64(i + 1), "count": 1, "name": "杂物"}
	}
	roamingGhostFull(t, env, acc, map[string]any{"bag": bag})

	k := newDailyReclaimKeeper(t, env, nil)
	if k.Tick(time.Now()) {
		t.Fatal("近满包号不该被回收转投日常（派下去必死在采购/交付）")
	}
	if cmd := rb.TryReadCmd(300 * time.Millisecond); cmd != nil {
		t.Fatalf("不该下发任何命令，实际 %v", cmd)
	}
}

// 同批差分：一个近满包 + 一个正常包（都抓鬼满额）→ 只回收正常包那个（换号派）。
func TestRoampoolDailyReclaimSwitchesToUsableAccount(t *testing.T) {
	env := newTestEnv(t, "")
	testsupport.InstallShareDailyNav(t, env.cfg.ChainDir)
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	if err := env.api.AutoTask.Start(autotask.KindShenbu, autotask.Config{
		Kind: autotask.KindShenbu, IntervalSec: 600, TargetOnline: 3, BatchMin: 1, BatchMax: 1,
	}); err != nil {
		t.Fatalf("启动大唐神捕池失败: %v", err)
	}
	bag := make([]any, 50)
	for i := range bag {
		bag[i] = map[string]any{"id": float64(i + 1), "count": 1, "name": "杂物"}
	}
	roamingGhostFull(t, env, "dr_full@xy3.com", map[string]any{"bag": bag})
	roamingGhostFull(t, env, "dr_ok@xy3.com", nil)

	k := newDailyReclaimKeeper(t, env, nil)
	if !k.Tick(time.Now()) {
		t.Fatal("有可用号时应回收转投")
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	accs := asSlice(cmd["accounts"])
	if len(accs) != 1 || accs[0] != "dr_ok@xy3.com" {
		t.Fatalf("应换号派（只收包正常的 dr_ok），实际 %v", accs)
	}
}
