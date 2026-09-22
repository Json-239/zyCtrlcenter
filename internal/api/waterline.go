// 在线人数水位保持器：api 侧的壳。
//
// 引擎在 internal/services/waterline（纯决策、可注入依赖）；这里只负责把"真实世界"接上：
//   - 取数：服务端全服在线（含真人）读数：直连 provider（livecount，新鲜时）→ 机器人
//     `@online` 回执（state.SvrOnlineLatest）→ 本地握手数兜底（见 svrOnlineReading）；
//   - 候选：账号池里**当前区可用**的号（自己压下去的号豁免 IsRemoved，目标上调时能拉回来）；
//   - 上线：与手动「批量上线」共用 sendOnlineChunks（robot_manage add，密码只从池里按区取）；
//   - 下线：与手动「批量下线」共用 sendOfflineChunks（先本地标记移除防心跳复活，再分批 remove）。
//
// 面板「大屏」页（web/src/components/Dashboard.vue）的"在线水位"卡与 /api/waterline 都从这里取数。
package api

import (
	"errors"
	"math/rand"
	"net/http"
	"time"

	"zyctrlcenter/internal/services/accounts"
	"zyctrlcenter/internal/services/waterline"
	"zyctrlcenter/internal/state"
)

// ---------------------------------------------------------------- 依赖装配

// WaterlineDeps 组装"在线水位保持器"的依赖（main 用它创建 waterline.Keeper）。
func (a *API) WaterlineDeps() waterline.Deps {
	return waterline.Deps{
		Now: time.Now,
		Rand: func(n int) int {
			if n <= 0 {
				return 0
			}
			return rand.Intn(n)
		},
		Robots:     a.waterlineRobots,
		SvrOnline:  a.waterlineSvrOnline,
		Local:      a.waterlineLocalCount,
		Candidates: a.waterlineCandidates,
		Online:     a.waterlineOnline,
		Offline:    a.waterlineOffline,
		Log:        func(format string, args ...any) { a.Log.Printf(format, args...) },
	}
}

// waterlineSvrOnline 水位保持器的"服务端全服在线（含真人）"读数：
// provider（直连 /gm/online，新鲜时）→ 机器人 @online 回执 → 无（保持器内部再用本地握手数兜底）。
// 保持器自己还会判一次新鲜度（水线 SvrStaleSec=180s），这里只负责按优先级给出读数。
func (a *API) waterlineSvrOnline() (int, float64, bool) {
	if c, ts, _, ok := a.svrOnlineReading(); ok {
		return c, ts, true
	}
	return 0, 0, false
}

// waterlineRobots 当前区的机器人快照（挑"断谁"用：别的区的号不参与）。
func (a *API) waterlineRobots() []state.Robot {
	if a.St == nil {
		return nil
	}
	key := a.currentZoneKey()
	all := a.St.Snapshot()
	out := make([]state.Robot, 0, len(all))
	for _, r := range all {
		if key != "" && r.Zone != "" && r.Zone != key {
			continue // 只对当前区生效
		}
		out = append(out, r)
	}
	return out
}

// waterlineLocalCount 本地兜底读数：当前区**握手完成**的机器人数（含真人？不含——这是我们自己的号）。
func (a *API) waterlineLocalCount() int {
	if a.St == nil {
		return 0
	}
	key := a.currentZoneKey()
	n := 0
	for _, r := range a.St.Snapshot() {
		if !r.HS {
			continue
		}
		if key != "" && r.Zone != "" && r.Zone != key {
			continue
		}
		n++
	}
	return n
}

// waterlineCandidates 可上线候选（账号池 + 运行时状态摊平；纯决策在 waterline 包里）。
func (a *API) waterlineCandidates() []waterline.Candidate {
	if a.Accounts == nil {
		return nil
	}
	gameAddr := a.gameAddrOf("")
	live := map[string]state.Robot{}
	if a.St != nil {
		for _, r := range a.St.Snapshot() {
			live[r.Account] = r
		}
	}
	out := make([]waterline.Candidate, 0, 64)
	for _, pa := range a.Accounts.List(accounts.Filter{}) {
		acc := pa.Name
		r, hasLive := live[acc]
		usable := false
		if z := pa.Zone(gameAddr); z != nil && z.Usable {
			usable = true
		}
		if !usable && hasLive && r.Online {
			usable = true // 已在线的号本来就在该区跑着
		}
		removed := a.St != nil && a.St.IsRemoved(acc)
		if removed && a.Waterline != nil && a.Waterline.SelfOfflined(acc) {
			removed = false // 是我们自己压下去的号：目标上调时可以再拉回来
		}
		level, chainDone := pa.Level, pa.ChainDone
		if hasLive {
			if r.Level > 0 {
				level = r.Level
			}
			chainDone = chainDone || r.ChainDone
		}
		out = append(out, waterline.Candidate{
			Account: acc, Usable: usable, Removed: removed,
			Online: hasLive && r.Online, Busy: hasLive && waterline.Busy(r),
			Level: level, ChainDone: chainDone,
		})
	}
	return out
}

// waterlineOnline 补号上线（复用批量上线通路：密码只从池里按区取，库里没密码的跳过）。
func (a *API) waterlineOnline(accs []string) ([]string, error) {
	sent, chunks, noPwd := a.sendOnlineChunks(accs, a.gameAddrOf(""), 10, 300, "waterline_add")
	if len(sent) == 0 {
		return nil, errors.New("上线下发失败：机器人通道未连接或池里没密码")
	}
	if a.Store != nil {
		a.Store.LogEvent(map[string]any{"type": "api", "action": "waterline_online",
			"zone": a.currentZoneKey(), "requested": len(accs), "sent": len(sent),
			"chunks": chunks, "accounts": sent, "skipped_no_password": noPwd})
	}
	return sent, nil
}

// waterlineOffline 压号下线（复用批量下线通路：先本地标记移除防心跳复活，再分批 remove）。
func (a *API) waterlineOffline(accs []string) ([]string, error) {
	sent, chunks := a.sendOfflineChunks(accs, 10, 300, "waterline_remove")
	if len(sent) == 0 {
		return nil, errors.New("下线下发失败：机器人通道未连接")
	}
	if a.Store != nil {
		a.Store.LogEvent(map[string]any{"type": "api", "action": "waterline_offline",
			"zone": a.currentZoneKey(), "requested": len(accs), "sent": len(sent),
			"chunks": chunks, "accounts": sent})
	}
	return sent, nil
}

// LoadWaterline 启动时恢复上次的水位参数（没有文件 = 默认 enabled=false，等人工开）。
func (a *API) LoadWaterline() {
	if a.Waterline == nil {
		return
	}
	if err := a.Waterline.Load(); err != nil {
		a.Log.Printf("[WATERLINE] 参数加载失败（用默认值，enabled=false）: %v", err)
	}
}

// ---------------------------------------------------------------- 接口

// waterlineSnapshot 水位摘要（/api/status 顺带带上；保持器没装配时返回空对象）。
func (a *API) waterlineSnapshot() map[string]any {
	if a.Waterline == nil {
		return map[string]any{}
	}
	st := a.Waterline.Status()
	return map[string]any{
		"enabled": st.Enabled, "target": st.Target, "dead_zone": st.DeadZone, "max_step": st.MaxStep,
		"interval_sec": st.IntervalSec, "prefer_idle": st.PreferIdle, "random_fallback": st.RandomFallback,
		"min_keep": st.MinKeep, "allow_local": st.AllowLocal,
		"cur": st.Cur, "source": st.Source, "fresh": st.Fresh, "local": st.Local,
		"svr": st.Svr, "svr_age_sec": st.SvrAgeSec, "diff": st.Diff,
		"pending": st.Pending, "inflight": st.InFlight,
		"last_action": st.LastAction, "last_run_ts": st.LastRunTS, "last_err": st.LastErr,
	}
}

// handleWaterlineGet 水位状态（只读）。
func (a *API) handleWaterlineGet(w http.ResponseWriter, r *http.Request) {
	if a.Waterline == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "水位保持器未启用"})
		return
	}
	resp := map[string]any{"ok": true}
	for k, v := range a.waterlineSnapshot() {
		resp[k] = v
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleWaterlinePost 设置水位参数（写接口，可选鉴权）。
//
//	POST /api/waterline {"enabled":true, "target":100, "dead_zone":3, "max_step":5, "interval_sec":60}
//	校验：target 0~10000、dead_zone ≥0、max_step 1~50、interval_sec 10~3600（其余字段按需）。
func (a *API) handleWaterlinePost(w http.ResponseWriter, r *http.Request) {
	if a.Waterline == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "水位保持器未启用"})
		return
	}
	body := readBody(r)
	cfg := a.Waterline.Config() // 未给的字段保持原值
	if _, ok := body["enabled"]; ok {
		cfg.Enabled = toBool(body["enabled"], cfg.Enabled)
	}
	if _, ok := body["target"]; ok {
		cfg.Target = toInt(body["target"], cfg.Target)
	}
	if _, ok := body["dead_zone"]; ok {
		cfg.DeadZone = toInt(body["dead_zone"], cfg.DeadZone)
	}
	if _, ok := body["max_step"]; ok {
		cfg.MaxStep = toInt(body["max_step"], cfg.MaxStep)
	}
	if _, ok := body["interval_sec"]; ok {
		cfg.IntervalSec = toInt(body["interval_sec"], cfg.IntervalSec)
	}
	if _, ok := body["min_keep"]; ok {
		cfg.MinKeep = toInt(body["min_keep"], cfg.MinKeep)
	}
	if _, ok := body["prefer_idle"]; ok {
		cfg.PreferIdle = toBool(body["prefer_idle"], cfg.PreferIdle)
	}
	if _, ok := body["random_fallback"]; ok {
		cfg.RandomFallback = toBool(body["random_fallback"], cfg.RandomFallback)
	}
	// 2026-09-22 安全闸开关：口径是"含真人总数"时默认 false（服务端读数不可用就不动号）。
	// 用户口径改为"只用我们自己的握手数"时，把它设为 true 即可（无需改代码）。
	if _, ok := body["allow_local"]; ok {
		cfg.AllowLocal = toBool(body["allow_local"], cfg.AllowLocal)
	}
	if err := a.Waterline.SetConfig(cfg); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": err.Error()})
		return
	}
	if a.Store != nil {
		a.Store.LogEvent(map[string]any{"type": "api", "action": "waterline_set",
			"zone": a.currentZoneKey(), "config": cfg})
	}
	msg := "水位参数已保存"
	if cfg.Enabled {
		msg += "（已启用：每 " + itoa(cfg.IntervalSec) + " 秒检查一次，目标 " + itoa(cfg.Target) +
			" 人，死区 ±" + itoa(cfg.DeadZone) + "，单轮最多 " + itoa(cfg.MaxStep) + " 个）"
	} else {
		msg += "（未启用：保持器什么都不做）"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "msg": msg, "waterline": a.waterlineSnapshot(),
	})
}
