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

	// DefaultInflightTTLSec 「已派发、还没生效」的判定窗口（秒；2026-09-23 P1 派发节流）。
	// 生效判据是 Roaming（机器人收到命令即置 walk.enabled=true，实测秒级）——窗口只作用于
	// "派了没生效"的失败路径：TTL 内算「在途占位」（不再叠派），到期未生效才算失败并退避升级。
	DefaultInflightTTLSec = 120
	MinInflightTTLSec     = 10
	MaxInflightTTLSec     = 3600
	MaxBackoffSec         = 3600

	// 日志降噪窗口（2026-09-23，纯日志、不改行为；背景：平稳窗 ROAMPOOL 358 行/时、
	// Tick 360 轮/时，其中 no-op+达标占 52%）：
	//   - 达标行：状态翻转时立刻打一条 + 稳定期心跳一条（goalLogSec）；
	//   - 空转（no-op）行：同一「类型+缺口档+空闲档」noopLogSec 内只打一条；
	//   - 动作行（回收/补位）：actionLogSec 聚合一条（窗口内首条原样，后续累计成前缀）。
	goalLogSec   = 600 // 达标心跳间隔（秒）
	noopLogSec   = 300 // 空转行同键节流窗口（秒）
	actionLogSec = 60  // 动作行聚合窗口（秒）

	// idleBackoffMax 空转退避上限：连续"无动作且无活可干"的轮次把轮询间隔 ×2 拉长到它为止
	// （一有动作/有可动作候选立刻回到 interval_sec → 缺口回填延迟不受影响）。
	idleBackoffMax = 60 * time.Second

	// DefaultTaskMapBias 任务图"虚拟负载"偏移（2026-09-24 用户拍板：任务链图机器人已很多 →
	// 游荡"降权少去，具体根据当前图数量动态"）。语义：任务图（抓鬼刷鬼图/新手链/日常会去图）
	// 的**有效负载 = 实际在线人数 + 本偏移**，游荡补位（人最少的图优先）与回收（人多的图先回收）
	// 都按有效负载评分 —— 任务图人少（如空图）时仍可能被选，人多时自动避开（动态降权）。
	// 8 ≈ 一轮 max_step(5) 全派到同一张图的量级（可配 task_map_bias；<0 关闭）。
	DefaultTaskMapBias = 8
	// MaxTaskMapBias 偏移上限（Validate 用）。
	MaxTaskMapBias = 100
)

// defaultTaskMaps 任务图兜底集合（降权对象；2026-09-24 口径，来源 = 链数据）：
//   - 抓鬼刷鬼图（zhongkui_nav.json 的 ghost_maps）：9/10/11/12/13/25/26；
//   - 新手链 NPC 图 + 神捕/烽火 patrol_map（12/5/9/11）+ 610（烽火接取图）；
//   - 店铺/房内小图 602、604~618（任务链会进出）。
//
// 运行时以壳层热读（Deps.TaskMaps，链数据里的任务图）为准并与之合并；这里兜底 ——
// 链数据不可用时降权仍生效。配置 task_maps 非空则**完全覆盖**（人工指定）。
var defaultTaskMaps = []int{
	5, 6, 7, 9, 10, 11, 12, 13, 24, 25, 26,
	602, 604, 605, 606, 607, 608, 609, 610, 611, 612, 613, 614, 615, 616, 617, 618,
}

// defaultBackoffSec 默认退避档位（秒）：60s → 120s → 300s（上限）。
// 为什么是这个序列：机器人收到命令到上报 walk.enabled 是秒级，第一次失败多半是"拒收/走不到"，
// 60s 足够等到状态收敛；再失败就是真问题，用 120/300 把重派压到可接受频率（现场 10s 一轮 → 300s）。
func defaultBackoffSec() []int { return []int{60, 120, 300} }

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
	envTaskBias = "CTRL_ROAMPOOL_TASK_MAP_BIAS"
	envTaskMaps = "CTRL_ROAMPOOL_TASK_MAPS"
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

	// TaskMapBias 任务图"虚拟负载"偏移（2026-09-24 降权；0 = 用默认 DefaultTaskMapBias，
	// <0 = 关闭降权）。只影响"选哪张图/先回收谁"的排序，不剔除任务图。
	TaskMapBias int `json:"task_map_bias,omitempty"`
	// TaskMaps 任务图集合（降权对象；空 = 壳层热读 + 内置兜底集合；非空 = 完全按它）。
	TaskMaps []int `json:"task_maps,omitempty"`

	// InflightTTLSec 「已派发、还没生效」的判定窗口（秒；2026-09-23 P1 派发节流）。
	InflightTTLSec int `json:"inflight_ttl_sec"`
	// BackoffSec 连续"派发没生效"的退避档位（秒）：第 N 次失败用第 N 档，超出取最后一档（=上限）。
	BackoffSec []int `json:"backoff_sec,omitempty"`
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
		InflightTTLSec:   DefaultInflightTTLSec,
		BackoffSec:       defaultBackoffSec(),
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
	// 2026-09-24 降权参数：0 = 默认偏移（8）；负 = 关闭（夹到 0）；超上限夹回。
	if c.TaskMapBias == 0 {
		c.TaskMapBias = DefaultTaskMapBias
	} else if c.TaskMapBias < 0 {
		c.TaskMapBias = 0
	} else if c.TaskMapBias > MaxTaskMapBias {
		c.TaskMapBias = MaxTaskMapBias
	}
	if len(c.TaskMaps) > 0 {
		c.TaskMaps = normMaps(c.TaskMaps)
	} else {
		c.TaskMaps = nil
	}
	// 2026-09-23 P1：在途/退避参数夹取（0/负/超范围都回到合法值，脏配置不会引发异常动作）。
	if c.InflightTTLSec <= 0 {
		c.InflightTTLSec = DefaultInflightTTLSec
	}
	if c.InflightTTLSec < MinInflightTTLSec {
		c.InflightTTLSec = MinInflightTTLSec
	}
	if c.InflightTTLSec > MaxInflightTTLSec {
		c.InflightTTLSec = MaxInflightTTLSec
	}
	c.BackoffSec = normBackoff(c.BackoffSec)
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
	if c.TaskMapBias > MaxTaskMapBias {
		return fmt.Errorf("task_map_bias 不能大于 %d（0=默认 %d，<0=关闭降权）", MaxTaskMapBias, DefaultTaskMapBias)
	}
	for _, m := range c.TaskMaps {
		if m <= 0 {
			return fmt.Errorf("task_maps 里有非法图号 %d（应为正整数）", m)
		}
	}
	if c.InflightTTLSec < MinInflightTTLSec || c.InflightTTLSec > MaxInflightTTLSec {
		return fmt.Errorf("inflight_ttl_sec 必须在 %d~%d 之间", MinInflightTTLSec, MaxInflightTTLSec)
	}
	for _, v := range c.BackoffSec {
		if v <= 0 || v > MaxBackoffSec {
			return fmt.Errorf("backoff_sec 里的 %d 非法（每个档位 1~%d 秒）", v, MaxBackoffSec)
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
	if v, ok := envInt(envTaskBias); ok {
		c.TaskMapBias, pinned = v, append(pinned, "task_map_bias")
	}
	if v := strings.TrimSpace(os.Getenv(envTaskMaps)); v != "" {
		if maps, err := parseMaps(v); err == nil {
			c.TaskMaps, pinned = maps, append(pinned, "task_maps")
		}
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

// Inflight 一条"已派发、还没生效"的游荡派发记录（2026-09-23 P1 派发节流/退避）。
//
// 为什么要记：keeper 只把 walk.enabled=true 算"在游荡"，命令发出到机器人上报之间（或**根本没生效**
// ——机器人拒收/走不到目标图）不记账 → 每轮（现场 interval_sec=10）重挑同一批号重复派。
// 现场实测：60 派/分、54 拒/分、同一号同图 10s 一轮（机器人端 `游荡排除图: 给定选图全被排除`），
// 号永远进不了游荡、事件流刷屏。
//
// 判据与生命周期：
//   - 派发即记（markDispatch）；**生效 = Roaming(r)（walk.enabled=true）→ 立刻清除**；
//     机器人离线/状态行被清理 → 清除（这号不在池子里了）；
//   - Until 之前不再派这个号（退避 60→120→300s，末档为上限）；连续未生效时每次重派 +1 档，
//     真正生效（walk.enabled）后再派从第 1 档重新起算；
//   - Pending = 还算「在途占位」（Until 未到 **且** 距上次派发 < TTL）：占位计入目标缺口，
//     避免"上一批还在路上又派一批"；占位窗口取两者小者（结果已知就释放，300s 档位最多占 120s）。
type Inflight struct {
	Account string    `json:"account"`
	MapID   int       `json:"mapid"`   // 最近一次派发的目标图（0 = 随机图）
	At      time.Time `json:"at"`      // 最近一次派发时间
	Fails   int       `json:"fails"`   // 连续"派发未生效"次数（= 退避档位）
	Until   time.Time `json:"until"`   // 退避截止：在此之前不再派这个号
	Pending bool      `json:"pending"` // 是否还算「在途占位」（见上：Until 内且 < TTL）
}

// roamAttempt 在途记账的内部形态（Inflight 是它的对外快照）。
type roamAttempt struct {
	mapID int
	at    time.Time
	fails int
	until time.Time
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
	// TaskMaps 任务图"热读"集合（2026-09-24 降权）：链数据里的任务图（如抓鬼 ghost_maps）。
	// 与内置兜底集合（defaultTaskMaps）合并后作为降权对象；配置 task_maps 非空则覆盖之。
	// nil / 读失败 = 只用兜底集合（降权不依赖链数据可用性）。
	TaskMaps func() []int
	// Dispatch 下发游荡：mapid 是 int（指定图）或 "random"（随机图，每号自抽）；
	// 返回**实际下发成功**的号数（失败返回 err）。
	Dispatch func(accounts []string, mapid any, mode string, minutes int) (int, error)
	// Stop 停止游荡（回收）：返回实际下发成功的号数（失败返回 err）。
	Stop func(accounts []string) (int, error)
	// Now 时钟（Status 计算"在途/退避"用；nil = time.Now）。
	Now func() time.Time
	Log func(format string, args ...any)
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
	TaskMapBias      int    `json:"task_map_bias"`  // 生效的任务图虚拟负载偏移（0 = 关闭降权）
	TaskMapCount     int    `json:"task_map_count"` // 生效的降权任务图张数
	InflightTTLSec   int    `json:"inflight_ttl_sec"`
	BackoffSec       []int  `json:"backoff_sec,omitempty"`
	// Running 当前在游荡的号数；Idle 当前空闲（无任务/无抓鬼/未游荡/非战斗）在线号数；
	// Deficit 任务池缺口（正=缺人）；Assignable 本轮最多可调整的号数（min(max_step, …)）。
	Running  int `json:"running"`
	Idle     int `json:"idle"`
	Deficit  int `json:"deficit"`
	MapCount int `json:"map_count"` // 可用游荡图张数（白名单 ∩ 有网格）
	// InflightPending 在途占位（已派发、TTL 内还没生效的号数；计目标缺口时占位）；
	// Inflight 在途/退避明细（面板可见：这些号这轮为什么没被再派）。
	InflightPending int        `json:"inflight_pending"`
	Inflight        []Inflight `json:"inflight,omitempty"`
	// IdleStreak 连续空转轮数（2026-09-23 降噪）：>0 时轮询间隔按 ×2 拉长（上限 60s）；
	// 有动作/有可动作候选立刻归零 —— 面板据此解释"为什么最近没动作"。
	IdleStreak int `json:"idle_streak"`
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

	// inflight 在途/退避记账（2026-09-23 P1）：account → 最近一次派发与退避窗口。
	inflight map[string]*roamAttempt

	// —— 日志降噪状态（2026-09-23，纯日志；lastAct 不受影响）——
	throttle   map[string]time.Time // 空转行：键 → 上次真正写日志的时间
	goalState  bool                 // 上一轮是否"达标"（翻转时立刻打一条）
	goalLogAt  time.Time            // 达标行上次真正写日志的时间（心跳）
	actLogAt   time.Time            // 动作行上次真正写日志的时间（聚合窗口）
	actDispN   int                  // 聚合窗口内被抑制的"补位"次数/号数
	actDispAcc int
	actRecN    int
	actRecAcc  int
	idleStreak int // 连续"无动作且无活可干"的轮数（空转退避用）
}

// New 创建保持器（path 通常为 <数据目录>/roampool.json；不自动读盘，由 Load 负责）。
func New(path string, d Deps) *Keeper {
	if d.Log == nil {
		d.Log = func(string, ...any) {}
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	return &Keeper{path: path, d: d, cfg: DefaultConfig(),
		inflight: map[string]*roamAttempt{}, throttle: map[string]time.Time{}}
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
	k.logf("[ROAMPOOL] 已恢复参数：enabled=%v target=%d 间隔=%ds 单轮≤%d 限时=%d 分钟 均匀=%v 立刻回收=%v 白名单=%v 任务图降权=+%d/%v 在途TTL=%ds 退避=%v",
		cfg.Enabled, cfg.Target, cfg.IntervalSec, cfg.MaxStep, cfg.Minutes, cfg.Balance, cfg.ReclaimOnDeficit, cfg.Maps,
		cfg.TaskMapBias, cfg.TaskMaps, cfg.InflightTTLSec, cfg.BackoffSec)
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
	k.logf("[ROAMPOOL] 参数已更新：enabled=%v target=%d 间隔=%ds 单轮≤%d 限时=%d 分钟 均匀=%v 立刻回收=%v 白名单=%v 档位=%q 任务图降权=+%d/%v 在途TTL=%ds 退避=%v",
		c.Enabled, c.Target, c.IntervalSec, c.MaxStep, c.Minutes, c.Balance, c.ReclaimOnDeficit, c.Maps, c.Mode,
		c.TaskMapBias, c.TaskMaps, c.InflightTTLSec, c.BackoffSec)
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
// （NAV/CLICK/DIALOG/FIGHT/SHOP/ALLOC/WAIT_NEXT/**SUBMIT**，以及 WAIT_TASK 且任务索引非 0）、
// 以及 **ERROR**（卡住/停链的异常态）都不算空闲。
//
// 判据复用 waterline.Busy —— 与在线水位保持器**同一口径**（否则会出现"水位以为它空闲、游荡池以为它在忙"
// 这种两套判据打架的场面）。
//
// WAIT_GHOST（钟馗等刷鬼的"等待段"）**不在** Busy 黑名单里 → 无活跃抓鬼会话时算空闲。
// 与 Interruptible 的白名单口径一致（都把等待段当"可中断、只损失本轮"）；有活跃抓鬼会话时
// GhostActive 已经把它判成忙，两处不会打架。若现场认为"等刷鬼也不能派游荡/不能压号"，
// 需用户确认后在 Busy 里单列排除（会同时影响水位压号，别只改这里）。
func Idle(r state.Robot) bool {
	if r.Account == "" || !r.Online {
		return false
	}
	return !waterline.Busy(r)
}

// Interruptible 该号是否"可中断"（超编收敛用，2026-09-23 P1）：允许从任务池转去游荡的号。
//
// 用户口径：**绝不打断**正在战斗/交付/对话/跨图导航的号。超编收敛只动"等待段"的号
// （在等刷鬼、刚启动还没接活、空闲）—— 中断这类号只损失当前这一轮，链路由任务池
// 后续按意图补发恢复（restorer/autotask 会重派；且超编期间池闸本身就拦补发）。
//
//   - 离线 / 战斗中（r.Fight）/ 游荡中 / 孵化中 → false（后者由本池自己管，不重复处理）；
//   - WAIT_GHOST（钟馗等刷鬼/巡逻）、READY（刚启动未接活）、IDLE/ONLINE/空 → true；
//   - FIGHT/SUBMIT/DIALOG/NAV/CLICK/SHOP/ALLOC/WAIT_NEXT → false（推进中，不打断）；
//   - ERROR（机器人上报的卡住/停链态）→ false（异常，先人工处理，别当成可回收的号）。
//
// 与 Idle/Busy 的关系（2026-09-23 口径核对）：ERROR 已并入 waterline.Busy（不派活、不硬压），
// 这里是白名单式实现，天然不含 ERROR/SUBMIT —— 两处结论一致，无冲突。
func Interruptible(r state.Robot) bool {
	if r.Account == "" || !r.Online {
		return false
	}
	if r.Fight || r.Walking() {
		return false
	}
	switch strings.ToUpper(strings.TrimSpace(r.State)) {
	case "WAIT_GHOST", "READY", "IDLE", "ONLINE", "":
		return true
	}
	return false
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
	return MapLoadsAs(robots, maps, nil, 0)
}

// MapLoadsAs 带"任务图虚拟负载偏移"的候选图负载（2026-09-24 降权）：
// Count = 该图在线号数 +（该图是任务图 ? bias : 0）—— **有效负载**。
//
// BalanceAssign（人最少的图优先）与 PickReclaimAs（人多的图先回收）都按有效负载评分：
// 任务图上做任务的号已多，虚拟偏移把它推到"人多"一侧 → 游荡补位自动避开；而任务图很空
// （实际人数少）时有效负载仍可能最低 → 照样会被派（"降权少去，不去也行"的动态口径）。
//
// bias<=0 或 taskSet 为 nil = 无偏移（与 MapLoads 等价）。
func MapLoadsAs(robots []state.Robot, maps []int, taskSet map[int]bool, bias int) []MapLoad {
	if bias < 0 {
		bias = 0
	}
	counts := MapCounts(robots)
	out := make([]MapLoad, 0, len(maps))
	seen := map[int]bool{}
	for _, m := range maps {
		if m <= 0 || seen[m] {
			continue
		}
		seen[m] = true
		c := counts[m]
		if bias > 0 && taskSet[m] {
			c += bias
		}
		out = append(out, MapLoad{MapID: m, Count: c})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MapID < out[j].MapID })
	return out
}

// PickReclaim 从**游荡中**的号里挑 n 个回收：优先挑"所在图**我们号最多**"的那些，
// 顺带纠偏分布（图人数并列时按账号升序，结果稳定、便于测试）。
//
// 2026-09-23 有领双的必须优先抓鬼（用户口径）：**今日已领双倍**的游荡号置顶 —— 双倍
// 有时长，先把它们回收进任务池去抓鬼，别让加成耗在游荡上。
//
// 人数不足 n 就有几个给几个；n<=0 / 没有游荡号 → nil。
//
// 2026-09-22 P0 修复（三池交互分析）：eligible 非 nil 时**只挑"回收后真能进任务池"的号**。
// 原实现只判 Roaming —— 会把"今日满额/等级不够"的号也回收给任务池，而任务池会跳过它们，
// 90s 后机器人端 auto_roam 又派去游荡 → 回收-重派来回损耗（实测 robot0001108/1127/1176
// 各被回收 20+ 次，游荡产出被反复清空）。这两类号留在游荡池继续游荡更有价值。
func PickReclaim(robots []state.Robot, n int, eligible func(state.Robot) bool) []string {
	return PickReclaimAs(robots, n, eligible, nil, 0)
}

// PickReclaimAs 带"任务图虚拟负载偏移"的回收（2026-09-24 降权）：排序用的"图负载"取有效负载
// （人数 + 任务图偏移）—— 任务图天然负载高 → 其上的游荡号**更早被回收**（别滞留在任务图），
// 与"游荡少去任务图"同向。其余口径与 PickReclaim 完全一致（领双置顶/并列按账号/资格闸）。
func PickReclaimAs(robots []state.Robot, n int, eligible func(state.Robot) bool,
	taskSet map[int]bool, bias int) []string {
	if n <= 0 {
		return nil
	}
	if bias < 0 {
		bias = 0
	}
	load := MapCounts(robots) // 各图的在线号数（含非游荡号：口径是"这张图挤了多少我们的号"）
	effLoad := func(mapID int) int {
		c := load[mapID]
		if bias > 0 && taskSet[mapID] {
			c += bias
		}
		return c
	}
	type cand struct {
		account string
		count   int
		prio    bool
	}
	cands := make([]cand, 0, len(robots))
	for _, r := range robots {
		if !Roaming(r) {
			continue
		}
		if r.Paused {
			continue // 2026-09-23 R3：人工暂停的号不回收（回收了也进不了任务池，留着继续游荡）
		}
		if eligible != nil && !eligible(r) {
			continue // 2026-09-22 P0：回收后进不了任务池的号不回收（详见函数头注释）
		}
		cands = append(cands, cand{account: r.Account, count: effLoad(r.MapID),
			prio: r.DoubleClaimedToday()})
	}
	if len(cands) == 0 {
		return nil
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].prio != cands[j].prio {
			return cands[i].prio // 2026-09-23 今日领双 → 优先回收去抓鬼
		}
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
//
// 2026-09-23 R3：人工暂停（面板点过「停止」）的号不派游荡 —— 暂停=别自动打扰它。
func PickIdle(robots []state.Robot, n int) []string {
	if n <= 0 {
		return nil
	}
	out := make([]string, 0, n)
	for _, r := range robots {
		if len(out) >= n {
			break
		}
		if r.Paused {
			continue
		}
		if Idle(r) {
			out = append(out, r.Account)
		}
	}
	return out
}

// PickExcess 从**任务池**（在抓鬼）里挑 n 个"可中断"的号转游荡（超编收敛用，2026-09-23 P1）。
//
// 只挑"真的在抓鬼（GhostActive）且当前可中断（Interruptible）"的号 ——
// 空闲号走 PickIdle 的正常补位；战斗中/交付中/对话中/导航中的号**一律不碰**。
// 按账号升序输出（结果稳定，便于测试与日志对照）。
//
// 2026-09-23 有领双的必须优先抓鬼（用户口径）：**今日已领双倍**的号一律不转游荡 ——
// 双倍有时长，把它从抓鬼拉去游荡等于把加成浪费掉（等它把双倍用完/跨日失效后自然可再转）。
//
// 为什么不用"所在图人数多"排序：超编收敛关心的是"能不能安全中断"，与图负载无关；
// 已经超编的号分散在各图，按账号稳定挑即可（每轮 ≤ MaxStep，不会一次搬空）。
func PickExcess(robots []state.Robot, n int) []string {
	if n <= 0 {
		return nil
	}
	cands := make([]string, 0, len(robots))
	for _, r := range robots {
		if !r.GhostActive() || !Interruptible(r) {
			continue
		}
		if r.Paused {
			continue // 2026-09-23 R3：人工暂停的号不转游荡（用户点过「停止」）
		}
		if r.DoubleClaimedToday() {
			continue // 2026-09-23 今日领双 → 不转游荡（留着抓鬼，双倍有时长）
		}
		cands = append(cands, r.Account)
	}
	if len(cands) == 0 {
		return nil
	}
	sort.Strings(cands)
	if n > len(cands) {
		n = len(cands)
	}
	return cands[:n]
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

// taskMapSet 本轮"任务图降权"集合（2026-09-24）：
//   - 配置 task_maps 非空 → **完全按它**（人工指定，含替换兜底）；
//   - 否则 = 内置兜底集合（defaultTaskMaps）∪ 壳层热读（Deps.TaskMaps，链数据里的任务图）。
//
// 偏移为 0（关闭降权）时返回 nil（省一次集合构造；调用方按 nil = 无偏移处理）。
func (k *Keeper) taskMapSet(cfg Config) map[int]bool {
	if cfg.TaskMapBias <= 0 {
		return nil
	}
	src := cfg.TaskMaps
	if len(src) == 0 {
		src = make([]int, 0, len(defaultTaskMaps)+8)
		src = append(src, defaultTaskMaps...)
		if k.d.TaskMaps != nil {
			src = append(src, k.d.TaskMaps()...)
		}
	}
	out := make(map[int]bool, len(src))
	for _, m := range src {
		if m > 0 {
			out[m] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// taskBiasNote 动作文案里的降权说明（没开降权/没有任务图 → 空串）。
// maps 非空时列出**候选图里命中降权**的图号（"选了哪些任务图"的日志口径）；
// 为空（回收路径不取候选图）则只报偏移与集合大小。
func taskBiasNote(taskSet map[int]bool, bias int, maps []int) string {
	if bias <= 0 || len(taskSet) == 0 {
		return ""
	}
	hits := make([]int, 0, 4)
	for _, m := range maps {
		if taskSet[m] {
			hits = append(hits, m)
		}
	}
	if len(hits) == 0 {
		return fmt.Sprintf("；任务图降权 +%d（%d 张）", bias, len(taskSet))
	}
	return fmt.Sprintf("；任务图降权 +%d（候选命中 %v，共 %d 张）", bias, hits, len(taskSet))
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
	now := k.now()
	running, idle := 0, 0
	for _, r := range robots {
		if Roaming(r) {
			running++
		}
		if Idle(r) {
			idle++
		}
	}
	inflight, pending := k.inflightView(now, cfg)
	taskSet := k.taskMapSet(cfg) // 生效的降权集合（面板可见：偏移 + 张数）
	k.mu.Lock()
	defer k.mu.Unlock()
	return Status{
		Enabled: cfg.Enabled, Target: cfg.Target, IntervalSec: cfg.IntervalSec, MaxStep: cfg.MaxStep,
		Minutes: cfg.Minutes, Balance: cfg.Balance, ReclaimOnDeficit: cfg.ReclaimOnDeficit,
		Maps: cfg.Maps, Mode: cfg.Mode,
		TaskMapBias: cfg.TaskMapBias, TaskMapCount: len(taskSet),
		InflightTTLSec: cfg.InflightTTLSec, BackoffSec: cfg.BackoffSec,
		Running: running, Idle: idle, Deficit: k.deficit(), MapCount: mapCount,
		InflightPending: pending, Inflight: inflight, IdleStreak: k.idleStreak,
		EnvPinned:  EnvPinned(),
		LastAction: k.lastAct, LastRunTS: tsOf(k.lastRun), LastErr: k.lastErr,
	}
}

// Tick 跑一轮（enabled=false 时**什么都不做**，返回 false）。
// 返回本轮是否真的下发了游荡/回收命令。
//
// 判决顺序（用户口径：任务链路优先，游荡吃余量）：
//  1. 回收优先：任务池缺口 > 0 且 reclaim_on_deficit → 回收 min(缺口, max_step, 在游荡) 个游荡号，本轮结束；
//  2. 补位：否则在游荡（含在途占位）< target → 取 min(缺额, max_step, 空闲数) 个空闲号，按图均匀派出去。
//
// 2026-09-23 P1（派发节流降噪）：派出去的号记「在途」——TTL（inflight_ttl_sec，默认 120s）内没等到
// walk.enabled=true 就不再派它；连续未生效按 backoff_sec（默认 60→120→300s）退避；在途数计入目标缺口，
// 避免"上一批还在路上又派一批"。背景：现场 60 派/分、54 拒/分、同号同图 10s 一轮（详见 Inflight 注释）。
func (k *Keeper) Tick(now time.Time) bool {
	cfg := k.Config()
	if !cfg.Enabled {
		return false
	}
	robots := k.robots()
	k.syncInflight(robots) // 先保鲜：已生效（walk.enabled）/已离线的从在途账里清除
	running, idleN := 0, 0
	for _, r := range robots {
		if Roaming(r) {
			running++
		}
		if Idle(r) {
			idleN++
		}
	}
	pending := k.inflightPending(now, cfg) // 在途占位（TTL 内、还没生效的）
	k.mu.Lock()
	k.lastRun = now
	k.mu.Unlock()

	deficit := k.deficit()
	taskSet := k.taskMapSet(cfg) // 2026-09-24 降权：任务图集合（nil = 关闭/无任务图）
	// ① 回收优先：任务池缺人 → 把游荡号还给任务池（本轮不再补位，名额让给任务池）
	if deficit > 0 && cfg.ReclaimOnDeficit {
		n := minInt(deficit, cfg.MaxStep, running)
		picked := PickReclaimAs(robots, n, k.d.ReclaimEligible, taskSet, cfg.TaskMapBias)
		if len(picked) == 0 {
			k.noop(fmt.Sprintf("任务池缺口 %d，但没有可回收的游荡号（在游荡 %d）", deficit, running),
				"noreclaim|d"+notch(deficit)+"|i"+notch(idleN), now)
			k.markRound(false)
			return false
		}
		sent, err := k.doStop(picked)
		if err != nil {
			k.fail("回收", err)
			k.markRound(true) // 出错要按正常节奏重试
			return false
		}
		k.noteAction(actionReclaim, len(sent),
			fmt.Sprintf("回收 %d 个游荡号给任务池（缺口 %d，今日领双优先/其余按图人数从多到少%s）：%s",
				len(sent), deficit, taskBiasNote(taskSet, cfg.TaskMapBias, nil), strings.Join(sent, ",")), now)
		k.markRound(true)
		return true
	}

	// ② 补位：在游荡 + 在途 < 目标 → 优先空闲号；不够且任务池**超编**时，从"可中断的超编号"里补
	//    （2026-09-23 P1 超编温和收敛：把多余的抓鬼号转成游荡 → 任务池回到目标附近、
	//    游荡池回补；每轮 ≤ MaxStep，绝不打断战斗/交付/对话/跨图导航中的号）。
	if running+pending >= cfg.Target {
		k.noteGoal(fmt.Sprintf("游荡池达标（在游荡 %d + 在途 %d / 目标 %d，空闲 %d，缺口 %d）",
			running, pending, cfg.Target, idleN, deficit), now)
		k.markRound(false)
		return false
	}
	want := minInt(cfg.Target-running-pending, cfg.MaxStep)
	pool := k.pickableRobots(robots, now) // P1：剔掉在途/退避窗口内的号（别再重复派）
	idle := PickIdle(pool, want)
	excess := 0
	if len(idle) < want && deficit < 0 {
		// 任务池超编（deficit<0）才有"多余的号"可收；不足部分从可中断的在抓鬼号里补。
		if extra := PickExcess(pool, want-len(idle)); len(extra) > 0 {
			excess = len(extra)
			idle = append(idle, extra...)
		}
	}
	if len(idle) == 0 {
		k.noop(fmt.Sprintf("游荡池差 %d 个，但没有空闲号/可中断的超编号可派（在游荡 %d + 在途 %d / 目标 %d，空闲 %d，任务池缺口 %d）",
			cfg.Target-running-pending, running, pending, cfg.Target, idleN, deficit),
			"nofill|d"+notch(deficit)+"|i"+notch(idleN), now)
		k.markRound(false)
		return false
	}
	excessNote := ""
	if excess > 0 {
		excessNote = fmt.Sprintf("；含超编回收 %d 个（任务池缺口 %d）", excess, deficit)
	}
	if !cfg.Balance {
		sent, err := k.doDispatch(idle, "random")
		if err != nil {
			k.fail("补位", err)
			k.markRound(true)
			return false
		}
		k.noteAction(actionDispatch, len(sent),
			fmt.Sprintf("补位 %d 个（随机图，每号自抽，在游荡 %d / 目标 %d）：%s%s",
				len(sent), running, cfg.Target, strings.Join(sent, ","), excessNote), now)
		k.markDispatch(sent, "random", now, cfg) // P1：在途记账（等 walk.enabled 确认生效）
		k.markRound(true)
		return true
	}
	maps := k.roamMaps(true)
	if len(maps) == 0 {
		k.noop("没有可用的游荡图（链数据里没有带网格的图 / 白名单与网格没有交集）", "nomaps", now)
		k.markRound(false)
		return false
	}
	// 2026-09-24 降权：候选图负载用**有效负载**（任务图 +TaskMapBias）—— BalanceAssign 的
	// "人最少的图优先"自动少去任务图（任务图很空时仍可能被选；人多时避开）。
	groups := BalanceAssign(idle, MapLoadsAs(robots, maps, taskSet, cfg.TaskMapBias), len(idle))
	sent := map[int][]string{} // 只记**真的下发成功**的图（失败的不写进动作文案，别虚报）
	for _, m := range sortedKeys(groups) {
		got, err := k.doDispatch(groups[m], m)
		if err != nil {
			// 单张图失败不影响其它图（下一轮会再补）
			k.fail(fmt.Sprintf("补位→图%d", m), err)
			continue
		}
		sent[m] = got
		k.markDispatch(got, m, now, cfg) // P1：在途记账（记下目标图，退避按"同号"统一算）
	}
	if len(sent) == 0 {
		k.markRound(true)
		return false
	}
	k.noteAction(actionDispatch, countAssigned(sent),
		fmt.Sprintf("补位 %d 个 → %s（在游荡 %d / 目标 %d%s）%s",
			countAssigned(sent), assignText(sent), running, cfg.Target,
			taskBiasNote(taskSet, cfg.TaskMapBias, maps), excessNote), now)
	k.markRound(true)
	return true
}

// Start 周期跑（ctx 结束即退出）。1 秒粒度轮询：interval_sec 改小了下一轮立刻生效。
// 空转（连续无动作、无活可干）时把间隔 ×2 拉长到 idleBackoffMax（60s）——降噪用；
// 一有动作/可动作候选立刻回到 interval_sec。
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
			if !last.IsZero() && now.Sub(last) < k.effectiveInterval(cfg) {
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
//
// 日志降噪（2026-09-23）：三类行各有节流 ——
//   - noop()：空转说明行，同"类型+缺口档+空闲档"noopLogSec 内只打一条；
//   - noteGoal()：达标行，状态翻转时立刻打 + 稳定期心跳一条（goalLogSec）；
//   - noteAction()：动作行，actionLogSec 聚合一条（窗口内首条原样，后续累计成"近Ns 另：…"前缀）。
// **三者都照常更新 k.lastAct**（面板 UI 状态不丢，只是日志行被节流）。

// actionKind 动作行种类（聚合计数用；拼日志时也用它的中文名）。
type actionKind string

const (
	actionDispatch actionKind = "补位"
	actionReclaim  actionKind = "回收"
)

// noop 空转说明行（无动作、无活可干）：同键 noopLogSec 内只真正写一条日志；
// lastAct 每轮都更新（面板显示"最近一次判决说明"不受影响）。
func (k *Keeper) noop(msg, key string, now time.Time) {
	k.mu.Lock()
	k.lastAct, k.lastErr = msg, ""
	k.goalState = false // 非达标
	last, ok := k.throttle[key]
	fire := !ok || now.Sub(last) >= noopLogSec*time.Second
	if fire {
		k.throttle[key] = now
	}
	k.mu.Unlock()
	if fire {
		k.logf("[ROAMPOOL] %s", msg)
	}
}

// noteGoal 达标行：状态翻转（非达标 → 达标）时立刻写一条；稳定期每 goalLogSec 心跳一条。
func (k *Keeper) noteGoal(msg string, now time.Time) {
	k.mu.Lock()
	flip := !k.goalState
	k.goalState = true
	fire := flip || k.goalLogAt.IsZero() || now.Sub(k.goalLogAt) >= goalLogSec*time.Second
	if fire {
		k.goalLogAt = now
	}
	k.lastAct, k.lastErr = msg, ""
	k.mu.Unlock()
	if fire {
		k.logf("[ROAMPOOL] %s", msg)
	}
}

// noteAction 动作行（补位/回收）聚合写日志：窗口内第一条原样打；之后的被抑制并累计，
// 到下一条真正写日志时带上"近Ns 另：补位 x 次/y 个、回收 z 次/w 个"前缀（被抑制的量不静默）；
// lastAct 每次都更新（面板状态不丢）。
func (k *Keeper) noteAction(kind actionKind, n int, msg string, now time.Time) {
	k.mu.Lock()
	if kind == actionReclaim {
		k.actRecN++
		k.actRecAcc += n
	} else {
		k.actDispN++
		k.actDispAcc += n
	}
	fire := k.actLogAt.IsZero() || now.Sub(k.actLogAt) >= actionLogSec*time.Second
	prefix := ""
	if fire {
		// 前缀只报"本行之前被抑制的"量（本行马上就原样打出来，不重复计）。
		supD, supDA, supR, supRA := k.actDispN, k.actDispAcc, k.actRecN, k.actRecAcc
		if kind == actionReclaim {
			supR, supRA = supR-1, supRA-n
		} else {
			supD, supDA = supD-1, supDA-n
		}
		if supD > 0 || supR > 0 {
			prefix = fmt.Sprintf("近%ds 另：补位 %d 次/%d 个、回收 %d 次/%d 个；", actionLogSec, supD, supDA, supR, supRA)
		}
		k.actLogAt = now
		k.actDispN, k.actDispAcc, k.actRecN, k.actRecAcc = 0, 0, 0, 0
	}
	k.lastAct, k.lastErr = msg, ""
	k.goalState = false
	k.mu.Unlock()
	if fire {
		k.logf("[ROAMPOOL] %s%s", prefix, msg)
	}
}

// markRound 记一轮"有没有活可干/干过活"（空转退避用）：acted=true（有动作、有可动作候选、
// 或出错要按正常节奏重试）→ 退避清零；false（真空转）→ 退避档位 +1（上限见 effectiveInterval）。
func (k *Keeper) markRound(acted bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if acted {
		k.idleStreak = 0
		return
	}
	if k.idleStreak < 8 {
		k.idleStreak++
	}
}

// effectiveInterval 本轮该等多久（空转退避，2026-09-23 降噪）：
// 连续空转 → interval×2、×4… 上限 idleBackoffMax（60s）；一有动作/可动作候选立刻回到 interval_sec。
func (k *Keeper) effectiveInterval(cfg Config) time.Duration {
	base := time.Duration(cfg.IntervalSec) * time.Second
	k.mu.Lock()
	streak := k.idleStreak
	k.mu.Unlock()
	d := base
	for i := 0; i < streak && d < idleBackoffMax; i++ {
		d *= 2
	}
	if d > idleBackoffMax {
		d = idleBackoffMax
	}
	if d < base {
		d = base
	}
	return d
}

// ---------------------------------------------------------------- 在途/退避（P1，2026-09-23）

// now 当前时间（Deps.Now 注入；New 已保证非 nil）。
func (k *Keeper) now() time.Time { return k.d.Now() }

// syncInflight 在途账保鲜（每轮先跑）：已生效（walk.enabled=true）/已离线的清除。
// 「已判失败但还在退避窗口内」的记录**保留**（until 管着"不再派"，TTL 只管"算不算在途占位"）。
func (k *Keeper) syncInflight(robots []state.Robot) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.inflight) == 0 {
		return
	}
	live := make(map[string]state.Robot, len(robots))
	for _, r := range robots {
		if r.Account != "" {
			live[r.Account] = r
		}
	}
	for acc, a := range k.inflight {
		r, ok := live[acc]
		if !ok || !r.Online {
			delete(k.inflight, acc) // 这号不在池子里了（离线/状态行被清理）
			continue
		}
		if Roaming(r) { // 生效（walk.enabled=true）→ 结案；连续失败过的记一条（退避解除）
			delete(k.inflight, acc)
			if a.fails > 1 {
				k.logf("[ROAMPOOL] %s 游荡已生效（此前连续 %d 次派发未生效，退避解除）", acc, a.fails)
			}
		}
	}
}

// inflightPending 在途占位号数：仍在「退避/生效等待窗口」内（until 未到）且距上次派发不超过 TTL。
//
// 为什么取两者的小者：until 是"不再派这个号"的窗口（退避 60/120/300s），TTL 是"我们还不知道
// 这次派发算不算成功"的窗口（默认 120s，机器人秒级就该上报 walk.enabled）—— 占位只该持续到
// "结果已知"为止：300s 档位时占位最多 120s（之后就不再挤占目标缺口，让其它号补进来），
// 但 blocked 仍然持续到 until（不重复派）。这样重派节奏恰好是 60→120→300s。
func (k *Keeper) inflightPending(now time.Time, cfg Config) int {
	ttl := time.Duration(cfg.InflightTTLSec) * time.Second
	k.mu.Lock()
	defer k.mu.Unlock()
	n := 0
	for _, a := range k.inflight {
		if now.Before(a.until) && now.Sub(a.at) < ttl {
			n++
		}
	}
	return n
}

// inflightView 在途明细快照（按账号升序；面板用）+ 在途占位数（口径同上）。
func (k *Keeper) inflightView(now time.Time, cfg Config) ([]Inflight, int) {
	ttl := time.Duration(cfg.InflightTTLSec) * time.Second
	k.mu.Lock()
	out := make([]Inflight, 0, len(k.inflight))
	pending := 0
	for acc, a := range k.inflight {
		p := now.Before(a.until) && now.Sub(a.at) < ttl
		if p {
			pending++
		}
		out = append(out, Inflight{Account: acc, MapID: a.mapID, At: a.at, Fails: a.fails,
			Until: a.until, Pending: p})
	}
	k.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Account < out[j].Account })
	return out, pending
}

// pickableRobots 派发候选池：剔掉"退避窗口内（until 未到）"的号（在途/已判失败都算）。
func (k *Keeper) pickableRobots(robots []state.Robot, now time.Time) []state.Robot {
	k.mu.Lock()
	if len(k.inflight) == 0 {
		k.mu.Unlock()
		return robots
	}
	blocked := make(map[string]bool, len(k.inflight))
	for acc, a := range k.inflight {
		if now.Before(a.until) {
			blocked[acc] = true
		}
	}
	k.mu.Unlock()
	if len(blocked) == 0 {
		return robots
	}
	out := make([]state.Robot, 0, len(robots))
	for _, r := range robots {
		if !blocked[r.Account] {
			out = append(out, r)
		}
	}
	return out
}

// markDispatch 记一批"已下发游荡"的号（P1 在途记账）。
//
// 能走到这里说明这些号不在退避窗口内（pickableRobots 已剔除）→ 账上若已有记录，
// 只能是"上一次派发窗口过了还没生效（没等到 walk.enabled）" → 退避升级 60→120→300s（末档为上限）。
func (k *Keeper) markDispatch(accounts []string, mapid any, now time.Time, cfg Config) {
	mid := 0
	if v, ok := mapid.(int); ok {
		mid = v // "random"/非法值 → 0（随机图）
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, acc := range accounts {
		if acc == "" {
			continue
		}
		fails := 1
		if prev, ok := k.inflight[acc]; ok {
			fails = prev.fails + 1
		}
		if n := len(cfg.BackoffSec); n > 0 && fails > n {
			fails = n // 档位封顶（= 退避上限）
		}
		k.inflight[acc] = &roamAttempt{mapID: mid, at: now, fails: fails,
			until: now.Add(backoffWindow(cfg, fails))}
	}
}

// backoffWindow 第 fails 档退避时长（超出档位取最后一档；空档位=0）。
func backoffWindow(cfg Config, fails int) time.Duration {
	list := cfg.BackoffSec
	if len(list) == 0 {
		return 0
	}
	i := fails - 1
	if i < 0 {
		i = 0
	}
	if i >= len(list) {
		i = len(list) - 1
	}
	return time.Duration(list[i]) * time.Second
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

// normBackoff 退避档位规范化：丢掉 <=0/超上限的、去重、升序；空/全非法 → 默认 [60,120,300]。
func normBackoff(in []int) []int {
	seen := map[int]bool{}
	out := make([]int, 0, len(in))
	for _, v := range in {
		if v <= 0 || v > MaxBackoffSec || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	if len(out) == 0 {
		return defaultBackoffSec()
	}
	sort.Ints(out)
	return out
}

// notch 数值分档（日志节流键用）：把"缺口/空闲数"归到粗档，避免每轮一个键导致去重失效。
func notch(n int) string {
	switch {
	case n <= 0:
		return "0"
	case n < 5:
		return "1-4"
	case n < 10:
		return "5-9"
	case n < 50:
		return "10-49"
	default:
		return "50+"
	}
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
