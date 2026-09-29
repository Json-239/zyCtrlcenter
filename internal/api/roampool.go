// 游荡池 keeper：api 侧的壳。
//
// 引擎在 internal/services/roampool（纯决策、可注入依赖）；这里只负责把"真实世界"接上：
//   - 取数：当前区机器人快照（判"在游荡/空闲"、算各图人数）；
//   - 缺口：任务池 deficit = 抓鬼 + 新手（**直接读 autotask.Runner.States()**，不走 HTTP 自调）；
//   - 图：链数据里有寻路网格的图（白名单为空时用它）；
//   - 下发/停止：**与面板接口共用同一实现**（DispatchRoam / StopRoam，见 randomwalk.go）——
//     不另写一套，避免"面板下发"与"池子自动派发"漂移。
//
// 面板「大屏」页（web/src/components/Dashboard.vue）的"游荡池"卡与 GET/POST /api/roampool 都走这里。
package api

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"zyctrlcenter/internal/services/autotask"
	"zyctrlcenter/internal/services/roampool"
	"zyctrlcenter/internal/state"
)

// ---------------------------------------------------------------- 依赖装配

// RoampoolDeps 组装"游荡池 keeper"的依赖（main 用它创建 roampool.Keeper）。
func (a *API) RoampoolDeps() roampool.Deps {
	return roampool.Deps{
		Robots: a.roampoolRobots,
		// 2026-09-22 P0：回收资格闸 —— 只回收"回收后真能进任务池"的号（否则回收-重派空转）
		ReclaimEligible: a.roamReclaimEligible,
		// 2026-09-29 P0-2 修法 B：日常池（神捕/烽火）纳入回收目标 ——
		// 池缺员时把"抓鬼满额转游荡"的富余号回收转投 share_daily_start。
		DailyDeficit:         a.roampoolDailyDeficit,
		ReclaimDailyEligible: a.roamDailyReclaimEligible,
		ReclaimDaily:         a.roampoolReclaimDaily,
		Deficit:              a.roampoolDeficit,
		Maps:                 a.roampoolMaps,
		TaskMaps:             a.roampoolTaskMaps,
		Dispatch:             a.roampoolDispatch,
		Stop:                 a.roampoolStop,
		Now:                  time.Now, // P1：Status 算"在途/退避"用
		Log:                  func(format string, args ...any) { a.Log.Printf(format, args...) },
	}
}

// roamReclaimEligible 回收资格（2026-09-22 P0，三池交互分析）：
// 只回收"回收后真能进任务池"的号 ——
//   · 今日抓鬼已满/不可用（GhostDoneToday）：任务池会跳过它；
//   · 等级不到抓鬼门槛（ghostGate）：任务池同样跳过。
// 这两类回收给任务池只是空转（90s 后 auto_roam 又派游荡，实测单号 20+ 次往返），
// 让它们留在游荡池继续游荡（有产出）。
//
// 2026-09-23 有领双的必须优先抓鬼（用户口径）：**今日已领双倍**的号不在这里拦 ——
// 正相反，roampool.PickReclaim 会把它们**置顶优先回收**去抓鬼（双倍有时长）；同时
// PickExcess 不会把已领双倍的抓鬼号转游荡。这里仍按上面两条客观资格判定。
func (a *API) roamReclaimEligible(r state.Robot) bool {
	if a.St != nil && a.St.GhostDoneToday(r.Account) {
		return false
	}
	// 2026-09-24 补（现场 robot0005261 实证）：当日卡死熔断（capped）的号不回收 ——
	// 它被任务链反复派发→抓鬼钟馗对话卡死→熔断，回收只会再来一轮空转（实测同一批号
	// 每分钟被回收 10 个、任务池缺口 2 小时不缩，游荡被抽到 48/110）。
	if a.St != nil && a.St.IsRestoreCapped(r.Account) {
		return false
	}
	// 2026-09-28 穷号闸（G3）：储备金 < 阈值的号不回收 —— 回收去抓鬼也只会"低血买药
	// 552/超时"空转（见 docs/04-测试/分析-20260928-商店买药卡住排查.md），留在游荡
	// 继续有产出；reserve 缺席(=0)不拦（G1/G2 同口径，机器人端兜底）。
	if bad, _ := a.reserveTooLowForGhost(r); bad {
		return false
	}
	lvl, req := a.accountLevel(r.Account)
	if ok, _ := a.ghostGate(lvl, req); !ok {
		return false
	}
	return true
}

// ---------------------------------------------------------------- 日常池回收（2026-09-29 P0-2 修法 B）

// roampoolDailyDeficit 日常池缺口 = 神捕 + 烽火 deficit 之和（正=缺人）。
//
// 口径与 roampoolDeficit（抓鬼/新手）同款：
//   - 未启用 / 没配目标（Target<=0）的池不参与（停用的池不会要人）；
//   - deficit = Target - 在跑 - 在途（在途 = 直派/补发的记账，防"上一批还在路上又回收一批"）。
//
// 与 /api/autotask 面板的 pools.<kind>.deficit（不含在途）略有差异：这里保守多减在途，
// 只用于"是否值得回收"的粗判；真正下发时的配额截断在 roampoolReclaimDaily（cutByPoolQuota）。
func (a *API) roampoolDailyDeficit() int {
	if a.AutoTask == nil {
		return 0
	}
	states := a.AutoTask.States()
	sum := 0
	for _, k := range shareDailyFamilyKinds() {
		st, ok := states[k]
		if !ok || !st.Enabled || st.Target <= 0 {
			continue
		}
		inflight := a.dailyInflightCountOf(k) + a.restoreInflightCountByKind(k)
		if d := st.Target - st.Online - inflight; d > 0 {
			sum += d
		}
	}
	return sum
}

// roamDailyReclaimEligible 日常池回收资格（2026-09-29 P0-2 修法 B）：
// 只回收"回收后神捕/烽火真能接走"的游荡号 —— 否则回收→90s 后机器人 auto_roam 又派游荡，
// 白损耗（与 2026-09-22 抓鬼回收资格闸 roamReclaimEligible 同一教训：单号 20+ 次往返）。
//
// 判据（合取）：
//   - 非人工暂停 / 非当日熔断——回收了也没人会派（restorer 与候选都跳过）；
//   - **抓鬼已满**（GhostDoneToday / 心跳 done≥limit）：号源画像 = "抓鬼满额→转游荡"的号
//     （现场 94% 满额号在游荡；游荡池 running≈124 > target 100 有富余）。没满的号留给
//     抓鬼通道（roamReclaimEligible）与抓鬼候选，本通路不抢；
//   - 对某个**已启用**的日常池满足准入：等级 ≥ 门槛（默认 40）、今日未满、余额闸不拦、
//     心跳背包未近似满（dailyBagTooFull）。
//
// 调用场景：roampool keeper 的 Tick（独立 goroutine，**不在** autotask Runner 持锁回调里，
// 可安全读 States()）。逐号一次 States() 读取（≤~124 游荡号/轮、60s 一轮）开销可忽略。
func (a *API) roamDailyReclaimEligible(r state.Robot) bool {
	_, ok := a.dailyReclaimKindOf(r)
	return ok
}

// dailyReclaimKindOf 该号此刻"回收转投日常池"的目标玩法（第一个就绪的：shenbu → fenghuo，
// 与 decideIntent / 台账兜底的固定次序一致）；没有 → ("", false)。
func (a *API) dailyReclaimKindOf(r state.Robot) (autotask.Kind, bool) {
	if a.St == nil || a.AutoTask == nil {
		return "", false
	}
	if a.St.IsPaused(r.Account) || a.St.IsRestoreCapped(r.Account) {
		return "", false
	}
	if !a.St.GhostDoneToday(r.Account) && !ghostDailyFull(r) {
		return "", false // 抓鬼没满 → 留给抓鬼（本通路不抢）
	}
	states := a.AutoTask.States()
	for _, kind := range shareDailyFamilyKinds() {
		if !a.shareDailyPoolEnabled(kind) {
			continue // 池停用 = 不拉新（口径同 shareDailyPoolEnabled）
		}
		st, ok := states[kind]
		if !ok || st.Target <= 0 {
			continue
		}
		if a.shareDailyReclaimReady(kind, r, st.Config) {
			return kind, true
		}
	}
	return "", false
}

// shareDailyReclaimReady 该号对某日常池的准入（口径与 shareDailyCandidateOf 的"号码自身"
// 判据同源：等级/满额/余额；另加背包预检 + 重复派防护 + **每玩法配额闸**）—— 这里是
// **直发通道**，不含"让路（抓鬼）"这类分配类判据（那条由 dailyReclaimKindOf 把关）。
func (a *API) shareDailyReclaimReady(kind autotask.Kind, r state.Robot, cfg autotask.Config) bool {
	if a.shareDailyInFlightTodayOf(r.Account, r, kind) {
		return false // 该玩法今天已派/在跑（未满）→ 不需要回收转投（防重复下发，也防刚下发的号被再派）
	}
	// 每玩法配额闸（2026-09-29 线上复核补）：该玩法"在跑+在途"已达目标 → 本轮不回收。
	// 背景：服务侧 k.dailyDeficit 是**两玩法聚合**缺口，会出现"神捕缺 3、烽火已满"仍触发回收集合；
	// 若此处不按 kind 复核配额，回收批会在 cutByPoolQuota 被**整批截断** → 每轮空转报
	// "回收→日常池失败"（现场 14:32-14:39 每 30s 一条）。前置拦截后：配额满 = 该玩法无候选，
	// 自然走 noop，不再空转刷失败日志/污染 pool.LastErr。
	if a.poolQuota(kind, false) == 0 {
		return false
	}
	// 跨日常让路（与 shareDailyCandidateOf 同口径）：已在跑/已派**另一个**玩法（未满）→
	// 本通路也不抢（否则两个玩法各派一次，后派者顶掉前者）。
	for _, other := range shareDailyFamilyKinds() {
		if other != kind && a.shareDailyInFlightTodayOf(r.Account, r, other) {
			return false
		}
	}
	if a.shareDailyFullTodayOf(r.Account, r, kind) {
		return false // 今日该玩法已满/不可用（心跳 + 独立满额表双闸）
	}
	if a.shareDailyMoneyShort(r, cfg.BalanceGate) {
		return false // 余额闸（余额未知不拦）
	}
	level, _ := a.accountLevel(r.Account)
	if r.Level > 0 {
		level = r.Level // 心跳等级优先（accountLevel 可能读到池里旧记录）
	}
	if level < a.shareDailyMinLevelOf(kind, cfg) {
		return false
	}
	if dailyBagTooFull(r) {
		return false // 背包预检（2026-09-29 P0-2）：近满包派下去必死在采购/交付
	}
	return true
}

// roampoolReclaimDaily keeper 的 ReclaimDaily 注入实现：把选中的游荡号回收**转投日常池**。
//
// 流程：逐号选玩法（shenbu 优先）→ 按玩法分组 → 配额截断（cutByPoolQuota，池停用=全截）→
// LaunchTask 下发 share_daily_start（与手动「启动」同口径：带链载荷/share_key/daily_limit/done，
// 并记在途防重复派）。机器人端收到 share_daily_start 会主动停游荡（share_daily.py:1983-1989），
// 所以**不需要先发 stop_roam**（少一条命令、也避免两条命令竞争）。
func (a *API) roampoolReclaimDaily(accounts []string) (int, error) {
	if a.St == nil || len(accounts) == 0 {
		return 0, errors.New("没有可回收的号")
	}
	groups := map[autotask.Kind][]string{}
	for _, acc := range accounts {
		r, ok := a.St.Get(acc)
		if !ok {
			continue
		}
		// 复核对齐 keeper 侧的资格闸（同一 Tick 内结果一致；防调用方直接注入）
		if k, ready := a.dailyReclaimKindOf(r); ready {
			groups[k] = append(groups[k], acc)
		}
	}
	if len(groups) == 0 {
		return 0, errors.New("没有号满足日常池准入（等级/满额/余额/包满/熔断闸）")
	}
	total := 0
	attempted := 0
	for _, kind := range shareDailyFamilyKinds() { // shenbu → fenghuo 固定次序
		accs := groups[kind]
		if len(accs) == 0 {
			continue
		}
		if cut := a.cutByPoolQuota(accs, kind, false); len(cut) > 0 {
			a.Log.Printf("[ROAMPOOL] 回收→%s：池配额截断 %d 个（%v 本轮不派）", kind.Label(), len(cut), cut)
			accs = accs[:len(accs)-len(cut)]
			if len(accs) == 0 {
				// 全被截断 = 池在"挑选→下发"窗口内刚好被填满（竞态；常规情形已被
				// shareDailyReclaimReady 的每玩法配额闸前置拦截）。不派、记一条，不当故障刷屏。
				a.Log.Printf("[ROAMPOOL] 回收→%s 跳过：池配额在截断时已满（本轮不派）", kind.Label())
				continue
			}
		}
		attempted++
		ok, msg := a.LaunchTask(kind, accs)
		if !ok {
			a.Log.Printf("[ROAMPOOL] 回收→%s 下发失败：%s", kind.Label(), msg)
			continue
		}
		total += len(accs)
		a.Log.Printf("[ROAMPOOL] 回收→%s %d 个游荡号已转投 share_daily_start：%v", kind.Label(), len(accs), accs)
	}
	if total == 0 {
		if attempted == 0 {
			return 0, errors.New("池配额已满（本轮无人可派）")
		}
		return 0, errors.New("下发失败（机器人通道未连接/号全被暂停）")
	}
	return total, nil
}

// roampoolRobots 当前区的机器人快照（别的区的号不参与：游荡池只对当前区生效）。
func (a *API) roampoolRobots() []state.Robot {
	if a.St == nil {
		return nil
	}
	key := a.currentZoneKey()
	all := a.St.Snapshot()
	out := make([]state.Robot, 0, len(all))
	for _, r := range all {
		if key != "" && r.Zone != "" && r.Zone != key {
			continue
		}
		// 2026-09-29 阶段 2（组队）：队内号不进游荡池视野 —— 不参与游荡挑选/回收/补位，
		// 也不计入"空闲余量"（它们不是余量号；阶段 3 升级为全量编排避让）。
		if role, _ := a.teamRoleOf(r.Account); role != "" {
			continue
		}
		out = append(out, r)
	}
	return out
}

// roampoolDeficit 任务池缺口 = **抓鬼池 deficit + 新手池 deficit**（正=缺人，负=超编）。
//
// 与 GET /api/autotask 的 pools.<kind>.deficit 同一口径（TargetOnline - 在跑数），
// 这里直接读 Runner.States()（进程内调用，不走 HTTP）。
//
// 2026-09-23 口径修正：**池未启用 / 没配目标（Target<=0）的不参与合计** ——
// 停用的池不会要人（生产 newbie 池 target=30 但 disabled，原口径把它算成"缺 30"，
// 会让游荡池误判任务池不缺人、新加的"超编收敛"也提前收工）。
func (a *API) roampoolDeficit() int {
	if a.AutoTask == nil {
		return 0
	}
	states := a.AutoTask.States()
	sum := 0
	for _, k := range []autotask.Kind{autotask.KindGhost, autotask.KindNewbie} {
		st, ok := states[k]
		if !ok || !st.Enabled || st.Target <= 0 {
			continue // 没启用/没配目标：不参与缺口口径
		}
		sum += st.Deficit
	}
	return sum
}

// roampoolMaps 可用游荡图 = 链数据里**有寻路网格**的图（图号升序）。
// keeper 的白名单为空时用它；白名单非空时 keeper 自己取交集（缺网格的图会被跳过并记日志）。
// 链数据不可用（文件缺失/解析失败）时返回 nil 且**不在这里刷日志**：keeper 每轮/面板每次读取都会调它，
// 而"启用了游荡池却没图可派"这件事由 keeper 的动作文案（`last_action`）与日志说清楚。
func (a *API) roampoolMaps() []int {
	base, err := a.chainPayloads().walkBase()
	if err != nil {
		return nil
	}
	out := make([]int, 0, len(base.MapGrids))
	for k := range base.MapGrids {
		if n, err := strconv.Atoi(strings.TrimSpace(k)); err == nil && n > 0 {
			out = append(out, n)
		}
	}
	// 2026-09-22 世界图过滤：与白名单取交集（白名单为空 = 不过滤）
	if wm := a.Cfg.RoamWorldMaps; len(wm) > 0 {
		ok := make(map[int]bool, len(wm))
		for _, m := range wm {
			ok[m] = true
		}
		filtered := out[:0]
		for _, m := range out {
			if ok[m] {
				filtered = append(filtered, m)
			}
		}
		out = filtered
	}
	sort.Ints(out)
	return out
}

// roampoolTaskMaps 任务图"热读"集合（2026-09-24 游荡降权）：链数据里任务链会去到的图 ——
// 目前取抓鬼导航声明的 ghost_maps（zhongkui_nav.json；动态跟随链数据变化）。
//
// keeper 侧会与内置兜底集合（roampool.defaultTaskMaps：新手链/神捕/烽火 patrol 图 + 店铺小图）
// 合并作为降权对象；这里失败/为空**不刷日志**（keeper 每轮都调）—— 降权不依赖链数据可用性。
func (a *API) roampoolTaskMaps() []int {
	nav, err := a.chainPayloads().Ghost()
	if err != nil {
		return nil
	}
	ms, err := ghostMapsOf(nav)
	if err != nil {
		return nil
	}
	return ms
}

// roampoolDispatch 下发游荡（走面板同一个 DispatchRoam）。
func (a *API) roampoolDispatch(accounts []string, mapid any, mode string, minutes int) (int, error) {
	res := a.DispatchRoam(accounts, mapid, mode, minutes)
	if res.Err != "" {
		return 0, errors.New(res.Err)
	}
	if res.Sent == 0 {
		return 0, errors.New(res.Msg)
	}
	return res.Sent, nil
}

// roampoolStop 回收游荡（走面板同一个 StopRoam）。
//
// 2026-09-22 用户口径：**孵化也是一种游荡**（前提是已经放了宠物蛋在等待孵化），
// 所以孵化号也在游荡池里、也参与回收。但只停 random_walk 不够 —— 孵化会话
// (mount_egg) 会立刻把号再派回孵化图 → 这里**先把在孵化的号 hatch_stop 收工**
// （蛋留在身上、等待下次继续孵），再统一停游荡，回收才真正生效。
func (a *API) roampoolStop(accounts []string) (int, error) {
	if hatching := a.roampoolHatchingAmong(accounts); len(hatching) > 0 {
		n := a.hatchStop(hatching)
		a.Log.Printf("[ROAMPOOL] 回收含孵化号 %d 个 → 已下发 hatch_stop（让位任务链；蛋保留）", n)
	}
	res := a.StopRoam(accounts)
	if res.Err != "" {
		return 0, errors.New(res.Err)
	}
	if res.Sent == 0 {
		return 0, errors.New(res.Msg)
	}
	return res.Sent, nil
}

// roampoolHatchingAmong 从给定账号里挑出"当前在孵化"的（读状态表）。
func (a *API) roampoolHatchingAmong(accounts []string) []string {
	if a.St == nil || len(accounts) == 0 {
		return nil
	}
	want := make(map[string]bool, len(accounts))
	for _, acc := range accounts {
		want[acc] = true
	}
	out := []string{}
	for _, r := range a.St.Snapshot() {
		if want[r.Account] && r.HatchActive() {
			out = append(out, r.Account)
		}
	}
	return out
}

// LoadRoampool 启动时恢复上次的游荡池参数（没有文件 = 默认 enabled=false，等人工开）。
func (a *API) LoadRoampool() {
	if a.Roampool == nil {
		return
	}
	if err := a.Roampool.Load(); err != nil {
		a.Log.Printf("[ROAMPOOL] 参数加载失败（用默认值，enabled=false）: %v", err)
	}
}

// ---------------------------------------------------------------- 接口

// roampoolSnapshot 游荡池摘要（GET /api/roampool；字段展平，面板少一层嵌套）。
func (a *API) roampoolSnapshot() map[string]any {
	if a.Roampool == nil {
		return map[string]any{}
	}
	st := a.Roampool.Status()
	return map[string]any{
		"enabled": st.Enabled, "target": st.Target, "interval_sec": st.IntervalSec, "max_step": st.MaxStep,
		"minutes": st.Minutes, "balance": st.Balance, "reclaim_on_deficit": st.ReclaimOnDeficit,
		"maps": st.Maps, "mode": st.Mode,
		"task_map_bias": st.TaskMapBias, "task_map_count": st.TaskMapCount,
		"inflight_ttl_sec": st.InflightTTLSec, "backoff_sec": st.BackoffSec,
		"running": st.Running, "idle": st.Idle, "deficit": st.Deficit, "map_count": st.MapCount,
		"inflight_pending": st.InflightPending, "inflight": st.Inflight, "idle_streak": st.IdleStreak,
		"env_pinned":  st.EnvPinned,
		"last_action": st.LastAction, "last_run_ts": st.LastRunTS, "last_err": st.LastErr,
	}
}

// handleRoampoolGet 游荡池状态（只读）。
func (a *API) handleRoampoolGet(w http.ResponseWriter, r *http.Request) {
	if a.Roampool == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "游荡池 keeper 未启用"})
		return
	}
	resp := map[string]any{"ok": true, "path": a.Roampool.Path()}
	for k, v := range a.roampoolSnapshot() {
		resp[k] = v
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleRoampoolPost 设置游荡池参数（写接口，可选鉴权）。
//
//	POST /api/roampool {"enabled":true,"target":100,"interval_sec":60,"max_step":5,
//	                    "minutes":0,"balance":true,"reclaim_on_deficit":true,"maps":[6,17],"mode":"dense"}
//	校验：target 0~10000、interval_sec 10~3600、max_step 1~50、minutes 0~1440（0=不限）；
//	maps 给 [] / "" 表示清空白名单（回到"链数据里有网格的图"）。
func (a *API) handleRoampoolPost(w http.ResponseWriter, r *http.Request) {
	if a.Roampool == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "游荡池 keeper 未启用"})
		return
	}
	body := readBody(r)
	cfg := a.Roampool.Config() // 未给的字段保持原值
	if _, ok := body["enabled"]; ok {
		cfg.Enabled = toBool(body["enabled"], cfg.Enabled)
	}
	if _, ok := body["target"]; ok {
		cfg.Target = toInt(body["target"], cfg.Target)
	}
	if _, ok := body["interval_sec"]; ok {
		cfg.IntervalSec = toInt(body["interval_sec"], cfg.IntervalSec)
	}
	if _, ok := body["max_step"]; ok {
		cfg.MaxStep = toInt(body["max_step"], cfg.MaxStep)
	}
	if _, ok := body["minutes"]; ok {
		cfg.Minutes = toInt(body["minutes"], cfg.Minutes)
	}
	if _, ok := body["balance"]; ok {
		cfg.Balance = toBool(body["balance"], cfg.Balance)
	}
	if _, ok := body["reclaim_on_deficit"]; ok {
		cfg.ReclaimOnDeficit = toBool(body["reclaim_on_deficit"], cfg.ReclaimOnDeficit)
	}
	if _, ok := body["mode"]; ok {
		cfg.Mode = strings.TrimSpace(toStr(body["mode"]))
	}
	// 2026-09-24 游荡降权：任务图虚拟负载偏移（0=默认 8，<0=关闭）+ 任务图集合（空=热读+兜底）。
	if _, ok := body["task_map_bias"]; ok {
		cfg.TaskMapBias = toInt(body["task_map_bias"], cfg.TaskMapBias)
	}
	if _, ok := body["task_maps"]; ok {
		if isEmptyMapsVal(body["task_maps"]) {
			cfg.TaskMaps = nil // 清空：回到"热读 + 内置兜底"
		} else {
			list, err := parseMapList(body["task_maps"])
			if err != nil {
				writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "task_maps 非法：" + err.Error()})
				return
			}
			cfg.TaskMaps = list
		}
	}
	// 2026-09-23 P1（派发节流）：在途 TTL 与退避档位（可热改；backoff_sec 给空数组 = 恢复默认）。
	if _, ok := body["inflight_ttl_sec"]; ok {
		cfg.InflightTTLSec = toInt(body["inflight_ttl_sec"], cfg.InflightTTLSec)
	}
	if _, ok := body["backoff_sec"]; ok {
		list, err := parseIntListVal(body["backoff_sec"])
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "backoff_sec 非法：" + err.Error()})
			return
		}
		cfg.BackoffSec = list
	}
	if _, ok := body["maps"]; ok {
		if isEmptyMapsVal(body["maps"]) {
			cfg.Maps = nil // 清空白名单：回到"链数据里有网格的图"
		} else {
			list, err := parseMapList(body["maps"])
			if err != nil {
				writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "maps 非法：" + err.Error()})
				return
			}
			cfg.Maps = list
		}
	}
	if err := a.Roampool.SetConfig(cfg); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": err.Error()})
		return
	}
	if a.Store != nil {
		a.Store.LogEvent(map[string]any{"type": "api", "action": "roampool_set",
			"zone": a.currentZoneKey(), "config": cfg})
	}
	msg := "游荡池参数已保存"
	if cfg.Enabled {
		msg += "（已启用：每 " + strconv.Itoa(cfg.IntervalSec) + " 秒一轮，目标 " + strconv.Itoa(cfg.Target) +
			" 个游荡号，单轮最多调 " + strconv.Itoa(cfg.MaxStep) + " 个；任务池缺人时立刻回收）"
	} else {
		msg += "（未启用：keeper 什么都不做）"
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "msg": msg, "roampool": a.roampoolSnapshot()})
}

// isEmptyMapsVal body 里的 maps 是不是"清空白名单"（[] / "" / null）。
func isEmptyMapsVal(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case []any:
		return len(t) == 0
	case string:
		return strings.TrimSpace(t) == ""
	}
	return false
}

// parseIntListVal 解析整数列表（2026-09-23 P1：backoff_sec 用）：
// 接受 [60,120,300] / ["60","120"] / "60,120,300"；nil/空 → nil,nil（由 Config.Normalize 补默认）。
func parseIntListVal(v any) ([]int, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case []any:
		out := make([]int, 0, len(t))
		for _, it := range t {
			s := strings.TrimSpace(toStr(it))
			if s == "" {
				continue
			}
			n, err := strconv.Atoi(s)
			if err != nil {
				return nil, fmt.Errorf("项 %q 非法（应为正整数秒）", toStr(it))
			}
			out = append(out, n)
		}
		return out, nil
	case string:
		out := make([]int, 0, 4)
		for _, p := range splitList(t) {
			n, err := strconv.Atoi(strings.TrimSpace(p))
			if err != nil {
				return nil, fmt.Errorf("项 %q 非法（应为正整数秒）", p)
			}
			out = append(out, n)
		}
		return out, nil
	}
	return nil, fmt.Errorf("不支持的类型 %T（应为秒数数组，如 [60,120,300]）", v)
}
