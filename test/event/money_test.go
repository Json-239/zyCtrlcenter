package event_test

import (
	"testing"

	"zyctrlcenter/internal/config"
	"zyctrlcenter/test/testsupport"
)

// 2026-09-23 货币详情（MapView 选号列表「银两 / 储备」列）：
// status_reply 里的 money/deposit/reserve 落到 state.Robot（/api/status 的 robots 行），
// 且缺字段心跳保留旧值、上报 0 覆盖为 0。
func TestStatusMoneyFields(t *testing.T) {
	cfg := config.Default()
	h, st, _, _ := testsupport.NewTestHandler(t, cfg)

	h.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1", "robots": []any{
		map[string]any{"account": "money@t.com", "state": "IDLE", "online": true,
			"level": 60, "money": 1234567, "deposit": 89000, "reserve": 555},
	}})

	r, ok := st.Get("money@t.com")
	if !ok {
		t.Fatal("status_reply 应建立机器人行")
	}
	if r.Money != 1234567 || r.Deposit != 89000 || r.Reserve != 555 {
		t.Fatalf("货币字段应落库，实际 money=%d deposit=%d reserve=%d", r.Money, r.Deposit, r.Reserve)
	}
	// Snapshot（/api/status 数据源）必须带出
	var got *struct{ Money, Deposit, Reserve int64 }
	for _, s := range st.Snapshot() {
		if s.Account == "money@t.com" {
			got = &struct{ Money, Deposit, Reserve int64 }{s.Money, s.Deposit, s.Reserve}
		}
	}
	if got == nil || got.Money != 1234567 || got.Reserve != 555 {
		t.Fatalf("Snapshot 应带出货币字段，实际 %+v", got)
	}

	// 后续心跳不带货币字段 → 保留旧值（不被打回 0）
	h.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1", "robots": []any{
		map[string]any{"account": "money@t.com", "state": "IDLE", "online": true, "level": 60},
	}})
	r2, _ := st.Get("money@t.com")
	if r2.Money != 1234567 || r2.Reserve != 555 {
		t.Fatalf("缺字段心跳不应清零，实际 money=%d reserve=%d", r2.Money, r2.Reserve)
	}

	// 机器人上报 0（真没钱）→ 覆盖为 0（前端显示 '-'）
	h.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1", "robots": []any{
		map[string]any{"account": "money@t.com", "state": "IDLE", "online": true,
			"level": 60, "money": 0, "deposit": 0, "reserve": 0},
	}})
	r3, _ := st.Get("money@t.com")
	if r3.Money != 0 || r3.Deposit != 0 || r3.Reserve != 0 {
		t.Fatalf("上报 0 应覆盖为 0，实际 %d/%d/%d", r3.Money, r3.Deposit, r3.Reserve)
	}
}
