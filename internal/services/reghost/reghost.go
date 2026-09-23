// Package reghost "卡死自动重登恢复"：机器人遇到**钟馗对话卡死**这类错误时，会自己停止抓鬼
// 并在事件里说明"等中控重登后重新下发"（见机器人端 daily_ghost.py:441-449）——本包就是中控
// 侧那条链路：**下线 → 重登 → 延迟补发任务**。
//
// 口径对齐参考实现 robot/ctrlcenter/services/event.py:977 `_reghost_later`：
//   - 重登上线后**延迟 8 秒**再补发（等角色数据/地图就绪；太早发会被当"未登录"丢掉）；
//   - 用户手动停止抓鬼/下机时清掉待恢复（routers/ghost.py 的 `_PENDING_REGHOST.pop`），
//     否则会出现"我明明停了，它又被自动拉起来"。
//
// 状态机（每个号一条，Tick 驱动、可用假时钟测）：
//
//	waiting_offline（已下线，等机器人把它摘掉）
//	  → waiting_online（已重新 add，等它登录/上报）
//	  → waiting_launch（上线了，等到 NextAt）
//	  → done / failed（连续失败到上限 → 交人工）
//	另有 capped（2026-09-23）：当日卡死 ≥ ChurnLimit → **熔断到次日**（不再自动重登，
//	  面板显示明确原因与恢复时刻；到期/中控重启/手动解除后自动清零）。
package reghost

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// 默认参数。
const (
	DefaultLaunchDelaySec = 8   // 重登上线后多久补发（参考实现 8s）
	DefaultOfflineWaitSec = 60  // 等"摘掉"的最长时间（超时也继续）
	DefaultOnlineWaitSec  = 90  // 等"上线"的最长时间
	DefaultMaxAttempts    = 3   // 连续恢复失败上限
	DefaultCooldownSec    = 300 // 同一号两次恢复之间的最小间隔（防风暴）
	DefaultInterval       = 5 * time.Second
	// ChurnLimit 当日卡死 ≥ 这个数 → 不再自动重登（等人工/跨日清零）。
	// 2026-09-21 加：服务端对话态卡死的号"卡→重登→补发→又卡"会无限循环
	// （生产实测单号一天循环 4+ 轮），与 autotaskCandidates 的 churn 判据同口径。
	ChurnLimit = 3
)

// Phase 恢复阶段。
type Phase string

const (
	PhaseWaitingOffline Phase = "waiting_offline"
	PhaseWaitingOnline  Phase = "waiting_online"
	PhaseWaitingLaunch  Phase = "waiting_launch"
	// PhaseCapped 当日卡死触顶 → 熔断到次日（2026-09-23）：
	// 明确的中性语义 —— 不是"恢复失败"（failed），而是"今天不再救、等跨日"。
	PhaseCapped Phase = "capped"
	PhaseDone   Phase = "done"
	PhaseFailed Phase = "failed"
)

// Status 一个号的恢复状态（面板显示）。
type Status struct {
	Account   string    `json:"account"`
	Phase     Phase     `json:"phase"`
	Attempts  int       `json:"attempts"`
	LastMsg   string    `json:"last_msg"`
	Reason    string    `json:"reason,omitempty"` // 触发原因（机器人给的原文）
	Since     time.Time `json:"since"`
	NextAt    time.Time `json:"next_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
	// CapUntil 熔断失效时刻（Phase=capped 时有意义）：到点自动清除，跨日后可再重登。
	CapUntil time.Time `json:"cap_until,omitempty"`
}

// Deps 依赖（壳层提供真实动作；测试注入假的）。
type Deps struct {
	Now    func() time.Time
	Online func(account string) bool // 该号现在是否在线
	Remove func(account string) bool // 下线（robot_manage remove）
	Add    func(account string) bool // 上线（robot_manage add，密码由壳层从池里取）
	Launch func(account string) bool // 补发任务（壳层按意图：抓鬼带导航载荷）
	Log    func(format string, args ...any)
	// Stuck 该号"当日卡死次数"（churn 防护：≥ ChurnLimit 不再自动重登）。
	// nil = 不检查（老行为）。
	Stuck func(account string) int
	// Cap 触顶登记（2026-09-23）：壳层写"独立熔断表"（跨 robot 行删除存续、跨日惰性失效），
	// 返回熔断失效时刻。nil = 不登记（老行为：只在内存里记一条 capped 状态）。
	Cap func(account, reason string) time.Time
	// Capped 该号是否**已处于熔断**（入口闸）：即便 Stuck 计数因行被删/重启丢失，
	// 只要熔断表还在就不重登。nil = 不检查。
	Capped func(account string) bool

	LaunchDelaySec int
	OfflineWaitSec int
	OnlineWaitSec  int
	MaxAttempts    int
	CooldownSec    int
	Interval       time.Duration
}

type entry struct {
	st      Status
	doneAt  time.Time // done/failed 后保留一小段时间供面板看
	lastTry time.Time
}

// Runner 恢复执行器（并发安全）。
type Runner struct {
	d  Deps
	mu sync.Mutex
	m  map[string]*entry
}

// New 创建执行器。
func New(d Deps) *Runner {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Log == nil {
		d.Log = func(string, ...any) {}
	}
	if d.LaunchDelaySec <= 0 {
		d.LaunchDelaySec = DefaultLaunchDelaySec
	}
	if d.OfflineWaitSec <= 0 {
		d.OfflineWaitSec = DefaultOfflineWaitSec
	}
	if d.OnlineWaitSec <= 0 {
		d.OnlineWaitSec = DefaultOnlineWaitSec
	}
	if d.MaxAttempts <= 0 {
		d.MaxAttempts = DefaultMaxAttempts
	}
	if d.CooldownSec <= 0 {
		d.CooldownSec = DefaultCooldownSec
	}
	if d.Interval <= 0 {
		d.Interval = DefaultInterval
	}
	return &Runner{d: d, m: map[string]*entry{}}
}

// Request 收到卡死事件时登记（重复触发按冷却去重）。
func (r *Runner) Request(account, reason string) {
	account = trim(account)
	if account == "" {
		return
	}
	now := r.d.Now()
	// 2026-09-23：已熔断（独立表，跨 robot 行删除存续）→ 幂等记一条 capped 状态并静默返回。
	// 机器人连点上报时不会刷日志 —— 首次触顶的日志在下面触顶分支打。
	if r.d.Capped != nil && r.d.Capped(account) {
		r.markCapped(account, reason, now, false, true)
		return
	}
	// 2026-09-21 churn 防护：当日卡死 ≥ ChurnLimit 次（反复卡又反复重登）→ 不再自动重登。
	// 2026-09-23：触顶语义升级为**显式熔断**（capped）—— 状态与恢复时刻对面板可见，
	// 并写入壳层的独立熔断表（restorer/autotask 据此停止补发与候选，跨行删除仍有效）。
	// 注意：判定放在**冷却去重之前** —— 第 3 次卡死时即便上一轮重登还在途，也立即熔断
	// （否则请求被冷却吞掉 → 计数虚挂 3 却没有任何熔断记录，调度侧静默拦死无法解释）。
	if r.d.Stuck != nil && r.d.Stuck(account) >= ChurnLimit {
		r.markCapped(account, reason, now, true, false)
		return
	}
	r.mu.Lock()
	if e, ok := r.m[account]; ok {
		// 冷却期内（或已经在跑）不重复登记 —— 机器人会连点 15 次才报一次错，
		// 但多个入口（error / ghost_offline）可能各报一次。
		if e.st.Phase != PhaseDone && e.st.Phase != PhaseFailed && e.st.Phase != PhaseCapped &&
			now.Sub(e.st.Since) < time.Duration(r.d.CooldownSec)*time.Second {
			r.mu.Unlock()
			return
		}
	}
	r.m[account] = &entry{st: Status{
		Account: account, Phase: PhaseWaitingOffline, Reason: reason,
		Since: now, UpdatedAt: now, LastMsg: "收到卡死事件，准备重登恢复",
	}}
	r.mu.Unlock()

	r.d.Log("[REGHOST] %s 需要重登恢复：%s", account, reason)
	if r.d.Remove != nil && r.d.Remove(account) {
		r.mu.Lock()
		if e, ok := r.m[account]; ok {
			e.st.Phase = PhaseWaitingOffline
			e.st.LastMsg = "已下发下线，等机器人摘掉该号"
			e.st.UpdatedAt = r.d.Now()
			e.st.NextAt = r.d.Now().Add(time.Duration(r.d.OfflineWaitSec) * time.Second)
		}
		r.mu.Unlock()
	}
}

// markCapped 记"当日卡死触顶 → 熔断到次日"：明确状态（面板可见原因与恢复时刻）+ 登记
// 独立熔断表（壳层提供 Cap；nil 时用本地推算的次日 0 点兜底）。
//
// register=false：该号已在熔断表里（Capped 命中）→ 只补记内存状态，不重复登记表；
// quiet=true：重复上报的去重写（不刷日志）。首次触顶（phase 从非 capped 变 capped）才打日志。
// 熔断期间**不执行任何动作**（不 Remove/Add/Launch）—— 这正是"不再空转"的落点。
func (r *Runner) markCapped(account, reason string, now time.Time, register, quiet bool) {
	until := nextDay0(now)
	if register && r.d.Cap != nil {
		if u := r.d.Cap(account, reason); !u.IsZero() {
			until = u
		}
	}
	r.mu.Lock()
	prev, existed := r.m[account]
	if !register && existed && prev.st.Phase == PhaseCapped && !prev.st.CapUntil.IsZero() {
		until = prev.st.CapUntil // 已有条目：保留原失效时刻（熔断表里的权威值）
	}
	first := !existed || prev.st.Phase != PhaseCapped
	r.m[account] = &entry{st: Status{
		Account: account, Phase: PhaseCapped, Reason: reason,
		Since: now, UpdatedAt: now, CapUntil: until,
		LastMsg: fmt.Sprintf("当日卡死已达上限 %d 次：熔断到 %s 自动恢复（面板可手动解除，期间不自动重登/补发）",
			ChurnLimit, until.Format("01-02 15:04")),
	}}
	r.mu.Unlock()
	if first && !quiet {
		r.d.Log("[REGHOST] %s 跳过重登：当日卡死已达上限 %d 次（%s）；已熔断到 %s，期间不再自动重登/补发",
			account, ChurnLimit, reason, until.Format("2006-01-02 15:04"))
	}
}

// nextDay0 次日本地 0 点（"当日"标记的统一失效时刻；服务端按 0 点切日）。
func nextDay0(now time.Time) time.Time {
	return time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())
}

// Cancel 手动停止/下机时清除（对齐参考实现：停了就不许自动拉起）。
func (r *Runner) Cancel(account string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.m[account]; !ok {
		return false
	}
	delete(r.m, account)
	return true
}

// Status 快照（按账号排序；done/failed 保留 10 分钟便于面板确认）。
func (r *Runner) Status() []Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Status, 0, len(r.m))
	for _, e := range r.m {
		out = append(out, e.st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Account < out[j].Account })
	return out
}

// Tick 推进一步（纯决策 + 调用注入的动作；返回本轮产生的新状态）。
func (r *Runner) Tick(now time.Time) []Status {
	r.mu.Lock()
	accs := make([]string, 0, len(r.m))
	for a := range r.m {
		accs = append(accs, a)
	}
	sort.Strings(accs)
	r.mu.Unlock()

	out := []Status{}
	for _, acc := range accs {
		if st, changed := r.step(acc, now); changed {
			out = append(out, st)
		}
	}
	return out
}

func (r *Runner) step(account string, now time.Time) (Status, bool) {
	r.mu.Lock()
	e, ok := r.m[account]
	if !ok {
		r.mu.Unlock()
		return Status{}, false
	}
	phase := e.st.Phase
	nextAt := e.st.NextAt
	attempts := e.st.Attempts
	since := e.st.Since
	capUntil := e.st.CapUntil
	r.mu.Unlock()

	online := false
	if r.d.Online != nil {
		online = r.d.Online(account)
	}

	switch phase {
	case PhaseWaitingOffline:
		// 等它真的被摘掉（离线）；超时也继续（可能本来就没在线）
		if online && now.Before(nextAt) {
			return Status{}, false
		}
		if r.d.Add == nil || !r.d.Add(account) {
			return r.fail(account, attempts, "重新上线失败（机器人通道未连接或池里没密码）")
		}
		return r.set(account, func(s *Status) {
			s.Phase = PhaseWaitingOnline
			s.LastMsg = "已重新下发上线，等它登录上报"
			s.NextAt = now.Add(time.Duration(r.d.OnlineWaitSec) * time.Second)
			s.UpdatedAt = now
		})

	case PhaseWaitingOnline:
		if !online {
			if now.Before(nextAt) {
				return Status{}, false // 还没上线，继续等
			}
			return r.fail(account, attempts, "重登后一直没上线（超时）")
		}
		delay := time.Duration(r.d.LaunchDelaySec) * time.Second
		return r.set(account, func(s *Status) {
			s.Phase = PhaseWaitingLaunch
			s.LastMsg = fmt.Sprintf("已上线，%s 后补发任务", delay)
			s.NextAt = now.Add(delay)
			s.UpdatedAt = now
		})

	case PhaseWaitingLaunch:
		if now.Before(nextAt) {
			return Status{}, false
		}
		if r.d.Launch == nil || !r.d.Launch(account) {
			return r.fail(account, attempts, "补发任务失败（通道未连接/载荷不可用）")
		}
		return r.set(account, func(s *Status) {
			s.Phase = PhaseDone
			s.Attempts++
			s.LastMsg = "已重登并补发任务（恢复完成）"
			s.UpdatedAt = now
		})

	case PhaseCapped:
		// 熔断到次日（2026-09-23）：期间不做任何动作 —— 等跨日/中控重启/面板手动解除。
		// 到点自动清除条目（跨日后当日卡死计数也归零，下次卡死可正常走重登）。
		if !now.Before(capUntil) {
			r.mu.Lock()
			if e := r.m[account]; e != nil && e.st.Phase == PhaseCapped && !now.Before(e.st.CapUntil) {
				delete(r.m, account)
			}
			r.mu.Unlock()
		}
		return Status{}, false

	case PhaseDone, PhaseFailed:
		// 保留 10 分钟供面板确认，之后自动清掉
		r.mu.Lock()
		e := r.m[account]
		if e != nil && now.Sub(e.st.UpdatedAt) > 10*time.Minute {
			delete(r.m, account)
		}
		r.mu.Unlock()
		_ = since
		return Status{}, false
	}
	return Status{}, false
}

// set 更新状态并返回（changed=true）。
func (r *Runner) set(account string, fn func(*Status)) (Status, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.m[account]
	if !ok {
		return Status{}, false
	}
	fn(&e.st)
	return e.st, true
}

// fail 记一次失败：到上限就 failed（交人工），否则回到 waiting_offline 再试一轮。
func (r *Runner) fail(account string, attempts int, msg string) (Status, bool) {
	now := r.d.Now()
	st, _ := r.set(account, func(s *Status) {
		s.Attempts++
		s.UpdatedAt = now
		if s.Attempts >= r.d.MaxAttempts {
			s.Phase = PhaseFailed
			s.LastMsg = fmt.Sprintf("%s（连续 %d 次，放弃自动恢复，等人工处理）", msg, s.Attempts)
			return
		}
		s.Phase = PhaseWaitingOffline
		s.LastMsg = fmt.Sprintf("%s（第 %d 次，稍后重试）", msg, s.Attempts)
		s.NextAt = now.Add(time.Duration(r.d.CooldownSec) * time.Second)
	})
	r.d.Log("[REGHOST] %s %s", account, st.LastMsg)
	return st, true
}

// Run 周期 Tick（ctx 结束即退出）。
func (r *Runner) Run(ctx context.Context) {
	t := time.NewTicker(r.d.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.Tick(r.d.Now())
		}
	}
}

func trim(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}
