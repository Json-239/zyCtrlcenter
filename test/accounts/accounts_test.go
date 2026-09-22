// accounts 测试：账号池加载/过滤/统计/选号/增删/区状态写入/热更新。
package accounts_test

import (
	"encoding/json"
	"os"
	"testing"

	"zyctrlcenter/internal/services/accounts"
	"zyctrlcenter/test/testsupport"
)

const zone = "47.96.8.240:2300"

func TestLoadAndQueryFromRealFixture(t *testing.T) {
	pool := testsupport.NewTestAccounts(t)
	if pool.Count() != 3 {
		t.Fatalf("池内应有 3 个账号（真实库裁剪），实际 %d", pool.Count())
	}
	acc, ok := pool.Get("robot0001000@xy3.com")
	if !ok {
		t.Fatal("应能取到 robot0001000@xy3.com")
	}
	if acc.Password != "123456" || acc.Level != 19 {
		t.Fatalf("字段不符: %+v", acc)
	}
	z := acc.Zone(zone)
	if z == nil || !z.Usable || !z.Verified || z.Msg != "登录成功" {
		t.Fatalf("区状态不符: %+v", z)
	}
	// 不可用号（真实数据里 msg=账号不存在(服务端 100)）
	bad, _ := pool.Get("robot0001001@xy3.com")
	if bad.UsableIn(zone) {
		t.Fatal("robot0001001 在该区应不可用")
	}
	// 元信息（面板展示"数据何时导入"）
	meta := pool.Meta()
	if meta["count"] != 3 || meta["source"] == "" {
		t.Fatalf("Meta 不符: %v", meta)
	}
}

func TestFilterAndStats(t *testing.T) {
	pool := testsupport.NewTestAccounts(t)

	// 全部
	if got := pool.List(accounts.Filter{}); len(got) != 3 {
		t.Fatalf("无过滤应返回 3 条，实际 %d", len(got))
	}
	// 按区可用过滤
	usable := pool.List(accounts.Filter{Zone: zone, Usable: true})
	if len(usable) != 2 {
		t.Fatalf("该区可用应 2 个，实际 %d", len(usable))
	}
	// 关键词（角色名命中）
	if got := pool.List(accounts.Filter{Keyword: "测试角色"}); len(got) != 1 {
		t.Fatalf("关键词按角色名过滤失败: %d", len(got))
	}
	// 分页
	if got := pool.List(accounts.Filter{Limit: 2}); len(got) != 2 {
		t.Fatalf("limit 未生效: %d", len(got))
	}
	if got := pool.List(accounts.Filter{Limit: 2, Offset: 2}); len(got) != 1 {
		t.Fatalf("offset 未生效: %d", len(got))
	}

	st := pool.Stats(zone)
	if st["total"] != 3 || st["assigned"] != 3 || st["usable"] != 2 || st["chain_done"] != 1 {
		t.Fatalf("统计不符: %v", st)
	}
}

func TestPickForBatchOnline(t *testing.T) {
	pool := testsupport.NewTestAccounts(t)

	// 只选可用号
	got := pool.Pick(zone, 0, true, nil)
	if len(got) != 2 {
		t.Fatalf("应选出 2 个可用号，实际 %v", got)
	}
	for _, n := range got {
		if n == "robot0001001@xy3.com" {
			t.Fatal("不可用号不应被选中")
		}
	}
	// 排除已在线的号
	picked := pool.Pick(zone, 0, true, map[string]bool{"robot0001000@xy3.com": true})
	if len(picked) != 1 || picked[0] != "robot0001002@xy3.com" {
		t.Fatalf("排除后选号不符: %v", picked)
	}
	// limit 生效
	if got := pool.Pick(zone, 1, true, nil); len(got) != 1 {
		t.Fatalf("limit 未生效: %v", got)
	}
	// 不过滤可用性时能选到全部
	if got := pool.Pick(zone, 0, false, nil); len(got) != 3 {
		t.Fatalf("不过滤时应 3 个: %v", got)
	}
}

func TestAddRemoveAndZoneState(t *testing.T) {
	pool := testsupport.NewTestAccounts(t)

	// 新增（带密码 + 分配到区）
	added, existed := pool.Add([]string{"robot0009999@xy3.com", "robot0001000@xy3.com"}, "pwd999", zone, "手工加池")
	if len(added) != 1 || added[0] != "robot0009999@xy3.com" {
		t.Fatalf("新增清单不符: added=%v", added)
	}
	if len(existed) != 1 || existed[0] != "robot0001000@xy3.com" {
		t.Fatalf("已存在清单不符: %v", existed)
	}
	acc, _ := pool.Get("robot0009999@xy3.com")
	if acc.Password != "pwd999" || acc.Zone(zone) == nil || acc.Note != "手工加池" {
		t.Fatalf("新增账号字段不符: %+v", acc)
	}

	// 写入区状态（模拟验证结果）
	pool.SetZoneState("robot0009999@xy3.com", zone, accounts.ZoneState{Verified: true, Usable: true, Msg: "登录成功"})
	acc, _ = pool.Get("robot0009999@xy3.com")
	if !acc.UsableIn(zone) {
		t.Fatalf("区状态写入失败: %+v", acc.Zone(zone))
	}

	// 删除
	if n := pool.Remove([]string{"robot0009999@xy3.com"}); n != 1 {
		t.Fatalf("删除数量不符: %d", n)
	}
	if _, ok := pool.Get("robot0009999@xy3.com"); ok {
		t.Fatal("删除后不应还能取到")
	}
	if pool.Count() != 3 {
		t.Fatalf("删除后应回到 3 个，实际 %d", pool.Count())
	}
}

// 外部重新导入（文件被覆盖）后，池应自动热更新。
func TestHotReloadAfterReimport(t *testing.T) {
	pool, path := testsupport.NewTestAccountsWithPath(t)
	if pool.Count() != 3 {
		t.Fatalf("初始应有 3 个，实际 %d", pool.Count())
	}
	content := map[string]any{
		"servers": map[string]string{"1": zone},
		"accounts": []map[string]any{
			{"name": "robot0002000@xy3.com", "password": "123456"},
			{"name": "robot0002001@xy3.com", "password": "123456"},
		},
	}
	raw, _ := json.MarshalIndent(content, "", " ")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := pool.Count(); got != 2 {
		t.Fatalf("重新导入后应热更新为 2 个，实际 %d", got)
	}
	if _, ok := pool.Get("robot0002000@xy3.com"); !ok {
		t.Fatal("重新导入的新账号应可见")
	}
}

// 密码只从库里取：查不到就是空（**没有"统一/默认密码"**，用错密码只会反复登录失败）。
func TestPasswordOnlyFromPool(t *testing.T) {
	pool := testsupport.NewTestAccounts(t)
	if got := pool.Password("robot0001000@xy3.com", testZone); got != "123456" {
		t.Fatalf("应取池内密码，实际 %q", got)
	}
	if got := pool.Password("robot9999999@xy3.com", testZone); got != "" {
		t.Fatalf("不在池里应为空（不回退默认密码），实际 %q", got)
	}
	if _, ok := pool.PasswordFor("robot9999999@xy3.com", testZone); ok {
		t.Fatal("不在池里应 ok=false")
	}
}
