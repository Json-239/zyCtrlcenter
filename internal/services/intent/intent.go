// Package intent 账号"该跑哪条链"的意图表（P0：判据 + 单链互斥 + 快照）。
//
// 为什么需要它：中控只透传链数据、机器人端只跑链，**"这个号现在该跑哪条链"没人记账**，
// 于是会出现同一个号被两处同时拉起（新手链/抓鬼双跑）。这里把意图收敛成一份表：
//
//   - 判据对齐参考实现：等级 < NewbieMaxLevel(默认 31) 且新手链未完成 → 新手链优先；
//     ≥31 或已完成 → 抓鬼；**等级未知时判"待定"**（等机器人上线报一次等级，不瞎跑）。
//   - 2026-09-23 分享日常（大唐神捕）：等级 ≥ ShareDailyMinLevel(40) 且心跳明确上报"今日未满"
//     且开关启用 → shenbu；否则回落旧判据（详见 DecideDaily）。
//   - 2026-09-24 烽火大唐（fenghuo，P1）：判据与 shenbu 同款独立开关（默认关），
//     两个都满足时 shenbu 优先（详见 DecideDailyStates）。
//   - 一个账号同一时刻只有一条链（切换 = 先停旧链再上新，Apply 会把 prev 还给调用方）。
//   - 选号/上线前用 Conflict() 再校验一次：同账号要跑别的链就是冲突（两侧双保险）。
//
// P0 只做判据与记账；按意图自动补发 start_chain/ghost_start（恢复引擎）在 P1。
package intent

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Kind 意图类型（= 该账号此刻在跑的链）。
type Kind string

const (
	// KindNewbie 新手链（默认链 id newbie_full）
	KindNewbie Kind = "newbie"
	// KindZhuaogui 捉鬼链（20 级支线，允许主动找给予者接取）
	KindZhuaogui Kind = "zhuaogui"
	// KindGhost 抓鬼日常（钟馗捉鬼）
	KindGhost Kind = "ghost"
	// KindIdle 空闲/游荡（不跑任务链）
	KindIdle Kind = "idle"
	// KindShenbu 大唐神捕（分享日常体系，2026-09-23 接入）：等级达标 + 该玩法今日未满 →
	// 跑神捕；判据由 Decider.ShareDailyEnabled 显式打开（默认关，灰度期 P2 再开）。
	KindShenbu Kind = "shenbu"
	// KindFenghuo 烽火大唐（分享日常体系，2026-09-24 P1 接入）：与神捕同口径 ——
	// 等级 ≥40 + 该玩法今日未满 + Decider.FenghuoEnabled（默认关）→ 跑烽火大唐。
	KindFenghuo Kind = "fenghuo"
)

// DefaultNewbieMaxLevel 新手链等级阈值（与参考实现 start_chain_done_level / NEWBIE_MAX_LEVEL 一致）。
const DefaultNewbieMaxLevel = 31

// DefaultShareDailyMinLevel 分享日常等级门槛默认值（大唐神捕票条件：等级 ≥40，20283.xml:7-17）。
const DefaultShareDailyMinLevel = 40

// DefaultShareDailyKey 分享日常默认玩法键（随 share_daily_start 下发；与客户端口径一致）。
const DefaultShareDailyKey = "share_daily_大唐神捕"

// DefaultFenghuoKey 烽火大唐默认玩法键（服务端 20021.xml 的 share_daily_key；
// 与客户端 auto_task.csv 的「share_daily_宫廷10」行一致）。
const DefaultFenghuoKey = "share_daily_宫廷10"

// DailyInfo 一个账号"分享日常"的运行时信息（判据输入；**零值 = 未知 → 不判该玩法**）。
//
// 为什么要有 Known：机器人新版心跳才带 daily 块（{share_key,done,limit,state}）。
// 老版上报（没有 daily）时"是否满额"无从判断，此时必须保守回落旧判据（≥31 → 抓鬼），
// 否则一旦网关/机器人版本不齐，生产上所有 ≥40 号会被判成神捕、抓鬼池瞬间空掉。
type DailyInfo struct {
	Known bool // 心跳里有该玩法的计数（机器人已上报）
	Full  bool // 今日已满/不可用（state=DONE 或 done ≥ limit）
}

// DailyStates 分享日常家族（shenbu / fenghuo）两个玩法各自的运行时信息（判据输入）。
//
// 单独成一个结构（而不是多一个 map）是为了**编译期**绑死两个 kind —— 忘填哪个字段
// 时零值 = 未知 = 保守不判，不会把号误判去别的玩法。
type DailyStates struct {
	Shenbu  DailyInfo // 大唐神捕（share_daily_大唐神捕）
	Fenghuo DailyInfo // 烽火大唐（share_daily_宫廷10）
}

// ErrNotDecided 等级未知：调用方先别登记意图（也不要覆盖已有意图）。
var ErrNotDecided = errors.New("等级未知：等机器人上线报一次等级再判（不瞎跑）")

// Decision 判据结果（面板/排障会直接看 Reason）。
type Decision struct {
	Known   bool   `json:"known"`
	Kind    Kind   `json:"kind,omitempty"`
	ChainID string `json:"chain_id,omitempty"`
	Reason  string `json:"reason"`
}

// Decider 判据（阈值可配，避免与机器人端配置漂移）。
type Decider struct {
	NewbieMaxLevel  int
	NewbieChainID   string
	ZhuaoguiChainID string
	// ShareDaily* 分享日常判据（2026-09-23 大唐神捕）：
	//   - ShareDailyEnabled=false（默认）→ **不做** shenbu 判定，行为与旧版完全一致（≥31 全判抓鬼）；
	//   - 打开后：等级 ≥ ShareDailyMinLevel（默认 40）且心跳明确上报"该玩法今日未满"→ 判 shenbu。
	// 灰度顺序（P2）：先开本开关 + 神捕池，再逐批放号。
	ShareDailyEnabled  bool
	ShareDailyMinLevel int
	ShareDailyKey      string
	// Fenghuo* 烽火大唐判据（2026-09-24 P1 接入，口径与 shenbu 完全同款）：
	//   - FenghuoEnabled=false（默认）→ **不做** fenghuo 判定（与旧版行为一致）；
	//   - 打开后：等级 ≥ FenghuoMinLevel（默认 40，服务端票条件）且心跳明确"该玩法今日未满"
	//     → 判 fenghuo。
	// 两个玩法都启用且都未满时，按 Kinds 次序 **shenbu 优先**（先跑满一条再转下一条；
	// 前端队列固定次序 ghost→newbie→shenbu→fenghuo 同一口径）。
	FenghuoEnabled  bool
	FenghuoMinLevel int
	FenghuoKey      string
}

func (d Decider) normalized() Decider {
	if d.NewbieMaxLevel <= 0 {
		d.NewbieMaxLevel = DefaultNewbieMaxLevel
	}
	if d.NewbieChainID == "" {
		d.NewbieChainID = "newbie_full"
	}
	if d.ZhuaoguiChainID == "" {
		d.ZhuaoguiChainID = "zhuaogui"
	}
	if d.ShareDailyMinLevel <= 0 {
		d.ShareDailyMinLevel = DefaultShareDailyMinLevel
	}
	if d.ShareDailyKey == "" {
		d.ShareDailyKey = DefaultShareDailyKey
	}
	if d.FenghuoMinLevel <= 0 {
		// 烽火大唐票条件同为等级 ≥40（20021.xml:12-14），与神捕同值但**独立可配**。
		d.FenghuoMinLevel = DefaultShareDailyMinLevel
	}
	if d.FenghuoKey == "" {
		d.FenghuoKey = DefaultFenghuoKey
	}
	return d
}

// Decide 判定该账号该跑哪条链（level<=0 = 未知 → 待定）。
//
// 不带分享日常信息（等价于"未知"）→ 保持旧判据；要判 shenbu 请用 DecideDaily。
func (d Decider) Decide(level int, chainDone bool) Decision {
	return d.DecideDaily(level, chainDone, DailyInfo{})
}

// DecideDaily 同上，附该号"分享日常"的运行时信息（心跳 daily 块解析结果）。
//
// 只带 shenbu（大唐神捕）一个玩法的信息 —— 旧调用/旧测试的兼容入口；
// 要同时判 shenbu + fenghuo（烽火大唐）请用 DecideDailyStates。
func (d Decider) DecideDaily(level int, chainDone bool, day DailyInfo) Decision {
	return d.DecideDailyStates(level, chainDone, DailyStates{Shenbu: day})
}

// DecideDailyStates 判定该账号该跑哪条链（附分享日常家族两个玩法的运行时信息）。
//
// 判据（2026-09-23 方案 §4.3 G2 + 2026-09-24 烽火大唐 P1）：
//
//	等级 ≥ 门槛(默认 40) + 该玩法今日未满 + 开关启用 → 大唐神捕 / 烽火大唐
//	   （两个都满足时 **shenbu 优先** —— 与前端队列固定次序 ghost→newbie→shenbu→fenghuo 同口径）
//	31~39（或日常不可用/满额/未知/开关关）        → 抓鬼
//	< 31 且未毕业                                  → 新手链
//	等级未知                                       → 待定（不登记、不覆盖已有意图）
func (d Decider) DecideDailyStates(level int, chainDone bool, days DailyStates) Decision {
	d = d.normalized()
	graduated := chainDone || (level > 0 && level >= d.NewbieMaxLevel)
	if graduated && d.ShareDailyEnabled && days.Shenbu.Known && !days.Shenbu.Full &&
		level >= d.ShareDailyMinLevel {
		return Decision{
			Known: true, Kind: KindShenbu,
			Reason: fmt.Sprintf("等级 %d ≥ %d 且大唐神捕今日未满 → 大唐神捕", level, d.ShareDailyMinLevel),
		}
	}
	if graduated && d.FenghuoEnabled && days.Fenghuo.Known && !days.Fenghuo.Full &&
		level >= d.FenghuoMinLevel {
		return Decision{
			Known: true, Kind: KindFenghuo,
			Reason: fmt.Sprintf("等级 %d ≥ %d 且烽火大唐今日未满 → 烽火大唐", level, d.FenghuoMinLevel),
		}
	}
	switch {
	case chainDone:
		return Decision{Known: true, Kind: KindGhost, Reason: "新手链已完成 → 转抓鬼"}
	case level >= d.NewbieMaxLevel:
		return Decision{
			Known: true, Kind: KindGhost,
			Reason: fmt.Sprintf("等级 %d ≥ %d → 抓鬼", level, d.NewbieMaxLevel),
		}
	case level > 0:
		return Decision{
			Known: true, Kind: KindNewbie, ChainID: d.NewbieChainID,
			Reason: fmt.Sprintf("等级 %d < %d → 新手链优先", level, d.NewbieMaxLevel),
		}
	default:
		return Decision{Known: false, Reason: "等级未知：等机器人上线报一次等级再判（不瞎跑）"}
	}
}

// ShareDailyKeyOf 返回归一化后的分享日常（大唐神捕）玩法键（下发给机器人用）。
func (d Decider) ShareDailyKeyOf() string { return d.normalized().ShareDailyKey }

// FenghuoKeyOf 返回归一化后的烽火大唐玩法键（下发给机器人用）。
func (d Decider) FenghuoKeyOf() string { return d.normalized().FenghuoKey }

// Intent 一个账号当前意图。
type Intent struct {
	Account string `json:"account"`
	Kind    Kind   `json:"kind"`
	ChainID string `json:"chain_id,omitempty"`
	Zone    string `json:"zone,omitempty"`
	Source  string `json:"source,omitempty"` // decide（判据）/ manual（手动）/ restore（重启恢复）
	Reason  string `json:"reason,omitempty"`
	Since   int64  `json:"since"`
}

// Plan 意图表（并发安全；键 = 账号）。
type Plan struct {
	mu    sync.Mutex
	d     Decider
	items map[string]Intent
}

// NewPlan 创建意图表（零值 Decider 用默认阈值 31）。
func NewPlan(d Decider) *Plan {
	return &Plan{d: d.normalized(), items: map[string]Intent{}}
}

// Decider 返回（归一化后的）判据，供调用方复用。
func (p *Plan) Decider() Decider {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.d
}

// Apply 登记/切换意图；返回切换前的意图（Kind 非空说明"该账号原本在跑别的链，要先停"）。
func (p *Plan) Apply(account string, dec Decision, zone, source string) (prev Intent, changed bool, err error) {
	account = strings.TrimSpace(account)
	if account == "" {
		return Intent{}, false, errors.New("账号不能为空")
	}
	if !dec.Known {
		if _, ok := p.Get(account); ok {
			return Intent{}, false, ErrNotDecided // 已有意图：别被未知信息覆盖
		}
		return Intent{}, false, ErrNotDecided
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	old, had := p.items[account]
	if had && old.Kind == dec.Kind && old.ChainID == dec.ChainID && old.Zone == zone {
		return old, false, nil // 幂等：不刷 Since
	}
	it := Intent{
		Account: account, Kind: dec.Kind, ChainID: dec.ChainID,
		Zone: zone, Source: source, Reason: dec.Reason, Since: time.Now().Unix(),
	}
	p.items[account] = it
	return old, true, nil
}

// Get 取某账号意图。
func (p *Plan) Get(account string) (Intent, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	it, ok := p.items[account]
	return it, ok
}

// Remove 移除意图（下线/换号后调用）；返回是否真的删到了。
func (p *Plan) Remove(account string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.items[account]; !ok {
		return false
	}
	delete(p.items, account)
	return true
}

// Conflict 该账号是否正跑"别的"链（kind 传想跑的那条；选号/上线前校验）。
func (p *Plan) Conflict(account string, kind Kind) bool {
	it, ok := p.Get(account)
	return ok && it.Kind != kind
}

// Snapshot 全量快照（按账号升序，面板用）。
func (p *Plan) Snapshot() []Intent {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Intent, 0, len(p.items))
	for _, it := range p.items {
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Account < out[j].Account })
	return out
}

// Counts 每种意图各几个。
func (p *Plan) Counts() map[string]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]int{}
	for _, it := range p.items {
		out[string(it.Kind)]++
	}
	return out
}
