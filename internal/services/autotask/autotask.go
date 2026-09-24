// Package autotask 定时自动任务：把"该做但没在做"的号按**随机间隔**、**随机批量**拉起来。
//
// 口径对齐参考实现 robot/ctrlcenter/routers/auto_onboard.py：
//   - 每 N 分钟（我们默认 0~5 分钟随机）从账号池挑号 → 上线 → 启动任务；
//   - **各套策略互相独立**：newbie（新手链）/ ghost（抓鬼）/ hatch（孵化）/ shenbu（大唐神捕），
//     各自启停、各自参数；
//   - 没号可挑且开了"自动注册"→ 注册新号（下一轮自然被挑到）；
//   - 有"同时在线上限"（max_online）：达到上限就等空槽，不硬拉。
//
// 与手动「启动」的区别：这里只做"挑号 + 上线 + 下发"，判据（该不该跑某条链）由壳层
// 通过 Candidates 提供（复用意图判定 + 抓鬼等级门槛），避免两套逻辑漂移。
//
// 设计：Tick 是**纯决策**（不 sleep、可用假时钟反复测），Run 才真正跑循环；所有外部
// 动作（上线/注册/下发）都通过 Deps 注入，壳层（api）负责落到真实通道。
package autotask

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Kind 策略种类（各套策略完全独立）。
type Kind string

const (
	KindNewbie Kind = "newbie" // 新手链
	KindGhost  Kind = "ghost"  // 抓鬼
	KindHatch  Kind = "hatch"  // 孵化（坐骑蛋/元气蛋：抓鬼已满的有蛋号去孵化图游荡打暗雷）
	// KindShenbu 大唐神捕（分享日常体系：2026-09-23 方案 §4.3；命令 share_daily_start，
	// 与抓鬼同构 —— 载荷在发送时由中控组装：基座 newbie_full + shenbu_nav 声明）。
	KindShenbu Kind = "shenbu"
)

// Kinds 固定顺序（面板与日志都用它，保证输出稳定）。
var Kinds = []Kind{KindNewbie, KindGhost, KindHatch, KindShenbu}

// Valid 是否是受支持的策略。
func (k Kind) Valid() bool {
	return k == KindNewbie || k == KindGhost || k == KindHatch || k == KindShenbu
}

// Label 中文名（日志/面板用）。
func (k Kind) Label() string {
	switch k {
	case KindNewbie:
		return "新手链"
	case KindGhost:
		return "抓鬼"
	case KindHatch:
		return "孵化"
	case KindShenbu:
		return "大唐神捕"
	}
	return string(k)
}

// DefaultLaunchDelay 上线后等多久再下发任务（参考实现 _reghost_later 是 8 秒：
// 太早发会被机器人当"未登录缓冲"丢掉或直接失败）。
const DefaultLaunchDelay = 8 * time.Second

// DefaultIntervalSec 默认基础间隔（5 分钟）。
const DefaultIntervalSec = 300

// Candidate 一个"该做但没在做"的候选号（由壳层判定，附判据）。
type Candidate struct {
	Account string `json:"account"`
	Online  bool   `json:"online"` // 已在线 → 只需下发任务（不用 add）
	Level   int    `json:"level,omitempty"`
	Reason  string `json:"reason,omitempty"` // 为什么挑它（面板显示）
	// Priority 该候选本轮**优先挑**（2026-09-23 领双号优先抓鬼）：只要它还是候选，
	// 就先于普通候选被选中（数量不够才用普通候选补足）。随机洗牌仍在同优先级内生效。
	Priority bool `json:"priority,omitempty"`
}

// Config 一个策略的配置（面板 start 时传入；运行中可再次 Start 覆盖）。
type Config struct {
	Kind            Kind `json:"kind"`
	IntervalSec     int  `json:"interval_sec"`     // 基础间隔（秒）
	JitterSec       int  `json:"jitter_sec"`       // 随机增量 0~JitterSec（"5 分钟之内随机"= 0 + 300）
	BatchMin        int  `json:"batch_min"`        // 每轮最少挑几个
	BatchMax        int  `json:"batch_max"`        // 每轮最多挑几个
	TargetOnline    int  `json:"target_online"`    // **保持在线数**（0=不限；每轮只补差额）
	MaxOnline       int  `json:"max_online"`       // 硬上限（0=不限）：到顶就等空槽，不硬拉
	RegisterEnabled bool `json:"register_enabled"` // 没号可挑时自动注册
	RegisterCount   int  `json:"register_count"`   // 每次注册几个
	LaunchDelaySec  int  `json:"launch_delay_sec"` // 上线后等几秒再下发（0=默认 8s）
	// MaxMinutes 单次动作的时长上限（**目前只有孵化用**）：到期由壳层下发 hatch_stop 收工。
	// 0 = 用默认（DefaultHatchMinutes）。
	MaxMinutes int `json:"max_minutes,omitempty"`
	// MinLevel 该策略的最低等级门槛（0 = 未配置，回落全局/默认口径）。
	// 2026-09-23 前端口径：**shenbu 用**（默认 40，服务端票条件；覆盖全局
	// CTRL_SHARE_DAILY_MIN_LEVEL）。只影响自动派发（候选/补发闸），不影响手动「启动」。
	MinLevel int `json:"min_level,omitempty"`
	// BalanceGate 余额闸（0 = 不启用）：**仅 shenbu 用** —— 候选/派发时过滤
	// `money > 0 且 money < gate` 的号（防"传送费不够 → 派了又停"）。
	// 前端面板默认 1000（开元通宝/现金口径）；余额未知（未上报=0）不拦。
	BalanceGate int `json:"balance_gate,omitempty"`
}

// DefaultHatchMinutes 孵化单次时长上限（分钟）：打满 30 点灵气约 38 场暗雷，
// 现场踩雷速率下 1 小时内通常够；到期中控下发 hatch_stop（蛋留在装备栏，下次可续）。
const DefaultHatchMinutes = 60

// WithDefaults 补默认值（面板只传关心的字段也能用）。
func (c Config) WithDefaults() Config {
	if c.IntervalSec <= 0 {
		c.IntervalSec = DefaultIntervalSec
	}
	if c.JitterSec < 0 {
		c.JitterSec = 0
	}
	if c.BatchMin <= 0 {
		c.BatchMin = 1
	}
	if c.BatchMax <= 0 {
		c.BatchMax = 3
	}
	if c.BatchMax < c.BatchMin {
		c.BatchMax = c.BatchMin
	}
	if c.RegisterCount <= 0 {
		c.RegisterCount = 10
	}
	if c.LaunchDelaySec < 0 {
		c.LaunchDelaySec = 0
	}
	if c.MaxMinutes <= 0 {
		c.MaxMinutes = DefaultHatchMinutes
	}
	return c
}

// Round 一轮的记录（面板"最近几轮做了什么"）。
type Round struct {
	At         time.Time `json:"at"`
	Picked     []string  `json:"picked,omitempty"`     // 本轮挑中的号
	Online     []string  `json:"online,omitempty"`     // 其中需要（已）上线的
	Registered []string  `json:"registered,omitempty"` // 本轮注册的新号
	Msg        string    `json:"msg"`                  // 一句话说明
}

// State 一个策略的运行态（面板读）。
type State struct {
	Kind       Kind      `json:"kind"`
	Label      string    `json:"label"`
	Config     Config    `json:"config"`
	Enabled    bool      `json:"enabled"`
	NextAt     time.Time `json:"next_at,omitempty"`
	LastMsg    string    `json:"last_msg"`
	Online     int       `json:"online"`     // 当前该策略在线数（壳层实时算）
	Target     int       `json:"target"`     // 目标在线数（=配置）
	Deficit    int       `json:"deficit"`    // 还差几个才到目标（<=0 表示已达标）
	Picked     int       `json:"picked"`     // 累计拉起（号次）
	Registered int       `json:"registered"` // 累计注册
	Rounds     []Round   `json:"rounds"`     // 最近若干轮（新→旧）
}

// Deps 依赖注入（壳层提供真实实现；测试注入假的）。
//
// **回调与锁的契约（2026-09-24 生产事故后明确）**：Candidates / OnlineCount 在 Runner
// 持有内部锁（r.mu）时被调用 —— 实现里**禁止**回调 Runner 的任何方法
// （States / Start / Stop / RunNow / NoteRegister …）：Go 的 sync.Mutex 不可重入，
// 同 goroutine 重入即永久死锁（现场：shenbu 候选判定里读 States()，把 AUTOTASK 轮询、
// RESTORE 恢复引擎、/api/autotask 全部拖死）。需要本策略配置时用入参 cfg，不要回读。
// 其余回调（Online / Register / Launch / Tick / Log）都在**锁外**调用，没有此限制。
type Deps struct {
	Now  func() time.Time
	Rand func(n int) int // 返回 [0,n)；n<=0 → 0
	// Candidates "该做但没在做"的号（判据由壳层给；**持锁调用，禁止回读 Runner**，cfg 为本策略当前配置）。
	Candidates  func(kind Kind, cfg Config) []Candidate
	OnlineCount func(kind Kind) int                          // 该策略当前在线数（MaxOnline 用；**持锁调用，禁止回读 Runner**）
	Online      func(kind Kind, accs []string) (int, error)  // 上线（机器人 add）；返回成功数
	Register    func(kind Kind, count int) ([]string, error) // 自动注册；返回**计划/已建**的号（应尽快返回）
	Launch      func(kind Kind, accs []string) bool          // 下发任务命令
	// Tick 每轮回调（Run 每 5 秒一次；**与策略是否启用无关**）：壳层用它做"有时长的会话"的
	// 到期收工/完成清理（当前是孵化：max_minutes 到期下发 hatch_stop）。可为 nil。
	Tick func(now time.Time)
	Log  func(format string, args ...any)
}

const maxRounds = 12 // 面板只留最近 12 轮

type state struct {
	cfg     Config
	enabled bool
	nextAt  time.Time
	lastMsg string
	picked  int
	reged   int
	rounds  []Round

	// 上线后延迟下发的队列（一次只排一个策略的一条，够用）
	queueKind Kind
	queue     []string
	queueAt   time.Time
	queueNote string
}

// Runner 定时任务执行器（并发安全）。
type Runner struct {
	d  Deps
	mu sync.Mutex
	st map[Kind]*state
	// on 各策略启用状态的**无锁镜像**（2026-09-24 新增）：Start/Stop 在持 r.mu 时一并
	// Store；EnabledNoLock 只做 atomic.Load —— 供 Runner **持锁回调**（Deps.Candidates /
	// Deps.OnlineCount）判断"某池当前是否启用"而不触发锁重入（那些回调里调 States() 会
	// 同 goroutine 锁重入死锁，见 Deps 契约与 2026-09-24 事故报告）。map 在 New 之后只读。
	on map[Kind]*atomic.Bool
}

// New 创建执行器（各套策略默认都是"未启动"）。
func New(d Deps) *Runner {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Rand == nil {
		d.Rand = func(n int) int { return 0 }
	}
	if d.Log == nil {
		d.Log = func(string, ...any) {}
	}
	r := &Runner{d: d, st: map[Kind]*state{}, on: map[Kind]*atomic.Bool{}}
	for _, k := range Kinds {
		r.st[k] = &state{cfg: Config{Kind: k}.WithDefaults()}
		r.on[k] = &atomic.Bool{}
	}
	return r
}

// Start 启动（或改参数后重启）某个策略；会**立即跑一轮**（参考实现 next_at=now 同口径）。
func (r *Runner) Start(kind Kind, cfg Config) error {
	if !kind.Valid() {
		return fmt.Errorf("未知策略: %s（只支持 newbie / ghost / hatch / shenbu）", kind)
	}
	cfg.Kind = kind
	cfg = cfg.WithDefaults()
	r.mu.Lock()
	st := r.st[kind]
	st.cfg = cfg
	st.enabled = true
	st.nextAt = r.d.Now()
	st.lastMsg = kind.Label() + "定时任务已启动"
	if b := r.on[kind]; b != nil {
		b.Store(true) // 无锁镜像（与 st.enabled 同临界区更新，见 Runner.on 注释）
	}
	r.mu.Unlock()
	r.d.Log("[AUTOTASK] %s 定时任务启动（间隔 %ds + 随机 %ds，每轮 %d~%d 个，上限 %d，自动注册=%v×%d）",
		kind.Label(), cfg.IntervalSec, cfg.JitterSec, cfg.BatchMin, cfg.BatchMax, cfg.MaxOnline,
		cfg.RegisterEnabled, cfg.RegisterCount)
	return nil
}

// Stop 停止某个策略（已在队列里的"延迟下发"也一起撤掉，避免"停了又被拉起"）。
func (r *Runner) Stop(kind Kind) {
	r.mu.Lock()
	st := r.st[kind]
	st.enabled = false
	st.lastMsg = kind.Label() + "定时任务已停止"
	if st.queueKind == kind {
		st.queue, st.queueKind, st.queueNote = nil, "", ""
	}
	if b := r.on[kind]; b != nil {
		b.Store(false) // 无锁镜像（见 Runner.on 注释）
	}
	r.mu.Unlock()
	r.d.Log("[AUTOTASK] %s 定时任务已停止", kind.Label())
}

// RunNow 让某个策略下一轮立刻执行（面板「立即跑一轮」）。
func (r *Runner) RunNow(kind Kind) {
	r.mu.Lock()
	if st, ok := r.st[kind]; ok {
		st.nextAt = r.d.Now()
	}
	r.mu.Unlock()
}

// NoteRegister 通知"上一轮自动注册的结果"（壳层在后台注册完成后调）：
// 一个都没成功（多半是被同 IP 风控挡了）→ **长退避 30 分钟**，别把风控撞死。
func (r *Runner) NoteRegister(kind Kind, created int) {
	if !kind.Valid() {
		return
	}
	now := r.d.Now()
	r.mu.Lock()
	st := r.st[kind]
	if created <= 0 {
		st.nextAt = now.Add(30 * time.Minute)
	}
	r.mu.Unlock()
	if created <= 0 {
		r.record(kind, Round{At: now, Msg: "自动注册一个都没成功（可能被同 IP 风控），30 分钟后再试"})
	}
}

// EnabledNoLock 该策略当前是否启用（**无锁**：只做 atomic.Load，不取 r.mu、不调用任何回调）。
//
// 供 Runner **持锁回调**（Deps.Candidates / Deps.OnlineCount）查询"某池当前是否启用"用：
// 那些回调里调 States() 会锁重入死锁（2026-09-24 生产事故），本方法可安全调用 —— atomic
// Load 不阻塞、不参与 r.mu 的所有权，读到的值是 Start/Stop 某一次写入的结果（seq-cst），
// 与启停并发时最多"瞬时过期"一次调度周期，只影响一次决策，不影响正确性。
func (r *Runner) EnabledNoLock(kind Kind) bool {
	if r == nil {
		return false
	}
	b := r.on[kind] // map 在 New 之后只读，并发读安全；未登记的 kind → nil
	return b != nil && b.Load()
}

// States 运行态快照（面板用；新→旧排列轮次）。
func (r *Runner) States() map[Kind]State {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[Kind]State, len(r.st))
	for k, s := range r.st {
		rounds := make([]Round, len(s.rounds))
		copy(rounds, s.rounds)
		cur := 0
		if r.d.OnlineCount != nil {
			cur = r.d.OnlineCount(k)
		}
		out[k] = State{
			Kind: k, Label: k.Label(), Config: s.cfg, Enabled: s.enabled,
			NextAt: s.nextAt, LastMsg: s.lastMsg, Online: cur, Target: s.cfg.TargetOnline,
			Deficit: s.cfg.TargetOnline - cur, Picked: s.picked, Registered: s.reged,
			Rounds: rounds,
		}
	}
	return out
}

// Tick 算一轮（纯决策；返回本轮发生的记录，供测试与日志用）。
func (r *Runner) Tick(now time.Time) []Round {
	if r.d.Tick != nil {
		r.d.Tick(now) // 壳层的周期性维护（孵化到期收工等）——与策略启停无关，先跑
	}
	out := []Round{}
	for _, kind := range Kinds {
		if rd := r.tickKind(kind, now); rd != nil {
			out = append(out, *rd)
		}
	}
	return out
}

func (r *Runner) tickKind(kind Kind, now time.Time) *Round {
	r.mu.Lock()
	st := r.st[kind]
	if !st.enabled {
		r.mu.Unlock()
		return nil
	}
	// ① 先处理"上线后延迟下发"的队列（等机器人把角色数据就绪）
	if st.queueKind == kind && len(st.queue) > 0 && !now.Before(st.queueAt) {
		accs, note := st.queue, st.queueNote
		st.queue, st.queueKind, st.queueNote = nil, "", ""
		r.mu.Unlock()
		ok := true
		if r.d.Launch != nil {
			ok = r.d.Launch(kind, accs)
		}
		rd := Round{At: now, Picked: accs, Msg: fmt.Sprintf("%s（%s）", launchMsg(kind, accs, ok), note)}
		r.record(kind, rd)
		return &rd
	}
	// ② 没到点就跳过
	if now.Before(st.nextAt) {
		r.mu.Unlock()
		return nil
	}
	cfg := st.cfg

	cur := 0
	if r.d.OnlineCount != nil {
		cur = r.d.OnlineCount(kind)
	}
	// ③ 硬上限（参考实现：达到上限就等空槽，15 秒后再看）
	if cfg.MaxOnline > 0 && cur >= cfg.MaxOnline {
		msg := fmt.Sprintf("已达同时在线上限 %d（在跑 %d），等空槽再补", cfg.MaxOnline, cur)
		st.lastMsg, st.nextAt = msg, now.Add(15*time.Second)
		r.mu.Unlock()
		return nil
	}
	// ③.5 保持数：**保持在跑 X 个**（0=不限）；已达标就等下一轮再看，不重复拉起
	if cfg.TargetOnline > 0 && cur >= cfg.TargetOnline {
		st.lastMsg = fmt.Sprintf("在跑 %d / 目标 %d（已达标，暂不补）", cur, cfg.TargetOnline)
		st.nextAt = now.Add(r.interval(cfg))
		r.mu.Unlock()
		return nil
	}

	// ④ 挑号（随机批量）
	cands := []Candidate{}
	if r.d.Candidates != nil {
		cands = r.d.Candidates(kind, cfg) // 持锁回调：实现不得回读 Runner（见 Deps 契约）
	}
	if len(cands) == 0 {
		st.nextAt = now.Add(r.interval(cfg))
		if cfg.RegisterEnabled && r.d.Register != nil {
			st.lastMsg = fmt.Sprintf("没号可拉，注册 %d 个新号", cfg.RegisterCount)
			r.mu.Unlock()
			names, err := r.d.Register(kind, cfg.RegisterCount)
			rd := Round{At: now, Msg: fmt.Sprintf("没号可拉：已发起注册 %d 个", cfg.RegisterCount)}
			if err != nil {
				rd.Msg = "自动注册失败: " + err.Error()
			} else if len(names) > 0 {
				rd.Registered = names
				rd.Msg = fmt.Sprintf("没号可拉：已发起注册 %d 个（%s…）", len(names), names[0])
			}
			r.record(kind, rd)
			return &rd
		}
		st.lastMsg = "没号可拉（未开自动注册）"
		r.mu.Unlock()
		return nil
	}

	n := cfg.BatchMin
	if cfg.BatchMax > cfg.BatchMin {
		n += r.d.Rand(cfg.BatchMax - cfg.BatchMin + 1)
	}
	if cfg.TargetOnline > 0 { // 按目标只补差额（例：在跑 47 / 目标 50 → 本轮最多补 3 个）
		if d := cfg.TargetOnline - cur; n > d {
			n = d
		}
	}
	if n < 1 {
		n = 1
	}
	if n > len(cands) {
		n = len(cands)
	}
	picked := pickBatch(cands, n, r.d.Rand)

	needOnline := make([]string, 0, len(picked))
	all := make([]string, 0, len(picked))
	prioN := 0
	for _, c := range picked {
		all = append(all, c.Account)
		if c.Priority {
			prioN++ // 今日领双 → 优先抓鬼（面板日志可见）
		}
		if !c.Online {
			needOnline = append(needOnline, c.Account)
		}
	}
	st.picked += len(picked)
	st.lastMsg = fmt.Sprintf("本轮挑中 %d 个（在跑 %d%s）：%s%s", len(picked), cur, targetTxt(cfg),
		strings.Join(all, ", "), prioTxt(prioN))
	st.nextAt = now.Add(r.interval(cfg))
	delay := time.Duration(cfg.LaunchDelaySec) * time.Second
	if cfg.LaunchDelaySec == 0 {
		delay = DefaultLaunchDelay
	}
	if len(needOnline) > 0 {
		// 先上线，等角色数据就绪再下发（否则命令会被当"未登录"丢掉）
		st.queueKind, st.queue, st.queueAt = kind, all, now.Add(delay)
		st.queueNote = fmt.Sprintf("其中 %d 个需要上线，%s 后下发", len(needOnline), delay)
		r.mu.Unlock()
		sent := 0
		if r.d.Online != nil {
			if n, err := r.d.Online(kind, needOnline); err == nil {
				sent = n
			} else {
				r.d.Log("[AUTOTASK] %s 上线失败: %v", kind.Label(), err)
			}
		}
		rd := Round{At: now, Picked: all, Online: needOnline,
			Msg: fmt.Sprintf("挑中 %d 个（上线 %d/%d），%s 后下发任务%s",
				len(all), sent, len(needOnline), delay, prioTxt(prioN))}
		r.record(kind, rd)
		return &rd
	}
	r.mu.Unlock()
	ok := true
	if r.d.Launch != nil {
		ok = r.d.Launch(kind, all)
	}
	rd := Round{At: now, Picked: all, Msg: launchMsg(kind, all, ok) + prioTxt(prioN)}
	r.record(kind, rd)
	return &rd
}

// prioTxt 轮次文案后缀：本轮含"今日领双优先"的号时标注（面板可追溯"领双号优先抓鬼"）。
func prioTxt(n int) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf("（含今日领双优先 %d 个）", n)
}

func (r *Runner) interval(cfg Config) time.Duration {
	j := 0
	if cfg.JitterSec > 0 {
		j = r.d.Rand(cfg.JitterSec + 1)
	}
	return time.Duration(cfg.IntervalSec+j) * time.Second
}

func (r *Runner) record(kind Kind, rd Round) {
	r.mu.Lock()
	st := r.st[kind]
	st.rounds = append([]Round{rd}, st.rounds...)
	if len(st.rounds) > maxRounds {
		st.rounds = st.rounds[:maxRounds]
	}
	st.reged += len(rd.Registered)
	st.lastMsg = rd.Msg
	r.mu.Unlock()
	r.d.Log("[AUTOTASK] %s %s", kind.Label(), rd.Msg)
}

// Run 周期 Tick（ctx 结束即退出）。
func (r *Runner) Run(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
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

func launchMsg(kind Kind, accs []string, ok bool) string {
	if !ok {
		return fmt.Sprintf("下发%s任务失败（机器人通道未连接）: %s", kind.Label(), strings.Join(accs, ", "))
	}
	return fmt.Sprintf("已下发%s任务: %s", kind.Label(), strings.Join(accs, ", "))
}

// pickBatch 批量挑 n 个：**Priority 候选先挑**，不够再用普通候选随机补足。
//
// 2026-09-23 领双号优先抓鬼（用户口径"有领双的必须优先抓鬼"）：双倍有时长，领了要尽快
// 消耗掉；壳层把"今日已领双倍"的抓鬼候选标 Priority，这里保证它们不被随机洗牌挤掉。
// 结果按账号升序（稳定，便于日志/测试对照）。
func pickBatch(cands []Candidate, n int, rnd func(int) int) []Candidate {
	if n <= 0 || len(cands) == 0 {
		return nil
	}
	prio := make([]Candidate, 0, len(cands))
	rest := make([]Candidate, 0, len(cands))
	for _, c := range cands {
		if c.Priority {
			prio = append(prio, c)
		} else {
			rest = append(rest, c)
		}
	}
	out := make([]Candidate, 0, n)
	if len(prio) > 0 {
		out = append(out, pickRandom(prio, n, rnd)...)
	}
	if len(out) < n && len(rest) > 0 {
		out = append(out, pickRandom(rest, n-len(out), rnd)...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Account < out[j].Account })
	return out
}

// pickRandom 随机取 n 个（不改原切片；rand 注入便于测试）。
func pickRandom(cands []Candidate, n int, rand func(int) int) []Candidate {
	if n >= len(cands) {
		out := make([]Candidate, len(cands))
		copy(out, cands)
		sort.Slice(out, func(i, j int) bool { return out[i].Account < out[j].Account })
		return out
	}
	idx := make([]int, len(cands))
	for i := range idx {
		idx[i] = i
	}
	// Fisher–Yates 部分洗牌
	for i := 0; i < n; i++ {
		j := i + rand(len(cands)-i)
		idx[i], idx[j] = idx[j], idx[i]
	}
	out := make([]Candidate, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, cands[idx[i]])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Account < out[j].Account })
	return out
}

// targetTxt 目标文案（供日志/轮次显示）。
func targetTxt(cfg Config) string {
	if cfg.TargetOnline <= 0 {
		return ""
	}
	return fmt.Sprintf("/目标 %d", cfg.TargetOnline)
}
