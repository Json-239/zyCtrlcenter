// Package waterline 「在线人数水位保持器」：把**当前区**的在线人数维持在目标值附近
// （超了就压号下线、不足就从号池拉号补齐）。
//
// 口径（2026-09-22 用户拍板，勿改）：
//   - 人数 = **服务端全服在线（含真实玩家）**：机器人定时发 `@online` 管理命令，服务端回执
//     解析成 svr_online{count,ts} 随心跳上报（见 state.SvrOnlineLatest）。该读数**可能为空/过期**
//     （生产服没授权时为空）→ 兜底用我们自己的握手数（state.Counts 的 handshake），
//     并在状态里标注来源 source=svr|local 与新鲜度 fresh（面板必须显示来源，别把兜底当真值）。
//   - 不足 → 从号池拉号上线（走机器人端 `robot_manage add`，密码只从池里按区取）；
//     超出 → 断**空闲**的号；正在干活的（抓鬼会话/任务/战斗/游荡/**交付中 SUBMIT**）与
//     **异常号（ERROR 卡住/停链）绝不硬断**，标记「待下线」，等它回到空闲态再由下一轮断掉。
//   - 节奏：每 IntervalSec 秒一轮；|diff| < DeadZone（死区）不动；每轮最多调 MaxStep 个。
//   - 只对**当前区**生效（壳层传入的机器人快照已按当前区过滤）。
//
// 架构约定（与 internal/services/autotask、reghost 同款）：本包只做**纯决策**
// （ComputeAdjust / PickOffline / PickOnlineCandidates 是纯函数，便于单测），
// 真实动作（取数、上下线）由壳层（internal/api）通过 Deps 注入。
//
// 纯函数命名说明：Go 外部测试包只能调导出符号，故导出为
// ComputeAdjust / PickOffline / PickOnlineCandidates（对应任务书里的 computeAdjust 等）。
package waterline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"zyctrlcenter/internal/state"
)

// 默认参数（2026-09-22 用户拍板；落盘后可覆盖）。
const (
	DefaultTarget      = 100 // 目标在线人数
	DefaultDeadZone    = 3   // 死区：|diff| < 3 不动
	DefaultMaxStep     = 5   // 每轮最多调整个数
	DefaultIntervalSec = 60  // 检查间隔（秒）

	// SvrStaleSec 服务端读数（svr_online 的 age_sec）超过这个秒数算**过期** → 用本地握手数兜底。
	SvrStaleSec = 180

	// InflightTTLSec 「已下发上线但还没登录」的记账时效：超过它就不再算"预期会到"的号，
	// 免得拉起失败后永远不再补号（机器人登录本来就要几十秒：记账太短会重复补、太长会僵住）。
	InflightTTLSec = 300
)

// 数据来源（面板必须显示，别把本地兜底读成"全服在线"）。
const (
	SourceSvr   = "svr"   // 服务端全服在线（含真实玩家）
	SourceLocal = "local" // 本地兜底：我们自己的握手数
)

// 参数范围（Validate 的校验口径，与 POST /api/waterline 一致）。
const (
	MaxTarget      = 10000
	MaxDeadZone    = 1000
	MaxMaxStep     = 50
	MinIntervalSec = 10
	MaxIntervalSec = 3600
)

// Config 保持器参数（落盘 data/waterline.json，重启后保留；默认 enabled=false）。
type Config struct {
	Enabled        bool `json:"enabled"`         // 总开关（false 时循环什么都不做）
	Target         int  `json:"target"`          // 目标在线人数
	DeadZone       int  `json:"dead_zone"`       // 死区：|target-cur| < dead_zone 不动
	MaxStep        int  `json:"max_step"`        // 每轮最多调整个数
	IntervalSec    int  `json:"interval_sec"`    // 检查间隔（秒）
	PreferIdle     bool `json:"prefer_idle"`     // 优先断空闲号（false = 不挑空闲，随机补选）
	RandomFallback bool `json:"random_fallback"` // 空闲号不够时随机补选（false = 只压空闲号）
	MinKeep        int  `json:"min_keep"`        // 保底：我们自己的号不低于这个数（0 = 不保底）
	// AllowLocal 2026-09-22 安全闸：口径是"含真人总数"时，服务端读数不可用（用本地兜底）
	// 就**不该按本地数**调整（否则会把我们的号顶到 target，真人一多就超调）。默认 false = 只认服务端口径。
	AllowLocal bool `json:"allow_local"`
}

// DefaultConfig 默认参数：**enabled=false**，必须人工开（不会自己动生产）。
func DefaultConfig() Config {
	return Config{
		Enabled:        false,
		Target:         DefaultTarget,
		DeadZone:       DefaultDeadZone,
		MaxStep:        DefaultMaxStep,
		IntervalSec:    DefaultIntervalSec,
		PreferIdle:     true,
		RandomFallback: true,
		MinKeep:        0,
	}
}

// Normalize 把参数夹到合法范围（读盘/设置时都过一遍，脏配置不会引发异常动作）。
func (c Config) Normalize() Config {
	if c.Target < 0 {
		c.Target = 0
	}
	if c.Target > MaxTarget {
		c.Target = MaxTarget
	}
	if c.DeadZone < 0 {
		c.DeadZone = 0
	}
	if c.DeadZone > MaxDeadZone {
		c.DeadZone = MaxDeadZone
	}
	if c.MaxStep < 1 {
		c.MaxStep = 1
	}
	if c.MaxStep > MaxMaxStep {
		c.MaxStep = MaxMaxStep
	}
	if c.IntervalSec < MinIntervalSec {
		c.IntervalSec = MinIntervalSec
	}
	if c.IntervalSec > MaxIntervalSec {
		c.IntervalSec = MaxIntervalSec
	}
	if c.MinKeep < 0 {
		c.MinKeep = 0
	}
	return c
}

// Validate 校验参数范围（写接口用；不通过就**不落盘、不生效**）。
func (c Config) Validate() error {
	switch {
	case c.Target < 0 || c.Target > MaxTarget:
		return fmt.Errorf("target 必须在 0~%d 之间", MaxTarget)
	case c.DeadZone < 0 || c.DeadZone > MaxDeadZone:
		return fmt.Errorf("dead_zone 必须 ≥0 且 ≤%d", MaxDeadZone)
	case c.MaxStep < 1 || c.MaxStep > MaxMaxStep:
		return fmt.Errorf("max_step 必须在 1~%d 之间", MaxMaxStep)
	case c.IntervalSec < MinIntervalSec || c.IntervalSec > MaxIntervalSec:
		return fmt.Errorf("interval_sec 必须在 %d~%d 之间", MinIntervalSec, MaxIntervalSec)
	case c.MinKeep < 0:
		return errors.New("min_keep 必须 ≥0")
	}
	return nil
}

// Candidate 一个"可上线"候选（壳层从账号池 + 运行时状态摊平；本包只做过滤）。
type Candidate struct {
	Account   string
	Usable    bool // 该区已验证可用（或运行时在线）
	Removed   bool // 被用户/其它模块标记移除（心跳不复活）——我们自己压下线的号由壳层豁免
	Online    bool // 运行时已在线
	Busy      bool // 在忙（有任务/抓鬼会话/战斗/游荡）
	Paused    bool // 人工暂停（R3，面板点过「停止」）→ 不自动拉起
	Level     int
	ChainDone bool
}

// Deps 依赖（壳层提供真实世界；测试注入假的）。
type Deps struct {
	Now  func() time.Time
	Rand func(n int) int // [0,n) 随机数（随机补选用；nil = math/rand）
	// Robots 当前区机器人的状态快照（挑"断谁"用）。
	Robots func() []state.Robot
	// SvrOnline 最近一次"全服在线（含真人）"读数：count、时间戳(ms)、有没有读数。
	SvrOnline func() (count int, tsMS float64, ok bool)
	// Local 本地兜底读数：我们自己的握手数（state.Counts 的 handshake）。
	Local func() int
	// Candidates 可上线候选（账号池里该区可用的号）。
	Candidates func() []Candidate
	// Online 下发上线（返回**实际下发成功**的账号；失败返回 err）。
	Online func(accounts []string) ([]string, error)
	// Offline 下发下线（返回**实际下发成功**的账号；失败返回 err）。
	Offline func(accounts []string) ([]string, error)
	Log     func(format string, args ...any)
}

// Status 保持器状态（面板 / GET /api/waterline 展示）。
type Status struct {
	Enabled        bool     `json:"enabled"`
	Target         int      `json:"target"`
	DeadZone       int      `json:"dead_zone"`
	MaxStep        int      `json:"max_step"`
	IntervalSec    int      `json:"interval_sec"`
	PreferIdle     bool     `json:"prefer_idle"`
	RandomFallback bool     `json:"random_fallback"`
	MinKeep        int      `json:"min_keep"`
	AllowLocal     bool     `json:"allow_local"` // 见 Config.AllowLocal（默认 false）
	Cur            int      `json:"cur"`         // 本轮生效读数
	Source         string   `json:"source"`      // svr（含真人）| local（本地兜底）
	Fresh          bool     `json:"fresh"`       // 服务端读数是否新鲜（false = 用的本地兜底/过期值）
	Local          int      `json:"local"`       // 本地握手数
	Svr            int      `json:"svr"`         // 服务端读数（0 = 还没查到）
	SvrAgeSec      int      `json:"svr_age_sec"` // 服务端读数距今秒数（-1 = 没有读数）
	Diff           int      `json:"diff"`        // target - cur（正 = 要补号，负 = 要压号）
	Pending        []string `json:"pending"`     // 待下线（在忙，等任务收尾）
	InFlight       []string `json:"inflight"`    // 已下发上线、还没登录的号
	LastAction     string   `json:"last_action"` // 最近一次动作/判决说明
	LastRunTS      int64    `json:"last_run_ts"` // 最近一轮时间（unix 秒；0 = 还没跑过）
	LastErr        string   `json:"last_err,omitempty"`
}

// Keeper 水位保持器（并发安全）。
type Keeper struct {
	path string
	d    Deps

	mu       sync.Mutex
	cfg      Config
	lastRun  time.Time
	lastAct  string
	lastErr  string
	pending  []string             // 待下线：在忙，等收工
	inflight map[string]time.Time // 已下发上线、还没登录（防重复补号）
	selfOff  map[string]bool      // 我们自己压下线的号（供壳层豁免 IsRemoved 过滤）
}

// New 创建保持器（path 通常为 <数据目录>/waterline.json；不自动读盘，由 Load 负责）。
func New(path string, d Deps) *Keeper {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Log == nil {
		d.Log = func(string, ...any) {}
	}
	if d.Rand == nil {
		d.Rand = func(n int) int {
			if n <= 0 {
				return 0
			}
			return rand.Intn(n)
		}
	}
	return &Keeper{
		path:     path,
		d:        d,
		cfg:      DefaultConfig(),
		inflight: map[string]time.Time{},
		selfOff:  map[string]bool{},
	}
}

// Path 配置文件路径。
func (k *Keeper) Path() string { return k.path }

// ---------------------------------------------------------------- 持久化

// Load 读回上次的参数（不存在 = 首次运行，用默认值：enabled=false）。
//
// 先在默认值上解 JSON：老文件/手工写残的文件缺哪个字段就用默认值补哪个，
// **不会**因为缺 "target" 就把目标当成 0（那等于"把号全断"）。
func (k *Keeper) Load() error {
	raw, err := os.ReadFile(k.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	cfg := DefaultConfig()
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("水位配置 %s 解析失败: %w", k.path, err)
	}
	cfg = cfg.Normalize()
	k.mu.Lock()
	k.cfg = cfg
	k.mu.Unlock()
	k.logf("[WATERLINE] 已恢复参数：enabled=%v target=%d 死区=%d 单轮≤%d 间隔=%ds",
		cfg.Enabled, cfg.Target, cfg.DeadZone, cfg.MaxStep, cfg.IntervalSec)
	return nil
}

// save 原子落盘（tmp + rename，避免写一半的文件把参数读坏）。
func (k *Keeper) save() {
	k.mu.Lock()
	cfg := k.cfg
	k.mu.Unlock()
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		k.logf("[WATERLINE] 参数序列化失败: %v", err)
		return
	}
	if dir := filepath.Dir(k.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			k.logf("[WATERLINE] 参数落盘失败（建目录）: %v", err)
			return
		}
	}
	tmp := k.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		k.logf("[WATERLINE] 参数落盘失败: %v", err)
		return
	}
	if err := os.Rename(tmp, k.path); err != nil {
		k.logf("[WATERLINE] 参数落盘失败（rename）: %v", err)
	}
}

// Config 当前参数快照。
func (k *Keeper) Config() Config {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.cfg
}

// SetConfig 设置参数。**严格校验**（写接口口径：范围不对就返回 error，不落盘、不生效）；
// 校验通过后再夹一遍（等价无操作，防御性）。
func (k *Keeper) SetConfig(c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	c = c.Normalize()
	k.mu.Lock()
	k.cfg = c
	k.mu.Unlock()
	k.save()
	k.logf("[WATERLINE] 参数已更新：enabled=%v target=%d 死区=%d 单轮≤%d 间隔=%ds 空闲优先=%v 随机补选=%v 保底=%d",
		c.Enabled, c.Target, c.DeadZone, c.MaxStep, c.IntervalSec, c.PreferIdle, c.RandomFallback, c.MinKeep)
	return nil
}

// SelfOfflined 该号是不是"我们自己压下去的"（壳层据此豁免 IsRemoved 过滤：目标上调时可以再拉回来）。
func (k *Keeper) SelfOfflined(account string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.selfOff[account]
}

// ---------------------------------------------------------------- 纯函数（便于单测）

// ComputeAdjust 本轮要调整的数量：正 = 上线，负 = 下线，0 = 不动。
//
//	diff = target - cur；|diff| < deadZone（死区）→ 0；否则按 maxStep 限幅。
//	maxStep <= 0 视为"不动"（防呆：配置坏了也不许无限量动作）。
func ComputeAdjust(cur, target, deadZone, maxStep int) int {
	if maxStep <= 0 {
		return 0
	}
	if deadZone < 0 {
		deadZone = 0
	}
	diff := target - cur
	if diff < deadZone && diff > -deadZone { // |diff| < deadZone：死区不动
		return 0
	}
	if diff > maxStep {
		return maxStep
	}
	if diff < -maxStep {
		return -maxStep
	}
	return diff
}

// PickOffline 从在线机器人里挑 n 个下线：now = 空闲的（现在就能断），
// pending = 在忙的（有抓鬼会话/任务/战斗/游荡，**绝不硬断**，等它收工再断）。
//
// preferIdle=true（默认）：空闲号优先，不够再用忙号补 pending；
// preferIdle=false：按传入顺序挑（壳层传随机序 = 随机补选），但忙号仍只进 pending。
// 取舍顺序由输入顺序决定（纯函数、不用随机源，便于确定性单测）。
//
// 2026-09-23 R3：**人工暂停的号既不 now 也不 pending** —— 用户点过「停止」的号
// 水位保持器不主动断它（保留它在线，等用户自己决定；目标缺口由别的号摊）。
func PickOffline(robots []state.Robot, n int, preferIdle bool) (now []string, pending []string) {
	if n <= 0 {
		return nil, nil
	}
	if !preferIdle {
		for _, r := range robots {
			if len(now)+len(pending) >= n {
				break
			}
			if r.Account == "" || !r.Online || r.Paused {
				continue
			}
			if Busy(r) {
				pending = append(pending, r.Account)
			} else {
				now = append(now, r.Account)
			}
		}
		return now, pending
	}
	for _, r := range robots {
		if len(now) >= n {
			break
		}
		if r.Account == "" || !r.Online || r.Paused || Busy(r) {
			continue
		}
		now = append(now, r.Account)
	}
	for _, r := range robots {
		if len(now)+len(pending) >= n {
			break
		}
		if r.Account == "" || !r.Online || r.Paused || !Busy(r) {
			continue
		}
		pending = append(pending, r.Account)
	}
	return now, pending
}

// PickOnlineCandidates 从号池候选里挑 n 个可上线的（按账号升序，结果稳定）。
//
// 过滤口径：该区不可用的不拉、被标记移除的不拉（自己压下去的号由壳层豁免）、
// 已在线的不拉（重复 add 没意义）、在忙的不拉（它已经在线了，正常不会出现在池里）、
// **人工暂停的不拉**（R3：用户点过「停止」，自动编排不打扰）。
func PickOnlineCandidates(pool []Candidate, n int) []string {
	if n <= 0 {
		return nil
	}
	names := make([]string, 0, len(pool))
	for _, c := range pool {
		if c.Account == "" || !c.Usable || c.Removed || c.Online || c.Busy || c.Paused {
			continue
		}
		names = append(names, c.Account)
	}
	sort.Strings(names)
	if len(names) > n {
		names = names[:n]
	}
	return names
}

// Busy 该号是否"不该被自动打扰"（不硬断、不拉起、不派游荡的统一判据；与 api.isTasking /
// roampool.Idle 同口径，restorer.needsRestore 的"不打扰"集合是它的子集）：
//
//   - 战斗（r.Fight）/ 活跃抓鬼会话（r.GhostActive）/ 游荡或孵化（r.Walking）；
//   - 推进中的任务态：NAV / CLICK / DIALOG / FIGHT / SHOP / ALLOC / WAIT_NEXT、
//     **SUBMIT**（交付/提交中 —— 2026-09-23 按地图页口径补齐，与 FIGHT/NAV 并列）；
//     WAIT_TASK 且任务索引非 0；
//   - **ERROR**（机器人上报的卡住/停链态，如"换图推送迟迟未到"）：它不是"在干活"，
//     但属于**异常** —— 派活只会让卡住号更难处理，压号也不该拿它当可回收的空闲号
//     （等人工重登或机器人端 STUCK_ 自愈，见 docs/04-测试/修复-20260921-STUCK错误自动恢复.md）。
//     故与"忙"同列：不派活、不硬压；进了"待下线"也要等它自愈回正常态才按常规处理。
//
// 与前端 MapView（26f4660）的关系：前端把 SUBMIT 放进 BUSY_STATES、把 ERROR 单列「异常」桶
// （同样不进"空闲"候选）—— 结果等价；Go 侧合并进 Busy，是为了让所有"不打扰/不回收"的
// 判据天然统一，不再出现第三套口径（三池交互审计 C5）。
func Busy(r state.Robot) bool {
	if r.Fight || r.GhostActive() || r.Walking() {
		return true
	}
	switch strings.ToUpper(strings.TrimSpace(r.State)) {
	case "NAV", "CLICK", "DIALOG", "FIGHT", "SHOP", "ALLOC", "WAIT_NEXT", "SUBMIT", "ERROR":
		return true
	case "WAIT_TASK":
		return r.TaskIndex != 0
	}
	return false
}

// ---------------------------------------------------------------- 取数

// reading 取本轮生效读数：服务端新鲜读数优先，否则本地握手数兜底。
// 返回 (生效值, 来源, 是否新鲜, 服务端读数, 服务端读数距今秒数[-1=没有读数])。
func (k *Keeper) reading(now time.Time) (cur int, source string, fresh bool, svr int, svrAge int) {
	svrAge = -1
	if k.d.SvrOnline != nil {
		if c, tsMS, ok := k.d.SvrOnline(); ok {
			svr = c
			age := int(now.UnixMilli()/1000 - int64(tsMS)/1000)
			if age < 0 {
				age = 0
			}
			svrAge = age
			if age <= SvrStaleSec {
				return c, SourceSvr, true, svr, svrAge
			}
			k.logf("[WATERLINE] 服务端在线读数过期（%d 秒前），本轮用本地握手数兜底", age)
		}
	}
	return k.localCount(), SourceLocal, false, svr, svrAge
}

func (k *Keeper) localCount() int {
	if k.d.Local == nil {
		return 0
	}
	return k.d.Local()
}

// ---------------------------------------------------------------- 一轮

// Status 当前状态（读数取**实时**值，面板不用等下一轮）。
func (k *Keeper) Status() Status {
	now := k.d.Now()
	cur, source, fresh, svr, age := k.reading(now)
	local := k.localCount()
	k.syncQueues(now)

	k.mu.Lock()
	defer k.mu.Unlock()
	cfg := k.cfg
	return Status{
		Enabled: cfg.Enabled, Target: cfg.Target, DeadZone: cfg.DeadZone, MaxStep: cfg.MaxStep,
		IntervalSec: cfg.IntervalSec, PreferIdle: cfg.PreferIdle, RandomFallback: cfg.RandomFallback,
		MinKeep: cfg.MinKeep, AllowLocal: cfg.AllowLocal,
		Cur: cur, Source: source, Fresh: fresh, Local: local, Svr: svr, SvrAgeSec: age,
		Diff:       cfg.Target - cur,
		Pending:    append([]string(nil), k.pending...),
		InFlight:   k.inflightList(),
		LastAction: k.lastAct, LastRunTS: tsOf(k.lastRun), LastErr: k.lastErr,
	}
}

// Tick 跑一轮（enabled=false 时**什么都不做**，返回 false）。
// 返回本轮是否真的下发了上下线动作。
func (k *Keeper) Tick(now time.Time) bool {
	k.mu.Lock()
	cfg := k.cfg
	k.mu.Unlock()
	if !cfg.Enabled {
		return false
	}

	cur, source, fresh, _, _ := k.reading(now) // 判决只看生效读数与来源
	local := k.localCount()
	// 2026-09-22 安全闸：口径是"含真人总数"。服务端读数不可用（fresh=false，用的是本地兜底）时，
	// 默认**不按本地数调整** —— 否则会把我们自己的号顶到 target，真人一多就超总目标。
	if !fresh && !cfg.AllowLocal {
		k.setAct(fmt.Sprintf("跳过：服务端在线数不可用（svr_online 过期/为空），本地兜底=%d；"+
			"如需按本地口径维持请开 allow_local", local))
		return false
	}
	k.syncQueues(now) // 队列保鲜：已离线的从待下线摘掉；在途超时的丢掉

	k.mu.Lock()
	k.lastRun = now
	pendingN, inflightN := len(k.pending), len(k.inflight)
	k.mu.Unlock()

	raw := ComputeAdjust(cur, cfg.Target, cfg.DeadZone, cfg.MaxStep)
	// 保底：绝不让"我们自己的号"被压到 min_keep 以下（读数含真人时尤其重要）
	if raw < 0 && local+raw < cfg.MinKeep {
		raw = cfg.MinKeep - local
		if raw > 0 {
			raw = 0
		}
		k.setAct(fmt.Sprintf("保底生效：本地在线 %d 不低于 min_keep=%d", local, cfg.MinKeep))
	}

	switch {
	case raw > 0:
		// 补号：先撤销"待下线"（读数已不需要压），再扣掉在途数（别重复补）
		if pendingN > 0 {
			k.clearPending("目标已不需要下线，撤销待下线")
		}
		want := raw - inflightN
		if want > cfg.MaxStep {
			want = cfg.MaxStep // 在途数可能把 raw 抬过 maxStep，动作量仍守上限
		}
		if want <= 0 {
			k.note(fmt.Sprintf("等 %d 个在途号登录（本轮不补）", inflightN))
			return false
		}
		sent, err := k.doOnline(want, now)
		if err != nil {
			k.fail("补号", err)
			return false
		}
		k.note(fmt.Sprintf("补号 %d 个（%s口径 %d → 目标 %d）：%s",
			len(sent), sourceLabel(source), cur, cfg.Target, strings.Join(sent, ",")))
		return true

	case raw < 0:
		need := -raw + inflightN // 在途号上线后 cur 会 +1，摊销进来
		k.trimPending(need)
		budget := cfg.MaxStep // 本轮动作预算（含"待下线收工后现在断"的）
		acted := false

		// ① 待下线队列里已经收工的 → 现在断（同样受预算约束，别一次砸太多）
		if ready := k.pendingReady(budget); len(ready) > 0 {
			sent, err := k.doOffline(ready, "待下线收工")
			if err != nil {
				k.fail("压号", err)
			} else {
				budget -= len(sent)
				acted = true
				k.note(fmt.Sprintf("待下线号已收工，断 %d 个：%s", len(sent), strings.Join(sent, ",")))
			}
		}

		// ② 还不够 → 新挑（空闲的直接断，在忙的进待下线等收工）
		if budget > 0 {
			remaining := need - k.pendingLen()
			if remaining > budget {
				remaining = budget
			}
			if remaining > 0 {
				done, err := k.doOfflinePick(remaining, cfg.PreferIdle, cfg.RandomFallback, now)
				if err != nil {
					k.fail("压号", err)
				} else if done {
					acted = true
				}
			} else if !acted {
				k.note(fmt.Sprintf("压号已在计划内（待下线 %d 个，等它们收工）", k.pendingLen()))
			}
		}
		return acted

	default:
		// 死区内：读数已回到目标附近，撤销"待下线"（别再多断号）
		if pendingN > 0 {
			k.clearPending("读数已回到死区内，撤销待下线")
			return false
		}
		k.note(fmt.Sprintf("水位正常（%s口径 %d，目标 %d±%d）", sourceLabel(source), cur, cfg.Target, cfg.DeadZone))
		return false
	}
}

// Start 周期跑（ctx 结束即退出）。1 秒粒度轮询：interval_sec 改小了下一轮立刻生效。
func (k *Keeper) Start(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	var last time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			cfg := k.Config()
			if !cfg.Enabled {
				last = time.Time{} // 关掉再开：立刻跑一轮，不用等一个间隔
				continue
			}
			if !last.IsZero() && now.Sub(last) < time.Duration(cfg.IntervalSec)*time.Second {
				continue
			}
			last = now
			func() {
				// 单轮异常不许把循环带崩：任何意外只记日志，下一轮照跑。
				defer func() {
					if v := recover(); v != nil {
						k.logf("[WATERLINE] 本轮异常（已忽略，下一轮继续）: %v", v)
					}
				}()
				k.Tick(now)
			}()
		}
	}
}

// ---------------------------------------------------------------- 动作

// doOnline 补号：挑号 → 下发上线 → 记在途（避免下一轮重复补）。
func (k *Keeper) doOnline(want int, now time.Time) ([]string, error) {
	if k.d.Candidates == nil || k.d.Online == nil {
		return nil, errors.New("上线下发能力不可用")
	}
	skip := k.inflightSet()
	pool := k.d.Candidates()
	avail := make([]Candidate, 0, len(pool))
	for _, c := range pool {
		if skip[c.Account] {
			continue
		}
		avail = append(avail, c)
	}
	picked := PickOnlineCandidates(avail, want)
	if len(picked) == 0 {
		return nil, errors.New("号池里没有可上线的号（都不可用/已在线/已被移除）")
	}
	sent, err := k.d.Online(picked)
	if err != nil {
		return sent, err
	}
	if len(sent) == 0 {
		return nil, errors.New("上线下发失败（机器人通道未连接或池里没密码）")
	}
	k.mu.Lock()
	for _, a := range sent {
		k.inflight[a] = now
		delete(k.selfOff, a) // 又拉起来了：不再是"我们压下去"的号
	}
	k.mu.Unlock()
	return sent, nil
}

// doOfflinePick 挑号压制：空闲的直接断，在忙的进「待下线」。
func (k *Keeper) doOfflinePick(n int, preferIdle, randomFallback bool, now time.Time) (bool, error) {
	if k.d.Robots == nil || k.d.Offline == nil {
		return false, errors.New("下线下发能力不可用")
	}
	robots := k.d.Robots()
	planned := map[string]bool{}
	for _, a := range k.pendingAccounts() {
		planned[a] = true
	}
	pool := make([]state.Robot, 0, len(robots))
	for _, r := range robots {
		if r.Account == "" || planned[r.Account] {
			continue
		}
		pool = append(pool, r)
	}
	// 随机补选：关掉"空闲优先"时一律随机；开着时**空闲号不够**才随机补
	// （避免每轮都盯着同一小批号反复折腾）。
	if !preferIdle || (randomFallback && idleOnline(pool) < n) {
		shuffleRobots(pool, k.d.Rand)
	}
	nowList, pendList := PickOffline(pool, n, preferIdle)
	if len(nowList) == 0 && len(pendList) == 0 {
		// 没有可压的号（当前区没有我们自己的在线号）——不是错误，但要让人在面板上看得到原因
		k.note("没有可压的号（当前区没有我们自己的在线号）")
		return false, nil
	}
	acted := false
	if len(nowList) > 0 {
		sent, err := k.doOffline(nowList, "空闲号")
		if err != nil {
			return false, err
		}
		acted = true
		k.note(fmt.Sprintf("压号 %d 个空闲号：%s", len(sent), strings.Join(sent, ",")))
	}
	if len(pendList) > 0 {
		k.addPending(pendList)
		acted = true
		k.note(fmt.Sprintf("标记待下线 %d 个（在忙，等任务收尾再断）：%s",
			len(pendList), strings.Join(pendList, ",")))
	}
	return acted, nil
}

// doOffline 实际下发下线（动作成功后：从待下线摘掉 + 记入"自己压下去的号"）。
func (k *Keeper) doOffline(accounts []string, reason string) ([]string, error) {
	if k.d.Offline == nil {
		return nil, errors.New("下线下发能力不可用")
	}
	sent, err := k.d.Offline(accounts)
	if err != nil {
		return sent, err
	}
	if len(sent) == 0 {
		return nil, fmt.Errorf("下线下发失败（%s）：机器人通道未连接", reason)
	}
	k.mu.Lock()
	for _, a := range sent {
		k.selfOff[a] = true
		delete(k.inflight, a)
		k.removePendingLocked(a)
	}
	k.mu.Unlock()
	return sent, nil
}

// ---------------------------------------------------------------- 队列维护

// syncQueues 队列保鲜（每轮与面板读状态前都跑一次）：
//   - 待下线：已经不在线的（机器人摘掉了 / 被别的入口断开）→ 摘掉，别一直挂着；
//   - 在途：已经上线 / 超过 InflightTTLSec → 摘掉（超时后允许重新补号）。
func (k *Keeper) syncQueues(now time.Time) {
	k.mu.Lock()
	needSync := len(k.pending) > 0 || len(k.inflight) > 0
	k.mu.Unlock()
	if !needSync || k.d.Robots == nil {
		return
	}
	online := map[string]bool{}
	for _, r := range k.d.Robots() {
		if r.Online && r.Account != "" {
			online[r.Account] = true
		}
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	kept := k.pending[:0]
	for _, a := range k.pending {
		if online[a] {
			kept = append(kept, a)
			continue
		}
		delete(k.selfOff, a) // 已经不在线：不用再豁免（它再来就是新的一轮）
	}
	for i := len(kept); i < len(k.pending); i++ {
		k.pending[i] = ""
	}
	k.pending = kept
	for a, at := range k.inflight {
		if online[a] || now.Sub(at) > time.Duration(InflightTTLSec)*time.Second {
			delete(k.inflight, a)
		}
	}
}

// pendingReady 待下线里"已经回到空闲态"的号（可以现在断），最多 max 个（保序）。
func (k *Keeper) pendingReady(max int) []string {
	if max <= 0 || k.d.Robots == nil {
		return nil
	}
	if k.pendingLen() == 0 {
		return nil
	}
	idle := map[string]bool{}
	for _, r := range k.d.Robots() {
		if r.Online && r.Account != "" && !Busy(r) {
			idle[r.Account] = true
		}
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make([]string, 0, max)
	for _, a := range k.pending {
		if len(out) >= max {
			break
		}
		if idle[a] {
			out = append(out, a)
		}
	}
	return out
}

func (k *Keeper) pendingAccounts() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.pending...)
}

func (k *Keeper) pendingLen() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.pending)
}

// inflightList 在途号（已排序）。**调用方必须持有 k.mu**（目前只有 Status 用）。
func (k *Keeper) inflightList() []string {
	out := make([]string, 0, len(k.inflight))
	for a := range k.inflight {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

func (k *Keeper) inflightSet() map[string]bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make(map[string]bool, len(k.inflight))
	for a := range k.inflight {
		out[a] = true
	}
	return out
}

// clearPending 清空待下线（读数回到目标附近 / 目标被调高时用）。
func (k *Keeper) clearPending(why string) {
	k.mu.Lock()
	if len(k.pending) == 0 {
		k.mu.Unlock()
		return
	}
	accounts := append([]string(nil), k.pending...)
	k.pending = nil
	k.mu.Unlock()
	k.logf("[WATERLINE] %s：撤销 %d 个待下线 %s", why, len(accounts), strings.Join(accounts, ","))
}

// trimPending 待下线最多留 need 个（目标/读数变化后多余的从尾部撤掉）。
func (k *Keeper) trimPending(need int) {
	if need < 0 {
		need = 0
	}
	k.mu.Lock()
	if len(k.pending) <= need {
		k.mu.Unlock()
		return
	}
	drop := append([]string(nil), k.pending[need:]...)
	for i := need; i < len(k.pending); i++ {
		k.pending[i] = ""
	}
	k.pending = k.pending[:need]
	k.mu.Unlock()
	k.logf("[WATERLINE] 待下线队列收缩到 %d 个（撤销 %d 个：%s）", need, len(drop), strings.Join(drop, ","))
}

func (k *Keeper) addPending(accounts []string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	seen := make(map[string]bool, len(k.pending))
	for _, a := range k.pending {
		seen[a] = true
	}
	for _, a := range accounts {
		if a == "" || seen[a] {
			continue
		}
		seen[a] = true
		k.pending = append(k.pending, a)
	}
	sort.Strings(k.pending)
}

// removePendingLocked 从待下线里摘掉（调用方必须持有锁）。
func (k *Keeper) removePendingLocked(account string) {
	if len(k.pending) == 0 {
		return
	}
	out := k.pending[:0]
	for _, a := range k.pending {
		if a != account {
			out = append(out, a)
		}
	}
	for i := len(out); i < len(k.pending); i++ {
		k.pending[i] = ""
	}
	k.pending = out
}

// ---------------------------------------------------------------- 小工具

// note 记"最近一次动作/说明"（只保留最近一条，供面板看）并写日志。
func (k *Keeper) note(msg string) {
	k.mu.Lock()
	k.lastAct = msg
	k.lastErr = ""
	k.mu.Unlock()
	k.logf("[WATERLINE] %s", msg)
}

// setAct 只更新"最近动作"文案（不写日志）。
func (k *Keeper) setAct(msg string) {
	k.mu.Lock()
	k.lastAct = msg
	k.mu.Unlock()
}

// fail 记一次失败（不退出、不影响下一轮）。
func (k *Keeper) fail(what string, err error) {
	if err == nil {
		return
	}
	msg := what + "失败：" + err.Error()
	k.mu.Lock()
	k.lastErr = msg
	k.mu.Unlock()
	k.logf("[WATERLINE] %s", msg)
}

func (k *Keeper) logf(format string, args ...any) {
	if k.d.Log != nil {
		k.d.Log(format, args...)
	}
}

func sourceLabel(source string) string {
	if source == SourceSvr {
		return "服务端含真人"
	}
	return "本地兜底"
}

func tsOf(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func idleOnline(robots []state.Robot) int {
	n := 0
	for _, r := range robots {
		if r.Online && r.Account != "" && !Busy(r) {
			n++
		}
	}
	return n
}

// shuffleRobots 原地打乱（用注入的随机源，便于测试确定性）。
func shuffleRobots(robots []state.Robot, rnd func(int) int) {
	if rnd == nil {
		rnd = func(n int) int {
			if n <= 0 {
				return 0
			}
			return rand.Intn(n)
		}
	}
	for i := len(robots) - 1; i > 0; i-- {
		j := rnd(i + 1)
		if j < 0 || j > i {
			j = i
		}
		robots[i], robots[j] = robots[j], robots[i]
	}
}
