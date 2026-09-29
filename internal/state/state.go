// Package state 全局状态容器（骨架层：只存中控运行期状态，不含业务编排状态）。
//
// 所有跨模块共享的可变状态收敛于此；路由/服务通过注入的 *State 访问。
// 业务状态（如定时编排/游荡池/任务意图）请在使用方模块内自行维护。
package state

import (
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Robot 单个机器人状态（对应 status_reply/robot_state 上报字段）。
type Robot struct {
	Account string `json:"account"`
	// Zone 中控侧归属区（"<服key>/<区key>"，由控制通道在事件入队时打标）——命令路由依据。
	Zone string `json:"zone,omitempty"`
	// Server 机器人上报的游戏服地址（如 "47.96.8.240:2400"）。
	Server    string `json:"server,omitempty"`
	Online    bool   `json:"online"`
	State     string `json:"state"`
	TaskIndex int    `json:"task_index"`
	Done      int    `json:"done"`
	ChainDone bool   `json:"chain_done"`
	// GhostDoneToday 今天抓鬼已满/不可用（面板标记；跨日自动 false）。
	// 由 Snapshot() 从 ghostUnavail 表填入 —— 不存 robot 行：行会被 removeAccount
	// 删掉、标记会随之丢失（2026-09-22 踩过坑）。
	GhostDoneToday bool `json:"ghost_done_today,omitempty"`
	// Paused 人工暂停（2026-09-23 操作健壮性 R3）：用户在面板点了「停止/停链」时打标，
	// 各编排（恢复引擎/定时任务/水位/游荡池）派发前跳过暂停号，直到用户再次
	// 「启动 / 立即补发 / 上线」清标。
	// 与 GhostDoneToday 同款：由 Snapshot() 从 paused 表填入，**不存 robot 行**
	// （行会被 Remove 删掉，标记必须独立存活）。
	Paused bool `json:"paused,omitempty"`
	// RestoreCapped 当日卡死熔断（2026-09-23）：当日卡死 ≥ ChurnLimit 次后，该号到
	// **次日 0 点**前不再自动重登/补发/被自动任务拉起（等次日自动恢复或手动解除）。
	// 与 GhostDoneToday/Paused 同款：Snapshot() 从 restoreCap 表填入，**不存 robot 行**
	// （行会被 Remove 删掉；独立存活才能挡住"删行→计数归零→同日又重登 3 次"）。
	RestoreCapped bool `json:"restore_capped,omitempty"`
	// CapUntil 熔断失效时刻（RFC3339 本地时间；RestoreCapped=true 时有意义）。
	CapUntil string `json:"cap_until,omitempty"`
	// CapReason 触顶原因（机器人给的原文，面板显示"为什么被熔断"）。
	CapReason string `json:"cap_reason,omitempty"`
	RoleName  string `json:"role_name,omitempty"`
	Level     int    `json:"level"`
	// LevelPending / LevelPendingN 等级"大幅回退"待确认（连续 LevelPendingN 次上报同一新值才真切换，
	// 见 event.applyLevel）：单次错值不覆盖已确认等级、不触发意图切换。
	// json:"-"：纯内部防抖状态，不进 status 接口/面板。
	LevelPending  int   `json:"-"`
	LevelPendingN int   `json:"-"`
	MapID         int   `json:"mapid"`
	Fight         bool  `json:"fight"`
	Pos           []int `json:"pos,omitempty"`      // 服务端像素坐标（机器人上报原值）
	PosGrid       []int `json:"pos_grid,omitempty"` // 客户端显示的格子坐标 = 像素 / GridCell(16)
	Fpp           int   `json:"fpp,omitempty"`
	// Money / Deposit / Reserve / Kaiyuan 货币详情（机器人经 90353 全量 / 90073 增量解析后心跳上报）：
	// Money=银两(服务端 keys.MONEY=9560) / Deposit=钱庄存款(9561) / Reserve=储备金(9617)
	// / Kaiyuan=开元通宝(服务端 9727 COOKIE_KAIYUAN_MONEY，2026-09-24 接入)。
	// MapView「选择机器人」列表显示用；0 值不序列化（前端显示"-"）。
	Money   int64 `json:"money,omitempty"`
	Deposit int64 `json:"deposit,omitempty"`
	Reserve int64 `json:"reserve,omitempty"`
	Kaiyuan int64 `json:"kaiyuan,omitempty"`
	// DoubleClaimDate 今日已成功领取双倍经验的日期（机器人端 pre_daily 上报，YYYYMMDD；
	// 未领=空）。双倍**有时长**（1/2/4 小时）且每周总量受限 —— 2026-09-23 用户口径
	// "有领双的必须优先抓鬼"：调度侧据此把该号抓鬼候选置顶、超编回收/派游荡时避开。
	DoubleClaimDate string         `json:"double_claim_date,omitempty"`
	RoleID          int            `json:"role_id,omitempty"`
	Ghost           map[string]any `json:"ghost,omitempty"`
	// FightStats 今日战斗统计（机器人上报：total/wild/ghost/dur_ms/in_fight；上大屏用）。
	// 与 ghost 同口径：跨日由机器人端按日期归零（stat_begin 的 date 字段）。
	FightStats  map[string]any `json:"fight_stats,omitempty"`
	GhostTarget []int          `json:"ghost_target,omitempty"`
	// Hatch 孵化会话（机器人上报原样透传）：{active,kind,egg_item,mapid,battles,hatched,reason,since_ms}。
	// 与 ghost 同口径：字段存在但 active=false 不算在孵化。
	Hatch map[string]any `json:"hatch,omitempty"`
	// Daily 分享日常进度（机器人上报原样透传，2026-09-23 契约）：
	// 单条 {share_key,done,limit,state} 或数组（多日常）；老版机器人不带上报 → nil（未知）。
	// 判据/总览用 DailyEntries()/DailyFull() 解析（形状容错，空值安全）。
	Daily any `json:"daily,omitempty"`
	// GhostRequiredLevel 服务端要求的抓鬼等级（来自 ghost_offline.required_level）。
	// 低于它的号不再派抓鬼（等级当天不会变，所以不做当日失效）。
	GhostRequiredLevel int `json:"ghost_required_level,omitempty"`
	// StuckCount / StuckDay 当日"卡死"次数（GHOST_DIALOG_STUCK / TASK_STUCK，以及看门狗的
	// STUCK_<STATE> 一律计入，**不随进度清零**）：
	// 反复卡死又被重登拉起的号会 churn，达阈值当天不再派（跨日自动归零）。
	StuckCount int    `json:"stuck_count,omitempty"`
	StuckDay   string `json:"stuck_day,omitempty"`
	HP         []int  `json:"hp,omitempty"` // [当前, 上限]（机器人上报就是二元数组）
	MP         []int  `json:"mp,omitempty"` // [当前, 上限]
	Bag        any    `json:"bag,omitempty"`
	// BagFullAgeMS 最近一次"包裹满"类**权威拒绝**距今的毫秒数（机器人端心跳上报，
	// 2026-09-29 契约）：源 = 服务端"包满"通知（528/549/839/63 等）的时间戳，与
	// m_share_daily.bag_full_ms 同源；**不要求会话 enabled**（因包满而停止的号也要能报出，
	// 否则"停了别马上再派"的信号正好丢失）。
	//
	// 0 / 字段缺失 = 未知/从未（判据侧一律不拦；旧版机器人不带 → 天然兼容）。
	// 消费方：分享日常候选 / roampool 回收资格（api.dailyBagTooFull 双证据之一，窗口 30 分钟）。
	BagFullAgeMS int64 `json:"bag_full_age_ms,omitempty"`
	Equip        any   `json:"equip,omitempty"`
	Summons      any   `json:"summons,omitempty"`
	Booth        any   `json:"booth,omitempty"`
	// Walk 游荡状态（机器人上报原样透传）：{enabled,mapid,state}。
	// 2026-09-22 试跑实测：号刚被派去半月岛，恢复引擎就把它当"没任务"补发 ghost_start
	// （游荡内部会互相排斥地停掉抓鬼），游荡被打断 → 加这个字段让恢复/自动任务让路。
	Walk any `json:"walk,omitempty"`
	// SvrOnline 全服在线（含真实玩家）：机器人 `@online` 回执解析后上报，{count,ts}。
	// 只有"查到过"的那个号会带这个字段（整进程 2 分钟查一次），中控取最新一条展示。
	SvrOnline map[string]any `json:"svr_online,omitempty"`
	// Team 组队状态（机器人心跳上报原样透传，2026-09-29 阶段 2 契约）：
	// {role:captain|member, captain:<队长账号>, members:[<账号>], parted, setup_done, token_use_ts?}
	// 不在队时机器人省略/置 null → nil（消费方按"不在队"处理；心跳缺失会清残留）。
	// 用途：组队派发避让（普通 ghost_start 不发队内号）、/api/team/status 观测。
	Team  any  `json:"team,omitempty"`
	Route any  `json:"route,omitempty"`
	HS    bool `json:"hs"` // 游戏服握手完成（≠登录成功）
	// HasEgg 是否有坐骑蛋（装备栏五行珠槽在孵 or 背包里有）—— 孵化池过滤用；机器人上报。
	HasEgg    bool    `json:"has_egg,omitempty"`
	ErrCode   string  `json:"err_code,omitempty"`
	ErrTS     float64 `json:"err_ts,omitempty"`
	ErrRepeat int     `json:"err_repeat,omitempty"`
	ErrMsg    string  `json:"err_msg,omitempty"`
	LastTask  int     `json:"last_task,omitempty"`
	LastSeen  float64 `json:"last_seen"`
}

// DoubleClaimedToday 今日是否**已成功领取**双倍经验（机器人心跳 double_claim_date = YYYYMMDD）。
//
// 2026-09-23 用户口径："有领双的必须优先抓鬼" —— 双倍有时长，领了不马上用就是浪费。
// 调度侧据此：抓鬼候选置顶（autotask Priority）、超编回收/派游荡避开（roampool）。
// 跨日自动失效：机器人端按本地日期上报，中控与机器人同机同时区，口径一致；
// 缺字段/旧版上报（空串）→ false，保持旧行为。
func (r Robot) DoubleClaimedToday() bool {
	if r.DoubleClaimDate == "" {
		return false
	}
	return r.DoubleClaimDate == time.Now().Format("20060102")
}

// GhostActive 是否有**活跃**抓鬼会话（机器人上报的 ghost.enabled=true；字段存在但已停
// 不算）。与前端 Dashboard「👻抓鬼」筛选用同一口径。老版上报缺 enabled 字段时按
// "有会话"保守处理（不误伤）。
func (r Robot) GhostActive() bool {
	if r.Ghost == nil {
		return false
	}
	if en, ok := r.Ghost["enabled"].(bool); ok {
		return en
	}
	return true
}

// HatchActive 是否有**活跃**孵化会话（机器人上报的 hatch.active=true 且未孵出）。
//
// 口径：
//   - 缺 active 字段 / active=false → 不算在孵化（等机器人下一次心跳补报，别按"有会话"乐观处理：
//     孵化是有时长的动作，误算会让保持数永远"已达标"、不再补号）；
//   - hatched=true（已孵出）→ **不算**在孵化（该号已收工，running 必须立刻减 1）。
func (r Robot) HatchActive() bool {
	if r.Hatch == nil {
		return false
	}
	if b, ok := r.Hatch["hatched"].(bool); ok && b {
		return false
	}
	en, ok := r.Hatch["active"].(bool)
	return ok && en
}

// DailyEntry 一条"分享日常"进度（机器人心跳 daily 块解析结果，2026-09-23 契约）。
type DailyEntry struct {
	ShareKey string `json:"share_key"`
	Done     int    `json:"done"`  // 今日已完成次数
	Limit    int    `json:"limit"` // 日限（0=未知/不限）
	State    string `json:"state"` // 机器人给的相位（如 RUNNING/DONE；仅展示与判满用）
}

// DailyEntries 解析心跳 daily 块（兼容单对象与数组；无数据/形状不对 → nil）。
//
// 契约（方案 §7）：机器人上报 {share_key, done, limit, state}，可选多日常数组。
// 老版机器人不带 daily → nil（判据侧按"未知"保守处理，不判 shenbu）。
func (r Robot) DailyEntries() []DailyEntry {
	if r.Daily == nil {
		return nil
	}
	parseOne := func(m map[string]any) (DailyEntry, bool) {
		key, _ := m["share_key"].(string)
		if key == "" {
			return DailyEntry{}, false
		}
		e := DailyEntry{ShareKey: key, Done: numToInt(m["done"]), Limit: numToInt(m["limit"])}
		e.State, _ = m["state"].(string)
		return e, true
	}
	out := []DailyEntry{}
	switch v := r.Daily.(type) {
	case map[string]any:
		if e, ok := parseOne(v); ok {
			out = append(out, e)
		}
	case []any:
		for _, it := range v {
			if m, ok := it.(map[string]any); ok {
				if e, ok := parseOne(m); ok {
					out = append(out, e)
				}
			}
		}
	case []map[string]any:
		for _, m := range v {
			if e, ok := parseOne(m); ok {
				out = append(out, e)
			}
		}
	}
	return out
}

// DailyOf 取某玩法的进度条目（没有返回 false）。
func (r Robot) DailyOf(shareKey string) (DailyEntry, bool) {
	if shareKey == "" {
		return DailyEntry{}, false
	}
	for _, e := range r.DailyEntries() {
		if e.ShareKey == shareKey {
			return e, true
		}
	}
	return DailyEntry{}, false
}

// DailySessionActive 是否有"未停未满"的分享日常会话（心跳 daily 块）。
//
// 用途（2026-09-29 假补位止血）：分享日常的 ACCEPT（接取）/KILL（打鬼）/READY（轮间准备）
// 相位不在 waterline.Busy 的通用状态白名单里 —— 运行中的神捕/烽火号会被当"没在忙"：
// 池候选反复重派（消费名额、deficit 失真）、游荡/水位可能来打扰。Busy 用本判据把
// "有活跃日常会话"的号标忙（只影响"别打扰"方向）；STOPPED=已停会话不算，满额（DONE/到限）不算。
func (r Robot) DailySessionActive() bool {
	for _, e := range r.DailyEntries() {
		if strings.EqualFold(e.State, "STOPPED") {
			continue
		}
		if !DailyEntryFull(e) {
			return true
		}
	}
	return false
}

// DailyFull 该玩法今日是否已满/不可用（state=DONE 或 limit>0 且 done ≥ limit）。
//
// **无数据 = false（未知）**：本函数只表达"明确满了"；"未知"与"未满"的区分由调用方
// 用 DailyOf 的 ok 判断（intent.DailyInfo.Known）。
func (r Robot) DailyFull(shareKey string) bool {
	e, ok := r.DailyOf(shareKey)
	return ok && DailyEntryFull(e)
}

// DailyEntryFull 单条进度是否"已满/不可用"（state=DONE 或 limit>0 且 done ≥ limit）。
//
// 主判据是 **done ≥ limit**：机器人端满额停止时 state 会被 __request_stop 覆盖成 STOPPED
// （DONE 常量几乎不会出现在心跳里，gp-1 2026-09-23 确认）。
func DailyEntryFull(e DailyEntry) bool {
	if strings.EqualFold(e.State, "DONE") {
		return true
	}
	return e.Limit > 0 && e.Done >= e.Limit
}

// Walking 是否正在"游荡"（或正在孵化 —— 孵化内部就是游荡到孵化图打暗雷）。
//
// 恢复引擎/自动任务据此**让路**：游荡与抓鬼在机器人端互斥（random_walk 启动时会
// 停掉抓鬼），若恢复引擎按"无抓鬼会话"补发 ghost_start，会把游荡号抢回抓鬼
// （2026-09-22 半月岛试跑实测：2 个号刚下发挥荡就被补发打断）。
func (r Robot) Walking() bool {
	if r.HatchActive() {
		return true
	}
	m, ok := r.Walk.(map[string]any)
	if !ok {
		return false
	}
	en, ok := m["enabled"].(bool)
	return ok && en
}

// TeamBlock 组队心跳块（无块/形状不符 → nil；见 Robot.Team 字段注释）。
func (r Robot) TeamBlock() map[string]any {
	m, _ := r.Team.(map[string]any)
	return m
}

// TeamRole 心跳上报的队内角色（"captain"/"member"；不在队/未上报 → ""）。
func (r Robot) TeamRole() string {
	m := r.TeamBlock()
	if m == nil {
		return ""
	}
	s, _ := m["role"].(string)
	return s
}

// TeamCaptainAccount 心跳上报的队长账号（老口径/手工路径；不在队 → ""）。
//
// 注意：机器人端 2026-09-29 定稿的 team 块**只有 role_id、拿不到账号**（"captain_role_id"），
// 消费方（api.teamRoleOf）应先用本函数，空则退回 TeamCaptainRoleID() 再 rid→账号反查。
func (r Robot) TeamCaptainAccount() string {
	m := r.TeamBlock()
	if m == nil {
		return ""
	}
	s, _ := m["captain"].(string)
	return s
}

// TeamCaptainRoleID 心跳 team 块的队长 role_id（定稿字段 captain_role_id；无 → 0）。
func (r Robot) TeamCaptainRoleID() int {
	m := r.TeamBlock()
	if m == nil {
		return 0
	}
	switch v := m["captain_role_id"].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	}
	return 0
}

// TeamParted 是否暂离中（队友暂离=不参战/不被队长交付覆盖，观测用）。
func (r Robot) TeamParted() bool {
	m := r.TeamBlock()
	if m == nil {
		return false
	}
	b, _ := m["parted"].(bool)
	return b
}

// TeamSetupDone 建队集结是否已完成（心跳上报；观测用）。
func (r Robot) TeamSetupDone() bool {
	m := r.TeamBlock()
	if m == nil {
		return false
	}
	b, _ := m["setup_done"].(bool)
	return b
}

// TeamTokenUseTS 助战令最近一次使用时间（机器人本地毫秒；无上报 → 0 = 未知）。
// 领战令 1 令=60 分钟（计划 §3.4/D14），面板据此显示"令剩余时间"。
func (r Robot) TeamTokenUseTS() int64 {
	m := r.TeamBlock()
	if m == nil {
		return 0
	}
	switch v := m["token_use_ts"].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	}
	return 0
}

// ClearRuntimeBusyOnRestart 机器人进程重启（hello 握手）把该号标记离线时，**同步清运行时忙态**。
//
// 背景（2026-09-29 重登停滞事故）：hello 清扫原先只清 Online/HS，残留 State=FIGHT/Walking/
// GhostActive 等"在忙"字段 → `api/waterline.go` 的 `Busy: hasLive && Busy(r)` 把
// "已离线但仍显示在忙"的号全部滤出补号候选（544 可用 → ~77）→ 水位池空、无人补号；
// restorer 又按 state=FIGHT 判"在跑"跳过 → 双重致盲（人工 add×3 才恢复）。
//
// 清理边界（**只清运行时忙态**）：
//   - 清：Online/HS → false；State → "OFFLINE"；Fight=false；Walk/Hatch → nil；
//     Ghost.enabled → false（保留 done/limit/count_date/state，进度展示不丢）；
//     Booth → nil（重启=收摊，否则面板残留"摆摊中"）。
//   - **不动**：满额标记（St 独立表）、意图表、派发台账/在途、等级/货币/链完成、
//     卡死计数/熔断（St 表）、组队心跳块（等下次心跳自然刷）。
func (r *Robot) ClearRuntimeBusyOnRestart() {
	r.Online = false
	r.HS = false
	r.State = "OFFLINE"
	r.Fight = false
	r.Walk = nil
	r.Hatch = nil
	if r.Ghost != nil {
		m2 := make(map[string]any, len(r.Ghost))
		for k, v := range r.Ghost {
			m2[k] = v
		}
		m2["enabled"] = false // GhostActive 判据；done/limit/count_date/state 保留（进度展示）
		r.Ghost = m2
	}
	r.Booth = nil
}

// State 全局状态（并发安全）。
type State struct {
	// ghostUnavail 账号 → "今天抓鬼不可用/已满"的日期串（跨日自动失效）。
	//   独立于 robots 行：行会被 removeAccount 删掉，标记必须留着（否则会被反复拉起）。
	ghostUnavail map[string]string
	// paused 账号 → 人工暂停（2026-09-23 R3）。独立于 robots 行：行会被 Remove 删掉，
	//   暂停意图必须留着（下线再上线仍保持暂停，直到用户显式「启动/补发/上线」）。
	paused map[string]bool
	// restoreCap 账号 → 当日卡死熔断记录（2026-09-23）。同样独立于 robots 行：
	//   行会被 Remove 删掉（陈旧清理/下机），熔断标记必须独立存活 + 跨日惰性失效。
	restoreCap map[string]restoreCap
	// shareDailyFull "账号|玩法键" → "今天该玩法已满/不可用"的日期串（跨日自动失效）。
	//
	// 为什么需要独立表：机器人端**满额后会 request_stop（enabled=false）→ 心跳 daily 变 None**
	//（client.py: 只有 enabled=true 才上报 daily），中控从此看不到"已满"——不记的话
	// 候选会反复派、被机器人 done_limit 拒（还会打断该号已转的游荡/抓鬼）。
	// 写入点：心跳看到 done≥limit（含 state=DONE）的那一刻（event.onStatusReply）。
	shareDailyFull map[string]string
	// shareDailyAssigned "账号|玩法键" → "今天已成功下发该玩法"的日期串（跨日自动失效）。
	//
	// 为什么需要：「启动 = 恢复当前任务」（2026-09-24 用户口径）原先靠心跳 daily 判"在跑"，
	// 但**机器人端分享日常模块没有状态持久化** —— 机器人进程一重启，心跳 daily 块就没了，
	// 判定信号随之中断（掉任务/停机后点启动会错判成败其它链）。本台账在**下发成功**那一刻登记
	// （launchShareDaily），可选落盘（EnableShareDailyAssignedPersist，路径
	// <DataDir>/share_daily_assign.json）：中控/机器人重启后都能继续把"今天已派、未满"的号
	// 恢复成神捕。**只记"派过"，不表示未满** —— 满额仍以独立满额表 + 心跳为准。
	shareDailyAssigned map[string]string
	// shareDailyAssignPath 台账落盘路径（空 = 纯内存，不落盘；测试/未装配场景）。
	shareDailyAssignPath string
	// shareDailyFullPath 满额表落盘路径（空 = 纯内存；2026-09-29 落盘修复）。
	//
	// 为什么也要落盘：满额表是"今日完成 ✓"的合成来源（sharedaily.go 总览合成 done 分支）——
	// 中控重启清表后，**已完成并离线的号在面板上丢失 ✓**（现场 09-29 20:25 重启丢了 9 烽火+1 神捕，
	// 用户误判"一天零完成"）。落盘后重启即恢复；跨日惰性失效逻辑不变（载入只收今日条目）。
	shareDailyFullPath string

	mu sync.RWMutex

	robots  map[string]*Robot
	removed map[string]bool
	curServ map[string]string // zone -> "host:port"（当前单区，key="default"）

	connConnected bool
	connAddr      string
}

// restoreCap 一条"当日卡死熔断"记录（2026-09-23）。
//
// 语义：该号当日卡死次数触顶（reghost.ChurnLimit）→ 到 Until 前不再自动重登/补发/
// 被自动任务拉起。Until = 触顶日的**次日 0 点**（服务端按 0 点切日，所有"当日"标记
// 同口径）；跨日惰性失效（不跑后台清理线程）。
type restoreCap struct {
	Until  time.Time // 失效时刻（次日 00:00 本地时间）
	Reason string    // 触顶原因（机器人给的原文）
	At     time.Time // 触顶时刻
}

// New 创建空状态。
func New() *State {
	return &State{
		robots:     make(map[string]*Robot),
		removed:    make(map[string]bool),
		paused:     make(map[string]bool),
		restoreCap: make(map[string]restoreCap),
		curServ:    make(map[string]string),
	}
}

// ---------------------------------------------------------------- robots

// Update 对指定账号执行更新（不存在则创建）；fn 在锁内执行。
// MarkGhostDoneToday 记下"该号今天抓鬼不可用/已满额"。
//
// ⚠️ 必须存在**独立的表**里（不能写 robot 行）—— 调用方紧接着就 `removeAccount`
// 把行删掉了，写在行上的标记会随之丢失（2026-09-22 踩过：标记数一直是 0）。
func (s *State) MarkGhostDoneToday(account string) {
	if account == "" {
		return
	}
	day := time.Now().Format("20060102")
	s.mu.Lock()
	if s.ghostUnavail == nil {
		s.ghostUnavail = map[string]string{}
	}
	s.ghostUnavail[account] = day
	s.mu.Unlock()
}

// GhostDoneToday 该号今天是否已抓满/不可用（跨日自动 false）。
func (s *State) GhostDoneToday(account string) bool {
	s.mu.RLock()
	day, ok := s.ghostUnavail[account]
	s.mu.RUnlock()
	return ok && day == time.Now().Format("20060102")
}

// GhostUnavailableTodayCount 今天已标"抓鬼不可用"的号数（面板展示/排查用）。
func (s *State) GhostUnavailableTodayCount() int {
	today := time.Now().Format("20060102")
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, d := range s.ghostUnavail {
		if d == today {
			n++
		}
	}
	return n
}

// dailyFullKey 分享日常满额表的键（账号 + 玩法键；P0 只有一条链，键设计留多玩法扩展）。
func dailyFullKey(account, shareKey string) string { return account + "|" + shareKey }

// MarkShareDailyFull 记下"该号今天的这个玩法已满/不可用"（跨日自动失效）。
//
// 2026-09-29 落盘修复：写入前顺手清掉非今日条目（文件不随天数增长），配了落盘路径时
// 原子写（失败静默——满额表丢一条只影响"✓ 展示与拦派"，不该阻塞下发主流程）。
func (s *State) MarkShareDailyFull(account, shareKey string) {
	if account == "" || shareKey == "" {
		return
	}
	today := time.Now().Format("20060102")
	s.mu.Lock()
	if s.shareDailyFull == nil {
		s.shareDailyFull = map[string]string{}
	}
	for k, d := range s.shareDailyFull {
		if d != today {
			delete(s.shareDailyFull, k)
		}
	}
	s.shareDailyFull[dailyFullKey(account, shareKey)] = today
	path, raw := s.shareDailyFullPath, s.marshalShareDailyFullLocked()
	s.mu.Unlock()
	writeFileAtomic(path, raw)
}

// shareDailyFullFile 满额表落盘形态（JSON；见 EnableShareDailyFullPersist）。
type shareDailyFullFile struct {
	// Full "账号|玩法键" → "YYYYMMDD"（当天该玩法已满/不可用）。
	Full      map[string]string `json:"full"`
	UpdatedAt time.Time         `json:"updated_at,omitempty"`
}

// marshalShareDailyFullLocked 序列化满额表（须持锁调用；无落盘路径返回 nil → 写盘空操作）。
func (s *State) marshalShareDailyFullLocked() []byte {
	if s.shareDailyFullPath == "" {
		return nil
	}
	b, err := json.MarshalIndent(shareDailyFullFile{Full: s.shareDailyFull, UpdatedAt: time.Now()}, "", "  ")
	if err != nil {
		return nil
	}
	return b
}

// EnableShareDailyFullPersist 开启满额表落盘并载入既有文件（path 空 = 关闭，纯内存）。
//
// 返回载入的**今日**条目数（跨日条目丢弃，惰性失效）；文件不存在 = 首次运行，返回 (0, nil)。
// 载入/解析失败不阻断启动（调用方记日志；损坏文件按空表继续，首次 Mark 时覆盖重建）。
func (s *State) EnableShareDailyFullPersist(path string) (int, error) {
	if path == "" {
		return 0, nil
	}
	s.mu.Lock()
	s.shareDailyFullPath = path
	s.mu.Unlock()

	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil // 首次运行：等首次 Mark 创建
		}
		return 0, err
	}
	var f shareDailyFullFile
	if err := json.Unmarshal(b, &f); err != nil {
		return 0, err
	}
	today := time.Now().Format("20060102")
	n := 0
	s.mu.Lock()
	if s.shareDailyFull == nil {
		s.shareDailyFull = map[string]string{}
	}
	for k, d := range f.Full {
		if d == today { // 跨日惰性失效：只载入今天的
			s.shareDailyFull[k] = d
			n++
		}
	}
	s.mu.Unlock()
	return n, nil
}

// ShareDailyFullToday 该号今天该玩法是否已满/不可用（跨日自动 false）。
//
// 用途：机器人满额后 daily 心跳变 None（见 shareDailyFull 字段注释），候选/补发闸
// 靠这张表继续拦；总览接口靠它补一条"已满"展示。
func (s *State) ShareDailyFullToday(account, shareKey string) bool {
	s.mu.RLock()
	day, ok := s.shareDailyFull[dailyFullKey(account, shareKey)]
	s.mu.RUnlock()
	return ok && day == time.Now().Format("20060102")
}

// ShareDailyFullTodayCount 今天已标"分享日常满额"的号数（面板/排查用）。
func (s *State) ShareDailyFullTodayCount() int {
	today := time.Now().Format("20060102")
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, d := range s.shareDailyFull {
		if d == today {
			n++
		}
	}
	return n
}

// ------------------------------------------------ shareDailyAssigned（今日已派神捕台账）

// dailyAssignKey 已派台账的键（账号 + 玩法键；与 dailyFullKey 同形状，语义独立）。
func dailyAssignKey(account, shareKey string) string { return account + "|" + shareKey }

// MarkShareDailyAssigned 记下"今天已给该号下发过该玩法"（launchShareDaily 成功下发后调）。
//
// 幂等；跨日惰性失效 —— 写入前顺手清掉非今日条目（不跑后台线程，落盘文件也不随天数增长）。
// 配了落盘路径时原子写（临时文件 + rename；失败静默：台账丢一条只影响"启动续跑"的兜底，
// 不该反过来阻塞下发主流程）。
func (s *State) MarkShareDailyAssigned(account, shareKey string) {
	if account == "" || shareKey == "" {
		return
	}
	today := time.Now().Format("20060102")
	s.mu.Lock()
	if s.shareDailyAssigned == nil {
		s.shareDailyAssigned = map[string]string{}
	}
	for k, d := range s.shareDailyAssigned {
		if d != today {
			delete(s.shareDailyAssigned, k)
		}
	}
	s.shareDailyAssigned[dailyAssignKey(account, shareKey)] = today
	path, raw := s.shareDailyAssignPath, s.marshalShareDailyAssignedLocked()
	s.mu.Unlock()
	writeFileAtomic(path, raw)
}

// ShareDailyAssignedToday 该号今天是否已成功下发过该玩法（跨日自动 false）。
//
// 用途：「启动 = 恢复当前任务」的兜底判据 —— 机器人重启后心跳 daily 丢失，但今天派过的
// 号点启动仍应续跑神捕；是否已满另由独立满额表/心跳判（本表只记"派过"）。
func (s *State) ShareDailyAssignedToday(account, shareKey string) bool {
	if account == "" || shareKey == "" {
		return false
	}
	s.mu.RLock()
	day, ok := s.shareDailyAssigned[dailyAssignKey(account, shareKey)]
	s.mu.RUnlock()
	return ok && day == time.Now().Format("20060102")
}

// ClearShareDailyAssigned 清掉**某玩法**的全部"今日已派"台账（面板停用该玩法池时调；2026-09-24）。
//
// 语义：停用 = 该玩法今天派过的记录作废 —— 抓鬼让路（api.shareDailyBusyForGhost）据此停止
// 让路，这些号回抓鬼（保持 fcf923e 口径：池停用不该让号两头不跑）。**灰度玩法**（池从未启用、
// 用户手动派号）不走这里，台账保留 → 让路继续保护在跑/已派的号（2026-09-24 RESTORE 误补发
// 事故：robot0001029 烽火 / robot0005278 神捕 被补发 ghost_start 顶掉 → 熔断）。
//
// 返回清掉的条目数；配了落盘路径时原子写盘（失败静默，同 Mark 口径）。
func (s *State) ClearShareDailyAssigned(shareKey string) int {
	if shareKey == "" {
		return 0
	}
	suffix := "|" + shareKey
	s.mu.Lock()
	n := 0
	for k := range s.shareDailyAssigned {
		if strings.HasSuffix(k, suffix) {
			delete(s.shareDailyAssigned, k)
			n++
		}
	}
	path, raw := s.shareDailyAssignPath, s.marshalShareDailyAssignedLocked()
	s.mu.Unlock()
	if n > 0 {
		writeFileAtomic(path, raw)
	}
	return n
}

// shareDailyAssignedFile 台账落盘形态（JSON；见 EnableShareDailyAssignedPersist）。
type shareDailyAssignedFile struct {
	// Assigned "账号|玩法键" → "YYYYMMDD"（当天已成功下发 share_daily_start 的号）。
	Assigned  map[string]string `json:"assigned"`
	UpdatedAt time.Time         `json:"updated_at,omitempty"`
}

// EnableShareDailyAssignedPersist 开启台账落盘并载入既有文件（path 空 = 关闭，纯内存）。
//
// 返回载入的**今日**条目数（跨日条目丢弃，惰性失效）；文件不存在 = 首次运行，
// 返回 (0, nil)，首次 Mark 时创建。载入/解析失败不阻断启动（调用方记日志即可）。
func (s *State) EnableShareDailyAssignedPersist(path string) (int, error) {
	if path == "" {
		return 0, nil
	}
	s.mu.Lock()
	s.shareDailyAssignPath = path
	s.mu.Unlock()

	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil // 首次运行：等首次 Mark 创建
		}
		return 0, err
	}
	var f shareDailyAssignedFile
	if err := json.Unmarshal(b, &f); err != nil {
		return 0, err
	}
	today := time.Now().Format("20060102")
	n := 0
	s.mu.Lock()
	if s.shareDailyAssigned == nil {
		s.shareDailyAssigned = map[string]string{}
	}
	for k, d := range f.Assigned {
		if d == today { // 跨日惰性失效：只载入今天的
			s.shareDailyAssigned[k] = d
			n++
		}
	}
	s.mu.Unlock()
	return n, nil
}

// ShareDailyAssignedTodayCount 今天已登记"已派"的号数（面板/排查用）。
func (s *State) ShareDailyAssignedTodayCount() int {
	today := time.Now().Format("20060102")
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, d := range s.shareDailyAssigned {
		if d == today {
			n++
		}
	}
	return n
}

// marshalShareDailyAssignedLocked 序列化台账（须持锁调用；未配路径/序列化失败 → nil = 不写盘）。
func (s *State) marshalShareDailyAssignedLocked() []byte {
	if s.shareDailyAssignPath == "" {
		return nil
	}
	raw, err := json.MarshalIndent(shareDailyAssignedFile{
		Assigned: s.shareDailyAssigned, UpdatedAt: time.Now(),
	}, "", "  ")
	if err != nil {
		return nil
	}
	return raw
}

// writeFileAtomic 临时文件 + rename 原子落盘（path 空/内容空跳过；失败静默）。
// 仓库既有落盘（waterline/roampool/accounts）同款写法。
func writeFileAtomic(path string, raw []byte) {
	if path == "" || len(raw) == 0 {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

func (s *State) Update(account string, fn func(r *Robot)) *Robot {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.robots[account]
	if !ok {
		r = &Robot{Account: account}
		s.robots[account] = r
	}
	if fn != nil {
		fn(r)
	}
	return r
}

// Get 读取机器人状态副本。
func (s *State) Get(account string) (Robot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.robots[account]
	if !ok {
		return Robot{}, false
	}
	return *r, true
}

// Has 是否存在该账号行。
func (s *State) Has(account string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.robots[account]
	return ok
}

// Remove 删除账号行。
func (s *State) Remove(account string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.robots, account)
}

// Snapshot 返回全部机器人的稳定排序副本（按账号升序）。
func (s *State) Snapshot() []Robot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now()
	today := now.Format("20060102")
	out := make([]Robot, 0, len(s.robots))
	for _, r := range s.robots {
		cp := *r
		// 面板标记：今天抓鬼已满/不可用（同一把锁内查 ghostUnavail，不二次加锁）
		cp.GhostDoneToday = s.ghostUnavail[cp.Account] == today
		// 人工暂停（R3）：同样在同一把锁内查，保证与行数据一致
		cp.Paused = s.paused[cp.Account]
		// 当日卡死熔断（2026-09-23）：独立表 → 面板字段（跨日自动不填）
		if c, ok := s.restoreCap[cp.Account]; ok && now.Before(c.Until) {
			cp.RestoreCapped = true
			cp.CapUntil = c.Until.Format(time.RFC3339)
			cp.CapReason = c.Reason
		}
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Account < out[j].Account })
	return out
}

// Accounts 返回全部账号名（升序）。
func (s *State) Accounts() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.robots))
	for a := range s.robots {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// Counts 返回 (在线数, 握手数, 总数)。
func (s *State) Counts() (online, handshake, total int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.robots {
		total++
		if r.Online {
			online++
		}
		if r.HS {
			handshake++
		}
	}
	return
}

// SvrOnlineLatest 返回最近一次"全服在线(含真人)"读数与时间戳(ms)。
//
// 数据来源：机器人定时发 `@online`（服务端 DEPLOY 权限即可）→ 回执里带在线角色数
// （role_manager 全部在线角色 = 真人 + 我们的号）→ 机器人上报 svr_online{count,ts}。
// 多个号里取 ts 最新的一条（只有查到过的那一个号带该字段）。ok=false 表示还没有读数。
func (s *State) SvrOnlineLatest() (count int, tsMS float64, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.robots {
		if r.SvrOnline == nil {
			continue
		}
		c := numToInt(r.SvrOnline["count"])
		t := numToFloat(r.SvrOnline["ts"])
		if c <= 0 || t <= 0 {
			continue
		}
		if t > tsMS {
			count, tsMS, ok = c, t, true
		}
	}
	return
}

// numToInt / numToFloat：JSON 解出来的数字是 float64，这里做宽松转换（失败返回 0）。
func numToInt(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case float32:
		return int(x)
	case int:
		return x
	case int64:
		return int(x)
	case uint64:
		return int(x)
	}
	return 0
}

func numToFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case uint64:
		return float64(x)
	}
	return 0
}

// StaleAccounts 返回超过 ttl 秒未心跳的账号（清理陈旧行用）。
func (s *State) StaleAccounts(now float64, ttl float64) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []string
	for a, r := range s.robots {
		if now-r.LastSeen > ttl {
			out = append(out, a)
		}
	}
	sort.Strings(out)
	return out
}

// MarkTaskError 记「最近任务错误 + 重复次数」；同码累计、换码重计，返回最新重复次数。
func (s *State) MarkTaskError(account, code, msg string) int {
	if account == "" || code == "" {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.robots[account]
	if !ok {
		r = &Robot{Account: account}
		s.robots[account] = r
	}
	if r.ErrCode == code {
		r.ErrRepeat++
	} else {
		r.ErrRepeat = 1
	}
	r.ErrCode = code
	r.ErrTS = float64(time.Now().Unix())
	if len(msg) > 120 {
		msg = msg[:120]
	}
	r.ErrMsg = msg
	return r.ErrRepeat
}

// FailedTasks 反复失败（>= minRepeat 次）的任务清单（面板「任务失败待处理」）。
func (s *State) FailedTasks(minRepeat, limit int) map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now().Unix()
	items := make([]map[string]any, 0)
	for a, r := range s.robots {
		if r.ErrRepeat < minRepeat {
			continue
		}
		age := int64(0)
		if now > int64(r.ErrTS) {
			age = now - int64(r.ErrTS)
		}
		items = append(items, map[string]any{
			"account": a, "code": r.ErrCode, "repeat": r.ErrRepeat,
			"age": age, "msg": r.ErrMsg,
		})
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i]["repeat"].(int) > items[j]["repeat"].(int)
	})
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	return map[string]any{"count": len(items), "items": items}
}

// ---------------------------------------------------------------- removed

// MarkRemoved 标记账号已移除（心跳不再复活）。
func (s *State) MarkRemoved(account string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removed[account] = true
}

// UnmarkRemoved 取消已移除标记。
func (s *State) UnmarkRemoved(account string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.removed, account)
}

// IsRemoved 是否已标记移除。
func (s *State) IsRemoved(account string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.removed[account]
}

// RemovedList 返回已移除账号列表（升序）。
func (s *State) RemovedList() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.removed))
	for a := range s.removed {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// ClearRemoved 清空"已移除"名单，返回被清掉的账号（升序）。
//
// 2026-09-22 生产背景：AutoRemoveOnDone 原先把"抓鬼满额/下线换号"的号也永久移除，
// 一天累积 461 个（当前区可用号 544 个里 83% 被排除）→ 水位器候选池为空
// （"号池里没有可上线的号"）→ 在线数卡在 83 上不到 200。这里提供一键恢复：
// 清空后候选池立即恢复，水位器/池子会在下一轮（60s 内）按需补号。
func (s *State) ClearRemoved() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.removed))
	for a := range s.removed {
		out = append(out, a)
	}
	s.removed = make(map[string]bool)
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------- paused（人工暂停）

// MarkPaused 标记"人工暂停"（面板「停止/停链」打标；各编排派发前跳过）。
//
// 独立于 robots 行：号被下线/移除后标记仍在（再次上线不会被自动编排吵醒），
// 直到用户显式「启动 / 立即补发 / 上线」由壳层清标。
func (s *State) MarkPaused(account string) {
	if account == "" {
		return
	}
	s.mu.Lock()
	if s.paused == nil {
		s.paused = map[string]bool{}
	}
	s.paused[account] = true
	s.mu.Unlock()
}

// ClearPaused 解除该号的人工暂停（用户显式启动/补发/上线时调）。
func (s *State) ClearPaused(account string) {
	if account == "" {
		return
	}
	s.mu.Lock()
	delete(s.paused, account)
	s.mu.Unlock()
}

// ClearAllPaused 解除全部人工暂停，返回被解除的账号（升序）。
func (s *State) ClearAllPaused() []string {
	s.mu.Lock()
	out := make([]string, 0, len(s.paused))
	for a := range s.paused {
		out = append(out, a)
	}
	s.paused = make(map[string]bool)
	s.mu.Unlock()
	sort.Strings(out)
	return out
}

// IsPaused 该号是否处于人工暂停。
func (s *State) IsPaused(account string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.paused[account]
}

// PausedList 返回全部人工暂停的账号（升序；面板/状态接口用）。
func (s *State) PausedList() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.paused))
	for a := range s.paused {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------- restoreCap（卡死熔断）

// MarkRestoreCapped 登记"当日卡死熔断"（reghost 触顶时调）：该号到**次日 0 点**前不再
// 自动重登/补发/被自动任务拉起；跨日惰性失效（与 GhostDoneToday 同款）。返回失效时刻。
//
// 为什么要独立表（2026-09-23）：原先触顶只依赖 robots 行里的 StuckCount，而行会被
// Remove 删掉（陈旧清理/下机）→ 计数归零 → 同一天又给 3 次重登机会。独立表让熔断语义
// 跨过行删除存续；中控重启（内存清零）或手动「解除熔断」可提前结束。
//
// 幂等：已存在且未过期 → 保留首次记录（不覆盖 Until/Reason，重复的上报不刷屏）。
func (s *State) MarkRestoreCapped(account, reason string) time.Time {
	if account == "" {
		return time.Time{}
	}
	now := time.Now()
	until := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.restoreCap == nil {
		s.restoreCap = map[string]restoreCap{}
	}
	if c, ok := s.restoreCap[account]; ok && now.Before(c.Until) {
		return c.Until
	}
	s.restoreCap[account] = restoreCap{Until: until, Reason: reason, At: now}
	return until
}

// IsRestoreCapped 该号现在是否处于卡死熔断（顺手清掉过期记录 —— 跨日自动解除）。
func (s *State) IsRestoreCapped(account string) bool {
	if account == "" {
		return false
	}
	_, _, ok := s.RestoreCapInfo(account)
	return ok
}

// RestoreCapInfo 返回该号熔断详情（面板/接口用；未熔断 ok=false）。
func (s *State) RestoreCapInfo(account string) (until time.Time, reason string, ok bool) {
	if account == "" {
		return time.Time{}, "", false
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.restoreCap[account]
	if !ok {
		return time.Time{}, "", false
	}
	if !now.Before(c.Until) {
		delete(s.restoreCap, account) // 跨日惰性失效
		return time.Time{}, "", false
	}
	return c.Until, c.Reason, true
}

// RestoreCappedList 当前处于熔断的账号清单（升序；含失效时刻与原因）。
// 面板/状态接口用它一眼看到"哪些号今天不救了、什么时候恢复"。
func (s *State) RestoreCappedList() []map[string]any {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]map[string]any, 0, len(s.restoreCap))
	for a, c := range s.restoreCap {
		if !now.Before(c.Until) {
			delete(s.restoreCap, a) // 跨日惰性失效
			continue
		}
		out = append(out, map[string]any{
			"account": a, "cap_until": c.Until.Format(time.RFC3339), "cap_reason": c.Reason,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["account"].(string) < out[j]["account"].(string) })
	return out
}

// ClearRestoreCapped 解除该号熔断（手动解除/到点外部清理用）；返回原本是否处于熔断。
func (s *State) ClearRestoreCapped(account string) bool {
	if account == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.restoreCap[account]
	delete(s.restoreCap, account)
	return ok
}

// ClearAllRestoreCapped 解除全部熔断，返回被解除的账号（升序）。
func (s *State) ClearAllRestoreCapped() []string {
	s.mu.Lock()
	out := make([]string, 0, len(s.restoreCap))
	for a := range s.restoreCap {
		out = append(out, a)
	}
	s.restoreCap = make(map[string]restoreCap)
	s.mu.Unlock()
	sort.Strings(out)
	return out
}

// ClearStuckToday 清零该号 robot 行内的"当日卡死计数"（StuckCount/StuckDay）。
//
// 解除熔断时一并调（reghost 触顶判定 / restorer 补发闸 / autotask 候选过滤都读它）：
// 不清的话，即使熔断表被解除，这些判据仍会按旧计数拦着该号（"解除了还是不起作用"）。
// 行不存在（已被删）则无事 —— 计数本来就不在。
func (s *State) ClearStuckToday(account string) {
	if account == "" {
		return
	}
	s.mu.Lock()
	if r, ok := s.robots[account]; ok {
		r.StuckCount, r.StuckDay = 0, ""
	}
	s.mu.Unlock()
}

// ---------------------------------------------------------------- 连接/服务器

// SetCurServer 记录某区机器人上报的游戏服地址（zone 为空时记到 "default"）。
func (s *State) SetCurServer(zone, server string) {
	if server == "" {
		return
	}
	if zone == "" {
		zone = "default"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.curServ[zone] = server
}

// CurServer 返回某区最近上报的游戏服地址（空区 / 无记录返回 "--"）。
func (s *State) CurServer(zone string) string {
	if zone == "" {
		zone = "default"
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if v := s.curServ[zone]; v != "" {
		return v
	}
	return "--"
}

// CurServers 返回全部区的游戏服地址快照（zone -> "host:port"）。
func (s *State) CurServers() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]string, len(s.curServ))
	for k, v := range s.curServ {
		out[k] = v
	}
	return out
}

// ZoneCounts 按区统计 (在线, 握手, 总数)。
func (s *State) ZoneCounts() map[string]map[string]int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]map[string]int{}
	for _, r := range s.robots {
		z := r.Zone
		if z == "" {
			z = "default"
		}
		c, ok := out[z]
		if !ok {
			c = map[string]int{"total": 0, "online": 0, "handshake": 0}
			out[z] = c
		}
		c["total"]++
		if r.Online {
			c["online"]++
		}
		if r.HS {
			c["handshake"]++
		}
	}
	return out
}

// ZoneOfAccounts 返回这些账号各自所属区（未知账号不出现在结果里）。
func (s *State) ZoneOfAccounts(accounts []string) map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]string, len(accounts))
	for _, a := range accounts {
		if r, ok := s.robots[a]; ok && r.Zone != "" {
			out[a] = r.Zone
		}
	}
	return out
}
