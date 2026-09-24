// 分享日常"今日已派"台账（shareDailyAssigned）：登记/查询/幂等/跨日惰性失效 +
// 可选落盘（EnableShareDailyAssignedPersist：tmp+rename 原子写、载入、跨日丢弃）。
//
// 2026-09-24：机器人端分享日常模块无状态持久化 —— 机器人进程一重启，心跳 daily 块就没了，
// 「启动 = 恢复当前任务」的判据由本台账兜底（见 internal/api/handlers.go:decideKind）。
package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const testShareKey = "share_daily_大唐神捕"

func TestShareDailyAssignedLifecycle(t *testing.T) {
	s := New()
	acc := "a@x.com"
	if s.ShareDailyAssignedToday(acc, testShareKey) {
		t.Fatal("未登记不该命中")
	}
	s.MarkShareDailyAssigned(acc, testShareKey)
	if !s.ShareDailyAssignedToday(acc, testShareKey) {
		t.Fatal("登记后应命中")
	}
	if s.ShareDailyAssignedToday(acc, "share_daily_别的玩法") {
		t.Fatal("不同玩法键不该命中")
	}
	if s.ShareDailyAssignedToday("other@x.com", testShareKey) {
		t.Fatal("别的账号不该命中")
	}
	// 幂等：重复登记不改变结果（同日重复下发覆盖日期串即可）
	s.MarkShareDailyAssigned(acc, testShareKey)
	if !s.ShareDailyAssignedToday(acc, testShareKey) || s.ShareDailyAssignedTodayCount() != 1 {
		t.Fatal("重复登记应幂等")
	}
	// 空参数安全（不 panic 也不落脏记录）
	s.MarkShareDailyAssigned("", testShareKey)
	s.MarkShareDailyAssigned(acc, "")
	if s.ShareDailyAssignedToday("", testShareKey) || s.ShareDailyAssignedToday(acc, "") {
		t.Fatal("空账号/空玩法键不该命中")
	}

	// 跨日惰性失效：把记录改成"昨天"→ 不命中、计数不含（同包直接改内部表，模拟跨日）
	s.mu.Lock()
	s.shareDailyAssigned[dailyAssignKey(acc, testShareKey)] = time.Now().AddDate(0, 0, -1).Format("20060102")
	s.mu.Unlock()
	if s.ShareDailyAssignedToday(acc, testShareKey) {
		t.Fatal("昨天的记录不该命中（跨日自动失效）")
	}
	if n := s.ShareDailyAssignedTodayCount(); n != 0 {
		t.Fatalf("计数不该含过期条目: %d", n)
	}
	// 再次 Mark 时顺手清掉非今日条目（不跑后台线程，表不随天数增长）
	s.MarkShareDailyAssigned("b@x.com", testShareKey)
	s.mu.RLock()
	_, left := s.shareDailyAssigned[dailyAssignKey(acc, testShareKey)]
	s.mu.RUnlock()
	if left {
		t.Fatal("再次 Mark 应清掉跨日旧条目")
	}
}

func TestShareDailyAssignedPersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "share_daily_assign.json")
	acc, key := "robot0005275@xy3.com", testShareKey

	// 首次运行：文件不存在 → (0, nil)，且首次 Mark 前不创建文件
	s1 := New()
	if n, err := s1.EnableShareDailyAssignedPersist(path); n != 0 || err != nil {
		t.Fatalf("文件不存在应返回 (0, nil): %d, %v", n, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("未 Mark 不该创建文件: %v", err)
	}

	s1.MarkShareDailyAssigned(acc, key)
	// 落盘结构可解析（现场手工种子按这个结构写）
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("Mark 后应有落盘文件: %v", err)
	}
	var f shareDailyAssignedFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("落盘应是合法 JSON: %v", err)
	}
	today := time.Now().Format("20060102")
	if f.Assigned[dailyAssignKey(acc, key)] != today {
		t.Fatalf("落盘应含「账号|玩法键 → 今日」: %v", f.Assigned)
	}
	if f.UpdatedAt.IsZero() {
		t.Fatal("落盘应带 updated_at")
	}

	// "重启中控"：新 State 载入同一文件 → 命中
	s2 := New()
	n, err := s2.EnableShareDailyAssignedPersist(path)
	if err != nil || n != 1 {
		t.Fatalf("载入应命中 1 条: %d, %v", n, err)
	}
	if !s2.ShareDailyAssignedToday(acc, key) {
		t.Fatal("重启载入后台账应命中（启动续跑的依据）")
	}

	// 手工种子：文件里是"昨天"→ 载入 0 条、不命中（改日期→不命中）
	seed := func(day string) {
		t.Helper()
		b, _ := json.Marshal(shareDailyAssignedFile{
			Assigned: map[string]string{dailyAssignKey(acc, key): day}, UpdatedAt: time.Now(),
		})
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	seed(time.Now().AddDate(0, 0, -1).Format("20060102"))
	s3 := New()
	if n, err := s3.EnableShareDailyAssignedPersist(path); n != 0 || err != nil {
		t.Fatalf("跨日条目应被丢弃（不载入）: %d, %v", n, err)
	}
	if s3.ShareDailyAssignedToday(acc, key) {
		t.Fatal("昨天的种子不该命中")
	}

	// 手工种子今天的记录 → 命中（现场手工种子同款结构）
	seed(today)
	s4 := New()
	if n, err := s4.EnableShareDailyAssignedPersist(path); n != 1 || err != nil {
		t.Fatalf("今日种子应载入: %d, %v", n, err)
	}
	if !s4.ShareDailyAssignedToday(acc, key) {
		t.Fatal("手工种子应命中")
	}

	// 未配路径 = 纯内存：Mark 不 panic、不产生临时文件
	s5 := New()
	s5.MarkShareDailyAssigned("c@x.com", key)
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Fatal("未配路径不该写临时文件")
	}
}
