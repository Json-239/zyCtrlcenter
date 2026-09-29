// MergeZoneLevel：心跳"有效等级"回写账号池 zone 记录（2026-09-29 神捕闸门修复②）。
//
// 背景：池内 zone-level 只在建号/验证时写入 → 长期陈旧；离线号候选/补拉回落到该值
// 会被等级闸（min_level=40）淘汰。MergeZoneLevel 是唯一回写入口：读改写同锁、只改 Level、
// 保留验证/密码等字段、变更才算"脏"（调用方按需节流 Save）。
package accounts_test

import (
	"testing"

	"zyctrlcenter/test/testsupport"
)

func TestMergeZoneLevel(t *testing.T) {
	pool := testsupport.NewTestAccounts(t)
	name := "robot0001000@xy3.com"
	acc, ok := pool.Get(name)
	if !ok {
		t.Fatal("夹具应有该号")
	}
	prev := *acc.Zone(zone)

	// 同值：返回 false（不触发落盘）
	if pool.MergeZoneLevel(name, zone, prev.Level) {
		t.Fatal("同值应返回 false")
	}
	// 变更：Level 更新，其余字段原样保留（Get 返回副本 → 每次重新取）
	if !pool.MergeZoneLevel(name, zone, 47) {
		t.Fatal("变化应返回 true")
	}
	acc, _ = pool.Get(name)
	z := acc.Zone(zone)
	if z.Level != 47 {
		t.Fatalf("Level 应=47，实际 %d", z.Level)
	}
	if z.Usable != prev.Usable || z.Verified != prev.Verified || z.Msg != prev.Msg || z.Password != prev.Password {
		t.Fatalf("只该改 Level，其余字段应保留: prev=%+v now=%+v", prev, *z)
	}

	// 无效入参：不作变更
	if pool.MergeZoneLevel(name, zone, 0) {
		t.Fatal("level<=0 应返回 false")
	}
	if pool.MergeZoneLevel(name, zone, -3) {
		t.Fatal("level<0 应返回 false")
	}
	if pool.MergeZoneLevel("nobody@x.com", zone, 50) {
		t.Fatal("不在池的号应返回 false（不新造账号）")
	}
	if pool.MergeZoneLevel("", zone, 50) || pool.MergeZoneLevel(name, "", 50) {
		t.Fatal("空参数应返回 false")
	}

	// 缺区记录：新建一条，Level 起写，不虚标 verified/usable
	other := "10.0.0.1:2400"
	if !pool.MergeZoneLevel(name, other, 51) {
		t.Fatal("缺区记录应新建并返回 true")
	}
	acc, _ = pool.Get(name)
	nz := acc.Zone(other)
	if nz == nil || nz.Level != 51 {
		t.Fatalf("新记录应含 Level=51: %+v", nz)
	}
	if nz.Usable || nz.Verified {
		t.Fatalf("新记录不该虚标验证字段: %+v", nz)
	}
	// 再次同值 → false
	if pool.MergeZoneLevel(name, other, 51) {
		t.Fatal("新记录同值也应返回 false")
	}
	// 字段仍在（合并语义）= 先前 zone 的 47 未被覆盖
	if acc.Zone(zone).Level != 47 {
		t.Fatal("另一区的写入不该影响本区记录")
	}
}
