// ② 等级闸豁免 + 心跳等级回写账号池（2026-09-29 神捕闸门修复②）。
//
// 背景：池内 zone-level 只在建号/验证时写入 → 长期陈旧（现场当前区 <40 有 4893、≥40 仅 4）；
// ① 离线号候选等级回落该陈旧值被等级闸淘汰 → 掉线即出局；② 心跳有效等级从不回写池。
// 修复：候选等级闸对"已派（续跑）"豁免 + SyncPoolLevel 把心跳等级回写池（变化才写/节流落盘）。
package api_test

import (
	"testing"

	"zyctrlcenter/internal/services/accounts"
)

// 豁免：已派（心跳 daily 神捕、未满）的低等级号仍进 shenbu 候选；未派低等级号仍被等级闸拦。
func TestShareDailyCandidateLevelExemption(t *testing.T) {
	env := newTestEnv(t, "")
	// 候选由**账号池**枚举（autotaskCandidatesCfg 遍历 Accounts.List）→ 两号都要入池
	addUsableAccount(t, env, "lvex_a@xy3.com", 20)
	addUsableAccount(t, env, "lvex_b@xy3.com", 20)
	// A：等级 20 + 心跳 daily（已派续跑）→ 豁免放行
	feedRobot(t, env, "lvex_a@xy3.com", map[string]any{"level": 20, "daily": map[string]any{
		"share_key": "share_daily_大唐神捕", "done": 0, "limit": 10, "state": "READY"}})
	// B：等级 20 未派 → 等级闸拦（对照）
	feedRobot(t, env, "lvex_b@xy3.com", map[string]any{"level": 20})
	body := getJSON(t, env.srv.URL+"/api/autotask")
	cands, _ := body["candidates"].(map[string]any)
	if n, _ := cands["shenbu"].(float64); n != 1 {
		t.Fatalf("已派低等级号应豁免进候选、未派应被拦（期望 count=1），实际 %v", n)
	}

	// 干净环境对照：只有"未派 + 低等级" → 0
	env2 := newTestEnv(t, "")
	addUsableAccount(t, env2, "lvex_c@xy3.com", 20)
	feedRobot(t, env2, "lvex_c@xy3.com", map[string]any{"level": 20})
	body2 := getJSON(t, env2.srv.URL+"/api/autotask")
	cands2, _ := body2["candidates"].(map[string]any)
	if n, _ := cands2["shenbu"].(float64); n != 0 {
		t.Fatalf("未派低等级号不该进候选，实际 %v", n)
	}
}

// 回写：有效等级写入池 zone 记录（只改 Level、保留验证字段）；ctl 区键能归一；无效值不动。
func TestSyncPoolLevelWriteBack(t *testing.T) {
	env := newTestEnv(t, "")
	acc := "lvpool@xy3.com"
	env.pool.Add([]string{acc}, "pwd-lv", testZoneAddr, "")
	env.pool.SetZoneState(acc, testZoneAddr, accounts.ZoneState{Verified: true, Usable: true, Level: 33})

	env.api.SyncPoolLevel(acc, testZoneAddr, 48)
	a, _ := env.pool.Get(acc)
	if z := a.Zone(testZoneAddr); z == nil || z.Level != 48 || !z.Usable || !z.Verified {
		t.Fatalf("回写应只改 Level（保留 Usable/Verified）: %+v", z)
	}

	// ctrl 区键（心跳真实入参）→ gameAddrOf 归一后同样命中（Get 返回副本 → 重新取）
	env.api.SyncPoolLevel(acc, testsupportZone(), 49)
	a, _ = env.pool.Get(acc)
	if z := a.Zone(testZoneAddr); z.Level != 49 {
		t.Fatalf("ctrl 区键应归一到 %s 后写入，实际 level=%d", testZoneAddr, z.Level)
	}

	// 无效值/不在池：不动数据、不新造
	env.api.SyncPoolLevel(acc, testZoneAddr, 0)
	env.api.SyncPoolLevel("nobody@xy3.com", testZoneAddr, 50)
	a, _ = env.pool.Get(acc)
	if a.Zone(testZoneAddr).Level != 49 {
		t.Fatal("无效值不该改动已写等级")
	}
	if _, ok := env.pool.Get("nobody@xy3.com"); ok {
		t.Fatal("不在池的号不该被新造")
	}
}
