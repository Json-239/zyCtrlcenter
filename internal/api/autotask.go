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

// GhostSkipFunc 供恢复引擎（restorer）用的"派发前最后一道闸"：抓鬼等级门槛不过就不补发。
// 导出给 main 装配（restorer 在 api 之前构造，用闭包晚绑定）。
func (a *API) GhostSkipFunc() func(kind, account string) (bool, string) {
	return func(kind, account string) (bool, string) {
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

// ---------------------------------------------------------------- 抓鬼在途记账（2026-09-23 P0）
//
// 为什么需要：抓鬼"在跑数"以机器人上报的活跃会话（ghost.enabled）为准，而
// "命令下发 → 机器人建立会话"之间有几十秒空窗。批量上线时（一次 add 100+ 号），
// 池闸若只看会话数，每一批新号都会看到"缺口仍在"而被连续放行 —— 生产实测一次
// 直派/补发 200+（超 target 100，见 docs/04-测试/分析-20260923-抓鬼分配逻辑.md）。
// 这里把"已派发未确认"的号也计入占用量：配额判断用
// "活跃会话 + 在途"，避免"上一批还在路上就放行下一批"。
const ghostInflightTTL = 120 * time.Second

// ghostInflightTable 抓鬼在途表（实例级；挂在 API 上，见 api.go 的 ghostInflight 字段）。
type ghostInflightTable struct {
	mu sync.Mutex
	at map[string]time.Time // account → 上次派发时间
}

// mark 记录一批"刚下发抓鬼命令"的号（定时补号 / 卡死重登恢复 / 手动直派都调）。
func (t *ghostInflightTable) mark(accs []string) {
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
//   - live(acc) 返回 true（号已建立活跃抓鬼会话：命令已生效，避免重复占用）；
//   - 超过 TTL（命令大概率失败/丢失，允许重新派发）。
func (t *ghostInflightTable) count(live func(acc string) bool) int {
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

// markGhostDispatch 记录一批"刚下发抓鬼命令"的号（定时补号 / 卡死重登恢复 / 手动直派都调）。
func (a *API) markGhostDispatch(accs []string) { a.ghostInflight.mark(accs) }

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

// restoreInflightCount 恢复引擎"刚补发、还没跑起来"的号数（同样受 TTL 约束）。
// restorer 的补发不经过 LaunchTask，靠它自己的 Status().LastDispatchAt 记账，
// 否则"补发 + 直派"互相看不见，闸门仍会被连续放行。
func (a *API) restoreInflightCount() int {
	if a.Restorer == nil {
		return 0
	}
	now := time.Now()
	n := 0
	for _, st := range a.Restorer.Status() {
		if st.LastDispatchAt.IsZero() || now.Sub(st.LastDispatchAt) > ghostInflightTTL {
			continue
		}
		if a.St != nil {
			if r, ok := a.St.Get(st.Account); ok && r.GhostActive() {
				continue // 会话已起来：已计入在跑数
			}
		}
		n++
	}
	return n
}

// poolQuota 该池"本轮最多还能派几个"（-1 = 不限，保持旧行为）。
//
//   - AutoTask 未装配 / Target<=0 → -1：没配目标 = 不限（沿用 autotask.Config 的 0=不限 语义，
//     也让未配置池的测试环境保持旧行为）；
//   - Target>0 且池未启用 → 0：池停用就一个都不直派（避免"池停了还被手动/自动拉起"）；
//   - 其余 → max(0, Target - 在跑 - 在途)（抓鬼的在途 = 直派/重登恢复 + 恢复引擎补发两份）。
func (a *API) poolQuota(kind autotask.Kind) int {
	if a.AutoTask == nil {
		return -1
	}
	st, ok := a.AutoTask.States()[kind]
	if !ok || st.Target <= 0 {
		return -1
	}
	if !st.Enabled {
		return 0
	}
	inflight := 0
	if kind == autotask.KindGhost {
		inflight = a.ghostInflightCount() + a.restoreInflightCount()
	}
	if q := st.Target - st.Online - inflight; q > 0 {
		return q
	}
	return 0
}

// cutByPoolQuota 按池配额截断一批待派号，返回被截断（本次不派）的号。
//
// 配额口径见 poolQuota：-1（不限）/ 0（池停用或已满）/ N（还能派 N 个）。
// 返回的是**尾部**超出配额的号；调用方负责把它们从待派列表里去掉并回带原因。
func (a *API) cutByPoolQuota(accs []string, kind autotask.Kind) []string {
	q := a.poolQuota(kind)
	if q < 0 || len(accs) <= q {
		return nil
	}
	if q == 0 {
		return append([]string(nil), accs...)
	}
	return append([]string(nil), accs[q:]...)
}

// poolAllowsDispatch 池状态闸（2026-09-22 P1 / 2026-09-23 P0 加在途）：
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
	if q := a.poolQuota(k); q <= 0 {
		inflight := 0
		if k == autotask.KindGhost {
			inflight = a.ghostInflightCount() + a.restoreInflightCount()
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
			out = append(out, autotask.Candidate{Account: acc, Online: hasLive && r.Online, Level: level,
				Reason: fmt.Sprintf("抓鬼（%d 级）", level)})

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

// isTasking 是否在跑任务（与恢复引擎 needsRestore 同口径：这些状态不打扰）。
func isTasking(r state.Robot) bool {
	// 2026-09-22: 游荡/孵化中的号也算"在忙"—— 游荡与抓鬼互斥，自动任务
	// 若把它当空闲号下发抓鬼，会打断游荡（半月岛试跑实测被抢断）。
	if r.Walking() {
		return true
	}
	switch strings.ToUpper(strings.TrimSpace(r.State)) {
	case "NAV", "CLICK", "DIALOG", "FIGHT", "SHOP", "ALLOC", "WAIT_NEXT":
		return true
	case "WAIT_TASK":
		return r.TaskIndex != 0
	}
	return false
}

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
	if kind == autotask.KindHatch {
		// 孵化没有独立分区（号本来就在新手池/抓鬼池里）：usable = 当前可拉起的孵化候选数
		// （在线 + 抓鬼已满 + 有蛋，判据与自动任务同一套），running = 活跃孵化会话数。
		return len(a.autotaskCandidates(autotask.KindHatch)), a.autotaskOnlineCount(kind)
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
func (a *API) LaunchTask(kind autotask.Kind, accs []string) (bool, string) {
	if len(accs) == 0 || a.Events == nil {
		return false, "没有账号或事件通道不可用"
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
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "kind 必须是 newbie / ghost / hatch"})
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
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "kind 必须是 newbie / ghost / hatch"})
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
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "kind 必须是 newbie / ghost / hatch"})
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
