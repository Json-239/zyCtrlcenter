// 分享日常（大唐神捕，shenbu）链路：中控侧。
//
// 契约（docs/02-架构/方案-20260923-分享日常接入.md §7，2026-09-23 拍板）：
//
//	命令（中控→机器人）：share_daily_start {accounts, share_key, chain_id, chain, daily_limit, done?}
//	                      share_daily_stop  {accounts}
//	心跳（机器人→中控）：daily 块 {share_key, done, limit, state}（可选多日常数组）
//
// 中控职责（本文件）：命令下发 / 在途记账 / 停策略收工 / 轮转总览接口；
// 链载荷组装见 chainpayload.go:ShareDaily（基座 newbie_full + shenbu_nav 声明，发送时组装）；
// 机器人侧执行（通用日常驱动 share_daily.py）由机器人端负责，与 quest_engine/daily_ghost 互斥。
package api

import (
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

// accounts 全部在跑的号（升序，稳定）。
func (d *dailySessions) accounts() []string {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	out := make([]string, 0, len(d.m))
	for acc := range d.m {
		out = append(out, acc)
	}
	d.mu.Unlock()
	sort.Strings(out)
	return out
}

// runningSet 某个玩法键在跑的号集合（总览接口用）。
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

// dailyInflightActive 该号是否"已派发、命令还没生效"（TTL 内且未见心跳 daily 在跑）。
// 用于候选过滤/去重派发：命中就不要再派一次。
func (a *API) dailyInflightActive(acc string) bool {
	if !a.dailyInflight.hasFresh(acc) {
		return false
	}
	if a.St != nil {
		if r, ok := a.St.Get(acc); ok && dailyRunningOf(r, a.shareDailyKey()) {
			return false // 心跳已在跑：命令已生效，不算在途
		}
	}
	return true
}

// dailyInflightCount 当前"分享日常在途"号数（池配额用：在跑 + 在途）。
func (a *API) dailyInflightCount() int {
	key := a.shareDailyKey()
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

// shareDailyKey 当前玩法键（配置 CTRL_SHARE_DAILY_KEY，默认 share_daily_大唐神捕）。
func (a *API) shareDailyKey() string { return a.chainPayloads().ShareDailyKey() }

// shareDailyMinLevel 分享日常等级门槛（服务端票条件）：
// 策略配置 `min_level`（面板，覆盖全局）→ 全局 CTRL_SHARE_DAILY_MIN_LEVEL → 默认 40。
//
// 只用于**自动派发**（候选/补发闸）；手动「启动」按用户意图走，不再按它过滤。
func (a *API) shareDailyMinLevel() int {
	if a.AutoTask != nil {
		if st, ok := a.AutoTask.States()[autotask.KindShenbu]; ok && st.Config.MinLevel > 0 {
			return st.Config.MinLevel
		}
	}
	if a.Cfg != nil && a.Cfg.ShareDailyMinLevel > 0 {
		return a.Cfg.ShareDailyMinLevel
	}
	return intent.DefaultShareDailyMinLevel
}

// shareDailyBalanceGate 神捕余额闸（策略配置 `balance_gate`；0 = 不启用）：
// 候选/补发时过滤"余额已知且不足"的号（传送费不够 → 派了又停）。
func (a *API) shareDailyBalanceGate() int {
	if a.AutoTask == nil {
		return 0
	}
	if st, ok := a.AutoTask.States()[autotask.KindShenbu]; ok {
		return st.Config.BalanceGate
	}
	return 0
}

// shareDailyMoneyShort 余额不足（余额未知=不拦；闸值 0=不启用）。
func (a *API) shareDailyMoneyShort(r state.Robot) bool {
	gate := a.shareDailyBalanceGate()
	return gate > 0 && r.Money > 0 && r.Money < int64(gate)
}

// shareDailyFullToday 该号今日神捕是否已满/不可用：
//   - ① 心跳 daily 明确满额（done≥limit / state=DONE）；或
//   - ② 独立满额表命中 —— 机器人满额后会 request_stop，心跳 daily 随之变 None
//     （client.py 只在 enabled=true 时上报），靠这张表继续拦（跨日自动失效）。
func (a *API) shareDailyFullToday(acc string, r state.Robot) bool {
	key := a.shareDailyKey()
	if a.St != nil && a.St.ShareDailyFullToday(acc, key) {
		return true
	}
	return r.DailyFull(key)
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

// shareDailyDoneMap 每号"今日已做次数"（心跳 daily 块；有值才带 —— 重新下发不丢进度，
// 与抓鬼的 done 同口径）。
func (a *API) shareDailyDoneMap(accs []string) map[string]int {
	out := map[string]int{}
	if a.St == nil {
		return out
	}
	key := a.shareDailyKey()
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

// launchShareDaily 给这批号下发 share_daily_start（一方一条命令，账号同批合并）。
//
// 载荷取不到（声明文件缺 task_order / 基座缺网格路由）就是**硬失败**：一条都不发
// （宁可明确报错，也不发一份"跑不动/少环节"的载荷）。
func (a *API) launchShareDaily(accs []string) (bool, string) {
	if len(accs) == 0 {
		return false, "没有账号"
	}
	if a.Events == nil {
		return false, "事件通道不可用"
	}
	nav, err := a.chainPayloads().ShareDaily()
	if err != nil {
		return false, "分享日常链数据不可用: " + err.Error()
	}
	key := a.shareDailyKey()
	cmd := map[string]any{
		"cmd": "share_daily_start", "share_key": key, "chain": nav,
		"daily_limit": a.chainPayloads().ShareDailyLimit(), "accounts": accs,
	}
	if nav.ChainID != "" {
		cmd["chain_id"] = nav.ChainID
	}
	if done := a.shareDailyDoneMap(accs); len(done) > 0 {
		cmd["done"] = done // 重新下发不丢进度（与 ghost_start 的 done 同口径）
	}
	if !a.Events.SendCmd(cmd, "autotask_share_daily_start") {
		return false, "下发失败：机器人通道未连接"
	}
	a.markDailyDispatch(accs) // 在途记账：候选/池配额据此防重复派
	a.daily.track(accs, key)  // 会话记账：停策略时按它下发 stop
	return true, "已下发大唐神捕: " + strings.Join(accs, ", ")
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

// shareDailyStopAll 停掉全部在跑分享日常的号（停策略时收工）；返回下发条数。
func (a *API) shareDailyStopAll(reason string) int {
	accs := a.daily.accounts()
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
	"share_daily_宫廷10": "fenghuo", // 烽火大唐（P1 未接入；先给归属，便于前端按 kind 展示）
}

// shareDailyKindOf 玩法键 → 策略 kind（总览 queue 项用；认不出返回空串）。
func (a *API) shareDailyKindOf(shareKey string) string {
	if shareKey == "" {
		return ""
	}
	if shareKey == a.shareDailyKey() {
		return string(autotask.KindShenbu)
	}
	return shareDailyKindByKey[shareKey]
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
func (a *API) handleDailyOverview(w http.ResponseWriter, r *http.Request) {
	key := a.shareDailyKey()
	kinds, _ := a.intentKinds()
	running := a.daily.runningSet(key)
	dailyLimit := a.chainPayloads().ShareDailyLimit()
	rows := []any{}
	if a.St != nil {
		for _, rb := range a.St.Snapshot() {
			queue := rb.DailyEntries()
			onShenbu := kinds[rb.Account] == intent.KindShenbu
			shenbuFull := a.St.ShareDailyFullToday(rb.Account, key)
			if !rb.Online && len(queue) == 0 && !onShenbu && !shenbuFull {
				continue
			}
			q := make([]any, 0, len(queue)+1)
			hasShenbu := false
			for _, e := range queue {
				if e.ShareKey == key {
					hasShenbu = true
				}
			}
			for _, e := range queue {
				q = append(q, map[string]any{
					"share_key": e.ShareKey, "kind": a.shareDailyKindOf(e.ShareKey),
					"done": e.Done, "limit": e.Limit,
					"state": shareDailyStateEnum(e), "raw_state": e.State,
				})
			}
			// 合成条目（来源=中控记账而非心跳，marked=true；两者都只在"心跳没有该 key"时补）：
			//   ① 满额收工后（机器人 request_stop → 心跳不再带 daily）→ 补 done，
			//      前端仍显示 `10/10 ✓`（否则该玩法在总览里"消失"）；
			//   ② 已判该跑神捕但心跳没有条目（还没起来/被停）→ 补 pending，
			//      让"该跑未跑"在面板上可见（skipped 枚举预留给调度侧跳过记录，P0 暂不产生）。
			switch {
			case shenbuFull && !hasShenbu:
				q = append(q, map[string]any{
					"share_key": key, "kind": string(autotask.KindShenbu),
					"done": dailyLimit, "limit": dailyLimit,
					"state": "done", "raw_state": "DONE", "marked": true,
				})
			case onShenbu && !hasShenbu:
				q = append(q, map[string]any{
					"share_key": key, "kind": string(autotask.KindShenbu),
					"done": 0, "limit": dailyLimit,
					"state": "pending", "raw_state": "", "marked": true,
				})
			}
			current := ""
			if onShenbu || running[rb.Account] || hasShenbu {
				current = key
			}
			rows = append(rows, map[string]any{
				"account": rb.Account, "level": rb.Level, "queue": q,
				"current": current, "order": []any{},
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "share_keys": []string{key}, "rows": rows,
	})
}
