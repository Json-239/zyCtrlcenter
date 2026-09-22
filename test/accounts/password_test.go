// 密码来源：区级 > 账号级；**没有统一/默认密码**（查不到就是查不到）。
package accounts_test

import (
	"testing"

	"zyctrlcenter/internal/services/accounts"
	"zyctrlcenter/test/testsupport"
)

const otherZone = "10.0.0.9:2300"

func TestPasswordForPrefersZoneThenAccount(t *testing.T) {
	pool, path := testsupport.NewTestAccountsWithPath(t)
	acc := "robot0001000@xy3.com" // 夹具里账号级 password=123456

	// 区级没有 → 回退账号级
	if pw, ok := pool.PasswordFor(acc, testZone); !ok || pw != "123456" {
		t.Fatalf("应回退账号级密码，实际 ok=%v pw=%q", ok, pw)
	}
	// 写入区级密码（建号生成的随机密码走这里）→ 区级优先
	pool.SetZoneStateNoSave(acc, testZone, accounts.ZoneState{Verified: false, Password: "Zx9Qm2Lp7Kd3"})
	_ = pool.Save()
	if pw, ok := pool.PasswordFor(acc, testZone); !ok || pw != "Zx9Qm2Lp7Kd3" {
		t.Fatalf("区级密码应优先，实际 ok=%v pw=%q", ok, pw)
	}
	// 别的区没有区级密码 → 仍回退账号级
	if pw, ok := pool.PasswordFor(acc, otherZone); !ok || pw != "123456" {
		t.Fatalf("其它区应回退账号级，实际 ok=%v pw=%q", ok, pw)
	}
	// 池里没有的账号 / 没有密码 → ok=false（**不能编一个默认密码出来**）
	if _, ok := pool.PasswordFor("nobody@xy3.com", testZone); ok {
		t.Fatal("未知账号不该有密码")
	}

	// 区级密码要能落盘并读回（登录时就靠它）
	reloaded := accounts.New(path)
	if err := reloaded.Load(); err != nil {
		t.Fatal(err)
	}
	if pw, ok := reloaded.PasswordFor(acc, testZone); !ok || pw != "Zx9Qm2Lp7Kd3" {
		t.Fatalf("区级密码应落盘，实际 ok=%v pw=%q", ok, pw)
	}
}

// 同一个"服"（同 host 的不同区）密码通用：在 2300 上存的密码，2400 也能用；
// 别的服（不同 host）不通用 → 回退账号级。
func TestPasswordSharedWithinSameServer(t *testing.T) {
	pool, _ := testsupport.NewTestAccountsWithPath(t)
	acc := "robot0001000@xy3.com"

	pool.SetZoneState(acc, "47.96.8.240:2300", accounts.ZoneState{Password: "Zx9Qm2Lp7Kd3"})

	// 同服另一个区
	if pw, ok := pool.PasswordFor(acc, "47.96.8.240:2400"); !ok || pw != "Zx9Qm2Lp7Kd3" {
		t.Fatalf("同服的区应共用密码，实际 ok=%v pw=%q", ok, pw)
	}
	// 另一个服 → 不通用（回退账号级 123456）
	if pw, ok := pool.PasswordFor(acc, otherZone); !ok || pw != "123456" {
		t.Fatalf("别的服不应共用该密码，实际 ok=%v pw=%q", ok, pw)
	}
	// 精确区的密码优先于同服的其它区
	pool.SetZoneState(acc, "47.96.8.240:2400", accounts.ZoneState{Password: "AnOtherPwd9x"})
	if pw, _ := pool.PasswordFor(acc, "47.96.8.240:2400"); pw != "AnOtherPwd9x" {
		t.Fatalf("精确区密码应优先，实际 %q", pw)
	}
}

// 批量写回是"先写内存、最后统一落盘"：期间若有别的调用触发 refresh（热重载），
// 不能把还没落盘的内存改动冲掉（否则批量建号/验证的密码会丢）。
func TestNoSaveBatchSurvivesHotReload(t *testing.T) {
	pool, path := testsupport.NewTestAccountsWithPath(t)
	name := "robot0009500@xy3.com"
	zone := "47.96.8.240:2300"

	pool.Add([]string{name}, "", zone, "建号") // Add 会立刻落盘（文件变了）
	pool.SetZoneStateNoSave(name, zone, accounts.ZoneState{Password: "KeepMe7xQ2"})

	// 触发一次 refresh（真实场景里是另一个 goroutine 拿锁时调用的）
	if n := pool.Count(); n == 0 {
		t.Fatal("池不该为空")
	}
	if pw, ok := pool.PasswordFor(name, zone); !ok || pw != "KeepMe7xQ2" {
		t.Fatalf("未落盘的内存改动被热重载冲掉了: ok=%v pw=%q", ok, pw)
	}

	if err := pool.Save(); err != nil {
		t.Fatal(err)
	}
	reload := accounts.New(path)
	if err := reload.Load(); err != nil {
		t.Fatal(err)
	}
	if pw, ok := reload.PasswordFor(name, zone); !ok || pw != "KeepMe7xQ2" {
		t.Fatalf("落盘后应读回: ok=%v pw=%q", ok, pw)
	}
}

func TestPasswordForEmptyAccount(t *testing.T) {
	pool, _ := testsupport.NewTestAccountsWithPath(t)
	if _, ok := pool.PasswordFor("", testZone); ok {
		t.Fatal("空账号不该有密码")
	}
}

// 建号成功后：密码写进库、且"该区还没验证"（等下一次验证或直接可用）。
func TestCreateResultWrittenBackKeepsPasswordAndUnverified(t *testing.T) {
	pool, _ := testsupport.NewTestAccountsWithPath(t)
	name := "robot0009001@xy3.com"
	pw := "Ab3kQ9Zx7Lm2Np5R"

	// 建号流程：Add（入库 + 该区建空状态）→ 成功后 SetZoneState 写区级密码
	pool.Add([]string{name}, "", testZone, "建号")
	pool.SetZoneState(name, testZone, accounts.ZoneState{Password: pw})

	got, ok := pool.PasswordFor(name, testZone)
	if !ok || got != pw {
		t.Fatalf("建号后应能从库里取到随机密码，实际 ok=%v pw=%q", ok, got)
	}
	acc, _ := pool.Get(name)
	if z := acc.Zone(testZone); z == nil || z.Verified {
		t.Fatalf("新建的号在该区应为未验证（还没登录过）: %+v", z)
	}
}
