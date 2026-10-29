package api

import (
	"strings"
	"testing"
	"time"

	"zyctrlcenter/internal/config"
	"zyctrlcenter/internal/services/process"
)

func mustClock(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.ParseInLocation("2006-01-02 15:04:05", s, time.Local)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

func TestInQuietWindowBoundaries(t *testing.T) {
	cases := []struct {
		at   string
		want bool
	}{
		{"2026-10-27 04:59:59", false},
		{"2026-10-27 05:00:00", true},
		{"2026-10-27 06:29:59", true},
		{"2026-10-27 06:30:00", false},
		{"2026-10-27 12:00:00", false},
	}
	for _, c := range cases {
		if got := inQuietWindow(mustClock(t, c.at), 5*60, 6*60+30); got != c.want {
			t.Fatalf("%s 期望 %v 实际 %v", c.at, c.want, got)
		}
	}
}

func TestInQuietWindowCrossMidnight(t *testing.T) {
	cases := []struct {
		at   string
		want bool
	}{
		{"2026-10-27 22:59:59", false},
		{"2026-10-27 23:00:00", true},
		{"2026-10-27 00:30:00", true},
		{"2026-10-27 01:00:00", false},
	}
	for _, c := range cases {
		if got := inQuietWindow(mustClock(t, c.at), 23*60, 60); got != c.want {
			t.Fatalf("%s 期望 %v 实际 %v", c.at, c.want, got)
		}
	}
}

func TestInQuietWindowDisabled(t *testing.T) {
	if inQuietWindow(mustClock(t, "2026-10-27 05:30:00"), 300, 300) {
		t.Fatal("start==end 应视为关闭窗口")
	}
}

func guardCfg() robotGuardConfig {
	return robotGuardConfig{
		Enabled: true, Interval: 5 * time.Minute,
		WarnMB: 1800, QuietMB: 2200, UrgentMB: 3200,
		QuietStart: 5 * 60, QuietEnd: 6*60 + 30,
		MaxUptime: 96 * time.Hour, Cooldown: 180 * time.Minute,
	}
}

func sample(mem float64, quiet bool) robotGuardSample {
	return robotGuardSample{Now: time.Now(), Running: true, MemMB: mem, InQuiet: quiet,
		Uptime: 10 * time.Hour, Peers: 1}
}

func TestDecideActionThresholds(t *testing.T) {
	c := guardCfg()
	cases := []struct {
		name string
		s    robotGuardSample
		want guardAction
	}{
		{"未启用", func() robotGuardSample { s := sample(9999, true); return s }(), guardNone},
		{"未运行", robotGuardSample{Running: false, MemMB: 9999}, guardNone},
		{"1799 不告警", sample(1799, false), guardNone},
		{"1800 告警", sample(1800, false), guardWarn},
		{"2199 窗口内不重启", sample(2199, true), guardWarn},
		{"2200 窗口外只告警", sample(2200, false), guardWarn},
		{"2200 窗口内低峰重启", sample(2200, true), guardRestartQuiet},
		{"3199 窗口外告警", sample(3199, false), guardWarn},
		{"3200 窗口外紧急重启", sample(3200, false), guardRestartUrgent},
		{"3500 窗口内仍算紧急", sample(3500, true), guardRestartUrgent},
	}
	for _, tc := range cases {
		cfg := c
		if tc.name == "未启用" {
			cfg.Enabled = false
		}
		got, reason := decideAction(tc.s, cfg)
		if got != tc.want {
			t.Fatalf("%s: 期望 %v 实际 %v（%s）", tc.name, tc.want, got, reason)
		}
		if got != guardNone && strings.TrimSpace(reason) == "" {
			t.Fatalf("%s: 有动作但原因为空", tc.name)
		}
	}
}

func TestDecideActionCooldown(t *testing.T) {
	c := guardCfg()
	s := sample(3500, false)
	s.HasAttempt = true
	s.SinceLastAttempt = 179 * time.Minute
	if act, _ := decideAction(s, c); act == guardRestartUrgent {
		t.Fatal("冷却 179min 内不应重启（即使超紧急线）")
	}
	s.SinceLastAttempt = 180 * time.Minute
	if act, _ := decideAction(s, c); act != guardRestartUrgent {
		t.Fatalf("冷却满 180min 应放行紧急重启，实际 %v", act)
	}
	c.Cooldown = 0
	s.SinceLastAttempt = 1 * time.Second
	if act, _ := decideAction(s, c); act != guardRestartUrgent {
		t.Fatalf("冷却 0 应立即放行，实际 %v", act)
	}
}

func TestDecideActionUptimeFallback(t *testing.T) {
	c := guardCfg()
	s := sample(500, true)
	s.Uptime = 95*time.Hour + 59*time.Minute
	if act, _ := decideAction(s, c); act != guardNone {
		t.Fatalf("95h59m 不应重启，实际 %v", act)
	}
	s.Uptime = 96 * time.Hour
	if act, _ := decideAction(s, c); act != guardRestartQuiet {
		t.Fatalf("96h 窗口内应兜底重启，实际 %v", act)
	}
	s.InQuiet = false
	if act, _ := decideAction(s, c); act != guardNone {
		t.Fatalf("96h 窗口外不应重启，实际 %v", act)
	}
	c.MaxUptime = 0
	s.InQuiet = true
	if act, _ := decideAction(s, c); act != guardNone {
		t.Fatalf("时长兜底=0 应关闭，实际 %v", act)
	}
}

func TestDecideActionPeersWarn(t *testing.T) {
	s := sample(500, false)
	s.Peers = 2
	act, reason := decideAction(s, guardCfg())
	if act != guardWarn || !strings.Contains(reason, "2 个机器人进程") {
		t.Fatalf("多进程应告警，实际 %v %s", act, reason)
	}
}

func TestNormalizeGuard(t *testing.T) {
	c := guardCfg()
	c.WarnMB, c.QuietMB, c.UrgentMB = 3000, 2000, 2500
	got, notes := normalizeGuard(c)
	if got.WarnMB != 1800 || got.QuietMB != 2200 || got.UrgentMB != 3200 {
		t.Fatalf("乱序阈值应回落默认，实际 %+v", got)
	}
	if len(notes) == 0 {
		t.Fatal("应给出归一提示")
	}

	c = guardCfg()
	c.Interval = 30 * time.Second
	c.Cooldown = -time.Minute
	c.MaxUptime = -time.Hour
	if got, _ := normalizeGuard(c); got.Interval != time.Minute || got.Cooldown != 0 || got.MaxUptime != 0 {
		t.Fatalf("钳制失败: %+v", got)
	}

	c = guardCfg()
	c.Interval = 2 * time.Hour
	if got, _ := normalizeGuard(c); got.Interval != time.Hour {
		t.Fatalf("上界钳制失败: %s", got.Interval)
	}

	c = guardCfg()
	c.QuietStart, c.QuietEnd = -1, 9999
	if got, _ := normalizeGuard(c); got.QuietStart != 300 || got.QuietEnd != 390 {
		t.Fatalf("非法窗口应回落 05:00-06:30: %+v", got)
	}
}

func TestParseHHMM(t *testing.T) {
	ok := map[string]int{"05:00": 300, "5:00": 300, "23:59": 1439, " 06:30 ": 390}
	for in, want := range ok {
		if got, valid := parseHHMM(in); !valid || got != want {
			t.Fatalf("%q 期望 %d 实际 %d/%v", in, want, got, valid)
		}
	}
	for _, in := range []string{"", "24:00", "05:60", "abc", "05", "05:00:00"} {
		if _, valid := parseHHMM(in); valid {
			t.Fatalf("%q 应判非法", in)
		}
	}
}

func TestGuardWindowLabel(t *testing.T) {
	if got := guardWindowLabel(300, 390); got != "05:00-06:30" {
		t.Fatalf("窗口标签错误: %s", got)
	}
	if got := guardWindowLabel(300, 300); got != "关闭" {
		t.Fatalf("关闭窗口标签错误: %s", got)
	}
}

func TestRobotGuardConfigOfDefaults(t *testing.T) {
	c := robotGuardConfigOf(nil)
	if !c.Enabled || c.Interval != 5*time.Minute || c.WarnMB != 1800 || c.QuietMB != 2200 ||
		c.UrgentMB != 3200 || c.QuietStart != 300 || c.QuietEnd != 390 ||
		c.MaxUptime != 96*time.Hour || c.Cooldown != 180*time.Minute {
		t.Fatalf("默认值错误: %+v", c)
	}
	cfg := &config.Config{
		RobotGuard: true, RobotGuardDryRun: true, RobotGuardIntervalSec: 120,
		RobotGuardWarnMB: 1000, RobotGuardQuietMB: 1500, RobotGuardUrgentMB: 2000,
		RobotGuardQuietStart: "01:00", RobotGuardQuietEnd: "02:00",
		RobotGuardMaxUptimeH: 48, RobotGuardCooldownMin: 60,
	}
	c = robotGuardConfigOf(cfg)
	if !c.DryRun || c.Interval != 2*time.Minute || c.WarnMB != 1000 || c.QuietStart != 60 ||
		c.QuietEnd != 120 || c.MaxUptime != 48*time.Hour || c.Cooldown != time.Hour {
		t.Fatalf("读取配置错误: %+v", c)
	}
}

func TestRobotGuardSummary(t *testing.T) {
	a := &API{}
	if a.RobotGuardSummary() != nil {
		t.Fatal("未装配时应返回 nil")
	}
	st := &robotGuardState{cfg: guardCfg()}
	st.lastSample = time.Date(2026, 10, 27, 14, 30, 0, 0, time.Local)
	st.lastStat = process.Stat{PID: 42388, WorkingSetMB: 362.4, CommitMB: 415.2,
		Uptime: 90 * time.Minute, Peers: 1}
	st.lastAction = "none"
	st.lastReason = ""
	st.restartToday = 2
	a.guard = st

	s := a.RobotGuardSummary()
	if s == nil {
		t.Fatal("摘要不应为 nil")
	}
	if s["pid"].(int) != 42388 || s["running"].(bool) != true {
		t.Fatalf("pid/running 错误: %+v", s)
	}
	if s["mem_mb"].(float64) != 415.2 {
		t.Fatalf("mem 应取较大值 415.2，实际 %v", s["mem_mb"])
	}
	if s["peers"].(int) != 1 || s["restarts_today"].(int) != 2 {
		t.Fatalf("peers/restarts 错误: %+v", s)
	}
	if s["quiet_window"].(string) != "05:00-06:30" {
		t.Fatalf("窗口标签错误: %v", s["quiet_window"])
	}
	if _, ok := s["last_sample"]; !ok {
		t.Fatal("应有 last_sample")
	}
	if _, ok := s["last_restart_at"]; ok {
		t.Fatal("未重启过不应有 last_restart_at")
	}
}
