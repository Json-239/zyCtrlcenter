// 卡死熔断（restoreCap）表：登记/查询/幂等/跨日惰性失效/手动解除 + 行内当日计数清除
// + Snapshot 面板字段（restore_capped/cap_until/cap_reason）。
//
// 2026-09-23：当日卡死触顶的号要有明确语义（"熔断到次日"），且不依赖 robots 行 ——
// 行会被 Remove 删掉（陈旧清理/下机），独立表才能挡住"删行→计数归零→同日又重登 3 次"。
package state

import (
	"testing"
	"time"
)

func TestRestoreCapLifecycle(t *testing.T) {
	s := New()
	acc := "a@x.com"
	s.Update(acc, func(r *Robot) {
		r.StuckDay, r.StuckCount = time.Now().Format("20060102"), 3
	})

	until := s.MarkRestoreCapped(acc, "钟馗对话卡死")
	if until.IsZero() {
		t.Fatal("MarkRestoreCapped 应返回熔断失效时刻")
	}
	if !s.IsRestoreCapped(acc) {
		t.Fatal("登记后应处于熔断")
	}
	// 幂等：重复登记保留首条记录（原因/时刻不被覆盖）
	s.MarkRestoreCapped(acc, "另一条原因")
	if _, reason, _ := s.RestoreCapInfo(acc); reason != "钟馗对话卡死" {
		t.Fatalf("重复登记应保留首次原因: %q", reason)
	}

	// Snapshot 面板字段
	snap := s.Snapshot()
	if len(snap) != 1 || !snap[0].RestoreCapped || snap[0].CapUntil == "" || snap[0].CapReason == "" {
		t.Fatalf("Snapshot 应带 restore_capped/cap_until/cap_reason: %+v", snap)
	}
	if _, err := time.Parse(time.RFC3339, snap[0].CapUntil); err != nil {
		t.Fatalf("cap_until 应是 RFC3339 时间串: %q (%v)", snap[0].CapUntil, err)
	}

	// 名单
	list := s.RestoreCappedList()
	if len(list) != 1 || list[0]["account"] != acc {
		t.Fatalf("名单应含该号: %+v", list)
	}

	// 手动解除：清表 + 清行内当日计数（不清计数则调度侧仍按旧计数拦）
	if !s.ClearRestoreCapped(acc) {
		t.Fatal("解除应返回 true（原本处于熔断）")
	}
	s.ClearStuckToday(acc)
	if s.IsRestoreCapped(acc) {
		t.Fatal("解除后不该再熔断")
	}
	if r, _ := s.Get(acc); r.StuckCount != 0 || r.StuckDay != "" {
		t.Fatalf("行内当日卡死计数应清零: %+v", r)
	}
	if snap := s.Snapshot(); snap[0].RestoreCapped || snap[0].CapUntil != "" {
		t.Fatalf("解除后 Snapshot 不该再带熔断标记: %+v", snap[0])
	}
	if n := len(s.RestoreCappedList()); n != 0 {
		t.Fatalf("解除后名单应为空: %d", n)
	}
	// ClearStuckToday 幂等（行不存在也不 panic）
	s.ClearStuckToday("nobody@x.com")
}

// 跨日惰性失效：过期记录在查询/名单/Snapshot 里都不算熔断，并顺手清掉（不跑后台线程）。
func TestRestoreCapExpiresLazily(t *testing.T) {
	s := New()
	acc := "b@x.com"
	s.Update(acc, func(r *Robot) {})
	s.MarkRestoreCapped(acc, "触顶")
	// 直接把记录改成"已过期"（同包测试；生产里由时间推进自然发生）
	s.mu.Lock()
	s.restoreCap[acc] = restoreCap{Until: time.Now().Add(-time.Minute), Reason: "触顶"}
	s.mu.Unlock()

	if s.IsRestoreCapped(acc) {
		t.Fatal("过期记录不该算熔断")
	}
	s.mu.RLock()
	_, left := s.restoreCap[acc]
	s.mu.RUnlock()
	if left {
		t.Fatal("查询应顺手清掉过期记录")
	}
	if snap := s.Snapshot(); snap[0].RestoreCapped {
		t.Fatalf("Snapshot 不该带过期熔断: %+v", snap[0])
	}
	if n := len(s.RestoreCappedList()); n != 0 {
		t.Fatalf("名单应为空: %d", n)
	}
}

// 全量解除 + Snapshot 不受行删除影响（独立表）。
func TestClearAllRestoreCappedAndRowRemoval(t *testing.T) {
	s := New()
	s.Update("a@x.com", func(r *Robot) {})
	s.Update("b@x.com", func(r *Robot) {})
	s.MarkRestoreCapped("a@x.com", "x")
	s.MarkRestoreCapped("b@x.com", "y")

	// 删行（陈旧清理/下机）→ 熔断标记仍在（独立表的意义）
	s.Remove("a@x.com")
	if !s.IsRestoreCapped("a@x.com") {
		t.Fatal("行被删后熔断标记应仍在（否则同日又给 3 次重登）")
	}

	got := s.ClearAllRestoreCapped()
	if len(got) != 2 || got[0] != "a@x.com" || got[1] != "b@x.com" {
		t.Fatalf("应返回全部解除账号（升序）: %v", got)
	}
	if s.IsRestoreCapped("a@x.com") || s.IsRestoreCapped("b@x.com") || len(s.RestoreCappedList()) != 0 {
		t.Fatal("全量解除后不该有熔断")
	}
}
