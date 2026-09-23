// 定时自动任务 / 卡死恢复：api 侧的壳。
//
// 引擎在 internal/services/autotask（纯逻辑、可注入依赖）；这里只负责把"真实世界"接上：
//   - 候选：账号池（该区可用）+ 运行时状态 + 意图表 + **抓鬼等级门槛**；
//   - 上线：robot_manage add（密码只从池里取，没有统一密码）；
//   - 注册：复用建号链路（accountcreate.Register + 入池），后台跑、立即返回计划名单；
//   - 下发：与手动「启动」同口径（新手链带链数据；抓鬼带组装载荷 + role + daily_limit + done）。
//
// 面板「任务」页（web/src/components/ChainView.vue）与 /api/autotask* 都从这里取数。
package api

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"zyctrlcenter/internal/accountcreate"
	"zyctrlcenter/internal/accountverify"
	"zyctrlcenter/internal/chainlib"
	"zyctrlcenter/internal/services/accounts"
	"zyctrlcenter/internal/services/autotask"
	"zyctrlcenter/internal/services/intent"
	"zyctrlcenter/internal/services/reghost"
	"zyctrlcenter/internal/services/waterline"
	"zyctrlcenter/internal/state"
)

// ---------------------------------------------------------------- 依赖装配

// AutoTaskDeps 组装定时任务引擎的依赖（main 用它创建 autotask.Runner）。
func (a *API) AutoTaskDeps() autotask.Deps {
	return autotask.Deps{
		Now: time.Now,
		Rand: func(n int) int {
			if n <= 0 {
				return 0
			}
			return rand.Intn(n)
		},
		Candidates: func(kind autotask.Kind) []autotask.Candidate {
			return a.autotaskCandidates(kind)
		},
		OnlineCount: func(kind autotask.Kind) int { return a.autotaskOnlineCount(kind) },
		Online:      func(kind autotask.Kind, accs []string) (int, error) { return a.onlineForAuto(accs) },
		Register:    func(kind autotask.Kind, count int) ([]string, error) { return a.registerForAuto(count) },
		Launch:      func(kind autotask.Kind, accs []string) bool { ok, _ := a.LaunchTask(kind, accs); return ok },
		Tick:        a.hatchReconcile, // 孵化会话的周期维护（到期收工/完成清理）
		Log:         func(format string, args ...any) { a.Log.Printf(format, args...) },
	}
}

// ReghostDeps 组装"卡死重登恢复"的依赖（main 用它创建 reghost.Runner）。
func (a *API) ReghostDeps() reghost.Deps {
	return reghost.Deps{
		Now: time.Now,
		Online: func(acc string) bool {
			if a.St == nil {
				return false
			}
			r, ok := a.St.Get(acc)
			return ok && r.Online
		},
		Remove: func(acc string) bool {
			return a.Events != nil && a.Events.SendCmd(
				map[string]any{"cmd": "robot_manage", "action": "remove", "accounts": []string{acc}}, "reghost_remove")
		},
		Add: func(acc string) bool {
			pwd, ok := a.passwordFor(acc)
			if !ok {
				return false
			}
			return a.Events != nil && a.Events.SendCmd(
				map[string]any{"cmd": "robot_manage", "action": "add",
					"accounts": []any{[]string{acc, pwd}}}, "reghost_add")
		},
		Launch: func(acc string) bool {
			kind := autotask.KindGhost
			if kinds, _ := a.intentKinds(); kinds[acc] == intent.KindNewbie {
				kind = autotask.KindNewbie
			}
			ok, msg := a.LaunchTask(kind, []string{acc})
			if !ok {
				a.Log.Printf("[REGHOST] %s 补发%s失败: %s", acc, kind.Label(), msg)
			}
			return ok
		},
		Log: func(format string, args ...any) { a.Log.Printf(format, args...) },
		// churn 防护：当日卡死 ≥3 次 → 不再自动重登（与 autotaskCandidates 同口径）。
		Stuck: a.stuckCountToday,
		// 2026-09-23：触顶 → 写"独立熔断表"（跨 robot 行删除存续、跨日惰性失效）——
		// 面板据此显示 restore_capped/cap_until/cap_reason，调度侧据此停止补发/候选。
		Cap: func(acc, reason string) time.Time {
			if a.St == nil {
				return time.Time{}
			}
			return a.St.MarkRestoreCapped(acc, reason)
		},
		Capped: func(acc string) bool {
			return a.St != nil && a.St.IsRestoreCapped(acc)
		},
	}
}

// stuckCountToday 该号"当日卡死次数"（跨日自动算 0）。
func (a *API) stuckCountToday(acc string) int {
	if a.St == nil {
		return 0
	}
	r, ok := a.St.Get(acc)
	if !ok || r.StuckDay != time.Now().Format("20060102") {
		return 0
	}
	return r.StuckCount
}

// ---------------------------------------------------------------- 候选判定

// ghostGate 抓鬼等级门槛（口径同参考实现 services/ghost_fit.py）：
//
//	门槛 = max(新手链毕业线, 该号登记的服务端要求等级)
//	等级已知且低于门槛 → **不派抓鬼**（即使 chain_done；生产实测 <31 号被派去点钟馗必卡）；
//	等级未知（<=0）→ 不拦（保持原行为，真毕业号的等级心跳一定带）。
func (a *API) ghostGate(level, required int) (bool, string) {
	floor := 31
	if a.Cfg != nil && a.Cfg.NewbieMaxLevel > 0 {
		floor = a.Cfg.NewbieMaxLevel
	}
	if required > floor {
		floor = required
	}
	if level > 0 && level < floor {
		return false, fmt.Sprintf("抓鬼门槛 %d 级（当前 %d 级）", floor, level)
	}
	return true, ""
}

// GhostSkipFunc 供恢复引擎（restorer）用的"派发前最后一道闸"：人工暂停 / 抓鬼等级门槛 / 池闸。
// 导出给 main 装配（restorer 在 api 之前构造，用闭包晚绑定）。
//
// 2026-09-23 R3（操作健壮性审计）：**人工暂停闸对所有 kind 生效**（原先只处理 ghost）——
// 用户点过「停止/停链」的号，恢复引擎不再按意图把它拉起来；再次「启动/立即补发/上线」
// 由壳层清标（见 handleStart / handleIntentsRestore / batchOnline）。
// 抓鬼等级/池闸的判据沿用原口径（只对 ghost），不变；
// 2026-09-23 分享日常（shenbu）：自有池状态闸 + 今日满额闸（与抓鬼同款，只对 shenbu）。
func (a *API) GhostSkipFunc() func(kind, account string) (bool, string) {
	return func(kind, account string) (bool, string) {
		if a.St != nil && a.St.IsPaused(account) {
			return true, "人工暂停（面板点过「停止」；再点「启动/立即补发/上线」即解除）"
		}
		// 2026-09-23 卡死熔断闸：当日卡死触顶的号 → 不再自动补发（**对所有 kind**，
		// 与人工暂停同层）。独立熔断表不受 robot 行删除影响；跨日自动失效，
		// 或由面板「解除熔断」/「立即补发该号」提前解除（壳层清表 + 清当日计数）。
		if a.St != nil {
			if until, _, capped := a.St.RestoreCapInfo(account); capped {
				return true, "当日卡死已熔断（" + until.Format("01-02 15:04") +
					" 自动恢复；面板「解除熔断」或「立即补发该号」可提前解除）"
			}
		}
		// 2026-09-23 分享日常（shenbu）：自有闸门（池状态 + 今日满额），与抓鬼同款但只对本 kind。
		// 注意：shenbu 的池未启用时这里拦下 —— 与"池停用不自动补发"的既有口径一致。
		if strings.EqualFold(kind, "shenbu") {
			if ok, why := a.poolAllowsDispatch(kind); !ok {
				return true, why
			}
			if a.St != nil {
				if r, ok := a.St.Get(account); ok {
					if a.shareDailyFullToday(account, r) {
						return true, "今日大唐神捕已满/不可用（等跨日）"
					}
					if a.shareDailyMoneyShort(r) {
						return true, fmt.Sprintf("余额不足（%d < 闸值 %d，防传送卡死）",
							r.Money, a.shareDailyBalanceGate())
					}
				}
			}
			return false, ""
		}
		if !strings.EqualFold(kind, "ghost") {
			return false, ""
		}
		// 2026-09-22 P1（三池交互分析）：池状态闸 —— restorer 原来不看池的 enabled/保持数，
		//   池禁用或已超编时仍补发（实测 newbie target=0 却有 16 号在跑、ghost 102>100）。
		if ok, why := a.poolAllowsDispatch(kind); !ok {
			return true, why
		}
		// 2026-09-22 P1：满额闸 —— 今日已满/不可用的号不要再补发 ghost_start
		//   （实测 21:42:33 刚回收、21:42:40 就补发，机器人同秒回"启动时计数已满 50/50"）。
		if a.St != nil && a.St.GhostDoneToday(account) {
			return true, "今日抓鬼已满/不可用（等跨日或清空移除名单）"
		}
		if ok, why := a.ghostGateFor(account); !ok {
			return true, why
		}
		// 2026-09-21 churn 防护：当日卡死 ≥3 次 → 不再自动补发（免得"重登→补发→又卡"
		// 无限循环；与 reghost/autotask 候选同口径）。跨日自动放行。
		if n := a.stuckCountToday(account); n >= reghost.ChurnLimit {
			return true, fmt.Sprintf("当日卡死已达 %d 次（等人工/跨日清零）", n)
		}
		return false, ""
	}
}

// ---------------------------------------------------------------- 派发在途记账（2026-09-23）
//
// 为什么需要：任务"在跑数"以机器人上报的活跃会话（ghost.enabled / 链推进）为准，而
// "命令下发 → 机器人建立会话"之间有几十秒空窗。批量上线时（一次 add 100+ 号），
// 池闸若只看会话数，每一批新号都会看到"缺口仍在"而被连续放行 —— 生产实测一次
// 直派/补发 200+（超 target 100，见 docs/04-测试/分析-20260923-抓鬼分配逻辑.md）。
// 这里把"已派发未确认"的号也计入占用量：配额判断用
// "活跃会话 + 在途"，避免"上一批还在路上就放行下一批"。
//
// 2026-09-23 R1（操作健壮性审计）：同一套记账按**命令种类**分表 ——
//   - ghostInflight：ghost_start（抓鬼）；
//   - chainInflight：start_chain（新手链/捉鬼链）——原先只有抓鬼记账，连点
//     「启动(自动分配)」会把同一批号重复下发 start_chain。
//
// 两表的"生效"（live）判据各自与在跑口径同源：
//   - ghost：机器人上报活跃抓鬼会话（state.Robot.GhostActive）；
//   - chain：在线且任务在推进（isTasking；IDLE/等推送视为还没生效，保守留在途）。
const ghostInflightTTL = 120 * time.Second

// dispatchInflightTable 派发在途表（实例级；API 上按种类各挂一个，见 api.go）。
type dispatchInflightTable struct {
	mu sync.Mutex
	at map[string]time.Time // account → 上次派发时间
}

// mark 记录一批"刚下发命令"的号（定时补号 / 卡死重登恢复 / 手动直派都调）。
func (t *dispatchInflightTable) mark(accs []string) {
	if len(accs) == 0 {
		return
	}
	now := time.Now()
	t.mu.Lock()
	if t.at == nil {
		t.at = map[string]time.Time{}
	}
	for _, acc := range accs {
		if acc != "" {
			t.at[acc] = now
		}
	}
	t.mu.Unlock()
}

// count 当前有效在途号数（顺手清理两类失效项）：
//   - live(acc) 返回 true（号已建立活跃会话：命令已生效，避免重复占用）；
//   - 超过 TTL（命令大概率失败/丢失，允许重新派发）。
func (t *dispatchInflightTable) count(live func(acc string) bool) int {
	now := time.Now()
	t.mu.Lock()
	snap := make(map[string]time.Time, len(t.at))
	for acc, at := range t.at {
		snap[acc] = at
	}
	t.mu.Unlock()

	n := 0
	drop := make([]string, 0, 8)
	for acc, at := range snap {
		if now.Sub(at) > ghostInflightTTL || (live != nil && live(acc)) {
			drop = append(drop, acc)
			continue
		}
		n++
	}
	if len(drop) > 0 {
		t.mu.Lock()
		for _, acc := range drop {
			delete(t.at, acc)
		}
		t.mu.Unlock()
	}
	return n
}

// hasFresh 该号是否"刚派发、还没到 TTL"（只看时间，不判 live：判 live 要读状态，
// 调用方通常紧接着自己会判"在跑"，避免重复取数）。顺手清理过期项。
func (t *dispatchInflightTable) hasFresh(acc string) bool {
	if acc == "" {
		return false
	}
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	at, ok := t.at[acc]
	if !ok {
		return false
	}
	if now.Sub(at) > ghostInflightTTL {
		delete(t.at, acc)
		return false
	}
	return true
}

// dropFresh 从一批账号里剔掉"刚派发（TTL 内）"的号，返回 (保留, 剔除)。
// 剔除的号由调用方回带"为什么没派"（不静默）。
func (t *dispatchInflightTable) dropFresh(accs []string) (kept, dropped []string) {
	if len(accs) == 0 {
		return accs, nil
	}
	kept = make([]string, 0, len(accs))
	for _, acc := range accs {
		if t.hasFresh(acc) {
			dropped = append(dropped, acc)
			continue
		}
		kept = append(kept, acc)
	}
	return kept, dropped
}

// markGhostDispatch 记录一批"刚下发抓鬼命令"的号（定时补号 / 卡死重登恢复 / 手动直派都调）。
func (a *API) markGhostDispatch(accs []string) { a.ghostInflight.mark(accs) }

// markChainDispatch 记录一批"刚下发 start_chain（新手链/捉鬼链）"的号（2026-09-23 R1）。
func (a *API) markChainDispatch(accs []string) { a.chainInflight.mark(accs) }

// ghostInflightCount 当前在途号数；live 判定：号已建立活跃抓鬼会话 → 不再算在途。
func (a *API) ghostInflightCount() int {
	return a.ghostInflight.count(func(acc string) bool {
		if a.St == nil {
			return false
		}
		r, ok := a.St.Get(acc)
		return ok && r.GhostActive()
	})
}

// chainInflightCount 当前"链在途"号数（2026-09-23 R1）；live 判定：号在线且任务在推进
// （isTasking）→ 命令已生效，不再算在途。停在 IDLE/ONLINE（等推送/刚上线）不算生效，
// 保守留在途（最多 TTL 120s，之后允许重派）。
func (a *API) chainInflightCount() int {
	return a.chainInflight.count(func(acc string) bool {
		if a.St == nil {
			return false
		}
		r, ok := a.St.Get(acc)
		return ok && r.Online && isTasking(r)
	})
}

// ghostInflightActive 该号是否"抓鬼已派发、会话还没起来"（TTL 内且非活跃会话）。
// 用于候选过滤/去重派发（R2）：命中就不要再派一次。
func (a *API) ghostInflightActive(acc string) bool {
	if !a.ghostInflight.hasFresh(acc) {
		return false
	}
	if a.St != nil {
		if r, ok := a.St.Get(acc); ok && r.GhostActive() {
			return false // 会话已起来：不算在途（与 count 同口径）
		}
	}
	return true
}

// chainInflightActive 该号是否"start_chain 已派发、任务还没推进"（TTL 内且非在跑）。
// 用于候选过滤/去重派发（R1）：命中就不要再派一次。
func (a *API) chainInflightActive(acc string) bool {
	if !a.chainInflight.hasFresh(acc) {
		return false
	}
	if a.St != nil {
		if r, ok := a.St.Get(acc); ok && r.Online && isTasking(r) {
			return false // 链已在推进：不算在途
		}
	}
	return true
}

// restoreInflightCountByKind 恢复引擎"刚补发、还没跑起来"的号数（同样受 TTL 约束），
// 按**命令种类**过滤（2026-09-23 R1/R3）：
//
//	kind=ghost   → 只算 intent=ghost 的补发；
//	kind=newbie  → 算 start_chain 类补发（newbie / zhuaogui 两种意图都是 start_chain）；
//	kind=shenbu  → 算 share_daily_start 类补发（intent=shenbu）。
//
// restorer 的补发不经过 LaunchTask，靠它自己的 Status().LastDispatchAt 记账，
// 否则"补发 + 直派"互相看不见，闸门仍会被连续放行。
func (a *API) restoreInflightCountByKind(kind autotask.Kind) int {
	if a.Restorer == nil {
		return 0
	}
	now := time.Now()
	n := 0
	for _, st := range a.Restorer.Status() {
		if st.LastDispatchAt.IsZero() || now.Sub(st.LastDispatchAt) > ghostInflightTTL {
			continue
		}
		if !restoreKindMatches(st.Kind, kind) {
			continue
		}
		if a.St != nil {
			if r, ok := a.St.Get(st.Account); ok {
				// 会话/链已起来：已计入在跑数，不重复占用
				if kind == autotask.KindGhost && r.GhostActive() {
					continue
				}
				if kind == autotask.KindNewbie && r.Online && isTasking(r) {
					continue
				}
				if kind == autotask.KindShenbu && r.Online && dailyRunningOf(r, a.shareDailyKey()) {
					continue
				}
			}
		}
		n++
	}
	return n
}

// restoreKindMatches 恢复记录的意图 kind 是否属于该池（命令口径：ghost → 抓鬼；
// newbie/zhuaogui → start_chain 类；shenbu → share_daily_start）。
func restoreKindMatches(intentKind string, poolKind autotask.Kind) bool {
	switch poolKind {
	case autotask.KindGhost:
		return intentKind == string(intent.KindGhost)
	case autotask.KindNewbie:
		return intentKind == string(intent.KindNewbie) || intentKind == string(intent.KindZhuaogui)
	case autotask.KindShenbu:
		return intentKind == string(intent.KindShenbu)
	}
	return false
}

// poolQuota 该池"本轮最多还能派几个"（-1 = 不限，保持旧行为）。
//
//   - AutoTask 未装配 / Target<=0 → -1：没配目标 = 不限（沿用 autotask.Config 的 0=不限 语义，
//     也让未配置池的测试环境保持旧行为）；
//   - manual=false（**自动通道**：定时补号 / 恢复引擎补发）：Target>0 且池未启用 → 0
//     （池停用就不自动派，避免"停了还被自动拉起"）；
//   - manual=true（**手动通道**：面板"启动(自动分配)"，用户意图优先）：池停用**不拦**
//     （用户点了启动就是要跑），但仍按目标截断（在跑 + 在途 ≥ 目标 → 0，超编不该手动再加）；
//   - 其余 → max(0, Target - 在跑 - 在途)（抓鬼/新手链各算各的在途：直派/重登恢复 + 恢复引擎补发）。
//
// 2026-09-23 R1：新手链（KindNewbie）也计入在途（原先只有抓鬼）——避免连点「启动(自动分配)」
// 把同一批号重复下发 start_chain。
func (a *API) poolQuota(kind autotask.Kind, manual bool) int {
	if a.AutoTask == nil {
		return -1
	}
	st, ok := a.AutoTask.States()[kind]
	if !ok || st.Target <= 0 {
		return -1
	}
	if !st.Enabled && !manual {
		return 0
	}
	inflight := 0
	switch kind {
	case autotask.KindGhost:
		inflight = a.ghostInflightCount() + a.restoreInflightCountByKind(autotask.KindGhost)
	case autotask.KindNewbie:
		inflight = a.chainInflightCount() + a.restoreInflightCountByKind(autotask.KindNewbie)
	case autotask.KindShenbu:
		inflight = a.dailyInflightCount() + a.restoreInflightCountByKind(autotask.KindShenbu)
	}
	if q := st.Target - st.Online - inflight; q > 0 {
		return q
	}
	return 0
}

// cutByPoolQuota 按池配额截断一批待派号，返回被截断（本次不派）的号。
//
// 配额口径见 poolQuota：-1（不限）/ 0（已满；自动通道下池停用也算 0）/ N（还能派 N 个）。
// 返回的是**尾部**超出配额的号；调用方负责把它们从待派列表里去掉并回带原因。
func (a *API) cutByPoolQuota(accs []string, kind autotask.Kind, manual bool) []string {
	q := a.poolQuota(kind, manual)
	if q < 0 || len(accs) <= q {
		return nil
	}
	if q == 0 {
		return append([]string(nil), accs...)
	}
	return append([]string(nil), accs[q:]...)
}

// poolAllowsDispatch 池状态闸（2026-09-22 P1 / 2026-09-23 P0 加在途 / R1 按池分表）：
// 池未启用 / 配额已用尽（在跑 + 在途 ≥ 目标）→ 不自动补发。
func (a *API) poolAllowsDispatch(kind string) (bool, string) {
	if a.AutoTask == nil {
		return true, ""
	}
	k := autotask.Kind(kind)
	st, ok := a.AutoTask.States()[k]
	if !ok {
		return true, ""
	}
	if !st.Enabled {
		return false, kind + " 池未启用（不自动补发）"
	}
	if st.Target <= 0 {
		return true, "" // 没配目标 = 不限
	}
	if q := a.poolQuota(k, false); q <= 0 {
		inflight := 0
		switch k {
		case autotask.KindGhost:
			inflight = a.ghostInflightCount() + a.restoreInflightCountByKind(autotask.KindGhost)
		case autotask.KindNewbie:
			inflight = a.chainInflightCount() + a.restoreInflightCountByKind(autotask.KindNewbie)
		case autotask.KindShenbu:
			inflight = a.dailyInflightCount() + a.restoreInflightCountByKind(autotask.KindShenbu)
		}
		return false, fmt.Sprintf("%s 池配额已用满（在跑 %d + 在途 %d ≥ 目标 %d，不自动补发）",
			kind, st.Online, inflight, st.Target)
	}
	return true, ""
}

// accountLevel 取该号等级与"服务端要求的抓鬼等级"（运行时优先，其次账号池该区记录）。
func (a *API) accountLevel(acc string) (level, required int) {
	if a.St != nil {
		if r, ok := a.St.Get(acc); ok {
			level, required = r.Level, r.GhostRequiredLevel
		}
	}
	if level == 0 && a.Accounts != nil {
		if pa, ok := a.Accounts.Get(acc); ok {
			if z := pa.Zone(a.gameAddrOf("")); z != nil {
				level = z.Level
			}
		}
	}
	return level, required
}

// ghostGateFor 该号能不能派抓鬼（等级门槛；口径同参考实现 ghost_fit）。
func (a *API) ghostGateFor(acc string) (bool, string) {
	level, required := a.accountLevel(acc)
	return a.ghostGate(level, required)
}

// autotaskCandidates "该做该策略但没在做"的号（离线优先，其次在线空闲）。
func (a *API) autotaskCandidates(kind autotask.Kind) []autotask.Candidate {
	if a.Accounts == nil {
		return nil
	}
	kinds, _ := a.intentKinds()
	gameAddr := a.gameAddrOf("")
	live := map[string]state.Robot{}
	if a.St != nil {
		for _, r := range a.St.Snapshot() {
			live[r.Account] = r
		}
	}

	out := make([]autotask.Candidate, 0, 32)
	for _, pa := range a.Accounts.List(accounts.Filter{}) {
		acc := pa.Name
		if a.St != nil && a.St.IsRemoved(acc) {
			continue // 用户明确移除过的号不再自动拉起
		}
		// 2026-09-23 R3（操作健壮性审计）：人工暂停（面板点过「停止/停链」）→ 不自动拉起。
		// 再次「启动 / 立即补发 / 上线」会清标（壳层负责），不在这里静默过期。
		if a.St != nil && a.St.IsPaused(acc) {
			continue
		}
		// 2026-09-23 卡死熔断：当日卡死触顶（restore_capped）→ 今天不再当任何池的候选
		// （独立熔断表，跨行删除存续；跨日自动失效 / 面板手动解除后自然回到候选）。
		if a.St != nil && a.St.IsRestoreCapped(acc) {
			continue
		}
		// 2026-09-23 R2/R1：刚派发、命令还没生效的号不算候选 ——
		// 防"手动「启动(自动分配)」+ 定时任务"在几十秒空窗里重复派同一号
		// （抓鬼重复=机器人端重启会话；新手链重复=重复 start_chain）。TTL 过后自动回到候选。
		if kind == autotask.KindGhost && a.ghostInflightActive(acc) {
			continue
		}
		if kind == autotask.KindNewbie && a.chainInflightActive(acc) {
			continue
		}
		if kind == autotask.KindShenbu && a.dailyInflightActive(acc) {
			continue
		}
		z := pa.Zone(gameAddr)
		r, hasLive := live[acc]
		level, chainDone := 0, false
		if z != nil {
			level, chainDone = z.Level, z.ChainDone
			if !z.Usable {
				continue // 该区不可用（验证失败/封号）不拉
			}
		} else if !hasLive {
			continue // 该区既无池内记录也无运行时状态：不猜
		}
		if hasLive {
			if r.Level > 0 {
				level = r.Level
			}
			chainDone = chainDone || r.ChainDone
		}
		// 当日卡死 ≥3 次（反复卡又反复重登的 churn 号）→ 今天不再自动拉起，等人工看
		if r.StuckCount >= 3 && r.StuckDay == time.Now().Format("20060102") {
			continue
		}
		if r.ErrRepeat >= 3 {
			continue // 同错 ≥3 = 卡住等人工，别自动拉起（与恢复引擎同口径）
		}
		if hasLive && isTasking(r) {
			continue // 在跑就不打扰
		}

		switch kind {
		case autotask.KindNewbie:
			if chainDone || (level > 0 && level >= a.newbieMaxLevel()) {
				continue // 毕业线以上/已完成 → 归抓鬼
			}
			if level <= 0 && kinds[acc] != intent.KindNewbie && !a.usableInZone(acc) {
				continue // 等级未知、无意图、又没验证可用：不瞎拉（先把号验证/上线报等级）
			}
			if kinds[acc] != "" && kinds[acc] != intent.KindNewbie {
				continue
			}
			out = append(out, autotask.Candidate{Account: acc, Online: hasLive && r.Online, Level: level,
				Reason: fmt.Sprintf("新手链（%d 级，未毕业）", level)})
		case autotask.KindGhost:
			// 2026-09-22 今日抓鬼已满（服务端 50 次上限，钟馗只回闲聊菜单）→ 当天不再派，
			//   跨日自动恢复；否则会"满额→下线→又被拉起→钟馗空转"循环堆积。
			if a.St != nil && a.St.GhostDoneToday(acc) {
				continue
			}
			// 未毕业（新手链未完成）一律不派抓鬼，**含等级未知（<=0）**：
			// 旧判据 `level > 0 &&` 让 level=0 穿过这道过滤，ghostGate 又不拦 0，
			// 于是 1 级新手号在上线瞬间被当抓鬼候选下发，服务端以通知码 71
			// （等级不足，要求 ≥13 级）拒绝，号白跑一趟（2026-09-21 robot0009905）。
			// newbieMaxLevel() 恒 > 0，所以 `level < a.newbieMaxLevel()` 天然覆盖 level<=0。
			// 已毕业但等级未知的仍允许：服务端 gate / 学到的 required_level 兜底，别误伤重连中的号。
			if !chainDone && level < a.newbieMaxLevel() {
				continue // 没毕业 / 等级未知且未毕业 → 不派抓鬼
			}
			if kinds[acc] != "" && kinds[acc] != intent.KindGhost {
				continue
			}
			if ok, _ := a.ghostGate(level, r.GhostRequiredLevel); !ok {
				continue // 等级门槛（含服务端学到的 required_level）
			}
			if ghostDailyFull(r) {
				continue // 今日已抓满（重登也会被服务端拒）
			}
			// 2026-09-23 有领双的必须优先抓鬼（用户口径）：今日已领双倍 → Priority，
			//   由 autotask.pickBatch 保证先于普通候选被挑中（双倍有时长，领了要尽快用）。
			dbl := r.DoubleClaimedToday()
			reason := fmt.Sprintf("抓鬼（%d 级）", level)
			if dbl {
				reason = fmt.Sprintf("抓鬼（%d 级·今日领双优先）", level)
			}
			out = append(out, autotask.Candidate{Account: acc, Online: hasLive && r.Online, Level: level,
				Priority: dbl, Reason: reason})

		case autotask.KindHatch:
			// 孵化：**在线 + 抓鬼已满 + 有蛋**（口径见 hatchCandidates——
			// 判据集中在 hatch.go，这里只负责并入候选表，避免两处漂移）。
			if !hasLive || !r.Online {
				continue // 离线号拉了也做不了（先按其它策略上线，下一轮自然进候选）
			}
			if r.HatchActive() {
				continue // 已在孵化（running 由活跃会话数算，别再重复下发）
			}
			if !ghostFullForHatch(r) {
				continue // 抓鬼没满/今天没抓过 → 先去抓鬼，别把还在抓鬼的号拉走
			}
			plan, ok, _ := hatchPlanOf(r)
			if !ok {
				continue
			}
			out = append(out, autotask.Candidate{Account: acc, Online: true, Level: level,
				Reason: fmt.Sprintf("孵化（%s，图 %d）", hatchKindLabel(plan.Kind), plan.MapID)})

		case autotask.KindShenbu:
			// 大唐神捕（分享日常）：**等级 ≥ 服务端票条件（默认 40）+ 今日未满 + 没在跑别的链**。
			// 与抓鬼的分工（用户口径 2026-09-23"填补抓鬼满额后的空档"）：抓鬼未满且意图=抓鬼
			// 的号归抓鬼；本候选只收"神捕意图 / 无意图 / **抓鬼已满**"的号（满额的号抓鬼池
			// 已经不派它了）。轮转调度（随机起点 + 跑满自动转）P2 再接。
			if level < a.shareDailyMinLevel() {
				continue // 等级未知(0)/不足：服务端按票条件拒（≥40），别白跑
			}
			if a.shareDailyFullToday(acc, r) {
				continue // 今日神捕已满/不可用（含"机器人已不再上报 daily"的独立表兜底）
			}
			if a.shareDailyMoneyShort(r) {
				continue // 余额闸（策略配置 balance_gate；余额未知不拦）：传送费不够 → 派了又停
			}
			if k := kinds[acc]; k != "" && k != intent.KindShenbu {
				ghostFull := (a.St != nil && a.St.GhostDoneToday(acc)) || ghostDailyFull(r)
				if !(k == intent.KindGhost && ghostFull) {
					continue // 别的链在用（新手链 / 抓鬼未满）→ 让路
				}
			}
			out = append(out, autotask.Candidate{Account: acc, Online: hasLive && r.Online, Level: level,
				Reason: fmt.Sprintf("大唐神捕（%d 级）", level)})
		}
	}
	return out
}

// todayKey 本地日期键（YYYYMMDD）：跨夜判定"机器人快照是否今天的"用
// （中控与机器人同机部署，口径一致；服务端 0 点切日）。
func todayKey() string { return time.Now().Format("20060102") }

// ghostDailyFull 今日抓鬼是否已满（机器人上报的 ghost.done/limit；缺数据时不判满）。
//
// 2026-09-23 跨夜误判修复：快照必须"今天的"才判满 —— 号抓满转游荡后快照停在
// DONE/50，跨夜（服务端 0 点已清零）若仍按旧快照判满，候选过滤会一直跳过该号、
// 不给派抓鬼（现场 139 个号在 00:00~06:28 被旧计数误判）。机器人已在心跳
// ghost.count_date 上报计数日期；缺字段（旧版上报）时保持旧行为。
func ghostDailyFull(r state.Robot) bool {
	if r.Ghost == nil {
		return false
	}
	if cd := toStr(r.Ghost["count_date"]); cd != "" && cd != todayKey() {
		return false // 跨夜旧快照：视为未知，不判满
	}
	done := toInt(r.Ghost["done"], 0)
	limit := toInt(r.Ghost["limit"], 50)
	if st, _ := r.Ghost["state"].(string); st == "DONE" {
		return true
	}
	return limit > 0 && done >= limit
}

func (a *API) newbieMaxLevel() int {
	if a.Cfg != nil && a.Cfg.NewbieMaxLevel > 0 {
		return a.Cfg.NewbieMaxLevel
	}
	return 31
}

// isTasking 是否"在跑/不该被自动打扰"（判据 = waterline.Busy，与在线水位保持器、游荡池
// roampool.Idle、恢复引擎 needsRestore 同一份口径；2026-09-23 删掉此前的复制实现，避免第三套口径）。
//
// 2026-09-22 纳入游荡/孵化（Walking）：游荡与抓鬼在机器人端互斥，把游荡号当空闲号下发抓鬼
// 会打断游荡（半月岛试跑实测被抢断）。
//
// 2026-09-23 与地图页口径（26f4660）补齐：
//   - **SUBMIT**（交付/提交中）= 推进中，与 FIGHT/NAV 并列，不算空闲、不派活；
//   - **ERROR**（机器人上报的卡住/停链态）= 异常，同样不派活、不当可回收（等人工/机器人端自愈）；
//   - 顺带并入 Fight / GhostActive（三池交互审计 C5：修复前 39~43 个"正在跑"的号
//     —— WAIT_GHOST/READY/SUBMIT 态但抓鬼会话活跃 —— 被当空闲候选混进候选表）。
//
// 调用点：autotaskCandidates 的"在跑就不打扰"，链在途的"命令已生效"（同一份判据）。
func isTasking(r state.Robot) bool { return waterline.Busy(r) }

// usableInZone 该号在当前区是否"已验证可用"（池内该区记录 usable=true 或运行时在线）。
func (a *API) usableInZone(acc string) bool {
	if a.St != nil {
		if r, ok := a.St.Get(acc); ok && r.Online {
			return true
		}
	}
	return a.Accounts != nil && a.Accounts.Usable(acc, a.gameAddrOf(""))
}

// poolCounts 号池分区统计（可用数 / 在跑数）：面板"往对应池里拉号"与目标缺口显示用。
func (a *API) poolCounts(kind autotask.Kind) (usable, running int) {
	if kind == autotask.KindHatch || kind == autotask.KindShenbu {
		// 孵化与大唐神捕都**没有独立分区**（号本来就在新手池/抓鬼池里）：
		// usable = 当前可拉起的候选数（判据与自动任务同一套），running = 该策略在跑数。
		return len(a.autotaskCandidates(kind)), a.autotaskOnlineCount(kind)
	}
	if a.Accounts == nil {
		return 0, 0
	}
	gameAddr := a.gameAddrOf("")
	for _, pa := range a.Accounts.List(accounts.Filter{}) {
		if a.poolOf(pa.Name) != string(kind) {
			continue
		}
		if a.St != nil && a.St.IsRemoved(pa.Name) {
			continue
		}
		// 2026-09-23 R3：人工暂停的号不算"可拉"（面板"可用"与真实可派口径一致）。
		if a.St != nil && a.St.IsPaused(pa.Name) {
			continue
		}
		if z := pa.Zone(gameAddr); z == nil || !z.Usable {
			continue
		}
		usable++
	}
	return usable, a.autotaskOnlineCount(kind)
}

// autotaskOnlineCount 该策略当前"在跑"数（面板 running 与保持数差额用）。
//
// 2026-09-21 口径修正（生产：40+ 个号卡 DIALOG 但保持数判定"已达标"不补）：
// 抓鬼按**活跃抓鬼会话**计数（ghost.enabled=true），不是"在线 + 意图=ghost"。旧口径把
// "在线但没有活跃抓鬼会话"的号（重连/重登后命令没补发、卡死在对话里的）也算在跑 →
// running 虚高（实测 127 vs 真实 41），`cur >= TargetOnline` 永远成立，autotask
// 不再补号，那批号就永久空转。字段非空但已停的（现场 43 个：27 DONE/15 DIALOG/1 IDLE）
// 不算在跑，否则 running 虚高、保持数永久"已达标"不补号。新手链没有会话字段，仍按"在线 + 意图"计。
func (a *API) autotaskOnlineCount(kind autotask.Kind) int {
	kinds, _ := a.intentKinds()
	n := 0
	if a.St == nil {
		return 0
	}
	for _, r := range a.St.Snapshot() {
		if !r.Online {
			continue
		}
		if kind == autotask.KindHatch {
			// 孵化按**活跃孵化会话**计（hatch.active=true 且未孵出）——与抓鬼同口径：
			// 意图表里没有 hatch（这些号的意图仍是抓鬼），按意图算永远是 0。
			if r.HatchActive() {
				n++
			}
			continue
		}
		if string(kinds[r.Account]) != string(kind) {
			continue
		}
		if kind == autotask.KindGhost && !r.GhostActive() {
			continue // 在线但无活跃抓鬼会话 = 没真在跑（卡死/会话丢失/已停），不计入保持数
		}
		// 大唐神捕（shenbu）暂按"在线 + 意图"计（与新手链同款，见上：心跳 daily 块落地前
		// 拿不到活跃会话）；口径保守（宁可少派，不会超发），后续收紧为"活跃会话 + 意图兜底"。
		n++
	}
	return n
}

// ---------------------------------------------------------------- 真实动作

// onlineForAuto 上线一批号（密码只从池里取；分批 + 间隔，避免一次砸太多）。
// 与手动「批量上线」共用同一条下发通路（sendOnlineChunks）。
func (a *API) onlineForAuto(accs []string) (int, error) {
	sentAccounts, chunks, _ := a.sendOnlineChunks(accs, a.gameAddrOf(""), 10, 300, "autotask_add")
	if len(sentAccounts) == 0 {
		return 0, errors.New("没有可上线的号（池里没密码或通道未连接）")
	}
	a.Store.LogEvent(map[string]any{"type": "api", "action": "autotask_online",
		"zone": a.currentZoneKey(), "requested": len(accs), "sent": len(sentAccounts), "chunks": chunks})
	return len(sentAccounts), nil
}

// registerForAuto 自动注册：按配置前缀/序号/后缀生成名字 → **后台**注册并入池，
// 立即返回计划名单（引擎只关心"下一轮有号可挑"，不等注册完成）。
func (a *API) registerForAuto(count int) ([]string, error) {
	if a.Accounts == nil {
		return nil, errors.New("账号池不可用")
	}
	if count <= 0 {
		count = 10
	}
	if count > createMaxBatch {
		count = createMaxBatch
	}
	prefix, suffix, pad := "robot", "@xy3.com", 7
	if a.Cfg != nil {
		if strings.TrimSpace(a.Cfg.AutoRegisterPrefix) != "" {
			prefix = strings.TrimSpace(a.Cfg.AutoRegisterPrefix)
		}
		if strings.TrimSpace(a.Cfg.AutoRegisterSuffix) != "" {
			suffix = strings.TrimSpace(a.Cfg.AutoRegisterSuffix)
		}
		if a.Cfg.AutoRegisterPad > 0 {
			pad = a.Cfg.AutoRegisterPad
		}
	}
	// 起始序号 = 池内同前缀最大序号 +1；**从这儿往后逐个试**：
	// 服务端常常早有一批已注册的号（本地库没密码），撞上了就跳过继续往后找，
	// 直到凑够 count 个（或试满 maxTry 个序号）。不这么做的话，一次撞上 10 个"已存在"就白跑一轮。
	base := a.Accounts.NextSeq(prefix, suffix)
	maxTry := count * 5
	if maxTry < 20 {
		maxTry = 20
	}
	if maxTry > 200 {
		maxTry = 200
	}
	planned := make([]string, 0, count)
	for i := 1; i <= count; i++ {
		planned = append(planned, fmt.Sprintf("%s%0*d%s", prefix, pad, base+i, suffix))
	}
	addr := a.gameAddrOf("")
	go func() {
		regOpt := accountcreate.DefaultOptions()
		probeOpt := accountverify.DefaultOptions()
		if a.Cfg != nil && strings.TrimSpace(a.Cfg.GameVersion) != "" {
			probeOpt.Version = a.Cfg.GameVersion
		}
		created, tried, existed, failed := []string{}, 0, 0, 0
		for i := 1; i <= maxTry && len(created) < count; i++ {
			name := fmt.Sprintf("%s%0*d%s", prefix, pad, base+i, suffix)
			tried++
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			res := a.createOrReuse(ctx, name, addr, "", accountcreate.DefaultPasswordLen, regOpt, probeOpt, true)
			cancel()
			switch {
			case res.OK: // created 或 existing（已存在且库里密码可用 → 也已入池）
				created = append(created, name)
			case res.Status == "password_mismatch": // 服务端已有、我们没密码 → 跳过，继续往后找
				existed++
			default:
				failed++
				a.Log.Printf("[AUTOTASK] 自动注册失败 %s: %s", name, res.Msg)
			}
		}
		msg := fmt.Sprintf("自动注册完成：成功 %d/%d 个（试了 %d 个序号，跳过已存在 %d 个，失败 %d 个）",
			len(created), count, tried, existed, failed)
		a.Log.Printf("[AUTOTASK] %s", msg)
		a.Store.LogEvent(map[string]any{"type": "api", "action": "autotask_register",
			"zone": a.currentZoneKey(), "requested": count, "created": len(created),
			"tried": tried, "skipped_existing": existed, "failed": failed, "accounts": created})
		if a.AutoTask != nil {
			a.AutoTask.NoteRegister(autotask.KindNewbie, len(created)) // 全失败 → 长退避，别撞风控
		}
	}()
	return planned, nil
}

// passwordFor 取该号在当前区的密码（按池；没有统一密码）。
func (a *API) passwordFor(acc string) (string, bool) {
	if a.Accounts == nil {
		return "", false
	}
	return a.Accounts.PasswordFor(acc, a.gameAddrOf(""))
}

// LaunchTask 按策略下发任务（定时任务 / 卡死恢复 / 手动启动共用同一套载荷口径）。
//
// 2026-09-23 R3：入口统一过滤"人工暂停"的号（用户点过「停止/停链」）；
// 手动「启动 / 立即补发」路径在入口先清标，所以不会走到这里被误拦。
func (a *API) LaunchTask(kind autotask.Kind, accs []string) (bool, string) {
	if len(accs) == 0 || a.Events == nil {
		return false, "没有账号或事件通道不可用"
	}
	if paused := a.dropPaused(accs); len(paused) != len(accs) {
		accs = paused
		if len(accs) == 0 {
			return false, "全部为人工暂停（面板点过「停止」；再点「启动/立即补发/上线」即解除）"
		}
	}
	switch kind {
	case autotask.KindNewbie:
		chainID := a.Cfg.DefaultChainID
		cmd := map[string]any{"cmd": "start_chain", "chain_id": chainID, "accounts": accs}
		chain, err := chainlib.Build(chainID, a.Cfg.ChainDir)
		if err == nil {
			cmd["chain"] = chain
			if chain.ChainID != "" {
				cmd["chain_id"] = chain.ChainID
			}
		} else if !errors.Is(err, chainlib.ErrNotFound) {
			return false, "链数据解析失败: " + err.Error()
		}
		if !a.Events.SendCmd(cmd, "autotask_start_chain") {
			return false, "下发失败：机器人通道未连接"
		}
		a.markChainDispatch(accs) // 2026-09-23 R1：新手链在途记账（配额/候选据此防重复派）
		return true, "已下发新手链: " + strings.Join(accs, ", ")

	case autotask.KindGhost:
		nav, err := a.chainPayloads().Ghost()
		if err != nil {
			return false, "抓鬼导航数据不可用: " + err.Error()
		}
		ghostChainID := a.chainPayloads().GhostNavChainID()
		if nav.ChainID != "" {
			ghostChainID = nav.ChainID
		}
		cmd := map[string]any{"cmd": "ghost_start", "chain_id": ghostChainID, "chain": nav,
			"role": "solo", "daily_limit": a.chainPayloads().GhostDailyLimit(), "accounts": accs}
		if done := a.ghostDoneMap(accs); len(done) > 0 {
			cmd["done"] = done
		}
		if !a.Events.SendCmd(cmd, "autotask_ghost_start") {
			return false, "下发失败：机器人通道未连接"
		}
		a.markGhostDispatch(accs) // 2026-09-23 P0：在途记账（定时补号/卡死重登恢复共走这里）
		return true, "已下发抓鬼: " + strings.Join(accs, ", ")

	case autotask.KindHatch:
		// 孵化：按号推导蛋种/蛋编号/目标图 → 分组下发 hatch_start（载荷组装与会话记账见 hatch.go）
		return a.launchHatch(accs)

	case autotask.KindShenbu:
		// 大唐神捕（分享日常，2026-09-23 接入）：载荷 = 基座 newbie_full + shenbu_nav 声明
		// （发送时组装，见 chainpayload.go:ShareDaily）；命令与 ghost_start 同构。
		return a.launchShareDaily(accs)
	}
	return false, "未知策略: " + string(kind)
}

// ---------------------------------------------------------------- 面板接口

// handleAutoTaskGet 定时自动任务状态（只读）：三套策略 + 候选数 + 待恢复名单。
func (a *API) handleAutoTaskGet(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{"ok": true, "tasks": []any{}, "reghost": []any{}, "hatch_sessions": []any{}}
	cands := map[string]int{}
	for _, k := range autotask.Kinds {
		cands[string(k)] = len(a.autotaskCandidates(k))
	}
	resp["candidates"] = cands
	// 号池分区（新手池 / 抓鬼池）：可用数 + 在跑数 + 目标/缺口
	pools := map[string]any{}
	for _, k := range autotask.Kinds {
		usable, running := a.poolCounts(k)
		target := 0
		if a.AutoTask != nil {
			target = a.AutoTask.States()[k].Target
		}
		pools[string(k)] = map[string]any{"usable": usable, "running": running,
			"target": target, "deficit": target - running}
	}
	resp["pools"] = pools
	resp["hatch_sessions"] = a.hatchSessionList() // 在孵化的号（中控侧记账：到期/完成都按它收工）
	if a.AutoTask != nil {
		states := a.AutoTask.States()
		list := make([]any, 0, len(states))
		for _, k := range autotask.Kinds {
			if st, ok := states[k]; ok {
				list = append(list, st)
			}
		}
		resp["tasks"] = list
	}
	if a.Reghost != nil {
		resp["reghost"] = a.Reghost.Status()
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleAutoTaskStart 启动/改参数某个策略（写接口，可选鉴权）。
func (a *API) handleAutoTaskStart(w http.ResponseWriter, r *http.Request) {
	if a.AutoTask == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "定时任务引擎未启用"})
		return
	}
	body := readBody(r)
	kind := autotask.Kind(strings.ToLower(strings.TrimSpace(toStr(body["kind"]))))
	if !kind.Valid() {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "kind 必须是 newbie / ghost / hatch / shenbu"})
		return
	}
	prev := a.AutoTask.States()[kind].Config
	cfg := autotask.Config{
		Kind:            kind,
		IntervalSec:     pickInt(toInt(body["interval_sec"], 0), prev.IntervalSec),
		JitterSec:       pickInt(toInt(body["jitter_sec"], -1), prev.JitterSec),
		BatchMin:        pickInt(toInt(body["batch_min"], 0), prev.BatchMin),
		BatchMax:        pickInt(toInt(body["batch_max"], 0), prev.BatchMax),
		TargetOnline:    pickInt(toInt(body["target_online"], -1), prev.TargetOnline),
		MaxOnline:       pickInt(toInt(body["max_online"], -1), prev.MaxOnline),
		RegisterEnabled: toBool(body["register_enabled"], prev.RegisterEnabled),
		RegisterCount:   pickInt(toInt(body["register_count"], 0), prev.RegisterCount),
		LaunchDelaySec:  pickInt(toInt(body["launch_delay_sec"], -1), prev.LaunchDelaySec),
		MaxMinutes:      pickInt(toInt(body["max_minutes"], 0), prev.MaxMinutes),
		// 2026-09-23 前端口径：新日常（shenbu/fenghuo）参数。
		//   - min_level 0 = 未配置（回落全局 CTRL_SHARE_DAILY_MIN_LEVEL / 默认 40）；
		//   - balance_gate **允许显式 0**（0=不启用余额闸），缺省才沿用原值（toInt(nil, prev)）。
		MinLevel:    pickInt(toInt(body["min_level"], 0), prev.MinLevel),
		BalanceGate: toInt(body["balance_gate"], prev.BalanceGate),
	}
	if err := a.AutoTask.Start(kind, cfg); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": err.Error()})
		return
	}
	a.Store.LogEvent(map[string]any{"type": "api", "action": "autotask_start", "zone": a.currentZoneKey(),
		"kind": string(kind), "config": cfg})
	a.SaveAutoTask() // 参数落盘：重启中控不丢保持数/间隔/批量（2026-09-21 生产：重启后全变 0）
	msg := fmt.Sprintf("%s 定时任务已启动（间隔 %ds + 随机 %ds，每轮 %d~%d 个）",
		kind.Label(), cfg.IntervalSec, cfg.JitterSec, cfg.BatchMin, cfg.BatchMax)
	if kind == autotask.KindHatch {
		msg += fmt.Sprintf("；单次孵化限时 %d 分钟（到期中控下发 hatch_stop）", cfg.MaxMinutes)
	}
	if kind == autotask.KindShenbu {
		msg += fmt.Sprintf("；分享日常日限 %d（随命令下发）", a.chainPayloads().ShareDailyLimit())
		if cfg.MinLevel > 0 {
			msg += fmt.Sprintf("、等级门槛 %d", cfg.MinLevel)
		}
		if cfg.BalanceGate > 0 {
			msg += fmt.Sprintf("、余额闸 %d", cfg.BalanceGate)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "state": a.AutoTask.States()[kind], "msg": msg,
	})
}

// handleAutoTaskStop 停止某个策略（写接口）。
func (a *API) handleAutoTaskStop(w http.ResponseWriter, r *http.Request) {
	if a.AutoTask == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "定时任务引擎未启用"})
		return
	}
	kind := autotask.Kind(strings.ToLower(strings.TrimSpace(toStr(readBody(r)["kind"]))))
	if !kind.Valid() {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "kind 必须是 newbie / ghost / hatch / shenbu"})
		return
	}
	a.AutoTask.Stop(kind)
	a.SaveAutoTask() // 参数落盘：重启中控后保持"用户停过的策略不自动拉起"
	a.Store.LogEvent(map[string]any{"type": "api", "action": "autotask_stop", "zone": a.currentZoneKey(), "kind": string(kind)})
	msg := kind.Label() + " 定时任务已停止"
	if kind == autotask.KindHatch {
		// 孵化是有时长的动作：停策略顺手给在孵化的号收工（否则号会一直游荡没人管到期）
		if n := a.hatchStopAll(kind.Label() + "定时任务已停止"); n > 0 {
			msg += fmt.Sprintf("；已给 %d 个在孵化的号下发 hatch_stop 收工", n)
		}
	}
	if kind == autotask.KindShenbu {
		// 分享日常也是有时长的动作（跑到日限才停）：停策略顺手给在跑的号收工
		//（否则号会一直跑到 10/10，用户观感"停不住"）。
		if n := a.shareDailyStopAll(kind.Label() + "定时任务已停止"); n > 0 {
			msg += fmt.Sprintf("；已给 %d 个在跑的号下发 share_daily_stop 收工", n)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": a.AutoTask.States()[kind], "msg": msg})
}

// handleAutoTaskRun 立即跑一轮（写接口；仍受 MaxOnline 限制）。
func (a *API) handleAutoTaskRun(w http.ResponseWriter, r *http.Request) {
	if a.AutoTask == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "定时任务引擎未启用"})
		return
	}
	kind := autotask.Kind(strings.ToLower(strings.TrimSpace(toStr(readBody(r)["kind"]))))
	if !kind.Valid() {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "kind 必须是 newbie / ghost / hatch / shenbu"})
		return
	}
	a.AutoTask.RunNow(kind)
	rounds := a.AutoTask.Tick(time.Now())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "rounds": rounds,
		"state": a.AutoTask.States()[kind], "msg": kind.Label() + " 已立即跑一轮"})
}

// handleRegHostCancel 取消某个号的自动重登恢复（用户手动停止/下机时用）。
func (a *API) handleRegHostCancel(w http.ResponseWriter, r *http.Request) {
	acc := strings.TrimSpace(toStr(readBody(r)["account"]))
	if acc == "" || a.Reghost == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "缺少 account 或恢复引擎未启用"})
		return
	}
	ok := a.Reghost.Cancel(acc)
	msg := acc + " 的自动恢复已取消"
	if !ok {
		msg = acc + " 没有待恢复记录"
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "canceled": ok, "msg": msg})
}

// handleRegHostResume 解除"当日卡死熔断"（2026-09-23）：让 capped 号提前回到自动通道。
//
//	POST /api/reghost/resume
//	{} 或 {"account": "a@x.com"}   （省略 account = 全部解除）
//
// 语义：熔断本来跨日 0 点自动失效；这里是**提前解除**（等于告诉系统"今天还愿意再给它
// 3 次重登机会"）。解除后：
//   - 清独立熔断表 + robot 行内当日卡死计数 + reghost 内存条目；
//   - 该号回到正常自动通道（下一轮 autotask/restorer 按意图拉起；若仍卡 ERROR 则按
//     既有口径不补发 —— 等它自己恢复或人工「上线/重登」）。
func (a *API) handleRegHostResume(w http.ResponseWriter, r *http.Request) {
	acc := strings.TrimSpace(toStr(readBody(r)["account"]))
	var resumed, notCapped []string
	if acc != "" {
		resumed, notCapped = a.uncapAccounts([]string{acc})
	} else if a.St != nil {
		snap := a.St.RestoreCappedList()
		accs := make([]string, 0, len(snap))
		for _, it := range snap {
			if s, _ := it["account"].(string); s != "" {
				accs = append(accs, s)
			}
		}
		resumed, notCapped = a.uncapAccounts(accs)
	}
	msg := fmt.Sprintf("已解除 %d 个号的卡死熔断（回到自动通道；跨日 0 点本会自动解除）", len(resumed))
	if acc != "" && len(resumed) == 0 {
		msg = acc + " 未处于卡死熔断（无需解除）"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "resumed": resumed, "count": len(resumed), "not_capped": notCapped, "msg": msg,
	})
}

// uncapAccounts 解除一批号的"当日卡死熔断"（手动解除入口；2026-09-23）：
//   - 清独立熔断表（提前结束"等次日"）；
//   - 清 robot 行内当日卡死计数（不清的话 restorer 补发闸 / autotask 候选过滤仍按旧计数拦着）；
//   - 取消 reghost 内存里的 capped 条目（面板"待恢复"列表立刻清干净，不必等跨日）。
//
// 返回 (实际解除的号, 未处于熔断的号)，均升序。幂等：重复调用不报错。
// "有熔断痕迹"= 熔断表有条目 **或** 行内当日卡死计数 ≥ ChurnLimit（中控重启后表空、
// 计数仍在的半态 —— 用户点解除时必须一并清掉，否则"解除无效"）。
func (a *API) uncapAccounts(accs []string) (resumed, notCapped []string) {
	resumed, notCapped = []string{}, []string{}
	for _, acc := range accs {
		if acc == "" {
			continue
		}
		was := false
		if a.St != nil {
			was = a.St.ClearRestoreCapped(acc)
			if a.stuckCountToday(acc) >= reghost.ChurnLimit {
				was = true
			}
			a.St.ClearStuckToday(acc)
		}
		if a.Reghost != nil {
			a.Reghost.Cancel(acc)
		}
		if was {
			resumed = append(resumed, acc)
		} else {
			notCapped = append(notCapped, acc)
		}
	}
	sort.Strings(resumed)
	sort.Strings(notCapped)
	return
}

// pickInt 取请求值（<=0/-1 表示"没传，用原值"）。
func pickInt(v, prev int) int {
	if v == 0 || v == -1 {
		return prev
	}
	if v < 0 {
		return 0
	}
	return v
}
