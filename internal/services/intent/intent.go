// Package intent 账号"该跑哪条链"的意图表（P0：判据 + 单链互斥 + 快照）。
//
// 为什么需要它：中控只透传链数据、机器人端只跑链，**"这个号现在该跑哪条链"没人记账**，
// 于是会出现同一个号被两处同时拉起（新手链/抓鬼双跑）。这里把意图收敛成一份表：
//
//   - 判据对齐参考实现：等级 < NewbieMaxLevel(默认 31) 且新手链未完成 → 新手链优先；
//     ≥31 或已完成 → 抓鬼；**等级未知时判"待定"**（等机器人上线报一次等级，不瞎跑）。
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
)

// DefaultNewbieMaxLevel 新手链等级阈值（与参考实现 start_chain_done_level / NEWBIE_MAX_LEVEL 一致）。
const DefaultNewbieMaxLevel = 31

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
	return d
}

// Decide 判定该账号该跑哪条链（level<=0 = 未知 → 待定）。
func (d Decider) Decide(level int, chainDone bool) Decision {
	d = d.normalized()
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
