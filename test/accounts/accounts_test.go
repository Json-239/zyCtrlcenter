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

// 2026-09-23 选号轮换（反例钉死旧行为）：旧实现"按账号名升序取前 N" → 名字最靠前的
// robot0001000 永远先被挑走（现场：上线/补号长期集中在号段开头那批）。
// 新口径 = **从没上过线最前 + 最久未上线优先 + 同值随机平局**。
func TestPickPrefersLongestOffline(t *testing.T) {
	pool := testsupport.NewTestAccounts(t)
	// 0001000：名字最前但"刚上过线"；0001002：名字靠后但"很久没上" —— 都可用
	pool.SetFields("robot0001000@xy3.com", func(a *accounts.Account) { a.LastOnline = 5000 })
	pool.SetFields("robot0001002@xy3.com", func(a *accounts.Account) { a.LastOnline = 100 })

	got := pool.Pick(zone, 1, true, nil)
	if len(got) != 1 || got[0] != "robot0001002@xy3.com" {
		t.Fatalf("应挑最久未上线的号（旧实现会挑名字最前的 0001000）: %v", got)
	}

	// 新增一个"从没上过线"的可用号（last_online=0）→ 必须排在最久未上线号之前
	pool.Add([]string{"robot0009999@xy3.com"}, "pw9999", zone, "")
	pool.SetZoneState("robot0009999@xy3.com", zone, accounts.ZoneState{Verified: true, Usable: true, Msg: "登录成功"})
	if got := pool.Pick(zone, 1, true, nil); len(got) != 1 || got[0] != "robot0009999@xy3.com" {
		t.Fatalf("从没上过线的号应排最前: %v", got)
	}
	if got := pool.Pick(zone, 2, true, nil); len(got) != 2 ||
		got[0] != "robot0009999@xy3.com" || got[1] != "robot0001002@xy3.com" {
		t.Fatalf("顺序应为「从未上线 → 最久未上线」: %v", got)
	}
}

// 同 last_online 用注入的随机源打破平局：不同序列给出不同顺序；更旧的号永远优先（平局随机
// 不会越过"最久未上线优先"的边界）。
func TestPickTieBreakUsesInjectedRand(t *testing.T) {
	pool := testsupport.NewTestAccounts(t)
	pool.SetFields("robot0001000@xy3.com", func(a *accounts.Account) { a.LastOnline = 900 })
	pool.SetFields("robot0001002@xy3.com", func(a *accounts.Account) { a.LastOnline = 900 })

	pool.SetRand(func(n int) int { return n - 1 }) // 原地不换 = 保持池内顺序
	a := pool.Pick(zone, 1, true, nil)
	pool.SetRand(func(n int) int { return 0 }) // 每步与 0 交换 = 反序
	b := pool.Pick(zone, 1, true, nil)
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("平局组里应各挑 1 个: a=%v b=%v", a, b)
	}
	if a[0] == b[0] {
		t.Fatalf("不同随机源应给出不同的平局顺序: a=%v b=%v", a, b)
	}
	for _, n := range []string{a[0], b[0]} {
		if n != "robot0001000@xy3.com" && n != "robot0001002@xy3.com" {
			t.Fatalf("挑出的号必须来自候选: a=%v b=%v", a, b)
		}
	}

	// 把 0001000 调得更旧 → 不再平局：无论随机序列怎么给，都必须先挑它
	pool.SetFields("robot0001000@xy3.com", func(x *accounts.Account) { x.LastOnline = 1 })
	for _, rnd := range []func(int) int{
		func(int) int { return 0 },
		func(n int) int { return n - 1 },
	} {
		pool.SetRand(rnd)
		if got := pool.Pick(zone, 1, true, nil); len(got) != 1 || got[0] != "robot0001000@xy3.com" {
			t.Fatalf("更旧的号必须优先（随机不越过该边界）: %v", got)
		}
	}
}

// TouchOnline = 运行期唯一会写 last_online 的入口（上线下发成功后回写；旧代码没有写入点，
// 导致"最久未上线优先"会退化成静态顺序）。用例覆盖：跳过池外号、只往大改、落盘可恢复、
// 且与 Pick 联动（刚记过的号排到后面）。
func TestTouchOnlineStampsLastOnline(t *testing.T) {
	pool, path := testsupport.NewTestAccountsWithPath(t)

	if n := pool.TouchOnline([]string{"robot0001000@xy3.com", "robot0001002@xy3.com",
		"robot9999999@xy3.com"}, 424242); n != 2 {
		t.Fatalf("池外号应跳过（只记 2 个），实际 %d", n)
	}
	for _, name := range []string{"robot0001000@xy3.com", "robot0001002@xy3.com"} {
		acc, _ := pool.Get(name)
		if acc.LastOnline != 424242 {
			t.Fatalf("%s 的 last_online 应记为 424242: %d", name, acc.LastOnline)
		}
	}
	// 只往大改（防时钟回拨把"最近上过线"抹掉）
	pool.TouchOnline([]string{"robot0001000@xy3.com"}, 100)
	if acc, _ := pool.Get("robot0001000@xy3.com"); acc.LastOnline != 424242 {
		t.Fatalf("更小的时间戳不该覆盖（单调）: %d", acc.LastOnline)
	}

	// 已落盘：重新打开池仍能看到（轮换记账跨重启保留）
	pool2 := accounts.New(path)
	if err := pool2.Load(); err != nil {
		t.Fatalf("重开池失败: %v", err)
	}
	if acc, _ := pool2.Get("robot0001000@xy3.com"); acc.LastOnline != 424242 {
		t.Fatalf("TouchOnline 应落盘（重载后仍是 424242）: %d", acc.LastOnline)
	}

	// 与 Pick 联动：加一个从没上过线的新号 → 它排最前（刚记过的两个号排后面）
	pool2.Add([]string{"robot0009999@xy3.com"}, "pw9999", zone, "")
	pool2.SetZoneState("robot0009999@xy3.com", zone, accounts.ZoneState{Verified: true, Usable: true})
	if got := pool2.Pick(zone, 1, true, nil); len(got) != 1 || got[0] != "robot0009999@xy3.com" {
		t.Fatalf("刚被 TouchOnline 记过的号应排到后面: %v", got)
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
