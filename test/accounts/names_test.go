// 按"编号"生成账号名：前缀 + 序号(补零) + 邮箱后缀；支持从池内最大序号自动接续。
package accounts_test

import (
	"strings"
	"testing"

	"zyctrlcenter/internal/services/accounts"
	"zyctrlcenter/test/testsupport"
)

func TestBuildNamesWithSuffixAndAutoWidth(t *testing.T) {
	pool, _ := testsupport.NewTestAccountsWithPath(t)
	names, err := pool.BuildNames(accounts.NameSpec{
		Prefix: "robot000", Start: 3004, Count: 3, Suffix: "@xy3.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"robot0003004@xy3.com", "robot0003005@xy3.com", "robot0003006@xy3.com"}
	if len(names) != 3 {
		t.Fatalf("应生成 3 个: %v", names)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("第 %d 个=%q 期望 %q", i, names[i], want[i])
		}
	}
}

func TestBuildNamesKeepsLeadingZerosInPrefix(t *testing.T) {
	pool, _ := testsupport.NewTestAccountsWithPath(t)
	// 前缀自带补零（参考实现里默认 robot000）→ 序号原样拼接
	names, err := pool.BuildNames(accounts.NameSpec{Prefix: "robot000", Start: 1000, Count: 2, Suffix: "@xy3.com"})
	if err != nil {
		t.Fatal(err)
	}
	if names[0] != "robot0001000@xy3.com" {
		t.Fatalf("前导零必须保留: %v", names)
	}
	// 显式指定位数：前缀 robot + 7 位 → robot0001000
	padded, err := pool.BuildNames(accounts.NameSpec{Prefix: "robot", Start: 1000, Count: 1, Pad: 7, Suffix: "@xy3.com"})
	if err != nil {
		t.Fatal(err)
	}
	if padded[0] != "robot0001000@xy3.com" {
		t.Fatalf("显式补零宽度不符: %v", padded)
	}
}

// 起始序号留空/自动：取池内该前缀的最大序号 +1（避免撞已有账号）
func TestBuildNamesAutoStartFromPool(t *testing.T) {
	pool, _ := testsupport.NewTestAccountsWithPath(t) // 夹具里最大是 robot0001002
	names, err := pool.BuildNames(accounts.NameSpec{
		Prefix: "robot000", Count: 2, Suffix: "@xy3.com", AutoStart: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if names[0] != "robot0001003@xy3.com" || names[1] != "robot0001004@xy3.com" {
		t.Fatalf("自动接续应从 1003 开始: %v", names)
	}
	// 池里没有该前缀时，退回 Start（默认 1）
	none, err := pool.BuildNames(accounts.NameSpec{Prefix: "newbot", Count: 1, AutoStart: true})
	if err != nil {
		t.Fatal(err)
	}
	if none[0] != "newbot1" {
		t.Fatalf("无历史时应从 1 开始: %v", none)
	}
}

func TestBuildNamesValidation(t *testing.T) {
	pool, _ := testsupport.NewTestAccountsWithPath(t)
	cases := []struct {
		name string
		spec accounts.NameSpec
		want string // 期望错误里包含的关键字（空=不该报错）
	}{
		{"前缀为空", accounts.NameSpec{Count: 1}, "前缀"},
		{"数量为 0", accounts.NameSpec{Prefix: "robot000"}, "数量"},
		{"数量超上限", accounts.NameSpec{Prefix: "robot000", Count: accounts.MaxCreateNames + 1}, "上限"},
		{"序号为负", accounts.NameSpec{Prefix: "robot000", Start: -1, Count: 1}, "序号"},
		{"正常", accounts.NameSpec{Prefix: "robot000", Start: 1, Count: 1}, ""},
	}
	for _, c := range cases {
		_, err := pool.BuildNames(c.spec)
		if c.want == "" {
			if err != nil {
				t.Fatalf("%s: 不该报错: %v", c.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: 错误 %v 应含 %q", c.name, err, c.want)
		}
	}
}

// 生成的账号不重复，且与已有账号不冲突（AutoStart 的意义）
func TestBuildNamesUniqueAndNoClash(t *testing.T) {
	pool, _ := testsupport.NewTestAccountsWithPath(t)
	names, err := pool.BuildNames(accounts.NameSpec{Prefix: "robot000", Count: 20, Suffix: "@xy3.com", AutoStart: true})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			t.Fatalf("生成重复账号: %s", n)
		}
		seen[n] = true
		if _, ok := pool.Get(n); ok {
			t.Fatalf("生成的名字撞上池里已有账号: %s", n)
		}
	}
}
