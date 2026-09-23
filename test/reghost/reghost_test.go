// reghost 状态机测试（假时钟 + 假动作，不 sleep）：下线 → 重登 → 延迟补发 → 完成；
// 失败到上限 → 交人工；用户取消 → 立即清掉（"停了就不许自动拉起"）。
package reghost_test

import (
	"strings"
	"testing"
	"time"

	"zyctrlcenter/internal/services/reghost"
)

type fake struct {
	now      time.Time
	online   bool
	removed  int
	added    int
	launched int
	removeOK bool
	addOK    bool
	launchOK bool
	stuck    int // 当日卡死次数（churn 防护用）
	// 2026-09-23 熔断（capped）：
	capped   bool      // Capped() 查询结果（独立熔断表是否有效）
	capCalls int       // Cap() 登记次数
	capUntil time.Time // Cap() 返回的失效时刻（零值 → 用本地推算的次日 0 点）
}

func (f *fake) deps() reghost.Deps {
	return reghost.Deps{
		Now:            func() time.Time { return f.now },
		Online:         func(string) bool { return f.online },
		Remove:         func(string) bool { f.removed++; return f.removeOK },
		Add:            func(string) bool { f.added++; f.online = true; return f.addOK },
		Launch:         func(string) bool { f.launched++; return f.launchOK },
		Stuck:          func(string) int { return f.stuck },
		Cap:            func(string, string) time.Time { f.capCalls++; f.capped = true; return f.capUntil },
		Capped:         func(string) bool { return f.capped },
		LaunchDelaySec: 8,
		OfflineWaitSec: 60,
		OnlineWaitSec:  90,
		MaxAttempts:    3,
		CooldownSec:    300,
	}
}

func newFake() *fake {
	return &fake{now: time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC), removeOK: true, addOK: true, launchOK: true}
}

// 正常路径：下线 → （离线）→ 重登 → 上线 → 8 秒后补发 → 完成。
func TestHappyPathOfflineReloginLaunch(t *testing.T) {
	f := newFake()
	f.online = true // 卡死时是登录着的
	r := reghost.New(f.deps())

	r.Request("acc@x.com", "交付捉鬼任务连续 3 次无进展(点钟馗无交付对话)")
	if f.removed != 1 {
		t.Fatalf("登记后应立即下线: removed=%d", f.removed)
	}
	st := r.Status()
	if len(st) != 1 || st[0].Phase != reghost.PhaseWaitingOffline {
		t.Fatalf("阶段应为等离线: %+v", st)
	}

	f.online = false // 机器人把它摘掉了
	f.now = f.now.Add(5 * time.Second)
	r.Tick(f.now)
	st = r.Status()
	if st[0].Phase != reghost.PhaseWaitingOnline || f.added != 1 {
		t.Fatalf("离线后应重新上线: %+v added=%d", st, f.added)
	}
	f.online = true // 登录成功

	r.Tick(f.now.Add(2 * time.Second))
	st = r.Status()
	if st[0].Phase != reghost.PhaseWaitingLaunch {
		t.Fatalf("上线后应进入「等补发」: %+v", st)
	}
	if f.launched != 0 {
		t.Fatal("8 秒延迟没到不该补发")
	}
	r.Tick(f.now.Add(11 * time.Second))
	st = r.Status()
	if st[0].Phase != reghost.PhaseDone || f.launched != 1 {
		t.Fatalf("到点应补发并完成: %+v launched=%d", st, f.launched)
	}
}

// 重复登记要按冷却去重（error 与 ghost_offline 可能各报一次）。
func TestDuplicateRequestDeduped(t *testing.T) {
	f := newFake()
	r := reghost.New(f.deps())
	r.Request("acc@x.com", "第一次")
	r.Request("acc@x.com", "第二次")
	if len(r.Status()) != 1 || f.removed != 1 {
		t.Fatalf("冷却期内重复登记应去重: %+v removed=%d", r.Status(), f.removed)
	}
}

// 取消后不再有任何动作（用户手动停止/下机）。
func TestCancelStopsRecovery(t *testing.T) {
	f := newFake()
	f.online = true
	r := reghost.New(f.deps())
	r.Request("acc@x.com", "卡死")
	if !r.Cancel("acc@x.com") {
		t.Fatal("取消应成功")
	}
	f.online = false
	r.Tick(f.now.Add(time.Minute))
	if f.added != 0 || f.launched != 0 {
		t.Fatalf("取消后不该再上线/补发: added=%d launched=%d", f.added, f.launched)
	}
	if len(r.Status()) != 0 {
		t.Fatalf("取消后状态应清空: %+v", r.Status())
	}
}

// 连续失败到上限 → failed（交人工），不再无限重登。
func TestFailureStopsAfterMaxAttempts(t *testing.T) {
	f := newFake()
	f.addOK = false // 上线总是失败
	r := reghost.New(f.deps())
	r.Request("acc@x.com", "卡死")
	var st []reghost.Status
	for i := 0; i < 5; i++ {
		f.now = f.now.Add(6 * time.Minute) // 越过冷却
		r.Tick(f.now)
		st = r.Status()
		if len(st) == 1 && st[0].Phase == reghost.PhaseFailed {
			break
		}
	}
	if len(st) != 1 || st[0].Phase != reghost.PhaseFailed {
		t.Fatalf("连续失败到上限应交人工: %+v", st)
	}
	if st[0].Attempts < 3 {
		t.Fatalf("应记录尝试次数: %+v", st[0])
	}
	// failed 之后保留一段时间，但不再有任何新动作
	before := f.added
	f.now = f.now.Add(time.Minute)
	r.Tick(f.now)
	if f.added != before {
		t.Fatalf("交人工后不该继续重登: added=%d → %d", before, f.added)
	}
}

// 2026-09-21 churn 防护 / 2026-09-23 升级为显式熔断：当日卡死 ≥3 次 → 不再自动重登，
// 状态记 capped（明确"熔断到次日"+原因+失效时刻，面板可读），并登记独立熔断表；
// 差一次（2 次）仍正常重登。
func TestChurnGuardSkipsReloginAfterLimit(t *testing.T) {
	f := newFake()
	f.online = true
	f.stuck = reghost.ChurnLimit
	r := reghost.New(f.deps())

	r.Request("acc@x.com", "钟馗菜单无可动作项(仅说明/离开)")
	if f.removed != 0 || f.added != 0 || f.launched != 0 {
		t.Fatalf("触顶后不该有任何动作（不再空转）: removed=%d added=%d launched=%d",
			f.removed, f.added, f.launched)
	}
	if f.capCalls != 1 {
		t.Fatalf("触顶应登记熔断表（Cap 回调）: capCalls=%d", f.capCalls)
	}
	st := r.Status()
	if len(st) != 1 || st[0].Phase != reghost.PhaseCapped {
		t.Fatalf("应记 capped（面板能看到为什么没救它/什么时候恢复）: %+v", st)
	}
	if !strings.Contains(st[0].LastMsg, "当日卡死") || !strings.Contains(st[0].LastMsg, "熔断") {
		t.Fatalf("说明文案应含「当日卡死」「熔断」: %+v", st[0])
	}
	wantUntil := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC) // fake now=2026-09-21 12:00 UTC → 次日 0 点
	if !st[0].CapUntil.Equal(wantUntil) {
		t.Fatalf("熔断应到次日 0 点: got=%v want=%v", st[0].CapUntil, wantUntil)
	}

	// 机器人连点重复上报：幂等（不再重复登记/不产生动作）
	r.Request("acc@x.com", "又卡了")
	if f.capCalls != 1 || f.removed != 0 || f.added != 0 || f.launched != 0 {
		t.Fatalf("重复上报应幂等: capCalls=%d removed=%d added=%d launched=%d",
			f.capCalls, f.removed, f.added, f.launched)
	}

	// 未达上限（2 次）→ 仍要正常重登
	f2 := newFake()
	f2.online = true
	f2.stuck = reghost.ChurnLimit - 1
	r2 := reghost.New(f2.deps())
	r2.Request("acc@x.com", "钟馗对话 7 轮超 30 秒未关闭")
	if f2.removed != 1 {
		t.Fatalf("未达上限应正常下线重登: removed=%d", f2.removed)
	}
	if f2.capCalls != 0 {
		t.Fatalf("未达上限不该登记熔断: capCalls=%d", f2.capCalls)
	}
}

// 已熔断的号再报卡死（入口闸）：即便当日计数因行删除/中控重启归零，只要熔断表还在就不重登；
// 熔断期间 Tick 也不产生任何动作（不空转）。
func TestCappedEntryGuardSkipsRelogin(t *testing.T) {
	f := newFake()
	f.online = true
	f.stuck = 0     // 计数丢了（robot 行被删/重启后）
	f.capped = true // 但独立熔断表还在
	r := reghost.New(f.deps())

	r.Request("acc@x.com", "换图推送迟迟未到")
	if f.removed != 0 || f.added != 0 || f.launched != 0 {
		t.Fatalf("已熔断不该重登: removed=%d added=%d launched=%d", f.removed, f.added, f.launched)
	}
	st := r.Status()
	if len(st) != 1 || st[0].Phase != reghost.PhaseCapped {
		t.Fatalf("应显示 capped: %+v", st)
	}
	if f.capCalls != 0 {
		t.Fatalf("已熔断不该重复登记熔断表（幂等）: capCalls=%d", f.capCalls)
	}

	// 熔断期间（相位 capped）Tick 推进：没有任何动作；跨日前条目保留
	f.online = false
	r.Tick(f.now.Add(time.Hour))
	if f.added != 0 || f.removed != 0 || f.launched != 0 {
		t.Fatalf("熔断期间不该有任何动作: removed=%d added=%d launched=%d",
			f.removed, f.added, f.launched)
	}
	if len(r.Status()) != 1 {
		t.Fatalf("未到期前应保留 capped 状态（面板可见）: %+v", r.Status())
	}
}

// 熔断到期（跨日 0 点）→ 条目自动清除；新的一天计数归零 → 卡死事件恢复正常重登。
func TestCappedExpiresNextDay(t *testing.T) {
	f := newFake()
	f.online = true
	f.stuck = reghost.ChurnLimit
	r := reghost.New(f.deps())
	r.Request("acc@x.com", "触顶")
	if st := r.Status(); len(st) != 1 || st[0].Phase != reghost.PhaseCapped {
		t.Fatalf("应先进入 capped: %+v", st)
	}

	// 跨日：Tick 到次日 0 点后 → 条目自动清除（无需人工）
	r.Tick(time.Date(2026, 9, 22, 0, 0, 1, 0, time.UTC))
	if len(r.Status()) != 0 {
		t.Fatalf("跨日应自动清除熔断条目: %+v", r.Status())
	}

	// 新的一天：计数归零、熔断表失效 → 卡死事件正常走"下线→重登"
	f.now = time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	f.stuck = 0
	f.capped = false
	r.Request("acc@x.com", "新的一天又卡")
	st := r.Status()
	if len(st) != 1 || st[0].Phase != reghost.PhaseWaitingOffline || f.removed != 1 {
		t.Fatalf("跨日后应恢复正常重登: %+v removed=%d", st, f.removed)
	}
}

// Cap 回调返回壳层给的失效时刻（独立熔断表口径）时，状态里用它，而不是本地推算值。
func TestCappedUsesShellUntil(t *testing.T) {
	f := newFake()
	f.stuck = reghost.ChurnLimit
	custom := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	f.capUntil = custom
	r := reghost.New(f.deps())
	r.Request("acc@x.com", "触顶")
	st := r.Status()
	if len(st) != 1 || !st[0].CapUntil.Equal(custom) {
		t.Fatalf("应以壳层给的失效时刻为准: %+v", st)
	}
}
