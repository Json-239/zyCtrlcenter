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
	"net/http"
	"sort"
	"strconv"
	"strings"

	"zyctrlcenter/internal/services/autotask"
	"zyctrlcenter/internal/services/roampool"
	"zyctrlcenter/internal/state"
)

// ---------------------------------------------------------------- 依赖装配

// RoampoolDeps 组装"游荡池 keeper"的依赖（main 用它创建 roampool.Keeper）。
func (a *API) RoampoolDeps() roampool.Deps {
	return roampool.Deps{
		Robots:   a.roampoolRobots,
		// 2026-09-22 P0：回收资格闸 —— 只回收"回收后真能进任务池"的号（否则回收-重派空转）
		ReclaimEligible: a.roamReclaimEligible,
		Deficit:  a.roampoolDeficit,
		Maps:     a.roampoolMaps,
		Dispatch: a.roampoolDispatch,
		Stop:     a.roampoolStop,
		Log:      func(format string, args ...any) { a.Log.Printf(format, args...) },
	}
}

// roamReclaimEligible 回收资格（2026-09-22 P0，三池交互分析）：
// 只回收"回收后真能进任务池"的号 ——
//   · 今日抓鬼已满/不可用（GhostDoneToday）：任务池会跳过它；
//   · 等级不到抓鬼门槛（ghostGate）：任务池同样跳过。
// 这两类回收给任务池只是空转（90s 后 auto_roam 又派游荡，实测单号 20+ 次往返），
// 让它们留在游荡池继续游荡（有产出）。
func (a *API) roamReclaimEligible(r state.Robot) bool {
	if a.St != nil && a.St.GhostDoneToday(r.Account) {
		return false
	}
	lvl, req := a.accountLevel(r.Account)
	if ok, _ := a.ghostGate(lvl, req); !ok {
		return false
	}
	return true
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
		"running": st.Running, "idle": st.Idle, "deficit": st.Deficit, "map_count": st.MapCount,
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
