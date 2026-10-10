// 47/48 图自动救援 tick + 错峰抓鬼（2026-10-10）。
//
// 救援背景：图 47/48 是"进得去出不来"的困死图（出图 map_skip 被服务端拒；与 45 瑶池回廊同族）。
// 机器人端已有救援状态机（deploy/zones/.../script/map_rescue.py：收到 share_daily_start 且
// share_key=map45_rescue 时转交），手动通道 = POST /api/daily/start
// {"share_key":"map45_rescue","chain_id":"newbie_full","accounts":[...]}（见该模块 doc）。
// 本文件把"发现 + 下发"自动化：60s 一轮扫在线号快照，命中 47/48 的号按频控下发救援命令。
// 命令让机器人端 stop 当前任务并进入救援；mapid 过滤保证只命中 47/48 号（正常抓鬼号不受影响）。
//
// 频控（内存态；中控重启清零可接受——重启后每号最多多补一发）：
//   - 每号 ≥30 分钟一发（机器人端救援要走路/对话，短窗重发只会打断）；
//   - 每号每日 ≤3 发（当日未出图 = 需人工/服务端拉出，重发无收益）；
//   - 每轮批量 ≤10 号（一 tick 一条命令；批量走同一通道）。
//
// 错峰抓鬼（默认关；CTRL_GHOST_STAGGER / CTRL_GHOST_STAGGER_SAMPLE_PCT）也落在本文件：
// offset(acc) = fnv1a32(acc) % (18*3600) —— 今日可派抓鬼时刻铺在本地 0 点~18 点；
// 灰度只约束 fnv1a32("s"+acc)%100 < pct 的号。消费点见 autotask.go（抓鬼候选过滤 /
// GhostSkipFunc / 到点拉回豁免）；env 经 internal/config 读取（跟随既有风格）。
package api

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"zyctrlcenter/internal/state"
)

// ---------------------------------------------------------------- 常量

const (
	// rescue48ShareKey 机器人端救援转交键（map_rescue.py:RESCUE_KEY；复用 share_daily 通道）。
	rescue48ShareKey = "map45_rescue"
	// rescue48ChainID 随命令下发的链数据文件名（救援导航用网格/npcs/dijkstra；手动通道同款）。
	rescue48ChainID = "newbie_full"
	// rescue48Interval tick 间隔。
	rescue48Interval = 60 * time.Second
	// rescue48MinGap 同一号两次下发的最小间隔。
	rescue48MinGap = 30 * time.Minute
	// rescue48DailyMax 同一号每日下发上限。
	rescue48DailyMax = 3
	// rescue48MaxPerTick 每轮批量上限。
	rescue48MaxPerTick = 10
)

// rescue48MapHit 该图是否救援目标（47/48；后续要纳入 45 时改这一处）。
func rescue48MapHit(mapid int) bool { return mapid == 47 || mapid == 48 }

// rescue48Eligible 单号资格（纯函数）：在线 + 图 47/48 + 未人工暂停 + 未当日卡死熔断。
// "已移除/私人池"在快照行上没有标记，由调用方注入 exclude 先剔（见 rescue48Tick）。
func rescue48Eligible(r state.Robot) bool {
	return r.Online && rescue48MapHit(r.MapID) && !r.Paused && !r.RestoreCapped
}

// ---------------------------------------------------------------- 频控

// rescue48Limiter 救援下发频控（内存态；见文件头说明）。惰性跨日：count 按 dayOf 判日，
// 跨日自动视为 0（无需定时清理；map 键数 = 账号数，有界）。
type rescue48Limiter struct {
	mu    sync.Mutex
	last  map[string]time.Time // 账号 → 上次下发时刻（≥30min 节流）
	count map[string]int       // 账号 → 当日已发次数
	dayOf map[string]string    // 账号 → count 所属日期（YYYYMMDD）
}

// newRescue48Limiter 创建频控表。
func newRescue48Limiter() *rescue48Limiter {
	return &rescue48Limiter{last: map[string]time.Time{}, count: map[string]int{}, dayOf: map[string]string{}}
}

// allow 该号现在能不能再发一发（只判不记账；下发成功后由 mark 记账）。
// 返回 (false, 原因) 时调用方应把号留在原地、并把原因写进留痕日志。
func (l *rescue48Limiter) allow(acc string, now time.Time) (bool, string) {
	if acc == "" {
		return false, ""
	}
	day := now.Format("20060102")
	l.mu.Lock()
	defer l.mu.Unlock()
	if t, ok := l.last[acc]; ok && now.Sub(t) < rescue48MinGap {
		return false, fmt.Sprintf("距上次下发 %d 分钟（< %d 分钟）",
			int(now.Sub(t)/time.Minute), int(rescue48MinGap/time.Minute))
	}
	if l.dayOf[acc] == day && l.count[acc] >= rescue48DailyMax {
		return false, fmt.Sprintf("今日已发 %d 次（达上限 %d）", l.count[acc], rescue48DailyMax)
	}
	return true, ""
}

// mark 记一发（在命令真正下发成功后调用）。
func (l *rescue48Limiter) mark(acc string, now time.Time) {
	if acc == "" {
		return
	}
	day := now.Format("20060102")
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last == nil {
		l.last = map[string]time.Time{}
	}
	if l.count == nil {
		l.count = map[string]int{}
	}
	if l.dayOf == nil {
		l.dayOf = map[string]string{}
	}
	l.last[acc] = now
	if l.dayOf[acc] != day {
		l.dayOf[acc] = day
		l.count[acc] = 0
	}
	l.count[acc]++
}

// ---------------------------------------------------------------- 扫描（纯逻辑）

// rescue48Scan 本轮扫描（纯逻辑：资格过滤 + 频控 + 排序 + 截断 max）：
// exclude 判定由调用方注入（已移除/私人池等；便于单测不依赖 state.State）。
// 返回：batch=本轮下发名单（≤max，升序）；throttled/reasons=被频控跳过的号与一一对应的原因；
// queued=已放行但超出 max、留到下轮的号数。
func rescue48Scan(now time.Time, snap []state.Robot, lim *rescue48Limiter,
	exclude func(string) bool, max int) (batch, throttled, reasons []string, queued int) {
	ready := make([]string, 0, 8)
	for _, r := range snap {
		acc := r.Account
		if acc == "" || !rescue48Eligible(r) {
			continue
		}
		if exclude != nil && exclude(acc) {
			continue // 已移除 / 私人池（不自动编排，与候选/水位同口径）
		}
		if ok, why := lim.allow(acc, now); !ok {
			if why != "" {
				throttled = append(throttled, acc)
				reasons = append(reasons, why)
			}
			continue
		}
		ready = append(ready, acc)
	}
	sort.Strings(ready)
	if max > 0 && len(ready) > max {
		queued = len(ready) - max
		ready = ready[:max]
	}
	return ready, throttled, reasons, queued
}

// rescue48Summary 名单摘要（日志用，≤10 条：账号[（原因）]…，超出以 …(+N) 结尾）。
func rescue48Summary(accs, reasons []string) string {
	const maxShow = 10
	n := len(accs)
	show := n
	if show > maxShow {
		show = maxShow
	}
	parts := make([]string, 0, show+1)
	for i := 0; i < show; i++ {
		s := accs[i]
		if i < len(reasons) && reasons[i] != "" {
			s += "（" + reasons[i] + "）"
		}
		parts = append(parts, s)
	}
	if n > show {
		parts = append(parts, fmt.Sprintf("…(+%d)", n-show))
	}
	return strings.Join(parts, "、")
}

// ---------------------------------------------------------------- tick 与主循环

// rescue48Tick 跑一轮：扫描 → 组装救援命令 → 下发 → 记账 → 留痕。
func (a *API) rescue48Tick(now time.Time, lim *rescue48Limiter) {
	if a.St == nil || a.Events == nil {
		return
	}
	snap := a.St.Snapshot()
	batch, throttled, reasons, queued := rescue48Scan(now, snap, lim,
		// 已移除不自动拉起；私人池零自动编排（2026-09-30 契约，救援也不能例外）。
		func(acc string) bool { return a.St.IsRemoved(acc) || a.IsPersonal(acc) }, rescue48MaxPerTick)
	if len(batch) == 0 {
		if len(throttled) > 0 {
			a.Log.Printf("[RESCUE48] 本轮无可下发：%d 个号受频控跳过（%s）",
				len(throttled), rescue48Summary(throttled, reasons))
		}
		return
	}
	// 载荷构造 = 分享日常 raw 直发同款（shareDailyStartCmdOfRaw：share_key/chain_id 给定，
	// daily_limit=1；构造失败 = 硬失败，一条不发）。
	cmd, err := a.shareDailyStartCmdOfRaw(rescue48ShareKey, rescue48ChainID, batch)
	if err != nil {
		a.Log.Printf("[RESCUE48] 救援载荷构造失败（不发空命令）：%v", err)
		return
	}
	if !a.Events.SendCmd(cmd, "rescue48") {
		a.Log.Printf("[RESCUE48] 下发失败（机器人通道未连接）：%d 个号本轮不记账，下轮重试", len(batch))
		return
	}
	for _, acc := range batch {
		lim.mark(acc, now)
	}
	if queued > 0 {
		a.Log.Printf("[RESCUE48] %d 个号：%s（另有 %d 个号排队，下轮继续）",
			len(batch), rescue48Summary(batch, nil), queued)
	} else {
		a.Log.Printf("[RESCUE48] %d 个号：%s", len(batch), rescue48Summary(batch, nil))
	}
	if a.Store != nil {
		a.Store.LogEvent(map[string]any{"type": "api", "action": "rescue48",
			"zone": a.currentZoneKey(), "accounts": batch, "count": len(batch),
			"queued": queued, "throttled": len(throttled)})
	}
}

// StartRescue48 自动救援 tick 主循环（main 里 go webAPI.StartRescue48(ctx)）：
// 60s 一轮，直到 ctx.Done()；env CTRL_RESCUE48（默认 1）=0 时不启动。
func (a *API) StartRescue48(ctx context.Context) {
	if a.Cfg != nil && !a.Cfg.Rescue48Enabled {
		a.Log.Printf("[RESCUE48] 未启用（CTRL_RESCUE48=0），自动救援不启动")
		return
	}
	if a.St == nil || a.Events == nil {
		a.Log.Printf("[RESCUE48] 依赖不就绪（state/事件通道缺失），自动救援不启动")
		return
	}
	lim := newRescue48Limiter()
	tk := time.NewTicker(rescue48Interval)
	defer tk.Stop()
	a.Log.Printf("[RESCUE48] 自动救援已启动（%s 一轮；命中图 47/48；每号 ≥%s 一发、每日 ≤%d 发、每轮 ≤%d 个）",
		rescue48Interval, rescue48MinGap, rescue48DailyMax, rescue48MaxPerTick)
	for {
		select {
		case <-ctx.Done():
			a.Log.Printf("[RESCUE48] 退出（ctx done）")
			return
		case <-tk.C:
			a.rescue48Tick(time.Now(), lim)
		}
	}
}

// ---------------------------------------------------------------- 错峰抓鬼（默认关）

// ghostStaggerDaySec 错峰窗口长度（18h）：offset ∈ [0, 18h)，铺在本地 0 点~18 点。
const ghostStaggerDaySec = 18 * 3600

// fnv1a32 FNV-1a 32 位哈希（错峰 offset 与灰度取样的确定性来源）。
func fnv1a32(s string) uint32 {
	const (
		offset32 = 2166136261
		prime32  = 16777619
	)
	h := uint32(offset32)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= prime32
	}
	return h
}

// ghostStaggerOffsetSec 该号今日可派时刻相对本地 0 点的偏移秒数（空账号 = 0）。
func ghostStaggerOffsetSec(acc string) int64 {
	if acc == "" {
		return 0
	}
	return int64(fnv1a32(acc) % uint32(ghostStaggerDaySec))
}

// ghostStaggerCovered 灰度取样：fnv1a32("s"+acc)%100 < pct 的号才受约束
// （pct≤0 = 无号受约束；pct≥100 = 全量受约束）。
func ghostStaggerCovered(acc string, pct int) bool {
	if pct <= 0 {
		return false
	}
	if pct >= 100 {
		return true
	}
	return int(fnv1a32("s"+acc)%100) < pct
}

// ghostStaggerReady 该号今日是否已到可派时刻：now ≥ 本地 0 点 + offset（now 注入便于单测）。
func ghostStaggerReady(acc string, now time.Time) bool {
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	return now.Unix() >= dayStart.Unix()+ghostStaggerOffsetSec(acc)
}

// ghostStaggerConstrained 该号是否受错峰约束（开关开 + 灰度命中；Cfg 缺席视为关）。
func (a *API) ghostStaggerConstrained(acc string) bool {
	if a.Cfg == nil || !a.Cfg.GhostStagger {
		return false
	}
	return ghostStaggerCovered(acc, a.Cfg.GhostStaggerSamplePct)
}

// ghostStaggerPullBack 错峰"到点拉回"豁免判据（纯函数，便于单测）：**仅**"受约束 + 已到点 +
// 纯游荡"的号可豁免 Busy 过滤 —— 纯游荡 = walk.enabled=true 且不在孵化/战斗、无活跃抓鬼会话。
// （r.Walking() 含孵化；这里按"纯游荡"语义排除孵化，避免把在孵化的号拉去抓鬼。）
func ghostStaggerPullBack(constrained, ready bool, r state.Robot) bool {
	if !constrained || !ready || r.HatchActive() || r.Fight || r.GhostActive() {
		return false
	}
	m, ok := r.Walk.(map[string]any)
	if !ok {
		return false
	}
	en, _ := m["enabled"].(bool)
	return en
}
