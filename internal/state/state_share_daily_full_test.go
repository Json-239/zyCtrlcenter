// 分享日常"满额表"（shareDailyFull）落盘：本文覆盖 2026-09-29 修复 ——
// 满额表原为纯内存，中控重启清表后"已完成并离线"的号在面板丢 ✓（现场 20:25 重启丢
// 9 烽火+1 神捕，用户误判"一天零完成"）。落盘后：重启载入即恢复，跨日惰性失效不变。
//
// 覆盖面：登记/查询/计数/幂等/跨日清条目 + 落盘往返（新建、载入、跨日丢弃、损坏容错）。
package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestShareDailyFullLifecycle(t *testing.T) {
	s := New()
	acc, key := "robot0005162@xy3.com", "share_daily_宫廷10"
	if s.ShareDailyFullToday(acc, key) {
		t.Fatal("未登记不该命中")
	}
	s.MarkShareDailyFull(acc, key)
	if !s.ShareDailyFullToday(acc, key) {
		t.Fatal("登记后应命中")
	}
	if s.ShareDailyFullToday(acc, "share_daily_大唐神捕") || s.ShareDailyFullToday("other@x.com", key) {
		t.Fatal("玩法键/账号隔离失效")
	}
	if n := s.ShareDailyFullTodayCount(); n != 1 {
		t.Fatalf("计数应为 1，实际 %d", n)
	}
	// 空参数安全
	s.MarkShareDailyFull("", key)
	s.MarkShareDailyFull(acc, "")
	if s.ShareDailyFullToday("", key) || s.ShareDailyFullToday(acc, "") {
		t.Fatal("空账号/空玩法键不该命中")
	}
	// 跨日惰性失效 + 再次 Mark 顺手清旧条目
	s.mu.Lock()
	s.shareDailyFull[dailyFullKey(acc, key)] = time.Now().AddDate(0, 0, -1).Format("20060102")
	s.mu.Unlock()
	if s.ShareDailyFullToday(acc, key) {
		t.Fatal("昨天的条目不该命中")
	}
	if n := s.ShareDailyFullTodayCount(); n != 0 {
		t.Fatalf("计数不该含过期条目: %d", n)
	}
	s.MarkShareDailyFull("b@x.com", key)
	s.mu.RLock()
	_, left := s.shareDailyFull[dailyFullKey(acc, key)]
	s.mu.RUnlock()
	if left {
		t.Fatal("再次 Mark 应清掉跨日旧条目")
	}
}

func TestShareDailyFullPersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "share_daily_full.json")
	acc, key := "robot0005162@xy3.com", "share_daily_宫廷10"

	// 首次运行：文件不存在 → (0, nil)；未 Mark 前不创建文件
	s1 := New()
	if n, err := s1.EnableShareDailyFullPersist(path); n != 0 || err != nil {
		t.Fatalf("文件不存在应返回 (0, nil): %d, %v", n, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("未 Mark 不该创建文件: %v", err)
	}

	s1.MarkShareDailyFull(acc, key)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("Mark 后应有落盘文件: %v", err)
	}
	var f shareDailyFullFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("落盘应是合法 JSON: %v", err)
	}
	today := time.Now().Format("20060102")
	if f.Full[dailyFullKey(acc, key)] != today {
		t.Fatalf("落盘应含「账号|玩法键 → 今日」: %v", f.Full)
	}
	if f.UpdatedAt.IsZero() {
		t.Fatal("落盘应带 updated_at")
	}

	// "重启中控"：新 State 载入同一文件 → 命中（✓ 完成行恢复的根据）
	s2 := New()
	if n, err := s2.EnableShareDailyFullPersist(path); n != 1 || err != nil {
		t.Fatalf("载入应命中 1 条: %d, %v", n, err)
	}
	if !s2.ShareDailyFullToday(acc, key) {
		t.Fatal("重启载入后满额应命中（离线完成号 ✓ 行的依据）")
	}

	// 跨日种子：前天条目 → 载入 0 条、不命中
	seed := func(day string) {
		t.Helper()
		b, _ := json.Marshal(shareDailyFullFile{Full: map[string]string{dailyFullKey(acc, key): day}, UpdatedAt: time.Now()})
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	seed(time.Now().AddDate(0, 0, -2).Format("20060102"))
	s3 := New()
	if n, err := s3.EnableShareDailyFullPersist(path); n != 0 || err != nil {
		t.Fatalf("跨日条目应被丢弃: %d, %v", n, err)
	}
	if s3.ShareDailyFullToday(acc, key) {
		t.Fatal("前天的种子不该命中")
	}

	// 今日种子 → 命中
	seed(today)
	s4 := New()
	if n, err := s4.EnableShareDailyFullPersist(path); n != 1 || err != nil {
		t.Fatalf("今日种子应载入: %d, %v", n, err)
	}
	if !s4.ShareDailyFullToday(acc, key) {
		t.Fatal("手工种子应命中")
	}

	// 损坏文件容错：返回错误、不 panic、按空表继续，Mark 能覆盖重建
	if err := os.WriteFile(path, []byte("{not-json"), 0o644); err != nil {
		t.Fatal(err)
	}
	s5 := New()
	if _, err := s5.EnableShareDailyFullPersist(path); err == nil {
		t.Fatal("损坏文件应返回错误（调用方记日志后按未满继续）")
	}
	if s5.ShareDailyFullToday(acc, key) || s5.ShareDailyFullTodayCount() != 0 {
		t.Fatal("损坏文件应等效空表")
	}
	s5.MarkShareDailyFull(acc, key)
	if !s5.ShareDailyFullToday(acc, key) {
		t.Fatal("损坏后 Mark 应正常工作并覆盖重建")
	}
	raw2, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw2, &shareDailyFullFile{}); err != nil {
		t.Fatalf("重建后的文件应合法: %v", err)
	}

	// 未配路径 = 纯内存：Mark 不 panic、不产生文件
	s6 := New()
	s6.MarkShareDailyFull("c@x.com", key)
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Fatal("未配路径不该写临时文件")
	}
}
