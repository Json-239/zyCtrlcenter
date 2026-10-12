// Package rotation 号池轮换（最小版，2026-10-12 立项）：把"今日抓鬼已满 + 纯游荡"的号
// 按速率下线，让水位器从预备队（可用但离线的号）补新号上线继续抓鬼 —— 2+ 班次轮换
// （产能上限 ×2 级）；到龄（默认 24h，跨日）自动解除"已移除"回池。
//
// 分工（与 waterline / roampool / rescue48 同款架构）：
//   - 本包只做**纯决策**（Candidate / HourQuota / Store / Plan 可单测，不碰网络与 state）；
//   - 壳层（internal/api/rotation.go）负责：取状态快照、算预备队、组装 Facts、调 Plan、
//     用既有下线通路执行（sendOfflineChunks：cancelRegHost + MarkRemoved + 删行 +
//     robot_manage remove）、回池（state.UnmarkRemoved）与留痕。
//
// 安全护栏（逐条，宁可少下线不误下线）：
//  1. 候选（全满足才下线）：在线 ≥30 分钟 + 今日抓鬼已满（心跳 done≥limit 且
//     count_date=今日，或独立满额表）+ 纯游荡（walk.enabled 且非战斗/非孵化/非抓鬼会话）
//     + 非人工暂停 + 非熔断 + 非私人池 + 无队伍 + 不在自动重登处理中 + 今日领双已用
//     （心跳 double_claim_date==今日；字段缺失放行 —— 旧版机器人不报该字段）；
//  2. 预备队保护：可用且离线（且未移除/未暂停）的毕业号 < MinReserve（默认 50）→ 本轮不下线；
//  3. 速率：≤ PerHour（默认 30）/小时（整点窗口重置），且每轮 ≤ MaxPerTick（10）；
//  4. 名单：被下线的号持久化（data/rotation.json，原子落盘），age ≥ KeepHours → 回池。
//
// 开关 CTRL_ROTATION（默认 0=关）：关闭时**仍执行到龄回池**（名单是自记账，停用模块
// 不应让号永久卡在移除名单里）。
package rotation

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"zyctrlcenter/internal/state"
)

// ---------------------------------------------------------------- 常量与配置

const (
	// DefaultPerHour 每小时最多下线的号数（速率）。
	DefaultPerHour = 30
	// DefaultMinReserve 预备队保护下限：可用且离线的毕业号少于它 → 本轮不下线。
	DefaultMinReserve = 50
	// DefaultKeepHours 轮换号回池年龄（小时；默认 24=跨日回池）。
	DefaultKeepHours = 24
	// MaxPerTick 每轮（60s tick）最多下线数。
	MaxPerTick = 10
	// MinOnline 候选要求的"在线时长"下限：刚上线/刚回池的号先跑满 30 分钟再说。
	MinOnline = 30 * time.Minute
	// Interval tick 间隔（壳层主循环用）。
	Interval = 60 * time.Second

	MaxPerHour    = 1000
	MaxMinReserve = 10000
	MaxKeepHours  = 720
)

// Config 轮换配置（来自 env：CTRL_ROTATION / _PER_HOUR / _MIN_RESERVE / _KEEP_HOURS）。
type Config struct {
	Enabled    bool `json:"enabled"`
	PerHour    int  `json:"per_hour"`
	MinReserve int  `json:"min_reserve"`
	KeepHours  int  `json:"keep_hours"`
}

// DefaultConfig 默认配置：**enabled=false**（必须显式打开，不会自己动生产）。
func DefaultConfig() Config {
	return Config{Enabled: false, PerHour: DefaultPerHour, MinReserve: DefaultMinReserve, KeepHours: DefaultKeepHours}
}

// Normalize 把配置夹到合法范围（脏配置不会引发异常动作）。
// MinReserve=0 合法（显式关闭预备队保护）；负数归 0。
func (c Config) Normalize() Config {
	if c.PerHour <= 0 {
		c.PerHour = DefaultPerHour
	}
	if c.PerHour > MaxPerHour {
		c.PerHour = MaxPerHour
	}
	if c.MinReserve < 0 {
		c.MinReserve = 0
	}
	if c.MinReserve > MaxMinReserve {
		c.MinReserve = MaxMinReserve
	}
	if c.KeepHours <= 0 {
		c.KeepHours = DefaultKeepHours
	}
	if c.KeepHours > MaxKeepHours {
		c.KeepHours = MaxKeepHours
	}
	return c
}

// ---------------------------------------------------------------- 候选过滤（纯函数）

// Facts 单号"外部事实"（壳层预计算；核心只做纯判据，便于单测）。
type Facts struct {
	// GhostFull 今日抓鬼已满（心跳 done≥limit 且 count_date=今日；或独立满额表）。
	GhostFull bool
	// OnlineFor 已持续在线时长（壳层按"首见在线"起算；刚见到的号 = 0）。
	OnlineFor time.Duration
	// Personal 私人池（零自动编排，轮换也不能例外）。
	Personal bool
	// Removed 已在移除名单（防御；正常快照里不会走到）。
	Removed bool
	// Reghosting 正在"自动重登恢复"处理中（等待离线/在线/补发相位）。
	Reghosting bool
}

// Candidate 单号能否被轮换下线（纯函数；false 时返回原因供日志/状态展示）。
//
// today 由壳层传入（YYYYMMDD；便于单测注入）。注意"今日领双"判据的容错方向与现状口径：
//   - double_claim_date == today → 已领（放行）；
//   - 字段为空（旧版机器人不报 / 从未领过）→ **放行**（无法证明没领，不拦）；
//   - 字段有值且非今日 → 拦（能看出今天的每日流程还没走完，先不轮换它）。
//
// 2026-10-12 复核实测：机器人端 pre_daily.double_claim_date() 只产出"今日值"或空串
// （不存在"有值且非今日"态）⇒ 本条件当前**等价恒放行**（评估轮换面时勿按"能拦未领"理解）；
// 分支保留作前向兼容：若机器人端未来改为三态上报，"有值且非今日"将拦下流程未走完的号。
func Candidate(r state.Robot, f Facts, today string) (bool, string) {
	switch {
	case r.Account == "":
		return false, ""
	case !r.Online:
		return false, "离线"
	case f.Removed:
		return false, "已在移除名单"
	case f.Personal:
		return false, "私人池"
	case r.Paused:
		return false, "人工暂停"
	case r.RestoreCapped:
		return false, "当日卡死熔断"
	case f.Reghosting:
		return false, "自动重登处理中"
	case !f.GhostFull:
		return false, "今日抓鬼未满"
	case inTeam(r):
		return false, "在队伍中"
	case r.Fight:
		return false, "战斗中"
	case r.HatchActive():
		return false, "孵化中"
	case r.GhostActive():
		return false, "抓鬼会话活跃"
	case !walkEnabled(r):
		return false, "不在游荡"
	case f.OnlineFor < MinOnline:
		return false, "在线不足 30 分钟"
	case r.DoubleClaimDate != "" && r.DoubleClaimDate != today:
		return false, "今日领双未领（心跳有日期值且非今日；当前上报口径下不可达）"
	}
	return true, ""
}

// walkEnabled 机器人上报 walk.enabled=true（游荡中）。
func walkEnabled(r state.Robot) bool {
	m, ok := r.Walk.(map[string]any)
	if !ok {
		return false
	}
	en, _ := m["enabled"].(bool)
	return en
}

// inTeam 是否在队伍里。心跳契约（2026-09-29 定稿）：不在队时机器人省略 team 块或置 null
// → nil；因此**块存在即视为在队**（含集结中/暂离）—— 保守方向：宁可不轮换，不拆队。
func inTeam(r state.Robot) bool {
	return r.TeamBlock() != nil
}

// ---------------------------------------------------------------- 限速（整点窗口）

// HourQuota 小时窗口限速（固定窗口，整点重置）：本轮额度 = min(perTick, perHour-本小时已用)。
// Plan 只读（可重复调用），Commit 在真正执行后记账（同一 goroutine 内使用）。
type HourQuota struct {
	hourStart time.Time
	used      int
}

// sameHour a、b 是否落在同一个本地整点窗口（零值 hourStart 视为不同窗口）。
func sameHour(a, b time.Time) bool {
	return a.Truncate(time.Hour).Equal(b.Truncate(time.Hour))
}

// Plan 计算本轮可用额度（只读）：min(perTick, perHour-本小时已用)；跨小时自动视为 0 已用。
func (q *HourQuota) Plan(now time.Time, perHour, perTick int) int {
	if perHour <= 0 {
		return 0
	}
	if perTick <= 0 {
		perTick = MaxPerTick
	}
	used := q.used
	if !sameHour(q.hourStart, now) {
		used = 0
	}
	left := perHour - used
	if left <= 0 {
		return 0
	}
	if left < perTick {
		return left
	}
	return perTick
}

// Commit 记账本轮实际下线数（跨小时先重置窗口）。
func (q *HourQuota) Commit(now time.Time, n int) {
	if n <= 0 {
		return
	}
	if !sameHour(q.hourStart, now) {
		q.hourStart, q.used = now.Truncate(time.Hour), 0
	}
	q.used += n
}

// Used 本小时已用额度（状态接口用）。
func (q *HourQuota) Used(now time.Time) int {
	if !sameHour(q.hourStart, now) {
		return 0
	}
	return q.used
}

// ---------------------------------------------------------------- 一轮判决

// Result 一轮判决结果（壳层据它执行与留痕）。
type Result struct {
	// Batch 本轮应下线的号（≤min(MaxPerTick, 小时剩余)；快照升序）。
	Batch []string
	// Eligible 通过全部过滤的候选总数（含被额度截断排队的部分）。
	Eligible int
	// Queued 候选超出本轮额度、留到后面的号数。
	Queued int
	// Blocked 整轮被阻断的原因（如"预备队不足"；空 = 正常执行）。
	Blocked string
	// Reasons 被过滤号 → 原因（日志摘要/排查用；空账号不给原因）。
	Reasons map[string]string
}

// Plan 一轮下线判决（纯逻辑；执行由壳层做，见 internal/api/rotation.go）：
//  1. 预备队保护：MinReserve>0 且 reserve < MinReserve → 不下线（Blocked）；
//  2. 逐号过滤：Candidate 全过才算候选（快照已是账号升序，天然确定性）；
//  3. 额度截断：本 tick ≤ min(MaxPerTick, 本小时剩余额度)（quota.Plan；不 Commit）。
func Plan(now time.Time, cfg Config, snap []state.Robot, facts func(acc string) Facts,
	today string, reserve int, quota *HourQuota) Result {
	cfg = cfg.Normalize()
	res := Result{Reasons: map[string]string{}}
	if cfg.MinReserve > 0 && reserve < cfg.MinReserve {
		res.Blocked = fmt.Sprintf("预备队 %d < 下限 %d", reserve, cfg.MinReserve)
		return res
	}
	cands := make([]string, 0, 16)
	for _, r := range snap {
		ok, why := Candidate(r, facts(r.Account), today)
		if !ok {
			if why != "" {
				res.Reasons[r.Account] = why
			}
			continue
		}
		cands = append(cands, r.Account)
	}
	res.Eligible = len(cands)
	if len(cands) == 0 {
		return res
	}
	keep := quota.Plan(now, cfg.PerHour, MaxPerTick)
	if keep <= 0 {
		res.Queued = len(cands)
		return res
	}
	if len(cands) > keep {
		res.Queued = len(cands) - keep
		cands = cands[:keep]
	}
	res.Batch = cands
	return res
}

// Summary 名单摘要（日志用，≤maxShow 条：账号…，超出以 …(+N) 结尾）。
func Summary(accs []string, maxShow int) string {
	if maxShow <= 0 {
		maxShow = 10
	}
	n := len(accs)
	show := n
	if show > maxShow {
		show = maxShow
	}
	parts := make([]string, 0, show+1)
	parts = append(parts, accs[:show]...)
	if n > show {
		parts = append(parts, fmt.Sprintf("…(+%d)", n-show))
	}
	return strings.Join(parts, "、")
}

// ReasonSummary 过滤原因聚合摘要（日志用）："原因×计数"，按计数降序、计数相同按原因升序，
// 最多 maxTop 项，其余合并为 "…(+N 类)"。
func ReasonSummary(reasons map[string]string, maxTop int) string {
	if len(reasons) == 0 {
		return ""
	}
	if maxTop <= 0 {
		maxTop = 5
	}
	counts := map[string]int{}
	for _, why := range reasons {
		counts[why]++
	}
	type kv struct {
		why string
		n   int
	}
	list := make([]kv, 0, len(counts))
	for w, n := range counts {
		list = append(list, kv{w, n})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].n != list[j].n {
			return list[i].n > list[j].n
		}
		return list[i].why < list[j].why
	})
	parts := make([]string, 0, maxTop+1)
	for i, it := range list {
		if i >= maxTop {
			parts = append(parts, fmt.Sprintf("…(+%d 类)", len(list)-maxTop))
			break
		}
		parts = append(parts, fmt.Sprintf("%s×%d", it.why, it.n))
	}
	return strings.Join(parts, "、")
}

// ---------------------------------------------------------------- 名单持久化

// Entry 名单条目（被轮换下线的号）。
type Entry struct {
	Account    string `json:"account"`
	OfflinedAt int64  `json:"offlined_at"` // 下线时刻（unix 秒）
	Day        string `json:"day"`         // 下线日期（YYYYMMDD；"今日下线数"统计用）
}

// fileComment 落盘文件头注释（人工打开一眼知道这是什么）。
const fileComment = "号池轮换名单：被 rotation 下线的号（到龄 keep_hours 自动解除移除回池）"

type fileFormat struct {
	Comment   string  `json:"_comment,omitempty"`
	UpdatedAt int64   `json:"updated_at"`
	Entries   []Entry `json:"entries"`
}

// Store 轮换名单（并发安全；path 空 = 纯内存不落盘——测试/未装配场景）。
type Store struct {
	path string
	mu   sync.Mutex
	m    map[string]Entry
}

// NewStore 创建名单（path 通常为 <数据目录>/rotation.json；不自动读盘，由 Load 负责）。
func NewStore(path string) *Store {
	return &Store{path: path, m: map[string]Entry{}}
}

// Path 名单文件路径。
func (s *Store) Path() string { return s.path }

// Load 读回名单（文件不存在 = 首次运行，空名单；解析失败返回 error 并按空名单继续）。
// path 为空（纯内存）时无盘可读：no-op，返回当前内存条数。
func (s *Store) Load() (int, error) {
	if s.path == "" {
		return s.Count(), nil
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	var f fileFormat
	if err := json.Unmarshal(raw, &f); err != nil {
		return 0, fmt.Errorf("轮换名单 %s 解析失败: %w", s.path, err)
	}
	m := make(map[string]Entry, len(f.Entries))
	for _, e := range f.Entries {
		if e.Account == "" {
			continue
		}
		m[e.Account] = e
	}
	s.mu.Lock()
	s.m = m
	s.mu.Unlock()
	return len(m), nil
}

// Save 原子落盘（tmp + rename，避免半截文件）。
func (s *Store) Save() error {
	if s.path == "" {
		return nil
	}
	s.mu.Lock()
	list := make([]Entry, 0, len(s.m))
	for _, e := range s.m {
		list = append(list, e)
	}
	s.mu.Unlock()
	sort.Slice(list, func(i, j int) bool { return list[i].Account < list[j].Account })
	raw, err := json.MarshalIndent(fileFormat{
		Comment: fileComment, UpdatedAt: time.Now().Unix(), Entries: list,
	}, "", " ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(s.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Add 登记被下线的号（upsert：重复下线覆盖为最新时刻）；返回登记条数。
func (s *Store) Add(accounts []string, now time.Time) int {
	if len(accounts) == 0 {
		return 0
	}
	day := now.Format("20060102")
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, acc := range accounts {
		if acc == "" {
			continue
		}
		s.m[acc] = Entry{Account: acc, OfflinedAt: now.Unix(), Day: day}
		n++
	}
	return n
}

// Aged 返回"已到回池年龄"的号（age ≥ keep；升序）。keep<=0 → 全部视为到龄。
func (s *Store) Aged(now time.Time, keep time.Duration) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.m))
	for acc, e := range s.m {
		at := time.Unix(e.OfflinedAt, 0)
		if e.OfflinedAt <= 0 || keep <= 0 || !now.Before(at.Add(keep)) {
			out = append(out, acc)
		}
	}
	sort.Strings(out)
	return out
}

// Remove 从名单删除（回池后调；幂等）；返回删除条数。
func (s *Store) Remove(accounts []string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, acc := range accounts {
		if _, ok := s.m[acc]; ok {
			delete(s.m, acc)
			n++
		}
	}
	return n
}

// List 名单快照（升序）。
func (s *Store) List() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Entry, 0, len(s.m))
	for _, e := range s.m {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Account < out[j].Account })
	return out
}

// Count 名单条数。
func (s *Store) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}

// CountDay 名单里"某个日期下线"的条数（状态接口"今日下线数"用）。
func (s *Store) CountDay(day string) int {
	if day == "" {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.m {
		if e.Day == day {
			n++
		}
	}
	return n
}
