package api

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"zyctrlcenter/internal/config"
	"zyctrlcenter/internal/services/process"
)

// 机器人内存护栏（2026-10-27）
//
// 背景：机器人是**单进程承载全部账号**（C++ 外壳 + python39.dll）。实测 commit
// 约 1.1GB/天泄漏（10-23 10:39 拉起 → 10-27 14:07 达 4.52GB），叠加主机其它大
// 进程后耗尽虚拟内存（WER 2004），中控退出、看门狗拉起、全队重登，当日产出 -17%。
//
// 策略：按 max(工作集, 提交量) 分级，把"4 天必炸"变成"低峰可控重启"：
//   - 告警线：只记日志（30min 节流）
//   - 低峰线：仅在低峰窗口（默认 05:00-06:30）重启
//   - 紧急线：任何时段重启
//   - 运行时长兜底：超过 N 小时也在低峰窗口重启（防指标失真）
//
// 重启 = 全体账号重登（约 5-15 分钟产出损失），所以阈值取基线（~415MB）的 5 倍以上，
// 且冷却期内即使紧急也不穿透（防重启风暴）。

// guardAction 护栏动作。
type guardAction int

const (
	guardNone          guardAction = iota
	guardWarn                      // 只告警
	guardRestartUrgent             // 紧急重启（任何时段）
	guardRestartQuiet              // 低峰重启（窗口内）
)

func (a guardAction) String() string {
	switch a {
	case guardWarn:
		return "warn"
	case guardRestartUrgent:
		return "restart_urgent"
	case guardRestartQuiet:
		return "restart_quiet"
	default:
		return "none"
	}
}

// robotGuardConfig 护栏运行配置（启动时归一）。
type robotGuardConfig struct {
	Enabled    bool
	DryRun     bool
	Interval   time.Duration
	WarnMB     float64
	QuietMB    float64
	UrgentMB   float64
	QuietStart int           // 本地时间，分钟数（0-1439）
	QuietEnd   int           // start==end 表示关闭低峰窗口
	MaxUptime  time.Duration // 0=关闭
	Cooldown   time.Duration
}

// robotGuardSample 单轮采样 + 判定输入。
type robotGuardSample struct {
	Now              time.Time
	Running          bool
	MemMB            float64
	Uptime           time.Duration
	Peers            int
	InQuiet          bool
	HasAttempt       bool
	SinceLastAttempt time.Duration
}

// robotGuardState 运行态（tick 写、HTTP 读；实例级，不用包级变量）。
type robotGuardState struct {
	mu           sync.Mutex
	cfg          robotGuardConfig
	lastSample   time.Time
	lastStat     process.Stat
	lastErr      string
	lastAction   string
	lastReason   string
	lastLog      time.Time
	lastAttempt  time.Time
	lastRestart  time.Time
	restartDay   string
	restartToday int
}

// ---------------------------------------------------------------- 配置

// robotGuardConfigOf 从 Config 读配置（未装配/零值时用默认）。
func robotGuardConfigOf(cfg *config.Config) robotGuardConfig {
	c := robotGuardConfig{
		Enabled:    true,
		Interval:   300 * time.Second,
		WarnMB:     1800,
		QuietMB:    2200,
		UrgentMB:   3200,
		QuietStart: 5 * 60,
		QuietEnd:   6*60 + 30,
		MaxUptime:  96 * time.Hour,
		Cooldown:   180 * time.Minute,
	}
	if cfg == nil {
		return c
	}
	c.Enabled = cfg.RobotGuard
	c.DryRun = cfg.RobotGuardDryRun
	if cfg.RobotGuardIntervalSec > 0 {
		c.Interval = time.Duration(cfg.RobotGuardIntervalSec) * time.Second
	}
	if cfg.RobotGuardWarnMB > 0 {
		c.WarnMB = float64(cfg.RobotGuardWarnMB)
	}
	if cfg.RobotGuardQuietMB > 0 {
		c.QuietMB = float64(cfg.RobotGuardQuietMB)
	}
	if cfg.RobotGuardUrgentMB > 0 {
		c.UrgentMB = float64(cfg.RobotGuardUrgentMB)
	}
	if ms, ok := parseHHMM(cfg.RobotGuardQuietStart); ok {
		c.QuietStart = ms
	}
	if me, ok := parseHHMM(cfg.RobotGuardQuietEnd); ok {
		c.QuietEnd = me
	}
	if cfg.RobotGuardMaxUptimeH >= 0 {
		c.MaxUptime = time.Duration(cfg.RobotGuardMaxUptimeH) * time.Hour
	}
	if cfg.RobotGuardCooldownMin >= 0 {
		c.Cooldown = time.Duration(cfg.RobotGuardCooldownMin) * time.Minute
	}
	return c
}

// normalizeGuard 启动时校验/钳制（乱序阈值、非法窗口、间隔越界）。
func normalizeGuard(c robotGuardConfig) (robotGuardConfig, []string) {
	var notes []string
	if c.Interval < time.Minute {
		c.Interval = time.Minute
		notes = append(notes, "采样间隔 <60s 已钳制为 60s")
	}
	if c.Interval > time.Hour {
		c.Interval = time.Hour
		notes = append(notes, "采样间隔 >3600s 已钳制为 3600s")
	}
	if c.WarnMB <= 0 || c.QuietMB <= 0 || c.UrgentMB <= 0 || !(c.WarnMB < c.QuietMB && c.QuietMB < c.UrgentMB) {
		c.WarnMB, c.QuietMB, c.UrgentMB = 1800, 2200, 3200
		notes = append(notes, "阈值非法或未按 warn<quiet<urgent 递增，已回落默认 1800/2200/3200")
	}
	if c.Cooldown < 0 {
		c.Cooldown = 0
	}
	if c.MaxUptime < 0 {
		c.MaxUptime = 0
	}
	if c.QuietStart < 0 || c.QuietStart > 1439 || c.QuietEnd < 0 || c.QuietEnd > 1439 {
		c.QuietStart, c.QuietEnd = 5*60, 6*60+30
		notes = append(notes, "低峰窗口非法，已回落 05:00-06:30")
	}
	return c, notes
}

// parseHHMM 解析 "HH:MM" → 当天分钟数。
func parseHHMM(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return 0, false
	}
	h, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	m, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

// inQuietWindow 是否在低峰窗口内（支持跨零点；start==end=关闭）。
func inQuietWindow(now time.Time, startMin, endMin int) bool {
	if startMin == endMin {
		return false
	}
	m := now.Hour()*60 + now.Minute()
	if startMin < endMin {
		return m >= startMin && m < endMin
	}
	return m >= startMin || m < endMin
}

func guardWindowLabel(startMin, endMin int) string {
	if startMin == endMin {
		return "关闭"
	}
	return fmt.Sprintf("%02d:%02d-%02d:%02d", startMin/60, startMin%60, endMin/60, endMin%60)
}

// ---------------------------------------------------------------- 判定

// decideAction 纯函数：按优先级给出动作与中文原因。
//
// 优先级：冷却 → 紧急 → 低峰(内存) → 低峰(时长) → 多进程告警 → 内存告警。
// 冷却期内**不穿透**（即使超过紧急线也不重启），防止重启风暴。
func decideAction(s robotGuardSample, c robotGuardConfig) (guardAction, string) {
	if !c.Enabled {
		return guardNone, ""
	}
	if !s.Running {
		return guardNone, "机器人未运行"
	}
	if s.HasAttempt && c.Cooldown > 0 && s.SinceLastAttempt < c.Cooldown {
		if s.MemMB >= c.WarnMB {
			return guardWarn, fmt.Sprintf("冷却中（距上次尝试 %s，< %s）：内存 %.0fMB 已过告警线，暂不重启",
				fmtDur(s.SinceLastAttempt), fmtDur(c.Cooldown), s.MemMB)
		}
		return guardNone, fmt.Sprintf("冷却中（距上次尝试 %s）", fmtDur(s.SinceLastAttempt))
	}
	if s.MemMB >= c.UrgentMB {
		return guardRestartUrgent, fmt.Sprintf("内存 %.0fMB ≥ 紧急阈值 %.0fMB（任何时段）", s.MemMB, c.UrgentMB)
	}
	if s.InQuiet && s.MemMB >= c.QuietMB {
		return guardRestartQuiet, fmt.Sprintf("内存 %.0fMB ≥ 低峰阈值 %.0fMB（低峰窗口内）", s.MemMB, c.QuietMB)
	}
	if s.InQuiet && c.MaxUptime > 0 && s.Uptime >= c.MaxUptime {
		return guardRestartQuiet, fmt.Sprintf("运行 %.1fh ≥ 上限 %.0fh（低峰窗口内兜底）",
			s.Uptime.Hours(), c.MaxUptime.Hours())
	}
	if s.Peers > 1 {
		return guardWarn, fmt.Sprintf("同部署目录发现 %d 个机器人进程，疑似孤儿（不自动清理）", s.Peers)
	}
	if s.MemMB >= c.WarnMB {
		return guardWarn, fmt.Sprintf("内存 %.0fMB ≥ 告警阈值 %.0fMB（未到低峰窗口/重启线）", s.MemMB, c.WarnMB)
	}
	return guardNone, ""
}

func fmtDur(d time.Duration) string {
	if d >= time.Hour {
		return fmt.Sprintf("%.1fh", d.Hours())
	}
	return fmt.Sprintf("%.0fm", d.Minutes())
}

// ---------------------------------------------------------------- 服务

// StartRobotGuard 启动内存护栏（启动即采一次，之后按间隔）。
func (a *API) StartRobotGuard(ctx context.Context) {
	cfg, notes := normalizeGuard(robotGuardConfigOf(a.Cfg))
	st := &robotGuardState{cfg: cfg}
	a.guard = st
	for _, n := range notes {
		a.Log.Printf("[ROBOT-GUARD] 配置归一：%s", n)
	}
	if !cfg.Enabled {
		a.Log.Printf("[ROBOT-GUARD] 未启用（CTRL_ROBOT_GUARD=0）")
		return
	}
	if a.Proc == nil {
		a.Log.Printf("[ROBOT-GUARD] 机器人进程管理器未装配，护栏不启动")
		return
	}
	a.Log.Printf("[ROBOT-GUARD] 已启动：间隔 %s / 告警 %.0fMB / 低峰 %.0fMB(%s) / 紧急 %.0fMB / 时长上限 %.0fh / 冷却 %s / dryrun=%v",
		cfg.Interval, cfg.WarnMB, cfg.QuietMB, guardWindowLabel(cfg.QuietStart, cfg.QuietEnd),
		cfg.UrgentMB, cfg.MaxUptime.Hours(), cfg.Cooldown, cfg.DryRun)

	a.robotGuardTick(time.Now(), true)
	tk := time.NewTicker(cfg.Interval)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			a.Log.Printf("[ROBOT-GUARD] 退出（ctx done）")
			return
		case now := <-tk.C:
			a.robotGuardTick(now, false)
		}
	}
}

// robotGuardTick 单轮：采样 → 判定 → 执行/日志。
func (a *API) robotGuardTick(now time.Time, first bool) {
	st := a.guard
	if st == nil || a.Proc == nil {
		return
	}
	st.mu.Lock()
	cfg := st.cfg
	lastAttempt := st.lastAttempt
	st.mu.Unlock()

	stat, err := a.Proc.Stat()
	running := err == nil

	sample := robotGuardSample{
		Now:              now,
		Running:          running,
		MemMB:            stat.MemMB(),
		Uptime:           stat.Uptime,
		Peers:            stat.Peers,
		InQuiet:          inQuietWindow(now, cfg.QuietStart, cfg.QuietEnd),
		HasAttempt:       !lastAttempt.IsZero(),
		SinceLastAttempt: now.Sub(lastAttempt),
	}
	action, reason := decideAction(sample, cfg)

	errMsg := ""
	if err != nil {
		errMsg = err.Error()
	}
	st.mu.Lock()
	st.lastSample = now
	st.lastStat = stat
	st.lastErr = errMsg
	st.lastAction = action.String()
	st.lastReason = reason
	st.mu.Unlock()

	if action == guardRestartUrgent || action == guardRestartQuiet {
		if cfg.DryRun {
			a.Log.Printf("[ROBOT-GUARD] dryrun=1：本应重启（%s）pid=%d mem=%.0fMB uptime=%.1fh，未执行",
				reason, stat.PID, stat.MemMB(), stat.Uptime.Hours())
			return
		}
		st.mu.Lock()
		st.lastAttempt = now
		st.mu.Unlock()
		a.robotGuardRestart(now, stat, reason)
		return
	}

	a.robotGuardLog(now, first, action, reason, stat, err)
}

// robotGuardRestart 重启机器人（全体重登；不额外补号，依赖 hello 自动补）。
func (a *API) robotGuardRestart(now time.Time, stat process.Stat, reason string) {
	a.Log.Printf("[ROBOT-GUARD] 触发重启：%s（pid=%d peers=%d mem=%.0fMB ws=%.0fMB commit=%.0fMB uptime=%.1fh）",
		reason, stat.PID, stat.Peers, stat.MemMB(), stat.WorkingSetMB, stat.CommitMB, stat.Uptime.Hours())

	// 硬编码 true：Cfg.KillRobots 默认 false，而中控重启后的孤儿场景 m.cmd=nil
	// 杀不到；按部署目录清理才安全（绝不动同机其它栈的同名进程）。
	if err := a.Proc.Restart(true); err != nil {
		a.Log.Printf("[ROBOT-GUARD] 重启失败：%v（下轮按冷却重试）", err)
		st := a.guard
		st.mu.Lock()
		st.lastErr = "重启失败: " + err.Error()
		st.mu.Unlock()
		return
	}
	a.Log.Printf("[ROBOT-GUARD] 重启指令已下发；等待机器人 hello 后自动补号（CTRL_RESTART_AUTO_ADD，10min 冷却）")

	if a.Store != nil {
		a.Store.LogEvent(map[string]any{
			"type": "robot_guard", "action": "robot_guard_restart",
			"pid": stat.PID, "mem_mb": math.Round(stat.MemMB()),
			"uptime_h": math.Round(stat.Uptime.Hours()*10) / 10,
			"reason":   reason,
		})
	}

	st := a.guard
	st.mu.Lock()
	st.lastRestart = now
	day := now.Format("20060102")
	if st.restartDay != day {
		st.restartDay = day
		st.restartToday = 0
	}
	st.restartToday++
	st.mu.Unlock()
}

// robotGuardLog 常规日志（30min 节流；未运行时 60min）。
func (a *API) robotGuardLog(now time.Time, first bool, action guardAction, reason string, stat process.Stat, err error) {
	st := a.guard
	st.mu.Lock()
	gap := 30 * time.Minute
	if err != nil {
		gap = 60 * time.Minute
	}
	if !first && !st.lastLog.IsZero() && now.Sub(st.lastLog) < gap {
		st.mu.Unlock()
		return
	}
	st.lastLog = now
	st.mu.Unlock()

	if err != nil {
		if errors.Is(err, process.ErrNotRunning) {
			a.Log.Printf("[ROBOT-GUARD] 未发现机器人进程（部署目录下）；本轮跳过")
		} else {
			a.Log.Printf("[ROBOT-GUARD] 采样失败：%v；本轮跳过（不做任何动作）", err)
		}
		return
	}
	a.Log.Printf("[ROBOT-GUARD] sample pid=%d peers=%d ws=%.0fMB commit=%.0fMB mem=%.0fMB uptime=%.1fh action=%s reason=%s",
		stat.PID, stat.Peers, stat.WorkingSetMB, stat.CommitMB, stat.MemMB(),
		stat.Uptime.Hours(), action, reason)
}

// RobotGuardSummary 护栏摘要（挂 /api/status；未启用/未装配返回 nil）。
func (a *API) RobotGuardSummary() map[string]any {
	st := a.guard
	if st == nil {
		return nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	out := map[string]any{
		"enabled":        st.cfg.Enabled,
		"dry_run":        st.cfg.DryRun,
		"interval_sec":   int(st.cfg.Interval.Seconds()),
		"running":        st.lastStat.PID > 0,
		"pid":            st.lastStat.PID,
		"peers":          st.lastStat.Peers,
		"ws_mb":          round1(st.lastStat.WorkingSetMB),
		"commit_mb":      round1(st.lastStat.CommitMB),
		"mem_mb":         round1(st.lastStat.MemMB()),
		"uptime_h":       round1(st.lastStat.Uptime.Hours()),
		"quiet_window":   guardWindowLabel(st.cfg.QuietStart, st.cfg.QuietEnd),
		"last_action":    st.lastAction,
		"last_reason":    st.lastReason,
		"restarts_today": st.restartToday,
		"thresholds": map[string]any{
			"warn_mb":      st.cfg.WarnMB,
			"quiet_mb":     st.cfg.QuietMB,
			"urgent_mb":    st.cfg.UrgentMB,
			"max_uptime_h": int(st.cfg.MaxUptime.Hours()),
			"cooldown_min": int(st.cfg.Cooldown.Minutes()),
		},
	}
	if !st.lastSample.IsZero() {
		out["last_sample"] = st.lastSample.Format("2006-01-02 15:04:05")
	}
	if st.lastErr != "" {
		out["last_error"] = st.lastErr
	}
	if !st.lastRestart.IsZero() {
		out["last_restart_at"] = st.lastRestart.Format("2006-01-02 15:04:05")
	}
	return out
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }
