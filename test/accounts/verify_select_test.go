// 账号池「批量验证选号」相关用例：默认从本地库按区挑待验证账号 + 记录验证时间。
package accounts_test

import (
	"testing"

	"zyctrlcenter/internal/services/accounts"
	"zyctrlcenter/test/testsupport"
)

const testZone = "47.96.8.240:2300" // 夹具里 3 个号所在区

// 夹具现状：3 个号在该区都是 verified（1000 可用 / 1001 不可用 / 1002 可用）。
func TestSelectForVerifyScopesOnFixturePool(t *testing.T) {
	pool, _ := testsupport.NewTestAccountsWithPath(t)

	// 都验证过了 → unverified 应为空（否则会反复重验已知结果）
	if got := pool.SelectForVerify(testZone, accounts.VerifyScopeUnverified, 0, nil); len(got) != 0 {
		t.Fatalf("全部已验证时不该再选出来: %v", got)
	}
	// unusable → 只挑出"存在但不可用"的那个（重验看是否恢复）
	un := pool.SelectForVerify(testZone, accounts.VerifyScopeUnusable, 0, nil)
	if len(un) != 1 || un[0] != "robot0001001@xy3.com" {
		t.Fatalf("unusable 应只挑 robot0001001，实际 %v", un)
	}
	// all → 池内全部（升序）
	all := pool.SelectForVerify(testZone, accounts.VerifyScopeAll, 0, nil)
	if len(all) != 3 {
		t.Fatalf("all 应挑 3 个，实际 %v", all)
	}
	for i := 1; i < len(all); i++ {
		if all[i-1] > all[i] {
			t.Fatalf("必须按账号名升序（结果稳定可复现）: %v", all)
		}
	}
}

// 新加的号（该区没有任何状态）属于"未验证"——这才是日常要批量补验的对象。
func TestSelectForVerifyUnverifiedIncludesNewAccounts(t *testing.T) {
	pool, _ := testsupport.NewTestAccountsWithPath(t)
	pool.Add([]string{"robot0002001@xy3.com", "robot0002000@xy3.com"}, "123456", testZone, "")

	got := pool.SelectForVerify(testZone, accounts.VerifyScopeUnverified, 0, nil)
	want := []string{"robot0002000@xy3.com", "robot0002001@xy3.com"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("unverified 应挑出新加的两个号（升序），实际 %v", got)
	}

	// 验证过一个之后，它就不该再出现在 unverified 里
	st := accounts.ZoneState{Verified: true, Usable: true, Msg: "登录成功", VerifiedAt: 1789810000}
	pool.SetZoneState("robot0002000@xy3.com", testZone, st)
	got = pool.SelectForVerify(testZone, accounts.VerifyScopeUnverified, 0, nil)
	if len(got) != 1 || got[0] != "robot0002001@xy3.com" {
		t.Fatalf("已验证的号不该再被选中，实际 %v", got)
	}
}

// 关键口径："该区的账号" = 池里有该区记录的号；
// 完全没记录的属于 unknown（探明"该区有没有它"才用），**不能**混进默认范围（否则会把整池卷进来）。
func TestSelectForVerifyUnverifiedOnlyCountsKnownZoneRows(t *testing.T) {
	pool, _ := testsupport.NewTestAccountsWithPath(t)

	// 陌生区：三个号都没有该区记录 → 默认范围（unverified）应为空，unknown 才是 3
	if got := pool.SelectForVerify("10.0.0.9:2300", accounts.VerifyScopeUnverified, 0, nil); len(got) != 0 {
		t.Fatalf("陌生区不该进默认（unverified）范围: %v", got)
	}
	got := pool.SelectForVerify("10.0.0.9:2300", accounts.VerifyScopeUnknown, 0, nil)
	if len(got) != 3 {
		t.Fatalf("unknown 应挑出全部 3 个: %v", got)
	}

	// 该区有记录但"未验证"（导入库里 verified=false）→ 进默认范围
	pool.SetZoneStateNoSave("robot0001000@xy3.com", testZone, accounts.ZoneState{Msg: "待验证"})
	_ = pool.Save()
	def := pool.SelectForVerify(testZone, accounts.VerifyScopeUnverified, 0, nil)
	if len(def) != 1 || def[0] != "robot0001000@xy3.com" {
		t.Fatalf("该区有记录但未验证的号应进默认范围，实际 %v", def)
	}

	// 数量预览要与选号一致
	cnt := pool.CountForVerify(testZone)
	if cnt["unverified"] != 1 || cnt["unusable"] != 1 || cnt["unknown"] != 0 || cnt["all"] != 3 {
		t.Fatalf("数量预览不符（unverified=1/unusable=1/unknown=0/all=3）: %v", cnt)
	}
	cntNew := pool.CountForVerify("10.0.0.9:2300")
	if cntNew["unknown"] != 3 || cntNew["unverified"] != 0 {
		t.Fatalf("陌生区预览应为 unknown=3/unverified=0: %v", cntNew)
	}
}

func TestSelectForVerifyLimitAndExclude(t *testing.T) {
	pool, _ := testsupport.NewTestAccountsWithPath(t)
	// limit 生效
	if got := pool.SelectForVerify(testZone, accounts.VerifyScopeAll, 1, nil); len(got) != 1 {
		t.Fatalf("limit=1 应只挑 1 个，实际 %v", got)
	}
	// exclude（已在线等）生效
	skip := map[string]bool{"robot0001000@xy3.com": true, "robot0001002@xy3.com": true}
	got := pool.SelectForVerify(testZone, accounts.VerifyScopeAll, 0, skip)
	if len(got) != 1 || got[0] != "robot0001001@xy3.com" {
		t.Fatalf("exclude 应跳过指定账号，实际 %v", got)
	}
}

// 验证时间要能落盘（面板展示"什么时候验的"、后续可做"过期重验"）。
// 「本区全部」（scope=zone）：该区**有记录**的全部账号（含已验证/不可用），无记录的不算。
// 面板默认就用它：点一下把本区账号全验一遍。
func TestSelectForVerifyZoneScope(t *testing.T) {
	pool, _ := testsupport.NewTestAccountsWithPath(t)
	zone := "47.96.8.240:2300"

	has := func(list []string, name string) bool {
		for _, n := range list {
			if n == name {
				return true
			}
		}
		return false
	}

	if len(pool.SelectForVerify(zone, accounts.VerifyScopeZone, 0, nil)) == 0 {
		t.Fatal("夹具里该区应有带记录的账号")
	}

	// 已验证可用的号：unverified 不选，但"本区全部"要选
	pool.Add([]string{"robot0007777@xy3.com"}, "", "", "")
	pool.SetZoneState("robot0007777@xy3.com", zone, accounts.ZoneState{Verified: true, Usable: true})
	if has(pool.SelectForVerify(zone, accounts.VerifyScopeUnverified, 0, nil), "robot0007777@xy3.com") {
		t.Fatal("unverified 不该含已验证的号")
	}
	if !has(pool.SelectForVerify(zone, accounts.VerifyScopeZone, 0, nil), "robot0007777@xy3.com") {
		t.Fatal("本区全部应包含已验过且可用的号")
	}

	// 该区无记录的号：不算"本区账号"（探明有没有它要用 unknown）
	pool.Add([]string{"robot0007778@xy3.com"}, "", "", "")
	if has(pool.SelectForVerify(zone, accounts.VerifyScopeZone, 0, nil), "robot0007778@xy3.com") {
		t.Fatal("该区无记录的号不该算『本区账号』")
	}

	// limit：0/负数 = 不限；正数按数量截断
	all := pool.SelectForVerify(zone, accounts.VerifyScopeZone, 0, nil)
	if len(pool.SelectForVerify(zone, accounts.VerifyScopeZone, 1, nil)) != 1 {
		t.Fatal("limit=1 应只返回 1 个")
	}
	if len(pool.SelectForVerify(zone, accounts.VerifyScopeZone, -1, nil)) != len(all) {
		t.Fatal("limit<0（不限）应与 0 等价")
	}

	// 计数也要给出 zone（面板显示"本次将验 N 个"）
	if got := pool.CountForVerify(zone)["zone"]; got != len(all) {
		t.Fatalf("CountForVerify[zone]=%d 应=%d", got, len(all))
	}
}

func TestZoneStateVerifiedAtPersists(t *testing.T) {
	pool, path := testsupport.NewTestAccountsWithPath(t)
	pool.SetZoneState("robot0001000@xy3.com", testZone,
		accounts.ZoneState{Verified: true, Usable: true, Msg: "登录成功", VerifiedAt: 1789812345})

	reloaded := accounts.New(path)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("重新加载失败: %v", err)
	}
	acc, ok := reloaded.Get("robot0001000@xy3.com")
	if !ok {
		t.Fatal("账号应存在")
	}
	z := acc.Zone(testZone)
	if z == nil || z.VerifiedAt != 1789812345 {
		t.Fatalf("verified_at 应落盘并读回，实际 %+v", z)
	}
}

// 批量写回用的"不落盘"变体：多次写内存、最后统一 Save（避免 5000 个号一次批量写 5000 次文件）。
func TestSetZoneStateNoSaveThenSave(t *testing.T) {
	pool, path := testsupport.NewTestAccountsWithPath(t)
	pool.SetZoneStateNoSave("robot0001000@xy3.com", testZone, accounts.ZoneState{Verified: true, Usable: false, Msg: "暂不可用"})
	pool.SetZoneStateNoSave("robot0001001@xy3.com", testZone, accounts.ZoneState{Verified: true, Usable: true, Msg: "登录成功"})

	// 内存立即生效
	if acc, _ := pool.Get("robot0001001@xy3.com"); acc.Zone(testZone).Usable != true {
		t.Fatal("不落盘变体也应立即改内存")
	}
	if err := pool.Save(); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}

	reloaded := accounts.New(path)
	if err := reloaded.Load(); err != nil {
		t.Fatal(err)
	}
	if acc, _ := reloaded.Get("robot0001000@xy3.com"); acc.Zone(testZone).Usable {
		t.Fatal("落盘内容应与内存一致（1000 不可用）")
	}
	if acc, _ := reloaded.Get("robot0001001@xy3.com"); !acc.Zone(testZone).Usable {
		t.Fatal("落盘内容应与内存一致（1001 可用）")
	}
}
