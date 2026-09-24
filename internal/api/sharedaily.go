// 分享日常链路（分享日常家族：shenbu 大唐神捕 / fenghuo 烽火大唐）：中控侧。
//
// 契约（docs/02-架构/方案-20260923-分享日常接入.md §7，2026-09-23 拍板）：
//
//	命令（中控→机器人）：share_daily_start {accounts, share_key, chain_id, chain, daily_limit, done?}
//	                      share_daily_stop  {accounts}
//	心跳（机器人→中控）：daily 块 {share_key, done, limit, state}（可选多日常数组）
//
// 每个玩法（kind）的差异只有三样：玩法键（share_key）、声明文件（chain_id）、日限（daily_limit）
// —— 见 shareDailyKeyOf / shareDailyChainIDOf / shareDailyLimitOf；
// 其余（在途记账 / 满额表 / 台账 / 停策略收工 / 轮转总览）全部按玩法键参数化，两个玩法隔离。
//
// 中控职责（本文件）：命令下发 / 在途记账 / 停策略收工 / 轮转总览接口；
// 链载荷组装见 chainpayload.go:ShareDailyOf（基座 newbie_full + <玩法>_nav 声明，发送时组装）；
// 机器人侧执行（通用日常驱动 share_daily.py）由机器人端负责，与 quest_engine/daily_ghost 互斥。
package api

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"

	"zyctrlcenter/internal/services/autotask"
	"zyctrlcenter/internal/services/intent"
	"zyctrlcenter/internal/state"
)

// ---------------------------------------------------------------- 会话记账

// dailySessions 中控侧"正在跑分享日常"的号（账号 → 玩法键）。
//
// 为什么需要：停策略（面板「停止」）时要给在跑的号下发 share_daily_stop 收工 ——
// 机器人端收不到 stop 会一直跑到日限；心跳 daily 块在灰度初期未必可用，不能依赖它反查。
type dailySessions struct {
	mu sync.Mutex
	m  map[string]string
}

func newDailySessions() *dailySessions { return &dailySessions{m: map[string]string{}} }

// track 记一批"刚下发 share_daily_start"的号。
func (d *dailySessions) track(accs []string, shareKey string) {
	if d == nil || len(accs) == 0 || shareKey == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.m == nil {
		d.m = map[string]string{}
	}
	for _, acc := range accs {
		if acc != "" {
			d.m[acc] = shareKey
		}
	}
}

// drop 移除一批号（已收工）。
func (d *dailySessions) drop(accs []string) {
	if d == nil || len(accs) == 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, acc := range accs {
		delete(d.m, acc)
	}
}

// runningSet 某个玩法键在跑的号集合（停策略收工 / 总览接口用）。
func (d *dailySessions) runningSet(shareKey string) map[string]bool {
	out := map[string]bool{}
	if d == nil {
		return out
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for acc, key := range d.m {
		if key == shareKey {
			out[acc] = true
		}
	}
	return out
}

// ---------------------------------------------------------------- 在途记账

// markDailyDispatch 记录一批"刚下发 share_daily_start"的号（定时补号 / 手动 / 补发共用）。
func (a *API) markDailyDispatch(accs []string) { a.dailyInflight.mark(accs) }

// dailyInflightActiveOf 该号是否"该玩法已派发、命令还没生效"（TTL 内且未见该玩法心跳在跑）。
// 用于候选过滤/去重派发：命中就不要再派一次。
//
// 在途表按**账号**记账（不分玩法）：TTL 窗口内跨玩法的派发会互相算作在途（保守 ——
// 最多少派一次，不会重复派）。同号在 TTL 内先派 shenbu 再派 fenghuo 的场景罕见（一天一玩法）。
func (a *API) dailyInflightActiveOf(acc string, kind autotask.Kind) bool {
	if !a.dailyInflight.hasFresh(acc) {
		return false
	}
	key := a.shareDailyKeyOf(kind)
	if key == "" {
		return false
	}
	if a.St != nil {
		if r, ok := a.St.Get(acc); ok && dailyRunningOf(r, key) {
			return false // 心跳已在跑：命令已生效，不算在途
		}
	}
	return true
}

// dailyInflightCountOf 当前"该玩法在途"号数（池配额用：在跑 + 在途）。
func (a *API) dailyInflightCountOf(kind autotask.Kind) int {
	key := a.shareDailyKeyOf(kind)
	if key == "" {
		return 0
	}
	return a.dailyInflight.count(func(acc string) bool {
		if a.St == nil {
			return false
		}
		r, ok := a.St.Get(acc)
		return ok && dailyRunningOf(r, key)
	})
}

// dailyRunningOf 该号是否"正在跑该分享日常"（心跳 daily 条目：有该玩法且未满）。
//
// 判据保守：条目存在且未满就算在跑（宁可少派，不重复下发）。老版机器人没有 daily 块
// → 恒 false（等 TTL 过期，不误判）；"满额"由 DailyFull 单独表达（候选侧据此跳过）。
func dailyRunningOf(r state.Robot, shareKey string) bool {
	if _, ok := r.DailyOf(shareKey); !ok {
		return false
	}
	return !r.DailyFull(shareKey)
}

// ---------------------------------------------------------------- 命令下发

// shareDailyKey 当前**大唐神捕**玩法键（配置 CTRL_SHARE_DAILY_KEY，默认 share_daily_大唐神捕）。
// 多玩法入口用 shareDailyKeyOf(kind)。
func (a *API) shareDailyKey() string { return a.chainPayloads().ShareDailyKey() }

// shareDailyKindFromString 字符串 → 分享日常族 kind（shenbu / fenghuo，大小写不敏感）。
//
// 用于"命令/意图/配置里的 kind 字符串"入口（restorer.Group.Kind、GhostSkipFunc 的 kind 等）：
// 只有这两个 kind 走 share_daily_* 通道；不是 → ("", false)。
func shareDailyKindFromString(kind string) (autotask.Kind, bool) {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case string(autotask.KindShenbu):
		return autotask.KindShenbu, true
	case string(autotask.KindFenghuo):
		return autotask.KindFenghuo, true
	}
	return "", false
}

// shareDailyKeyOf 该日常玩法的玩法键（share_daily_start.share_key）；非日常 kind → ""。
//
//	shenbu  → 配置 CTRL_SHARE_DAILY_KEY（默认 share_daily_大唐神捕）
//	fenghuo → 配置 CTRL_FENGHUO_KEY（默认 share_daily_宫廷10）
func (a *API) shareDailyKeyOf(kind autotask.Kind) string {
	switch kind {
	case autotask.KindShenbu:
		return a.shareDailyKey()
	case autotask.KindFenghuo:
		return a.chainPayloads().FenghuoKey()
	}
	return ""
}

// shareDailyChainIDOf 该日常玩法的**声明文件**名（链数据）。
func (a *API) shareDailyChainIDOf(kind autotask.Kind) string {
	switch kind {
	case autotask.KindShenbu:
		return a.chainPayloads().ShareDailyChainID()
	case autotask.KindFenghuo:
		return a.chainPayloads().FenghuoChainID()
	}
	return ""
}

// shareDailyLimitOf 该日常玩法的日限（随命令下发）。
func (a *API) shareDailyLimitOf(kind autotask.Kind) int {
	switch kind {
	case autotask.KindShenbu:
		return a.chainPayloads().ShareDailyLimit()
	case autotask.KindFenghuo:
		return a.chainPayloads().FenghuoDailyLimit()
	}
	return 0
}

// shareDailyMinLevelOf 该日常玩法的等级门槛（服务端票条件）：策略配置 `min_level`（面板，覆盖全局）
// → 该玩法的全局配置 → 默认 40。
//
// **必须用入参 cfg，不能自行读 Runner**：本函数会被 Runner 在**持有 Runner.mu 时**经
// Candidates 回调进来（services/autotask.tickKind）；在回调里再调 `a.AutoTask.States()`
// 是同 goroutine 锁重入 → 永久死锁（2026-09-24 生产事故：AUTOTASK 轮询 / RESTORE 恢复
// 引擎 / GET /api/autotask 全挂；报告见 docs/04-测试/事故-20260924-调度器死锁.md）。
//
// 只用于**自动派发**（候选/补发闸）；手动「启动」按用户意图走，不再按它过滤。
func (a *API) shareDailyMinLevelOf(kind autotask.Kind, cfg autotask.Config) int {
	if cfg.MinLevel > 0 {
		return cfg.MinLevel
	}
	switch kind {
	case autotask.KindShenbu:
		if a.Cfg != nil && a.Cfg.ShareDailyMinLevel > 0 {
			return a.Cfg.ShareDailyMinLevel
		}
	case autotask.KindFenghuo:
		if a.Cfg != nil && a.Cfg.FenghuoMinLevel > 0 {
			return a.Cfg.FenghuoMinLevel
		}
	}
	return intent.DefaultShareDailyMinLevel
}

// shareDailyBalanceGateOf 该日常玩法的余额闸（策略配置 `balance_gate`；0 = 不启用）：
// 候选/补发时过滤"余额已知且不足"的号（传送费不够 → 派了又停）。
//
// **仅限无锁场景**（恢复引擎补发闸等）；持 Runner 锁的候选判定请直接用传入 cfg 的
// BalanceGate（原因同上：回调里读 States() 会锁重入）。
func (a *API) shareDailyBalanceGateOf(kind autotask.Kind) int {
	if a.AutoTask == nil {
		return 0
	}
	if st, ok := a.AutoTask.States()[kind]; ok {
		return st.Config.BalanceGate
	}
	return 0
}

// shareDailyMoneyShort 余额不足（余额未知=不拦；闸值 0=不启用）。
// gate 由调用方给出：无锁场景用 a.shareDailyBalanceGateOf(kind)，持锁候选用 cfg.BalanceGate。
func (a *API) shareDailyMoneyShort(r state.Robot, gate int) bool {
	return gate > 0 && r.Money > 0 && r.Money < int64(gate)
}

// shareDailyFullTodayOf 该号今日该玩法是否已满/不可用：
//   - ① 心跳 daily 明确满额（done≥limit / state=DONE）；或
//   - ② 独立满额表命中 —— 机器人满额后会 request_stop，心跳 daily 随之变 None
//     （client.py 只在 enabled=true 时上报），靠这张表继续拦（跨日自动失效）。
//
// 独立满额表与台账均按 (账号, 玩法键) 记账（internal/state），两个玩法天然隔离。
func (a *API) shareDailyFullTodayOf(acc string, r state.Robot, kind autotask.Kind) bool {
	key := a.shareDailyKeyOf(kind)
	if key == "" {
		return false
	}
	if a.St != nil && a.St.ShareDailyFullToday(acc, key) {
		return true
	}
	return r.DailyFull(key)
}

// shareDailyInFlightTodayOf 该号"当天已派/在跑**该玩法**且未满"——供**跨玩法让路**用：
//   - 心跳 daily 条目命中（机器人只在 enabled=true 时上报），或
//   - 中控持久台账命中（机器人进程重启后心跳丢失，见 state.MarkShareDailyAssigned），
//     任一即可；满额（done≥limit / 独立满额表）= 自由号 → 一律 false（不拦抓鬼）。
//
// 2026-09-24 现场（robot0005274）：12:13:06 神捕策略下发 share_daily_start ok，12 秒后
// 抓鬼策略又下发 ghost_start ok —— 机器人端 ghost_start 与（同驱动的）日常会话互斥，
// **把当天神捕顶掉**；抓鬼池在补位窗口（在跑<目标）会持续派发 → 反复杀掉当天的神捕号。
// 抓鬼候选/恢复引擎补发据此跳过"当天该玩法未满"的号（满额号仍归抓鬼）。
// 烽火大唐走同一驱动（share_daily.py），让路口径同款（按各自玩法键查）。
//
// 只读 St/r（可在 Runner 持锁回调里用）。
func (a *API) shareDailyInFlightTodayOf(acc string, r state.Robot, kind autotask.Kind) bool {
	key := a.shareDailyKeyOf(kind)
	if key == "" {
		return false
	}
	busy := false
	if _, has := r.DailyOf(key); has {
		busy = true
	} else if a.St != nil && a.St.ShareDailyAssignedToday(acc, key) {
		busy = true
	}
	return busy && !a.shareDailyFullTodayOf(acc, r, kind)
}

// shareDailyPoolEnabled 该日常玩法池当前是否**启用**（自动通道开关）。
//
// 为什么需要（2026-09-24 缺口修复）：抓鬼让路（shareDailyInFlightTodayOf）只看"当天派过该玩法"
// ——池被停用（面板 enabled=false）后，这些号既不会被该池自动派、又被让路拦在抓鬼外，
// 当天两头不跑空烧到跨日。口径：**池停用 = 这些号回抓鬼**（台账保留：重新启用池后
// 候选照旧能捡回；行内「启动」不受影响，仍是显式恢复）。
//
// **无锁安全**：走 autotask.Runner.EnabledNoLock（纯 atomic.Load，Start/Stop 同步的镜像）
// —— 本函数会被 Runner 持锁回调（autotaskCandidatesCfg 的抓鬼候选）调用，那里禁止
// States()（锁重入死锁，见 autotask.Deps 契约与 docs/04-测试/事故-20260924-调度器死锁.md）。
// AutoTask 未装配（测试/嵌入式）→ false（没有自动通道，视同"池停用"：不让路，
// 否则没人再会把这些号拉起来）。
func (a *API) shareDailyPoolEnabled(kind autotask.Kind) bool {
	return a.AutoTask != nil && a.AutoTask.EnabledNoLock(kind)
}

// shareDailyBusyForGhost 抓鬼让路判定：该号当天有"**池启用**且已派/在跑且未满"的日常玩法
// （shenbu/fenghuo 任一）→ 返回该玩法 kind；没有 → ("", false)。
//
// 口径 = 各玩法 池启用(EnabledNoLock，无锁) ∧ shareDailyInFlightTodayOf（心跳或台账命中且未满）。
// 满额号是自由号（回抓鬼）；池停用的号也回抓鬼（否则两头不跑）。
// **无锁安全**：只读 St/r 与 EnabledNoLock，可在 Runner 持锁回调（抓鬼候选）里调用。
func (a *API) shareDailyBusyForGhost(acc string, r state.Robot) (autotask.Kind, bool) {
	for _, kind := range shareDailyFamilyKinds() {
		if a.shareDailyPoolEnabled(kind) && a.shareDailyInFlightTodayOf(acc, r, kind) {
			return kind, true
		}
	}
	return "", false
}

// shareDailyCandidateOf 该号能否进"某日常玩法"（shenbu/fenghuo）的候选。
//
// 判据（2026-09-23 shenbu 口径，2026-09-24 参数化复用给 fenghuo，两者完全相同）：
//   - 等级 ≥ 门槛（cfg.MinLevel → 该玩法全局配置 → 默认 40；等级未知(0) 一律不进）；
//   - 今日该玩法**未满**（心跳 done≥limit / 独立满额表；含"机器人已不再上报 daily"的兜底）；
//   - 余额闸不拦（cfg.BalanceGate>0 且余额已知且不足 → 不进：传送费不够，派了又停）；
//   - **没在跑别的链**：只收"该玩法意图 / 无意图 / **抓鬼已满**"的号（满额号抓鬼池已不派它）；
//     另有"续跑"例外：当天有该玩法心跳记录或持久台账且未满的号不再让路（用户口径 2026-09-24
//     「启动/轮转 = 恢复当前任务」——机器人进程重启后心跳丢失靠台账兜底）。
//
// **持 Runner 锁回调**（autotaskCandidatesCfg）：门槛/余额闸用入参 cfg，只读 St/r/配置/链数据，
// 禁止回读 a.AutoTask（锁重入死锁，见 autotask.Deps 契约）。
func (a *API) shareDailyCandidateOf(kind autotask.Kind, acc string, r state.Robot, cfg autotask.Config,
	kinds map[string]intent.Kind, hasLive bool, level int) (autotask.Candidate, bool) {
	if level < a.shareDailyMinLevelOf(kind, cfg) {
		return autotask.Candidate{}, false // 等级未知(0)/不足：服务端按票条件拒（≥40），别白跑
	}
	if a.shareDailyFullTodayOf(acc, r, kind) {
		return autotask.Candidate{}, false // 今日该玩法已满/不可用（心跳 + 独立满额表双闸）
	}
	if a.shareDailyMoneyShort(r, cfg.BalanceGate) {
		return autotask.Candidate{}, false // 余额闸（策略配置 balance_gate；余额未知不拦）
	}
	key := a.shareDailyKeyOf(kind)
	if key == "" {
		return autotask.Candidate{}, false
	}
	assigned := false
	if _, has := r.DailyOf(key); has && !a.shareDailyFullTodayOf(acc, r, kind) {
		assigned = true
	}
	if !assigned && a.St != nil && a.St.ShareDailyAssignedToday(acc, key) &&
		!a.shareDailyFullTodayOf(acc, r, kind) {
		assigned = true
	}
	if k := kinds[acc]; k != "" && k != intent.Kind(kind) && !assigned {
		ghostFull := (a.St != nil && a.St.GhostDoneToday(acc)) || ghostDailyFull(r)
		if !(k == intent.KindGhost && ghostFull) {
			return autotask.Candidate{}, false // 别的链在用（新手链 / 抓鬼未满）→ 让路
		}
	}
	// 跨日常让路（2026-09-24 P1 保守口径）：号已在跑/已派**另一个**日常玩法（且那个池启用、
	// 未满）→ 本玩法候选让路 —— 否则两个日常池同时启用时会把同一个号各派一次，后派者顶掉前者
	//（机器人端同驱动互斥）。本玩法自己有 assigned（续跑）时不让路（优先收回自己的号）。
	// 轮转（跑满自动转下一条）P2 再做，这里只保证两个池不互相抢。
	if !assigned {
		for _, other := range shareDailyFamilyKinds() {
			if other == kind {
				continue
			}
			if a.shareDailyPoolEnabled(other) && a.shareDailyInFlightTodayOf(acc, r, other) {
				return autotask.Candidate{}, false
			}
		}
	}
	return autotask.Candidate{Account: acc, Online: hasLive && r.Online, Level: level,
		Reason: fmt.Sprintf("%s（%d 级）", kind.Label(), level)}, true
}

// shareDailyStateEnum 队列状态语义（契约枚举，2026-09-23 lead 裁决 A）：
//
//	done    满额（limit>0 且 done≥limit，或相位 DONE——满额收工的两种表达）
//	running 在跑且未满（本条目来自心跳：机器人只在 enabled=true 时上报 daily，
//	        所以"有条目"= 在跑；总览的 current 也会指向它）
//	pending 其余（已判该跑/已排但未跑）—— 由总览在"无心跳条目"时合成一条（P2 轮转排序后，
//	        这里将承接"队列里排了但还没轮到"）
//	skipped 调度侧跳过记录（余额不足/门槛不够等）—— **P0 暂无记录表，先不使用**（预留枚举，
//	        前端已兼容）。
//
// 机器人原始相位**不再占用 state**，另见 raw_state（排障用，前端不解析）。
func shareDailyStateEnum(e state.DailyEntry) string {
	if e.Limit > 0 && e.Done >= e.Limit {
		return "done"
	}
	if strings.EqualFold(strings.TrimSpace(e.State), "DONE") {
		return "done"
	}
	return "running"
}

// shareDailyDoneMapOf 每号"今日该玩法已做次数"（心跳 daily 块；有值才带 —— 重新下发不丢进度，
// 与抓鬼的 done 同口径）。
func (a *API) shareDailyDoneMapOf(accs []string, kind autotask.Kind) map[string]int {
	out := map[string]int{}
	if a.St == nil {
		return out
	}
	key := a.shareDailyKeyOf(kind)
	if key == "" {
		return out
	}
	for _, acc := range accs {
		r, ok := a.St.Get(acc)
		if !ok {
			continue
		}
		if e, ok := r.DailyOf(key); ok && e.Done > 0 {
			out[acc] = e.Done
		}
	}
	return out
}

// launchShareDailyOf 给这批号下发指定日常玩法的 share_daily_start（账号同批合并成一条命令）。
//
// 载荷取不到（声明文件缺 task_order / 基座缺网格路由）就是**硬失败**：一条都不发
// （宁可明确报错，也不发一份"跑不动/少环节"的载荷）。
func (a *API) launchShareDailyOf(kind autotask.Kind, accs []string) (bool, string) {
	if len(accs) == 0 {
		return false, "没有账号"
	}
	if a.Events == nil {
		return false, "事件通道不可用"
	}
	key := a.shareDailyKeyOf(kind)
	if key == "" {
		return false, "未知的分享日常玩法: " + string(kind)
	}
	nav, err := a.chainPayloads().ShareDailyOf(a.shareDailyChainIDOf(kind))
	if err != nil {
		return false, "分享日常链数据不可用: " + err.Error()
	}
	cmd := map[string]any{
		"cmd": "share_daily_start", "share_key": key, "chain": nav,
		"daily_limit": a.shareDailyLimitOf(kind), "accounts": accs,
	}
	if nav.ChainID != "" {
		cmd["chain_id"] = nav.ChainID
	}
	if done := a.shareDailyDoneMapOf(accs, kind); len(done) > 0 {
		cmd["done"] = done // 重新下发不丢进度（与 ghost_start 的 done 同口径）
	}
	if !a.Events.SendCmd(cmd, "autotask_share_daily_start") {
		return false, "下发失败：机器人通道未连接"
	}
	a.markDailyDispatch(accs) // 在途记账：候选/池配额据此防重复派
	a.daily.track(accs, key)  // 会话记账：停策略时按它下发 stop
	// 2026-09-24：**下发成功即登记持久台账**（所有下发路径的总入口都走这里）——
	// 机器人进程重启后心跳 daily 块丢失，「启动 = 恢复当前任务」/候选"不让路"由台账兜底；
	// 是否满额另由独立满额表判（台账只记"派过"）。台账按玩法键分账（两个玩法隔离）。
	if a.St != nil {
		for _, acc := range accs {
			a.St.MarkShareDailyAssigned(acc, key)
		}
	}
	return true, "已下发" + kind.Label() + ": " + strings.Join(accs, ", ")
}

// shareDailyStop 给这些号下发 share_daily_stop（收工）并清会话；返回下发条数。
func (a *API) shareDailyStop(accs []string) int {
	if len(accs) == 0 {
		return 0
	}
	if a.Events != nil && a.Events.SendCmd(
		map[string]any{"cmd": "share_daily_stop", "accounts": accs}, "share_daily_stop") {
		a.daily.drop(accs)
		return len(accs)
	}
	return 0
}

// dailyActiveAccountsOf 给定账号集合中"有活跃**该玩法**"的号（accs 为空 = 全部号，升序稳定）。
//
// 判据（2026-09-24）：心跳 daily 条目存在且相位非 STOPPED —— 机器人只在 enabled=true 时
// 上报 daily，所以"有条目"就是"当前在跑"；满额收工的号机器人会自己 request_stop
// （且相位为 STOPPED），不需要再补一刀。用于「停止」= 停当前任务时的收工名单：
// 既有 stop 只停任务链 + 抓鬼，日常会话不会停（用户观感"点了停止神捕还在跑"）。
func (a *API) dailyActiveAccountsOf(kind autotask.Kind, accs []string) []string {
	if a.St == nil {
		return nil
	}
	key := a.shareDailyKeyOf(kind)
	if key == "" {
		return nil
	}
	want := map[string]bool{}
	for _, acc := range accs {
		if acc != "" {
			want[acc] = true
		}
	}
	out := []string{}
	for _, r := range a.St.Snapshot() {
		if len(want) > 0 && !want[r.Account] {
			continue
		}
		e, ok := r.DailyOf(key)
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(e.State), "STOPPED") {
			continue
		}
		out = append(out, r.Account)
	}
	sort.Strings(out)
	return out
}

// dailyActiveAccounts 给定账号集合中"有活跃分享日常（任一玩法：shenbu/fenghuo）"的号。
//
// 合并去重、升序。share_daily_stop 命令不带玩法键（机器人端按账号停当前日常会话），
// 所以这里把两个玩法的活跃号合成一份名单。
func (a *API) dailyActiveAccounts(accs []string) []string {
	set := map[string]bool{}
	for _, kind := range []autotask.Kind{autotask.KindShenbu, autotask.KindFenghuo} {
		for _, acc := range a.dailyActiveAccountsOf(kind, accs) {
			set[acc] = true
		}
	}
	out := make([]string, 0, len(set))
	for acc := range set {
		out = append(out, acc)
	}
	sort.Strings(out)
	return out
}

// shareDailyStopAllOf 停掉该玩法全部在跑的号（停策略时收工）；返回下发条数。
//
// 名单 = **会话记账（中控派过）∪ 心跳 daily（机器人自己在跑的）**，按该玩法的 key 过滤；
// 去重后一次下发（share_daily_stop 不带玩法键）。
func (a *API) shareDailyStopAllOf(kind autotask.Kind, reason string) int {
	key := a.shareDailyKeyOf(kind)
	if key == "" {
		return 0
	}
	set := map[string]bool{}
	for acc := range a.daily.runningSet(key) {
		set[acc] = true
	}
	for _, acc := range a.dailyActiveAccountsOf(kind, nil) {
		set[acc] = true
	}
	accs := make([]string, 0, len(set))
	for acc := range set {
		accs = append(accs, acc)
	}
	sort.Strings(accs)
	if len(accs) == 0 {
		return 0
	}
	n := a.shareDailyStop(accs)
	if n > 0 {
		a.Log.Printf("[SHAREDAILY] %s：已给 %d 个号下发 share_daily_stop（%s）",
			reason, n, strings.Join(accs, ", "))
	}
	return n
}

// ---------------------------------------------------------------- 面板接口

// shareDailyKindByKey 已知玩法键 → 策略 kind（值域与 autotask.Kind 一致）。
//
// 只登记**已确证**的键（依据：方案 §2、config/task/20283.xml 与 20021.xml 分析）；
// 认不出来返回空串（前端可用 share_key 兜底显示）。
var shareDailyKindByKey = map[string]string{
	"share_daily_大唐神捕": string(autotask.KindShenbu),
	"share_daily_宫廷10": string(autotask.KindFenghuo), // 烽火大唐（20021.xml share_daily_key）
}

// shareDailyKindOf 玩法键 → 策略 kind（总览 queue 项用；认不出返回空串）。
//
// 先按**当前配置**的两个玩法键比对（配置覆盖也能认），再查静态表兜底。
func (a *API) shareDailyKindOf(shareKey string) string {
	if shareKey == "" {
		return ""
	}
	for _, kind := range []autotask.Kind{autotask.KindShenbu, autotask.KindFenghuo} {
		if shareKey == a.shareDailyKeyOf(kind) {
			return string(kind)
		}
	}
	return shareDailyKindByKey[shareKey]
}

// shareDailyFamilyKind 分享日常家族的 kind 固定次序（= Kinds 里日常两段的次序）：
// shenbu → fenghuo，与前端 queue 固定排序（ghost→newbie→shenbu→fenghuo）同口径。
func shareDailyFamilyKinds() []autotask.Kind {
	return []autotask.Kind{autotask.KindShenbu, autotask.KindFenghuo}
}

// shareDailyKeys 分享日常家族的全部玩法键（按 kind 固定次序；总览 share_keys 用）。
func (a *API) shareDailyKeys() []string {
	out := make([]string, 0, 2)
	for _, kind := range shareDailyFamilyKinds() {
		if key := a.shareDailyKeyOf(kind); key != "" {
			out = append(out, key)
		}
	}
	return out
}

// shareDailyKindRank queue 条目的固定显示次序（gp-2 2026-09-23 约定）：
// ghost → newbie → shenbu → fenghuo，未知/空排最后（与"跑满一条转下一条"的直觉一致）。
func shareDailyKindRank(item any) int {
	m, _ := item.(map[string]any)
	kind, _ := m["kind"].(string)
	switch kind {
	case string(autotask.KindGhost):
		return 0
	case string(autotask.KindNewbie):
		return 1
	case string(autotask.KindShenbu):
		return 2
	case string(autotask.KindFenghuo):
		return 3
	}
	return 9
}

// handleDailyOverview 分享日常轮转总览（前端「日常轮转」表数据源；方案 §7 契约）：
//
//	GET /api/daily/overview
//	→ {ok, share_keys[], rows:[{account, level, queue:[{share_key,kind,done,limit,state,raw_state}], current, order}]}
//
// queue 项的 `kind` 用策略 kind（ghost/newbie/shenbu/fenghuo，前端按它映射图标/表头；
// 认不出为空串）；`state` 是**队列状态枚举** `pending/running/done/skipped`（语义见
// shareDailyStateEnum；机器人原始相位在 `raw_state`，排障用）。数据源 = 机器人心跳 daily 块
// + 意图表 + 中控侧会话/满额记账；**轮转顺序（order）P2 再填**（值域拟 fixed/random；
// 现在给空数组，前端按"待规划"渲染）。空值安全：老版机器人没有 daily 块 → queue=[]、
// current=""；离线且既无数据也无意图的行不出（不刷屏）。
//
// 2026-09-24（烽火大唐 P1）：分享日常家族两个玩法（shenbu / fenghuo）**各算一行内的条目** ——
// share_keys 返回两个玩法键（前端表头），合成条目（满额 done / 意图 pending）按各自玩法补。
func (a *API) handleDailyOverview(w http.ResponseWriter, r *http.Request) {
	keys := a.shareDailyKeys()
	kinds, _ := a.intentKinds()
	rows := []any{}
	if a.St != nil {
		for _, rb := range a.St.Snapshot() {
			queue := rb.DailyEntries()
			// 每个玩法四个信号：意图命中 / 满额（独立表） / 会话记账在跑 / 心跳有条目
			type dailyStat struct {
				key                  string
				limit                int
				intentHit, full, run bool
				hasKey               bool
			}
			stats := make([]dailyStat, 0, len(keys))
			visible := false
			for _, kind := range shareDailyFamilyKinds() {
				key := a.shareDailyKeyOf(kind)
				if key == "" {
					continue
				}
				st := dailyStat{key: key, limit: a.shareDailyLimitOf(kind)}
				st.intentHit = kinds[rb.Account] == intent.Kind(kind)
				st.full = a.St.ShareDailyFullToday(rb.Account, key)
				st.run = a.daily.runningSet(key)[rb.Account]
				for _, e := range queue {
					if e.ShareKey == key {
						st.hasKey = true
						break
					}
				}
				if st.intentHit || st.full {
					visible = true
				}
				stats = append(stats, st)
			}
			if !rb.Online && len(queue) == 0 && !visible {
				continue
			}
			q := make([]any, 0, len(queue)+len(stats))
			for _, e := range queue {
				q = append(q, map[string]any{
					"share_key": e.ShareKey, "kind": a.shareDailyKindOf(e.ShareKey),
					"done": e.Done, "limit": e.Limit,
					"state": shareDailyStateEnum(e), "raw_state": e.State,
				})
			}
			// 合成条目（来源=中控记账而非心跳，marked=true；两者都只在"心跳没有该 key"时补）：
			//   ① 满额收工后（机器人 request_stop → 心跳不再带 daily）→ 补 done，
			//      前端仍显示 `N/N ✓`（否则该玩法在总览里"消失"）；
			//   ② 已判该跑该玩法但心跳没有条目（还没起来/被停）→ 补 pending，
			//      让"该跑未跑"在面板上可见（skipped 枚举预留给调度侧跳过记录，P0 暂不产生）。
			for _, st := range stats {
				switch {
				case st.full && !st.hasKey:
					q = append(q, map[string]any{
						"share_key": st.key, "kind": a.shareDailyKindOf(st.key),
						"done": st.limit, "limit": st.limit,
						"state": "done", "raw_state": "DONE", "marked": true,
					})
				case st.intentHit && !st.hasKey:
					q = append(q, map[string]any{
						"share_key": st.key, "kind": a.shareDailyKindOf(st.key),
						"done": 0, "limit": st.limit,
						"state": "pending", "raw_state": "", "marked": true,
					})
				}
			}
			// 固定次序（gp-2 2026-09-23 约定，前端不做排序）：ghost → newbie → shenbu →
			// fenghuo，未知/空排最后；同权重保持原顺序（stable）。P2 有 `order` 后按 order 插。
			sort.SliceStable(q, func(i, j int) bool {
				return shareDailyKindRank(q[i]) < shareDailyKindRank(q[j])
			})
			// current = 该号当前在跑的玩法键：心跳有条目的优先（机器人只在 enabled=true 上报），
			// 其次会话记账/意图命中；都没有 → 空串。多玩法命中时按家族固定次序取第一个。
			current := ""
			for _, st := range stats {
				if st.hasKey {
					current = st.key
					break
				}
			}
			if current == "" {
				for _, st := range stats {
					if st.intentHit || st.run {
						current = st.key
						break
					}
				}
			}
			rows = append(rows, map[string]any{
				"account": rb.Account, "level": rb.Level, "queue": q,
				"current": current, "order": []any{},
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "share_keys": keys, "rows": rows,
	})
}
