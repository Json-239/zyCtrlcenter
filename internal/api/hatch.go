// 孵化（hatch）链路：中控侧。机器人侧按同一契约实现（放蛋 → 游荡 → 计数 → 上报心跳 hatch）。
//
// # 蛋种与依据（服务端配置核实，证据见 docs/04-测试/改动-20260922-孵化链路Go侧.md）
//
//   - **坐骑蛋**（40 种：101316 青牛 / 101317 白色羊驼 / 101321 战虎 / 101333 赤兔 / 101343 九色神鹿 /
//     101394 谛听 …，`config/item/item.xml` 里 `related_mount_index` 非空的 40 条）：
//     必须先放进**装备栏五行珠位 pos=4108**（服务端 session `mount_egg_sign`，没放 = 战斗不加灵气），
//     再到孵化白名单图踩"暗雷"战斗胜利（80% 概率 +1 灵气，30 点孵出）。
//     白名单（`config/mount.xml` 的 `mount_hatch_place`）只有 4 张图：**6 半月岛（32 种）**、17 女儿国、
//     34 通天河、40 傲来国 —— 本阶段统一按 **6 半月岛** 下发（现场主流蛋就是青牛 101316）。
//   - **元气蛋 / 守护蛋**（102603 元气蛋）：放背包即可，服务端条件 `condition_role_in_map map_index=10`
//     + 等级 ≥ 50 + 元气 100~234（孵化率 2.5%）→ 目标图 **10 大唐东野林**。
//
// # 中控职责（机器人侧 agent 按契约实现另一半）
//
//  1. 候选筛选：在线 + **抓鬼已满**（别抢抓鬼的活）+ 有蛋（坐骑蛋判据：物品名在已知清单里，
//     或该物品就在装备栏 4108；元气蛋判据：物品名"元气蛋"，且等级 ≥ 50）；
//  2. 下发 `hatch_start`（mapid/kind/egg_item/chain/max_minutes/accounts，chain 复用 Payloads.Walk）；
//  3. 会话记账：`max_minutes` 到期由**中控**下发 `hatch_stop` 收工；心跳 `hatch.hatched=true` 即完成
//     （从 running 移除 + 记日志，见 event.onStatusReply）。
package api

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"zyctrlcenter/internal/services/autotask"
	"zyctrlcenter/internal/state"
)

// 与机器人侧契约一致的 kind 取值（hatch_start.kind）。
const (
	HatchKindMount    = "mount"    // 坐骑蛋（青牛等；装备栏 4108 + 图 6）
	HatchKindGuardian = "guardian" // 元气蛋/守护蛋（背包 + 图 10 + 等级 ≥ 50）
)

const (
	hatchMountMap      = 6    // 半月岛（mount.xml 孵化白名单：32 种坐骑蛋）
	hatchGuardianMap   = 10   // 大唐东野林（服务端 condition_role_in_map=10）
	hatchGuardianLevel = 50   // 元气蛋的等级门槛（服务端条件）
	hatchEquipPos      = 4108 // 装备栏五行珠位（坐骑蛋专位；实例位置 0x1000~0x10ff 属装备栏）
	// hatchStopGrace 下发后多久才把"机器人侧仍未报 active"当失败（避免刚发就误判）。
	hatchStopGrace = 60 * time.Second
)

// mountEggs 坐骑蛋：物品名 → item_index（`config/item/item.xml` 里带 related_mount_index 的 40 条）。
//
// 为什么用**名字**：机器人心跳的 bag 摘要只有 {id(实例), count, name, pos}，没有 item_index，
// 所以只能用物品名对号（`item_name` 就是坐骑名，如 101316 = "青牛"）。
//
// 2026-09-22r2 名字双份兼容：101317/101318/101319 在**服务端** config/item/item.xml 里叫
// "白色羊驼/棕色羊驼/黑色羊驼"，但机器人端本地 xml（robot .../script/xml/item.xml，
// **心跳 bag 的 name 就取自它**）叫"白羊羊/醉羊羊/酷羊羊" —— 只收服务端名会让这 3 种蛋
// "还在背包"时识别不到（蛋已在装备栏 4108 的路径不受影响）。两组名字都收，
// 其余 34 个名字两份 xml 一致。
var mountEggs = map[string]int{
	"青牛": 101316, "白色羊驼": 101317, "棕色羊驼": 101318, "黑色羊驼": 101319,
	"白羊羊": 101317, "醉羊羊": 101318, "酷羊羊": 101319,
	"战虎": 101321, "战狼": 101322, "大象": 101323, "青狮": 101324, "熊猫": 101325,
	"棕熊": 101326, "炙火赤炎兽": 101327, "白马": 101328, "黑马": 101329, "白虎": 101331,
	"赤兔": 101333, "驯鹿": 101336, "九色神鹿": 101343,
	"高级赤兔": 101357, "腾霜": 101358, "盗骊": 101359, "高级腾霜": 101360, "高级盗骊": 101361,
	"独角赤炎兽": 101362, "雄狮": 101363, "黑猩猩": 101364, "黑豹": 101365, "谛听": 101394,
	"红色小金鱼": 101406, "紫色小金鱼": 101407, "黄色小金鱼": 101408, "熊猫妹妹": 101410,
	"白色跳跳兔": 101434, "灰色跳跳兔": 101435, "黄色跳跳兔": 101436,
	"黑色小跳驴": 101594, "灰色小跳驴": 101595, "棕色小跳驴": 101596,
}

// guardianEgg 元气蛋（守护蛋）：放背包即可，服务端要 map_index=10 + 等级 ≥ 50。
const guardianEgg = "元气蛋"

// hatchPlan 一个号的孵化计划（从运行时状态推导；不在状态里存，避免与心跳漂移）。
type hatchPlan struct {
	Account string `json:"account"`
	Kind    string `json:"kind"`     // mount / guardian
	EggItem int    `json:"egg_item"` // 蛋的物品编号（0 = 只知道"装备栏 4108 里有蛋"，机器人端按已在位处理）
	MapID   int    `json:"mapid"`    // 目标图（坐骑蛋 6 / 元气蛋 10）
}

// hatchKindLabel 中文名（日志/面板用）。
func hatchKindLabel(kind string) string {
	if kind == HatchKindGuardian {
		return "元气蛋"
	}
	return "坐骑蛋"
}

// hatchPlanOf 该号该孵化什么：**只看蛋与等级**（"该不该现在派"由候选判定管，见 autotaskCandidates）。
//
// 判据（与用户口径一致）：
//   - 坐骑蛋：物品名在 mountEggs 清单里（蛋在背包），或实例位置 == 4108（已在装备栏五行珠位）；
//   - 元气蛋：物品名"元气蛋"，且等级 ≥ 50（等级未知（0）时**不拦**，由候选侧按在线账号处理）。
//
// 返回 ok=false 时 why 是人话（面板/日志显示"为什么不能孵化"）。
func hatchPlanOf(r state.Robot) (hatchPlan, bool, string) {
	plan := hatchPlan{Account: r.Account}
	eggInEquip := 0
	for _, it := range bagItems(r.Bag) {
		if idx, ok := mountEggs[it.Name]; ok {
			if plan.EggItem == 0 {
				plan.EggItem = idx
			}
			continue
		}
		if it.Pos == hatchEquipPos { // 装备栏五行珠位有东西 = 坐骑蛋已放好（名字对不上也能认）
			eggInEquip = 1
			if plan.EggItem == 0 {
				if idx, ok := mountEggs[it.Name]; ok {
					plan.EggItem = idx
				}
			}
			continue
		}
	}
	if plan.EggItem > 0 || eggInEquip > 0 {
		plan.Kind, plan.MapID = HatchKindMount, hatchMountMap
		return plan, true, ""
	}
	if hasGuardianEgg(r.Bag) {
		if r.Level > 0 && r.Level < hatchGuardianLevel {
			return plan, false, fmt.Sprintf("元气蛋要 %d 级（当前 %d 级）", hatchGuardianLevel, r.Level)
		}
		plan.Kind, plan.EggItem, plan.MapID = HatchKindGuardian, 102603, hatchGuardianMap
		return plan, true, ""
	}
	return plan, false, "背包/装备栏里没有坐骑蛋或元气蛋"
}

// bagEgg 背包摘要里的一个物品（机器人上报 {"id","count","name","pos"}）。
type bagEgg struct {
	Name string
	Pos  int
}

// bagItems 解析机器人上报的 bag（[]any{map[string]any{...}}；兼容直接构造成的 []map[string]any）。
func bagItems(v any) []bagEgg {
	out := []bagEgg{}
	switch arr := v.(type) {
	case []any:
		for _, it := range arr {
			if m, ok := it.(map[string]any); ok {
				out = append(out, bagEgg{Name: toStr(m["name"]), Pos: toInt(m["pos"], 0)})
			}
		}
	case []map[string]any:
		for _, m := range arr {
			out = append(out, bagEgg{Name: toStr(m["name"]), Pos: toInt(m["pos"], 0)})
		}
	}
	return out
}

// hasGuardianEgg bag 里有没有元气蛋。
func hasGuardianEgg(v any) bool {
	for _, it := range bagItems(v) {
		if it.Name == guardianEgg {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- 候选判定

// ghostFullForHatch 今日抓鬼是否已满（孵化候选的前置：别把还在抓鬼的号拉走）。
//
// 口径（用户给定）：ghost.done >= ghost.limit，或 ghost.state == DONE，或 done >= 50。
// 心跳里没有 ghost 字段（今天没跑过抓鬼）→ 不算满。
func ghostFullForHatch(r state.Robot) bool {
	if r.Ghost == nil {
		return false
	}
	if st, _ := r.Ghost["state"].(string); strings.EqualFold(st, "DONE") {
		return true
	}
	done := toInt(r.Ghost["done"], 0)
	if done >= 50 {
		return true
	}
	limit := toInt(r.Ghost["limit"], 0)
	return limit > 0 && done >= limit
}

// hatchPlanFor 取某号的孵化计划（运行时状态为准；没有状态/不在线/没蛋都返回 why）。
func (a *API) hatchPlanFor(acc string) (hatchPlan, bool, string) {
	if a.St == nil {
		return hatchPlan{}, false, "状态表不可用"
	}
	r, ok := a.St.Get(acc)
	if !ok {
		return hatchPlan{}, false, "没有状态记录（没上线过，或中控刚重启还没收到心跳）"
	}
	if !r.Online {
		return hatchPlan{}, false, "不在线"
	}
	return hatchPlanOf(r)
}

// ---------------------------------------------------------------- 下发

// launchHatch 给这批号下发 hatch_start（按 目标图+蛋种+蛋编号 分组，一组一条命令）。
//
// 返回 (是否至少发出一条, 说明)。载荷取不到（链缺网格/缺路由）就是硬失败：一条都不发。
func (a *API) launchHatch(accs []string) (bool, string) {
	if len(accs) == 0 {
		return false, "没有账号"
	}
	if a.Events == nil {
		return false, "事件通道不可用"
	}
	type group struct {
		plan  hatchPlan
		accs  []string
		chain any
	}
	groups := map[string]*group{}
	order := []string{}
	skipped := []string{}
	for _, acc := range accs {
		plan, ok, why := a.hatchPlanFor(acc)
		if !ok {
			skipped = append(skipped, acc+"（"+why+"）")
			continue
		}
		key := fmt.Sprintf("%d|%s|%d", plan.MapID, plan.Kind, plan.EggItem)
		g, has := groups[key]
		if !has {
			chain, err := a.chainPayloads().Walk(plan.MapID) // 目标图网格 + 跨图路由；缺就报错
			if err != nil {
				return false, "游荡导航数据不可用，未下发任何命令：" + err.Error()
			}
			g = &group{plan: plan, chain: chain}
			groups[key] = g
			order = append(order, key)
		}
		g.accs = append(g.accs, acc)
	}
	if len(order) == 0 {
		return false, "没有可孵化的号：" + strings.Join(skipped, "；")
	}
	sort.Strings(order) // 稳定顺序（日志/测试可预期）
	maxMin := a.hatchMaxMinutes()
	sent, msgs := 0, []string{}
	for _, key := range order {
		g := groups[key]
		cmd := map[string]any{
			"cmd": "hatch_start", "mapid": g.plan.MapID, "kind": g.plan.Kind,
			"chain": g.chain, "max_minutes": maxMin, "accounts": g.accs,
		}
		if g.plan.EggItem > 0 {
			cmd["egg_item"] = g.plan.EggItem
		}
		if !a.Events.SendCmd(cmd, "autotask_hatch_start") {
			return sent > 0, "下发失败：机器人通道未连接（" + strings.Join(g.accs, ", ") + "）"
		}
		sent += len(g.accs)
		msgs = append(msgs, fmt.Sprintf("%s×%d → 图 %d", hatchKindLabel(g.plan.Kind), len(g.accs), g.plan.MapID))
		a.hatchTrack(g.accs, g.plan, maxMin)
	}
	msg := fmt.Sprintf("已下发孵化：%d 个号（%s，限时 %d 分钟）", sent, strings.Join(msgs, "；"), maxMin)
	if len(skipped) > 0 {
		msg += "；跳过 " + strings.Join(skipped, "；")
	}
	return true, msg
}

// hatchMaxMinutes 单次孵化时长上限（分钟）：面板可配（autotask 的 max_minutes），默认 60。
func (a *API) hatchMaxMinutes() int {
	if a.AutoTask != nil {
		if st, ok := a.AutoTask.States()[autotask.KindHatch]; ok && st.Config.MaxMinutes > 0 {
			return st.Config.MaxMinutes
		}
	}
	return autotask.DefaultHatchMinutes
}

// ---------------------------------------------------------------- 会话记账（到期收工/完成清理）

type hatchSession struct {
	plan      hatchPlan
	startedAt time.Time
	maxMin    int
}

// hatchSessions 中控侧的在孵化会话（到期收工 / 完成清理的依据）。
type hatchSessions struct {
	mu sync.Mutex
	m  map[string]*hatchSession
}

func newHatchSessions() *hatchSessions { return &hatchSessions{m: map[string]*hatchSession{}} }

func (h *hatchSessions) track(accs []string, plan hatchPlan, maxMin int, now time.Time) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, acc := range accs {
		h.m[acc] = &hatchSession{plan: plan, startedAt: now, maxMin: maxMin}
	}
}

// hatchSessionView 会话快照的一行（账号 + 会话内容）。
type hatchSessionView struct {
	Acc string
	S   hatchSession
}

// snapshot 会话快照（按账号升序，稳定）。
func (h *hatchSessions) snapshot() []hatchSessionView {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]hatchSessionView, 0, len(h.m))
	for acc, s := range h.m {
		out = append(out, hatchSessionView{Acc: acc, S: *s})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Acc < out[j].Acc })
	return out
}

func (h *hatchSessions) drop(acc string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.m, acc)
}

// accounts 在孵化的账号（面板/诊断用）。
func (h *hatchSessions) accounts() []string {
	snap := h.snapshot()
	out := make([]string, 0, len(snap))
	for _, it := range snap {
		out = append(out, it.Acc)
	}
	return out
}

// hatchTrack 记一次下发（会话起始时间 = 中控下发时刻，到期以它计）。
func (a *API) hatchTrack(accs []string, plan hatchPlan, maxMin int) {
	if a.hatch == nil {
		return
	}
	a.hatch.track(accs, plan, maxMin, time.Now())
}

// hatchStop 给这些号下发 hatch_stop（收工）并把会话清掉；返回成功条数。
func (a *API) hatchStop(accs []string) int {
	if len(accs) == 0 {
		return 0
	}
	if a.Events != nil && a.Events.SendCmd(
		map[string]any{"cmd": "hatch_stop", "accounts": accs}, "hatch_stop") {
		for _, acc := range accs {
			a.hatch.drop(acc)
		}
		return len(accs)
	}
	return 0
}

// hatchStopAll 停掉全部在孵化的号（停策略时收工）；返回下发条数。
func (a *API) hatchStopAll(reason string) int {
	accs := a.hatch.accounts()
	if len(accs) == 0 {
		return 0
	}
	n := a.hatchStop(accs)
	if n > 0 {
		a.Log.Printf("[HATCH] %s：已给 %d 个号下发 hatch_stop（%s）", reason, n, strings.Join(accs, ", "))
	}
	return n
}

// hatchReconcile 孵化会话的周期维护（由 autotask 的每轮 Tick 调，5 秒一次）：
//
//	① 心跳 hatch.hatched=true（已孵出）      → 下发 hatch_stop 收工（"完成"日志在 event.onStatusReply 记）
//	② 到期（now - 下发时刻 >= max_minutes）  → 下发 hatch_stop 收工（蛋留在装备栏，下次可续）
//	③ 离线 / 状态行被清理                    → 会话作废（不发命令，记日志）
//	④ 机器人侧自己停了（hatch.active=false，超过宽限期）→ 会话作废 + 记原因
//
// 不做的事：不重发 hatch_start（机器人端会自己跨图/续走；重发会把走位节奏打断）。
func (a *API) hatchReconcile(now time.Time) {
	if a.hatch == nil || a.St == nil {
		return
	}
	for _, it := range a.hatch.snapshot() {
		acc, s := it.Acc, it.S
		r, ok := a.St.Get(acc)
		if !ok || !r.Online {
			why := "离线"
			if !ok {
				why = "状态行已被清理（长时间没心跳）"
			}
			a.hatch.drop(acc)
			a.Log.Printf("[HATCH] %s %s，孵化会话作废（%s，图 %d）", acc, why,
				hatchKindLabel(s.plan.Kind), s.plan.MapID)
			continue
		}
		hatched := toBool(r.Hatch["hatched"], false)
		active := r.HatchActive()
		switch {
		case hatched:
			if a.hatchStop([]string{acc}) > 0 {
				a.Log.Printf("[HATCH] %s 已孵出（%s），下发 hatch_stop 收工", acc, hatchKindLabel(s.plan.Kind))
			}
		case !active && now.Sub(s.startedAt) > hatchStopGrace:
			a.hatch.drop(acc)
			reason := toStr(r.Hatch["reason"])
			if reason == "" {
				reason = "机器人侧没有活跃孵化会话"
			}
			a.Log.Printf("[HATCH] %s 孵化会话结束（%s）：%s", acc, hatchKindLabel(s.plan.Kind), reason)
			a.Store.LogEvent(map[string]any{"type": "api", "action": "hatch_end", "zone": a.currentZoneKey(),
				"account": acc, "kind": s.plan.Kind, "mapid": s.plan.MapID, "reason": reason})
		case now.Sub(s.startedAt) >= time.Duration(s.maxMin)*time.Minute:
			if a.hatchStop([]string{acc}) > 0 {
				a.Log.Printf("[HATCH] %s 孵化到期（%d 分钟）→ 下发 hatch_stop 收工（蛋留在装备栏，下次可续）",
					acc, s.maxMin)
				a.Store.LogEvent(map[string]any{"type": "api", "action": "hatch_timeout",
					"zone": a.currentZoneKey(), "account": acc, "kind": s.plan.Kind,
					"mapid": s.plan.MapID, "minutes": s.maxMin})
			}
		}
	}
}

// hatchSessionList 面板/接口用的会话快照。
func (a *API) hatchSessionList() []map[string]any {
	out := []map[string]any{}
	for _, it := range a.hatch.snapshot() {
		out = append(out, map[string]any{
			"account": it.Acc, "kind": it.S.plan.Kind, "egg_item": it.S.plan.EggItem,
			"mapid": it.S.plan.MapID, "started_at": it.S.startedAt, "max_minutes": it.S.maxMin,
		})
	}
	return out
}
