// 号池轮换（rotation 最小版，2026-10-12）api 侧壳层：tick 主循环 + 状态接口。
//
// 引擎在 internal/services/rotation（纯决策：候选过滤/限速/名单/持久化，可单测）；
// 这里只负责把"真实世界"接上：
//   - 取数：当前机器人快照（判"纯游荡/满额/队伍/暂停/熔断"；口径同 rescue48/waterline）；
//   - 熔断号止损（2026-10-14）：当日卡死熔断的号当天无产出能力 → 满足物理护栏即换出
//     （判据全在 rotation.Candidate；本壳层不额外过滤，仅日志/事件按 CappedOut 区分措辞）；
//   - 预备队：账号池"当前区可用 且 离线 且 毕业（level≥31）"的号数（ROTATION_MIN_RESERVE 保护）；
//   - 执行：**复用批量下线/水位器压号的同一条通路**（handlers_accounts.go sendOfflineChunks：
//     cancelRegHost + MarkRemoved + 删行 + 分批 robot_manage remove）——绝不绕开 MarkRemoved；
//   - 回池：到龄（KeepHours）的号调 state.UnmarkRemoved（水位器下一轮按"最久未上线优先"拉起）。
//
// 开关 CTRL_ROTATION（默认 0=关）：关闭时仍执行"到龄回池"（自记账治理，见 rotation.go 包注释）。
// 状态可见性：GET /api/rotation（enabled/名单数/今日下线数/上一轮动作/预备队数）。
package api

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"zyctrlcenter/internal/services/accounts"
	"zyctrlcenter/internal/services/reghost"
	"zyctrlcenter/internal/services/rotation"
	"zyctrlcenter/internal/state"
)

// ---------------------------------------------------------------- 运行态

// rotationRuntime 轮换运行态（进程内存；名单本体在 rotation.Store 落盘，重启不丢）。
type rotationRuntime struct {
	mu          sync.Mutex
	store       *rotation.Store
	quota       rotation.HourQuota
	onlineSince map[string]time.Time // 账号 → 首见在线时刻（"在线 ≥30 分钟"判据）
	lastAction  string
	lastRunTS   int64
	// lastBlocked/lastQueued 日志去抖：同一原因/同一排队档只打一条（翻转时才再打）。
	lastBlocked string
	lastQueued  int
}

// rotationOf 取（懒建）轮换运行态。文件路径 <数据目录>/rotation.json；Cfg 缺席时纯内存。
func (a *API) rotationOf() *rotationRuntime {
	a.rotationMu.Lock()
	defer a.rotationMu.Unlock()
	if a.rotation == nil {
		path := ""
		if a.Cfg != nil {
			path = filepath.Join(a.Cfg.DataDir, "rotation.json")
		}
		a.rotation = &rotationRuntime{
			store:       rotation.NewStore(path),
			onlineSince: map[string]time.Time{},
		}
	}
	return a.rotation
}

// rotationConfig 从全局配置组装轮换配置（缺省=默认值；已夹范围）。
func (a *API) rotationConfig() rotation.Config {
	c := rotation.DefaultConfig()
	if a.Cfg != nil {
		c.Enabled = a.Cfg.RotationEnabled
		c.PerHour = a.Cfg.RotationPerHour
		c.MinReserve = a.Cfg.RotationMinReserve
		c.KeepHours = a.Cfg.RotationKeepHours
	}
	return c.Normalize()
}

// trackOnline 维护"首见在线"表：本轮在线的号首次见到记 now；离线/离快照的号清除。
// 进程内存态：中控重启后全表清零 → 重启后头 30 分钟不会有号被轮换（保守方向，防重启即宕）。
func (rt *rotationRuntime) trackOnline(snap []state.Robot, now time.Time) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	seen := make(map[string]bool, len(snap))
	for _, r := range snap {
		if !r.Online || r.Account == "" {
			continue
		}
		seen[r.Account] = true
		if _, ok := rt.onlineSince[r.Account]; !ok {
			rt.onlineSince[r.Account] = now
		}
	}
	for acc := range rt.onlineSince {
		if !seen[acc] {
			delete(rt.onlineSince, acc)
		}
	}
}

// onlineFor 该号已持续在线时长（没记录 = 0，等价"刚刚上线"）。
func (rt *rotationRuntime) onlineFor(acc string, now time.Time) time.Duration {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	since, ok := rt.onlineSince[acc]
	if !ok || since.IsZero() {
		return 0
	}
	d := now.Sub(since)
	if d < 0 {
		return 0
	}
	return d
}

// ---------------------------------------------------------------- 取数

// rotationReserveCount 预备队规模：当前区**可用 且 离线 且 已毕业（level≥31）**的号数。
//
// 口径说明：
//   - "可用" = 账号池当前区 usable=true（List 带 Zone 过滤；口径同水位器候选）；
//   - "离线" = 快照里不在线（含快照无行）；在线号不算预备队；
//   - 毕业线 = newbieMaxLevel()（默认 31；与 poolOf 的抓鬼池划分同源）；
//   - 额外剔除"已移除/人工暂停"：这两类水位器拉不动（removed 需先解除；paused 不自动拉起），
//     算进来会虚高预备队、让保护闸失灵。
func (a *API) rotationReserveCount() int {
	if a.Accounts == nil {
		return 0
	}
	addr := a.gameAddrOf("")
	online := map[string]bool{}
	if a.St != nil {
		for _, r := range a.St.Snapshot() {
			if r.Online {
				online[r.Account] = true
			}
		}
	}
	minLevel := a.newbieMaxLevel()
	n := 0
	for _, pa := range a.Accounts.List(accounts.Filter{Zone: addr, Usable: true}) {
		if pa == nil || pa.Name == "" || online[pa.Name] {
			continue
		}
		if a.St != nil && (a.St.IsRemoved(pa.Name) || a.St.IsPaused(pa.Name)) {
			continue
		}
		// 2026-10-12 复核实测修正：私号不参与自动编排（水位器/池均剔除），
		// 若计入预备队会使其虚高 → 保护闸偏松，统一口径剔除。
		if a.IsPersonal(pa.Name) {
			continue
		}
		level := pa.Level
		if z := pa.Zone(addr); z != nil && z.Level > 0 {
			level = z.Level
		}
		if level < minLevel {
			continue
		}
		n++
	}
	return n
}

// rotationReghostSet 正在"自动重登恢复"处理中的号集合（等待离线/在线/补发三相位才叫处理中；
// done/failed/capped 不算——capped 是终态：2026-10-14 起改由候选侧"熔断号止损换出"通道处理，
// 见 rotation.Candidate）。
func (a *API) rotationReghostSet() map[string]bool {
	if a.Reghost == nil {
		return nil
	}
	out := map[string]bool{}
	for _, st := range a.Reghost.Status() {
		switch st.Phase {
		case reghost.PhaseWaitingOffline, reghost.PhaseWaitingOnline, reghost.PhaseWaitingLaunch:
			out[st.Account] = true
		}
	}
	return out
}

// ---------------------------------------------------------------- tick

// rotationTick 跑一轮：回池（到龄）→ 预备队/通道闸 → 判决（rotation.Plan）→ 执行下线。
//
// now 注入便于单测（内部 api 测试用两次不同时刻驱动"在线 ≥30 分钟"）。
func (a *API) rotationTick(now time.Time) {
	if a.St == nil {
		return
	}
	cfg := a.rotationConfig()
	rt := a.rotationOf()
	today := now.Format("20060102")

	// 0) 到龄回池：**不论开关**。名单是 rotation 自己的记账——停用模块/临时关开关
	//    都不该让这些号永久卡在"已移除"（清名单动作只作用于本模块下过线的号，幂等）。
	a.rotationReturnAged(rt, cfg, now)

	if !cfg.Enabled {
		return
	}

	// 1) 通道闸：sendOfflineChunks 会先本地 MarkRemoved/删行（防心跳复活），命令发不出去会
	//    留下"本地已移除、机器人还在跑"的错位（心跳被滤掉）。自动化路径不做半完成动作：
	//    整轮跳过，通道恢复后下轮照常。
	if a.Events == nil || a.Ctrl == nil || !a.Ctrl.Connected() {
		a.logRotationFlip(rt, now, "跳过：机器人通道未连接")
		return
	}

	// 2) 在线时长跟踪 + 预备队保护。
	snap := a.St.Snapshot()
	rt.trackOnline(snap, now)
	reserve := a.rotationReserveCount()

	// 3) 判决（纯逻辑在 rotation.Plan）。
	reghostSet := a.rotationReghostSet()
	facts := func(acc string) rotation.Facts {
		r, _ := a.St.Get(acc)
		return rotation.Facts{
			GhostFull:  r.GhostDoneToday || ghostDailyFull(r), // 独立满额表 + 心跳口径（同 sharedaily.go:526 组合）
			OnlineFor:  rt.onlineFor(acc, now),
			Personal:   a.IsPersonal(acc),
			Removed:    a.St.IsRemoved(acc),
			Reghosting: reghostSet[acc],
		}
	}
	res := rotation.Plan(now, cfg, snap, facts, today, reserve, &rt.quota)

	switch {
	case res.Blocked != "":
		a.logRotationFlip(rt, now, "跳过：%s", res.Blocked)
		return
	case len(res.Batch) == 0:
		// 无动作不刷屏（60s 一轮）：仅"有候选但额度用尽"的翻转点打一条。
		rt.mu.Lock()
		queuedFlip := res.Queued > 0 && rt.lastQueued == 0
		rt.lastQueued = res.Queued
		rt.mu.Unlock()
		if queuedFlip {
			a.Log.Printf("[ROTATION] 候选 %d 个超额度排队（本小时额度用尽，整点后继续）", res.Queued)
		}
		if len(res.Reasons) > 0 {
			a.Log.Printf("[ROTATION] 本轮无可下线（跳过 %d：%s）", len(res.Reasons), rotation.ReasonSummary(res.Reasons, 5))
		}
		return
	}

	// 4) 执行：复用批量下线/水位器压号同一条通路（先 MarkRemoved+删行，再分批 remove）。
	sent, chunks := a.sendOfflineChunks(res.Batch, 10, 300, "rotation_remove")
	// 名单与额度按"已尝试"记账：本地 MarkRemoved 已生效（不可逆）→ 必须记名单（否则永不回池）
	// 且扣额度（否则限速失效）。sent 只用于留痕（通道抖动时告警）。
	rt.quota.Commit(now, len(res.Batch))
	rt.store.Add(res.Batch, now)
	if err := rt.store.Save(); err != nil {
		a.Log.Printf("[ROTATION] 名单落盘失败（内存已生效；下次动作再落）: %v", err)
	}

	rt.mu.Lock()
	rt.lastQueued = res.Queued
	rt.lastBlocked = ""
	rt.mu.Unlock()
	a.logRotationFlip(rt, now, "下线 %d 个（sent %d/%d 批；排队 %d；预备队 %d）",
		len(res.Batch), len(sent), chunks, res.Queued, reserve)
	// 2026-10-14 熔断号止损换出：含熔断号时用单独措辞（便于观测），并单列止损名单。
	if n := len(res.CappedOut); n > 0 {
		a.Log.Printf("[ROTATION] 轮换下线 %d 个（含 %d 个当日卡死熔断·止损）：%s",
			len(res.Batch), n, rotation.Summary(res.Batch, 10))
		a.Log.Printf("[ROTATION] 熔断·止损换出（当日已无产出能力，换预备队止损）：%s",
			rotation.Summary(res.CappedOut, 10))
	} else {
		a.Log.Printf("[ROTATION] 轮换下线 %d 个（已满额+纯游荡）：%s", len(res.Batch), rotation.Summary(res.Batch, 10))
	}
	if len(sent) < len(res.Batch) {
		a.Log.Printf("[ROTATION] 警告：remove 只送达 %d/%d（本地已标记移除；机器人端可能仍在线，下次 hello/运维核对）",
			len(sent), len(res.Batch))
	}
	if a.Store != nil {
		a.Store.LogEvent(map[string]any{"type": "api", "action": "rotation_offline",
			"zone": a.currentZoneKey(), "accounts": res.Batch, "sent": len(sent), "chunks": chunks,
			"queued": res.Queued, "reserve": reserve, "capped_out": res.CappedOut})
	}
}

// rotationReturnAged 到龄回池：解除"已移除"标记并移出名单（这批号随后可被水位器
// 按"最久未上线优先"重新拉起）。
func (a *API) rotationReturnAged(rt *rotationRuntime, cfg rotation.Config, now time.Time) {
	due := rt.store.Aged(now, time.Duration(cfg.KeepHours)*time.Hour)
	if len(due) == 0 {
		return
	}
	returned := 0
	for _, acc := range due {
		if a.St.UnmarkRemoved(acc) {
			returned++
		}
	}
	rt.store.Remove(due)
	if err := rt.store.Save(); err != nil {
		a.Log.Printf("[ROTATION] 回池后名单落盘失败（内存已清；下次动作再落）: %v", err)
	}
	a.logRotationFlip(rt, now, "回池 %d 个（下线已满 %dh）", len(due), cfg.KeepHours)
	a.Log.Printf("[ROTATION] 回池 %d 个（下满 %dh；其中 %d 个原在移除名单已解除）：%s",
		len(due), cfg.KeepHours, returned, rotation.Summary(due, 10))
	if a.Store != nil {
		a.Store.LogEvent(map[string]any{"type": "api", "action": "rotation_return",
			"zone": a.currentZoneKey(), "accounts": due, "returned": returned, "keep_hours": cfg.KeepHours})
	}
}

// logRotationFlip 记 lastAction，并按去抖规则打日志（同文案连续出现只打第一条）。
func (a *API) logRotationFlip(rt *rotationRuntime, now time.Time, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	rt.mu.Lock()
	flip := msg != rt.lastBlocked
	rt.lastBlocked = msg
	rt.lastAction = msg
	rt.lastRunTS = now.Unix()
	rt.mu.Unlock()
	if flip {
		a.Log.Printf("[ROTATION] %s", msg)
	}
}

// StartRotation 轮换 tick 主循环（main 里 go webAPI.StartRotation(ctx)）：60s 一轮，
// 直到 ctx.Done()。CTRL_ROTATION 默认关：关闭时仍执行"到龄回池"，但不主动下线。
func (a *API) StartRotation(ctx context.Context) {
	rt := a.rotationOf()
	if n, err := rt.store.Load(); err != nil {
		a.Log.Printf("[ROTATION] 名单加载失败（忽略；按空名单处理）: %v", err)
	} else if n > 0 {
		a.Log.Printf("[ROTATION] 轮换名单已载入: %d 条（到龄自动回池）", n)
	}
	cfg := a.rotationConfig()
	if !cfg.Enabled {
		a.Log.Printf("[ROTATION] 未启用（CTRL_ROTATION=0）：仅执行到龄回池，不主动下线")
	} else {
		a.Log.Printf("[ROTATION] 已启动（%s 一轮；≤%d 个/小时、每轮 ≤%d；预备队下限 %d；%dh 回池）",
			rotation.Interval, cfg.PerHour, rotation.MaxPerTick, cfg.MinReserve, cfg.KeepHours)
	}
	tk := time.NewTicker(rotation.Interval)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			a.Log.Printf("[ROTATION] 退出（ctx done）")
			return
		case <-tk.C:
			a.rotationTick(time.Now())
		}
	}
}

// ---------------------------------------------------------------- 接口

// handleRotationGet 轮换状态（只读；面板后续展示用）。
func (a *API) handleRotationGet(w http.ResponseWriter, r *http.Request) {
	cfg := a.rotationConfig()
	rt := a.rotationOf()
	now := time.Now()
	rt.mu.Lock()
	lastAction, lastRun := rt.lastAction, rt.lastRunTS
	used := rt.quota.Used(now)
	rt.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             true,
		"enabled":        cfg.Enabled,
		"per_hour":       cfg.PerHour,
		"min_reserve":    cfg.MinReserve,
		"keep_hours":     cfg.KeepHours,
		"max_per_tick":   rotation.MaxPerTick,
		"min_online_min": int(rotation.MinOnline / time.Minute),
		"reserve":        a.rotationReserveCount(),
		"list_count":     rt.store.Count(),
		"offlined_today": rt.store.CountDay(now.Format("20060102")),
		"quota_used":     used,
		"last_action":    lastAction,
		"last_run_ts":    lastRun,
		"path":           rt.store.Path(),
	})
}
