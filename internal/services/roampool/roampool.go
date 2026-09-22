// Package roampool 「游荡池 keeper」：把**任务池用不上的余量号**派去游荡（打野/找暗雷），
// 并在任务池缺人时**立刻回收**游荡号。
//
// 用户口径（2026-09-22 拍板，勿改）：
//   - 在线总数 200：**抓鬼池 100 + 新手池 0 + 游荡池 = 余量**（目标 100 左右）；
//   - 游荡要**均匀**：别让某一张图堆太多机器人（按各图在线数从少到多轮流派）；
//   - 任务缺人时**立刻回收**游荡号（不等它这轮走完，可能有路程损耗——用户接受）。
//
// 与其它模块的分工（三层互不打架）：
//   - internal/services/autotask：任务池（新手链/抓鬼/孵化）的保持数；
//   - internal/services/waterline：当前区**在线总数**水位；
//   - 本包：只负责"游荡**名额与分布**"。机器人端已有"空闲 90s 自动游荡"兜底，
//     所以这里**不抢时间**（每 IntervalSec 一轮、每轮最多 MaxStep 个），不跟机器人端抢时效。
//
// 架构约定（与 internal/services/waterline 同款）：本包只做**纯决策**
// （Roaming / Idle / PickReclaim / PickIdle / BalanceAssign / MapLoads 是纯函数，便于单测），
// 真实动作（取数、下发/停止游荡、读任务池缺口）由壳层（internal/api）通过 Deps 注入。
//
// 安全：enabled=false（默认）时**什么都不做**；单轮异常只记日志，不影响下一轮。
package roampool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"zyctrlcenter/internal/services/waterline"
	"zyctrlcenter/internal/state"
)

// 默认参数（2026-09-22 用户拍板；落盘后可覆盖）。
const (
	DefaultTarget      = 100 // 游荡池目标数（在线总数 200 - 抓鬼 100 - 新手 0）
	DefaultIntervalSec = 60  // 检查间隔（秒）
	DefaultMaxStep     = 5   // 每轮最多调整（回收/补位各自限幅）
	DefaultMinutes     = 0   // 游荡限时（分钟；0=不限）
	DefaultBalance     = true
	DefaultReclaim     = true
)

// 参数范围（Validate 的校验口径，与 POST /api/roampool 一致）。
const (
	MaxTarget      = 10000
	MinIntervalSec = 10
	MaxIntervalSec = 3600
	MaxMaxStep     = 50
	MaxMinutes     = 1440
)

// 环境变量名（优先级：启动时 **env > 文件 > 默认**；运行期面板 POST 的改动直接生效并落盘）。
const (
	envEnabled  = "CTRL_ROAMPOOL_ENABLED"
	envTarget   = "CTRL_ROAMPOOL_TARGET"
	envInterval = "CTRL_ROAMPOOL_INTERVAL_SEC"
	envMaxStep  = "CTRL_ROAMPOOL_MAX_STEP"
	envMinutes  = "CTRL_ROAMPOOL_MINUTES"
	envBalance  = "CTRL_ROAMPOOL_BALANCE"
	envReclaim  = "CTRL_ROAMPOOL_RECLAIM_ON_DEFICIT"
	envMaps     = "CTRL_ROAMPOOL_MAPS"
	envMode     = "CTRL_ROAMPOOL_MODE"
)

// Config 游荡池参数（落盘 data/roampool.json，重启后保留；**默认 enabled=false**）。
type Config struct {
	Enabled          bool   `json:"enabled"`            // 总开关（false 时循环什么都不做）
	Target           int    `json:"target"`             // 游荡池目标数（在游荡的号数）
	IntervalSec      int    `json:"interval_sec"`       // 检查间隔（秒）
	MaxStep          int    `json:"max_step"`           // 每轮最多调整（回收/补位）
	Minutes          int    `json:"minutes"`            // 游荡限时（分钟；0=不限）
	Balance          bool   `json:"balance"`            // true=按图均匀分配；false=每号自抽随机图
	ReclaimOnDeficit bool   `json:"reclaim_on_deficit"` // 任务池缺人时立刻回收游荡号
	Maps             []int  `json:"maps,omitempty"`     // 可选白名单（空 = 用链数据里有网格的图）
	Mode             string `json:"mode,omitempty"`     // 游荡档位（机器人端 ROAM_PROFILES；空=默认档）
}

// DefaultConfig 默认参数：**enabled=false**，必须人工开（不会自己动生产）。
func DefaultConfig() Config {
	return Config{
		Enabled:          false,
		Target:           DefaultTarget,
		IntervalSec:      DefaultIntervalSec,
		MaxStep:          DefaultMaxStep,
		Minutes:          DefaultMinutes,
		Balance:          DefaultBalance,
		ReclaimOnDeficit: DefaultReclaim,
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
	if c.IntervalSec < MinIntervalSec {
		c.IntervalSec = MinIntervalSec
	}
	if c.IntervalSec > MaxIntervalSec {
		c.IntervalSec = MaxIntervalSec
	}
	if c.MaxStep < 1 {
		c.MaxStep = 1
	}
	if c.MaxStep > MaxMaxStep {
		c.MaxStep = MaxMaxStep
	}
	if c.Minutes < 0 {
		c.Minutes = 0
	}
	if c.Minutes > MaxMinutes {
		c.Minutes = MaxMinutes
	}
	c.Mode = strings.TrimSpace(c.Mode)
	if len(c.Maps) > 0 {
		c.Maps = normMaps(c.Maps)
	}
	return c
}

// Validate 校验参数范围（写接口用；不通过就**不落盘、不生效**）。
func (c Config) Validate() error {
	switch {
	case c.Target < 0 || c.Target > MaxTarget:
		return fmt.Errorf("target 必须在 0~%d 之间", MaxTarget)
	case c.IntervalSec < MinIntervalSec || c.IntervalSec > MaxIntervalSec:
		return fmt.Errorf("interval_sec 必须在 %d~%d 之间", MinIntervalSec, MaxIntervalSec)
	case c.MaxStep < 1 || c.MaxStep > MaxMaxStep:
		return fmt.Errorf("max_step 必须在 1~%d 之间", MaxMaxStep)
	case c.Minutes < 0 || c.Minutes > MaxMinutes:
		return fmt.Errorf("minutes 必须在 0~%d 之间（0=不限）", MaxMinutes)
	}
	for _, m := range c.Maps {
		if m <= 0 {
			return fmt.Errorf("maps 里有非法图号 %d（应为正整数）", m)
		}
	}
	return nil
}

// ApplyEnv 用环境变量覆盖参数（启动读盘时调一次），返回被覆盖的字段名（日志里说明谁在起作用）。
//
//	CTRL_ROAMPOOL_ENABLED / _TARGET / _INTERVAL_SEC / _MAX_STEP / _MINUTES
//	CTRL_ROAMPOOL_BALANCE / _RECLAIM_ON_DEFICIT / _MAPS（逗号串）/ _MODE
func (c Config) ApplyEnv() (Config, []string) {
	var pinned []string
	if v, ok := envBool(envEnabled); ok {
		c.Enabled, pinned = v, append(pinned, "enabled")
	}
	if v, ok := envInt(envTarget); ok {
		c.Target, pinned = v, append(pinned, "target")
	}
	if v, ok := envInt(envInterval); ok {
		c.IntervalSec, pinned = v, append(pinned, "interval_sec")
	}
	if v, ok := envInt(envMaxStep); ok {
		c.MaxStep, pinned = v, append(pinned, "max_step")
	}
	if v, ok := envInt(envMinutes); ok {
		c.Minutes, pinned = v, append(pinned, "minutes")
	}
	if v, ok := envBool(envBalance); ok {
		c.Balance, pinned = v, append(pinned, "balance")
	}
	if v, ok := envBool(envReclaim); ok {
		c.ReclaimOnDeficit, pinned = v, append(pinned, "reclaim_on_deficit")
	}
	if v := strings.TrimSpace(os.Getenv(envMaps)); v != "" {
		if maps, err := parseMaps(v); err == nil {
			c.Maps, pinned = maps, append(pinned, "maps")
		}
	}
	if v := strings.TrimSpace(os.Getenv(envMode)); v != "" {
		c.Mode, pinned = v, append(pinned, "mode")
	}
	return c, pinned
}

// EnvPinned 只读：哪些字段被环境变量指定（GET /api/roampool 里提示面板"改了也不生效"）。
func EnvPinned() []string {
	_, pinned := Config{}.ApplyEnv()
	return pinned
}

// MapLoad 一张图当前的"我们的号"人数（均衡分配用）。
type MapLoad struct {
	MapID int
	Count int
}

// Deps 依赖（壳层提供真实世界；测试注入假的）。
type Deps struct {
	// Robots 当前区机器人的状态快照（判"在游荡/空闲"、算各图人数用）。
	Robots func() []state.Robot
	// ReclaimEligible 回收资格闸（可选）：只回收"回收后真能进任务池"的号。
	// nil = 全放行（保持旧行为）。2026-09-22 P0，详见 PickReclaim 注释。
	ReclaimEligible func(state.Robot) bool
	// Deficit 任务池缺口 = 抓鬼池 deficit + 新手池 deficit（**正数=缺人**，负数=超编）。
	Deficit func() int
	// Maps 可用游荡图（链数据里有寻路网格的图；白名单为空时用它）。
	Maps func() []int
	// Dispatch 下发游荡：mapid 是 int（指定图）或 "random"（随机图，每号自抽）；
	// 返回**实际下发成功**的号数（失败返回 err）。
	Dispatch func(accounts []string, mapid any, mode string, minutes int) (int, error)
	// Stop 停止游荡（回收）：返回实际下发成功的号数（失败返回 err）。
	Stop func(accounts []string) (int, error)
	Log  func(format string, args ...any)
}

// Status 保持器状态（面板 / GET /api/roampool 展示）。
type Status struct {
	Enabled          bool   `json:"enabled"`
	Target           int    `json:"target"`
	IntervalSec      int    `json:"interval_sec"`
	MaxStep          int    `json:"max_step"`
	Minutes          int    `json:"minutes"`
	Balance          bool   `json:"balance"`
	ReclaimOnDeficit bool   `json:"reclaim_on_deficit"`
	Maps             []int  `json:"maps,omitempty"`
	Mode             string `json:"mode,omitempty"`
	// Running 当前在游荡的号数；Idle 当前空闲（无任务/无抓鬼/未游荡/非战斗）在线号数；
	// Deficit 任务池缺口（正=缺人）；Assignable 本轮最多可调整的号数（min(max_step, …)）。
	Running  int `json:"running"`
	Idle     int `json:"idle"`
	Deficit  int `json:"deficit"`
	MapCount int `json:"map_count"` // 可用游荡图张数（白名单 ∩ 有网格）
	// EnvPinned 被环境变量固定的字段名（面板据此处提示"这些改了也不生效"）。
	EnvPinned  []string `json:"env_pinned,omitempty"`
	LastAction string   `json:"last_action"` // 最近一次动作/判决说明
	LastRunTS  int64    `json:"last_run_ts"` // 最近一轮时间（unix 秒；0 = 还没跑过）
	LastErr    string   `json:"last_err,omitempty"`
}

// Keeper 游荡池保持器（并发安全）。
type Keeper struct {
	path string
	d    Deps

	mu      sync.Mutex
	cfg     Config
	lastRun time.Time
	lastAct string
	lastErr string
}

// New 创建保持器（path 通常为 <数据目录>/roampool.json；不自动读盘，由 Load 负责）。
func New(path string, d Deps) *Keeper {
	if d.Log == nil {
		d.Log = func(string, ...any) {}
	}
	return &Keeper{path: path, d: d, cfg: DefaultConfig()}
}

// Path 配置文件路径。
func (k *Keeper) Path() string { return k.path }

// ---------------------------------------------------------------- 持久化

// Load 读回上次的参数（不存在 = 首次运行，用默认值：enabled=false），再用环境变量覆盖。
//
// 先在默认值上解 JSON：老文件/手工写残的文件缺哪个字段就用默认值补哪个，
// **不会**因为缺 "target" 就把目标当成 0（那等于"一个游荡号都不派"）。
func (k *Keeper) Load() error {
	var loadErr error
	cfg := DefaultConfig()
	if raw, err := os.ReadFile(k.path); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
	} else if err := json.Unmarshal(raw, &cfg); err != nil {
		loadErr = fmt.Errorf("游荡池配置 %s 解析失败: %w", k.path, err)
		cfg = DefaultConfig() // 读坏了就退回默认（默认 enabled=false，不会乱动）
	}
	cfg, pinned := cfg.ApplyEnv()
	cfg = cfg.Normalize()
	k.mu.Lock()
	k.cfg = cfg
	k.mu.Unlock()
	k.logf("[ROAMPOOL] 已恢复参数：enabled=%v target=%d 间隔=%ds 单轮≤%d 限时=%d 分钟 均匀=%v 立刻回收=%v 白名单=%v",
		cfg.Enabled, cfg.Target, cfg.IntervalSec, cfg.MaxStep, cfg.Minutes, cfg.Balance, cfg.ReclaimOnDeficit, cfg.Maps)
	if len(pinned) > 0 {
		k.logf("[ROAMPOOL] 环境变量覆盖了：%s（优先级：env > 文件 > 默认）", strings.Join(pinned, ","))
	}
	return loadErr
}

// save 原子落盘（tmp + rename，避免写一半的文件把参数读坏）。
func (k *Keeper) save() {
	k.mu.Lock()
	cfg := k.cfg
	k.mu.Unlock()
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		k.logf("[ROAMPOOL] 参数序列化失败: %v", err)
		return
	}
	if dir := filepath.Dir(k.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			k.logf("[ROAMPOOL] 参数落盘失败（建目录）: %v", err)
			return
		}
	}
	tmp := k.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		k.logf("[ROAMPOOL] 参数落盘失败: %v", err)
		return
	}
	if err := os.Rename(tmp, k.path); err != nil {
		k.logf("[ROAMPOOL] 参数落盘失败（rename）: %v", err)
	}
}

// Config 当前参数快照。
func (k *Keeper) Config() Config {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.cfg
}

// SetConfig 设置参数。**严格校验**（范围不对就返回 error，不落盘、不生效）；校验通过后再夹一遍。
//
// 运行期面板改动**直接生效并落盘**（不被环境变量顶掉；env 只在启动 Load 时起作用）。
func (k *Keeper) SetConfig(c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	c = c.Normalize()
	k.mu.Lock()
	k.cfg = c
	k.mu.Unlock()
	k.save()
	k.logf("[ROAMPOOL] 参数已更新：enabled=%v target=%d 间隔=%ds 单轮≤%d 限时=%d 分钟 均匀=%v 立刻回收=%v 白名单=%v 档位=%q",
		c.Enabled, c.Target, c.IntervalSec, c.MaxStep, c.Minutes, c.Balance, c.ReclaimOnDeficit, c.Maps, c.Mode)
	return nil
}

// ---------------------------------------------------------------- 纯函数（便于单测）

// Roaming 该号是否**正在游荡**（机器人上报 walk.enabled=true；**含孵化**）。
//
// 2026-09-22 用户口径修正：**孵化号也可以回收做任务**（"孵化的前提是有蛋，没蛋怎么孵化，
// 孵化的也可以回收做任务，不影响"）—— 所以这里不再排除 HatchActive：
//
//	回收一个孵化号 = 停掉它的孵化图游荡 → 回去做任务；它手上的蛋不会被消耗（孵化只是
//	"在孵化图游荡打暗雷"，停了不损失进度），下次有空再孵即可。
func Roaming(r state.Robot) bool {
	if r.Account == "" || !r.Online {
		return false
	}
	m, ok := r.Walk.(map[string]any)
	if !ok {
		return false
	}
	en, ok := m["enabled"].(bool)
	return ok && en
}

// Idle 该号是否**空闲**（在线 且 没在干活）：战斗 / 活跃抓鬼 / 游荡（含孵化）/ 任务态
// （NAV/CLICK/DIALOG/FIGHT/SHOP/ALLOC/WAIT_NEXT，以及 WAIT_TASK 且任务索引非 0）都不算空闲。
//
// 判据复用 waterline.Busy —— 与在线水位保持器**同一口径**（否则会出现"水位以为它空闲、游荡池以为它在忙"
// 这种两套判据打架的场面）。
func Idle(r state.Robot) bool {
	if r.Account == "" || !r.Online {
		return false
	}
	return !waterline.Busy(r)
}

// MapCounts 各图的"我们的号"人数（只数在线号；按图号升序返回）。
func MapCounts(robots []state.Robot) map[int]int {
	out := map[int]int{}
	for _, r := range robots {
		if r.Account == "" || !r.Online {
			continue
		}
		out[r.MapID]++
	}
	return out
}

// MapLoads 把"候选游荡图"摊平成 MapLoad 列表（图号升序，stable）：每张候选图一行，
// Count = 该图当前在线号数（没人的图 Count=0 → 优先被派到）。
func MapLoads(robots []state.Robot, maps []int) []MapLoad {
	counts := MapCounts(robots)
	out := make([]MapLoad, 0, len(maps))
	seen := map[int]bool{}
	for _, m := range maps {
		if m <= 0 || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, MapLoad{MapID: m, Count: counts[m]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MapID < out[j].MapID })
	return out
}

// PickReclaim 从**游荡中**的号里挑 n 个回收：优先挑"所在图**我们号最多**"的那些，
// 顺带纠偏分布（图人数并列时按账号升序，结果稳定、便于测试）。
//
// 人数不足 n 就有几个给几个；n<=0 / 没有游荡号 → nil。
//
// 2026-09-22 P0 修复（三池交互分析）：eligible 非 nil 时**只挑"回收后真能进任务池"的号**。
// 原实现只判 Roaming —— 会把"今日满额/等级不够"的号也回收给任务池，而任务池会跳过它们，
// 90s 后机器人端 auto_roam 又派去游荡 → 回收-重派来回损耗（实测 robot0001108/1127/1176
// 各被回收 20+ 次，游荡产出被反复清空）。这两类号留在游荡池继续游荡更有价值。
func PickReclaim(robots []state.Robot, n int, eligible func(state.Robot) bool) []string {
	if n <= 0 {
		return nil
	}
	load := MapCounts(robots) // 各图的在线号数（含非游荡号：口径是"这张图挤了多少我们的号"）
	type cand struct {
		account string
		count   int
	}
	cands := make([]cand, 0, len(robots))
	for _, r := range robots {
		if !Roaming(r) {
			continue
		}
		if eligible != nil && !eligible(r) {
			continue // 2026-09-22 P0：回收后进不了任务池的号不回收（详见函数头注释）
		}
		cands = append(cands, cand{account: r.Account, count: load[r.MapID]})
	}
	if len(cands) == 0 {
		return nil
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].count != cands[j].count {
			return cands[i].count > cands[j].count // 人多的图先回收
		}
		return cands[i].account < cands[j].account
	})
	if n > len(cands) {
		n = len(cands)
	}
	out := make([]string, 0, n)
	for _, c := range cands[:n] {
		out = append(out, c.account)
	}
	return out
}

// PickIdle 从空闲在线号里挑 n 个（保序；不足就有几个给几个；n<=0 → nil）。
func PickIdle(robots []state.Robot, n int) []string {
	if n <= 0 {
		return nil
	}
	out := make([]string, 0, n)
	for _, r := range robots {
		if len(out) >= n {
			break
		}
		if Idle(r) {
			out = append(out, r.Account)
		}
	}
	return out
}

// BalanceAssign 把空闲号**按图均匀**分配：从"人最少的图"开始轮流派（每派一个就把它的人数 +1，
// 所以并列时轮流占位），最多分配 n 个（n<=0 / 号为空 / 图为空 → 空结果）。
//
// 返回 map[图号][]账号（只含非空项）。
//   - 均匀：3 张空图 + 6 个号 → 各 2 个；
//   - 余数：3 张空图 + 5 个号 → 2/2/1（多出来的给人数并列里**图号小**的图，结果稳定）；
//   - 图数多于号数：5 张图 + 2 个号 → 只有 2 张图各 1 个（空图各派不出去）。
func BalanceAssign(idleAccounts []string, mapsByLoad []MapLoad, n int) map[int][]string {
	out := map[int][]string{}
	if n <= 0 || len(idleAccounts) == 0 || len(mapsByLoad) == 0 {
		return out
	}
	if n > len(idleAccounts) {
		n = len(idleAccounts)
	}
	loads := make([]MapLoad, len(mapsByLoad))
	copy(loads, mapsByLoad)
	// 排序：人少在前；并列图号小的在前（"最少"的搜索取第一个最小 → 结果稳定）
	sort.Slice(loads, func(i, j int) bool {
		if loads[i].Count != loads[j].Count {
			return loads[i].Count < loads[j].Count
		}
		return loads[i].MapID < loads[j].MapID
	})
	for i := 0; i < n; i++ {
		best := 0
		for j := 1; j < len(loads); j++ {
			if loads[j].Count < loads[best].Count {
				best = j
			}
		}
		m := loads[best].MapID
		out[m] = append(out[m], idleAccounts[i])
		loads[best].Count++
	}
	return out
}

// assignText 分配结果的日志文案："图10×2 图26×2 图24×1"（图号升序，稳定）。
func assignText(groups map[int][]string) string {
	if len(groups) == 0 {
		return "（无）"
	}
	ids := make([]int, 0, len(groups))
	for id := range groups {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, fmt.Sprintf("图%d×%d", id, len(groups[id])))
	}
	return strings.Join(parts, " ")
}

// countAssigned 分配出去的号总数。
func countAssigned(groups map[int][]string) int {
	n := 0
	for _, accs := range groups {
		n += len(accs)
	}
	return n
}

// ---------------------------------------------------------------- 取数

func (k *Keeper) robots() []state.Robot {
	if k.d.Robots == nil {
		return nil
	}
	return k.d.Robots()
}

func (k *Keeper) deficit() int {
	if k.d.Deficit == nil {
		return 0
	}
	return k.d.Deficit()
}

// roamMaps 本轮可用的游荡图：白名单（cfg.Maps）∩ 链数据里有网格的图（Deps.Maps）。
// 白名单为空 → 全部有网格的图。白名单里有图没有网格 → **明确记日志**（不静默丢项，warn=true 时）。
// Status 也用同一份口径（warn=false：面板每 15 秒读一次，别刷日志）。
func (k *Keeper) roamMaps(warn bool) []int {
	avail := []int(nil)
	if k.d.Maps != nil {
		avail = normMaps(k.d.Maps())
	}
	cfg := k.Config()
	if len(cfg.Maps) == 0 {
		return avail
	}
	inAvail := map[int]bool{}
	for _, m := range avail {
		inAvail[m] = true
	}
	use := make([]int, 0, len(cfg.Maps))
	missing := make([]int, 0)
	for _, m := range cfg.Maps {
		if inAvail[m] {
			use = append(use, m)
		} else {
			missing = append(missing, m)
		}
	}
	if warn && len(missing) > 0 {
		k.logf("[ROAMPOOL] 白名单里有 %d 张图没有寻路网格（已跳过）：%v", len(missing), missing)
	}
	return use
}

// ---------------------------------------------------------------- 一轮

// Status 当前状态（读数取**实时**值：面板不用等下一轮）。
func (k *Keeper) Status() Status {
	robots := k.robots()
	cfg := k.Config()
	mapCount := len(k.roamMaps(false)) // 与 Tick 同口径（白名单 ∩ 有网格的图；不记日志，别刷）
	running, idle := 0, 0
	for _, r := range robots {
		if Roaming(r) {
			running++
		}
		if Idle(r) {
			idle++
		}
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return Status{
		Enabled: cfg.Enabled, Target: cfg.Target, IntervalSec: cfg.IntervalSec, MaxStep: cfg.MaxStep,
		Minutes: cfg.Minutes, Balance: cfg.Balance, ReclaimOnDeficit: cfg.ReclaimOnDeficit,
		Maps: cfg.Maps, Mode: cfg.Mode,
		Running: running, Idle: idle, Deficit: k.deficit(), MapCount: mapCount,
		EnvPinned:  EnvPinned(),
		LastAction: k.lastAct, LastRunTS: tsOf(k.lastRun), LastErr: k.lastErr,
	}
}

// Tick 跑一轮（enabled=false 时**什么都不做**，返回 false）。
// 返回本轮是否真的下发了游荡/回收命令。
//
// 判决顺序（用户口径：任务链路优先，游荡吃余量）：
//  1. 回收优先：任务池缺口 > 0 且 reclaim_on_deficit → 回收 min(缺口, max_step, 在游荡) 个游荡号，本轮结束；
//  2. 补位：否则在游荡 < target → 取 min(缺额, max_step, 空闲数) 个空闲号，按图均匀派出去。
func (k *Keeper) Tick(now time.Time) bool {
	cfg := k.Config()
	if !cfg.Enabled {
		return false
	}
	robots := k.robots()
	running, idleN := 0, 0
	for _, r := range robots {
		if Roaming(r) {
			running++
		}
		if Idle(r) {
			idleN++
		}
	}
	k.mu.Lock()
	k.lastRun = now
	k.mu.Unlock()

	deficit := k.deficit()
	// ① 回收优先：任务池缺人 → 把游荡号还给任务池（本轮不再补位，名额让给任务池）
	if deficit > 0 && cfg.ReclaimOnDeficit {
		n := minInt(deficit, cfg.MaxStep, running)
		if n <= 0 {
			k.note(fmt.Sprintf("任务池缺口 %d，但没有可回收的游荡号（在游荡 %d）", deficit, running))
			return false
		}
		picked := PickReclaim(robots, n, k.d.ReclaimEligible)
		if len(picked) == 0 {
			k.note(fmt.Sprintf("任务池缺口 %d，但没有可回收的游荡号（在游荡 %d）", deficit, running))
			return false
		}
		sent, err := k.doStop(picked)
		if err != nil {
			k.fail("回收", err)
			return false
		}
		k.note(fmt.Sprintf("回收 %d 个游荡号给任务池（缺口 %d，按图人数从多到少）：%s",
			len(sent), deficit, strings.Join(sent, ",")))
		return true
	}

	// ② 补位：在游荡 < 目标 → 空闲号按图均匀派游荡
	if running >= cfg.Target {
		k.note(fmt.Sprintf("游荡池达标（在游荡 %d / 目标 %d，空闲 %d，缺口 %d）", running, cfg.Target, idleN, deficit))
		return false
	}
	want := minInt(cfg.Target-running, cfg.MaxStep, idleN)
	if want <= 0 {
		k.note(fmt.Sprintf("游荡池差 %d 个，但没有空闲号可派（在游荡 %d / 目标 %d，空闲 %d）",
			cfg.Target-running, running, cfg.Target, idleN))
		return false
	}
	idle := PickIdle(robots, want)
	if len(idle) == 0 {
		k.note("没有空闲号可派游荡")
		return false
	}
	if !cfg.Balance {
		sent, err := k.doDispatch(idle, "random")
		if err != nil {
			k.fail("补位", err)
			return false
		}
		k.note(fmt.Sprintf("补位 %d 个（随机图，每号自抽，在游荡 %d / 目标 %d）：%s",
			len(sent), running, cfg.Target, strings.Join(sent, ",")))
		return true
	}
	maps := k.roamMaps(true)
	if len(maps) == 0 {
		k.note("没有可用的游荡图（链数据里没有带网格的图 / 白名单与网格没有交集）")
		return false
	}
	groups := BalanceAssign(idle, MapLoads(robots, maps), len(idle))
	sent := map[int][]string{} // 只记**真的下发成功**的图（失败的不写进动作文案，别虚报）
	for _, m := range sortedKeys(groups) {
		got, err := k.doDispatch(groups[m], m)
		if err != nil {
			// 单张图失败不影响其它图（下一轮会再补）
			k.fail(fmt.Sprintf("补位→图%d", m), err)
			continue
		}
		sent[m] = got
	}
	if len(sent) == 0 {
		return false
	}
	k.note(fmt.Sprintf("补位 %d 个 → %s（在游荡 %d / 目标 %d）", countAssigned(sent), assignText(sent), running, cfg.Target))
	return true
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
						k.logf("[ROAMPOOL] 本轮异常（已忽略，下一轮继续）: %v", v)
					}
				}()
				k.Tick(now)
			}()
		}
	}
}

// ---------------------------------------------------------------- 动作

// doDispatch 下发游荡（给若干号指定同一张图 / 随机图）。
func (k *Keeper) doDispatch(accounts []string, mapid any) ([]string, error) {
	if k.d.Dispatch == nil {
		return nil, errors.New("游荡下发能力不可用")
	}
	if len(accounts) == 0 {
		return nil, errors.New("没有可下发的号")
	}
	cfg := k.Config()
	sent, err := k.d.Dispatch(accounts, mapid, cfg.Mode, cfg.Minutes)
	if err != nil {
		return nil, err
	}
	if sent <= 0 {
		return nil, errors.New("游荡下发失败（机器人通道未连接？）")
	}
	return accounts[:minInt(sent, len(accounts))], nil
}

// doStop 回收：给这些号下发停止游荡。
func (k *Keeper) doStop(accounts []string) ([]string, error) {
	if k.d.Stop == nil {
		return nil, errors.New("停止游荡能力不可用")
	}
	if len(accounts) == 0 {
		return nil, errors.New("没有可回收的号")
	}
	sent, err := k.d.Stop(accounts)
	if err != nil {
		return nil, err
	}
	if sent <= 0 {
		return nil, errors.New("停止游荡下发失败（机器人通道未连接？）")
	}
	return accounts[:minInt(sent, len(accounts))], nil
}

// ---------------------------------------------------------------- 小工具

// note 记"最近一次动作/说明"（只保留最近一条，供面板看）并写日志。
func (k *Keeper) note(msg string) {
	k.mu.Lock()
	k.lastAct = msg
	k.lastErr = ""
	k.mu.Unlock()
	k.logf("[ROAMPOOL] %s", msg)
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
	k.logf("[ROAMPOOL] %s", msg)
}

func (k *Keeper) logf(format string, args ...any) {
	if k.d.Log != nil {
		k.d.Log(format, args...)
	}
}

func minInt(vals ...int) int {
	out := 0
	for i, v := range vals {
		if i == 0 || v < out {
			out = v
		}
	}
	return out
}

func sortedKeys(m map[int][]string) []int {
	out := make([]int, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	sort.Ints(out)
	return out
}

func tsOf(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// normMaps 图号去重 + 升序（<=0 丢掉）；空/全非法返回 nil。
func normMaps(in []int) []int {
	seen := map[int]bool{}
	out := make([]int, 0, len(in))
	for _, m := range in {
		if m <= 0 || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	sort.Ints(out)
	return out
}

// parseMaps 解析 "6,17,34,40" / "6 17"（环境变量 CTRL_ROAMPOOL_MAPS 用）。
func parseMaps(s string) ([]int, error) {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '，' || r == '、'
	})
	out := make([]int, 0, len(fields))
	for _, f := range fields {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("图号 %q 非法（应为正整数）", f)
		}
		out = append(out, n)
	}
	return normMaps(out), nil
}

// envBool 读布尔环境变量（"1"/"true"/"on"/"yes" 为真；"0"/"false"/"off"/"no" 为假）。
func envBool(key string) (bool, bool) {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch v {
	case "1", "true", "on", "yes", "y":
		return true, true
	case "0", "false", "off", "no", "n":
		return false, true
	}
	return false, false
}

// envInt 读整数环境变量（没给/解析失败 = 未指定：ok=false）。
func envInt(key string) (int, bool) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, false
	}
	return n, true
}
