// Package event 事件处理核心（单通道 / 单机器人进程）。
//
// 数据流：
//
//	robot_single_robot.exe --TCP <ctrl_port>--> ctrl.Server（单连接）
//	  --> RunEventLoop --> HandleEvent(表驱动) --> state 更新（带区标记）
//	  --> store.LogEvent(data/runs_YYYYMMDD.jsonl) + WS 广播
//
// 区（zone）：控制通道在事件入队时打 "_zone" = 当前区 key（切区时由 API 更新通道标记）。
//
// 设计边界：本包只做**协议语义**的通用处理（状态映射/错误记账/下机/落盘/推送）；
// 业务编排（换号、冷却、资格判定、定时上线等）由使用方在自身模块里实现。
package event

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"zyctrlcenter/internal/config"
	"zyctrlcenter/internal/ctrl"
	"zyctrlcenter/internal/logging"
	"zyctrlcenter/internal/maplib"
	"zyctrlcenter/internal/services/intent"
	"zyctrlcenter/internal/state"
	"zyctrlcenter/internal/store"
)

// StaleRobotSec 心跳超过该秒数的账号行视为陈旧（清理）。
const StaleRobotSec = 600

// Handler 事件处理器。
type Handler struct {
	Cfg   *config.Config
	St    *state.State
	Store *store.Store
	Ctrl  *ctrl.Server
	Log   *logging.Logger
	// Intents 账号"该跑哪条链"的意图表（上线/链完成时登记；P1 的恢复引擎会按它补发命令）。
	Intents *intent.Plan

	broadcast func(map[string]any)

	// reghoster 卡死自动重登恢复入口（可空，由 main 注入 reghost.Runner.Request）。
	reghoster func(account, reason string)

	mu             sync.Mutex
	posBatch       map[string]map[string]any
	posFlusherOnce sync.Once
}

// New 创建事件处理器；Broadcast 由 API 层稍后注入。
func New(cfg *config.Config, st *state.State, runStore *store.Store, c *ctrl.Server,
	log *logging.Logger) *Handler {
	dec := intent.Decider{}
	if cfg != nil {
		dec.NewbieMaxLevel = cfg.NewbieMaxLevel
		dec.NewbieChainID = cfg.DefaultChainID
	}
	return &Handler{
		Cfg:      cfg,
		St:       st,
		Store:    runStore,
		Ctrl:     c,
		Log:      log,
		Intents:  intent.NewPlan(dec),
		posBatch: map[string]map[string]any{},
	}
}

// SetBroadcast 注入 WS 广播函数（api 层创建 Hub 后调用）。
// SetReghoster 注入"卡死自动重登恢复"入口（main 装配；nil = 不自动恢复）。
func (h *Handler) SetReghoster(fn func(account, reason string)) { h.reghoster = fn }

func (h *Handler) SetBroadcast(fn func(map[string]any)) {
	h.broadcast = fn
}

// SendCmd 下发命令并记录日志（单通道：只有一条机器人连接）。
func (h *Handler) SendCmd(cmd map[string]any, action string) bool {
	ok := h.Ctrl.SendCmd(cmd)
	if !ok {
		h.Log.Printf("[CTRL] 命令下发失败（通道未连接？）cmd=%v action=%s", cmd["cmd"], action)
	}
	return ok
}

// RemoveRobot 通知机器人端移除账号（下机），并记录历史。
func (h *Handler) RemoveRobot(account, reason string) bool {
	ok := h.SendCmd(map[string]any{
		"cmd": "robot_manage", "action": "remove", "accounts": []string{account},
	}, "remove_"+reason)
	h.Store.LogEvent(map[string]any{
		"type": "api", "action": "robots_manage", "sub": reason,
		"accounts": []string{account}, "sent": ok,
	})
	return ok
}

// ---------------------------------------------------------------- 事件表

// handlers 事件类型 → 处理函数（表驱动，与机器人端 g_handle_map 风格一致）。
func (h *Handler) handlers() map[string]func(map[string]any) {
	return map[string]func(map[string]any){
		"hello":              h.onHello,
		"pong":               h.onHello,
		"robot_online":       h.onRobotOnline,
		"robot_offline":      h.onRobotOffline,
		"status_reply":       h.onStatusReply,
		"robot_manage_reply": h.onRobotManageReply,
		"task_progress":      h.onTaskProgress,
		"robot_state":        h.onRobotState,
		"robot_pos":          h.onRobotPos,
		"chain_done":         h.onChainDone,
		"error":              h.onError,
		"ghost_done":         h.onGhostDone,
		"ghost_offline":      h.onGhostOffline,
		"log":                h.onLog,
	}
}

// HandledTypes 返回已注册处理的事件类型（升序）——
// 供 /api/protocols 与「协议覆盖」元测试校验：协议表里的每个事件都必须有分支。
func (h *Handler) HandledTypes() []string {
	out := make([]string, 0, 16)
	for k := range h.handlers() {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// HandleEvent 处理一条机器人事件（未知事件写历史）。
func (h *Handler) HandleEvent(ev map[string]any) {
	etype := str(ev, "type")
	fn := h.handlers()[etype]
	if fn == nil {
		h.Store.LogEvent(ev)
		return
	}
	defer func() {
		if r := recover(); r != nil {
			h.Log.Printf("[EVENT] 处理 %s 事件 panic: %v", etype, r)
		}
	}()
	fn(ev)
}

// RunEventLoop 消费控制通道事件（阻塞直到 ctx 取消）。
func (h *Handler) RunEventLoop(ctx context.Context) {
	h.Log.Printf("[EVENT] 事件循环已启动")
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-h.Ctrl.Events:
			h.HandleEvent(ev)
			h.AfterEvent(ev)
		}
	}
}

// AfterEvent 事件后置动作：WS 广播（robot_pos 走合并推送，debug 日志不推 WS）。
func (h *Handler) AfterEvent(ev map[string]any) {
	if str(ev, "type") == "robot_pos" || mutedLog(ev) {
		return
	}
	h.Broadcast(ev)
}

// Broadcast 向 WS 客户端推送（未注入时静默）。
func (h *Handler) Broadcast(obj map[string]any) {
	if h.broadcast != nil {
		h.broadcast(obj)
	}
}

func mutedLog(ev map[string]any) bool {
	if str(ev, "type") != "log" {
		return false
	}
	lv := str(ev, "level")
	return lv == "debug" || lv == "trace"
}

// zoneOf 事件来源区（控制通道打的 "_zone" = 当前区 key）。
func zoneOf(ev map[string]any) string { return str(ev, "_zone") }

// gridCell 客户端坐标口径：1 格 = 多少像素（默认 16）。
func (h *Handler) gridCell() int {
	if h.Cfg != nil && h.Cfg.GridCell > 0 {
		return h.Cfg.GridCell
	}
	return maplib.GridCell
}

// setPos 写入位置：同时保存原始像素与**客户端格子坐标**（面板显示用后者）。
func (h *Handler) setPos(r *state.Robot, x, y int) {
	r.Pos = []int{x, y}
	r.PosGrid = maplib.GridPosWith(x, y, h.gridCell())
}

// setPosFromEvent 用事件里的 pos 字段写入位置（形状不符时按原样保留，不猜）。
func (h *Handler) setPosFromEvent(r *state.Robot, v any) {
	p := toIntSlice(v)
	if len(p) >= 2 {
		h.setPos(r, p[0], p[1])
		return
	}
	r.Pos = p
	r.PosGrid = nil
}

// ---------------------------------------------------------------- 等级防抖

// 等级"大幅回退"防抖参数。
//
// 背景（2026-09-21 生产 robot0001028）：机器人端某次上报把 level 从 39 错成 9，
// 中控立即覆盖并触发 decideIntent → 正在正常抓鬼的号被切成"新手"意图
// （大屏「🆕新手」筛选于是显示了正在抓鬼的号）。单次错值不该有这么大的副作用：
// 大幅回退先挂"待确认"，同一新值**连续**出现 N 次才认账。
const (
	levelRegressThreshold = 10 // 已确认等级 - 新上报 ≥ 10 视为"大幅回退"（可疑）
	levelRegressConfirmN  = 3  // 同一可疑新值连续出现 N 次才切换
)

// levelUpdate 一次等级写入的结果（在状态锁内生成，供调用方在锁外打日志）。
type levelUpdate struct {
	Applied   bool // 是否写入了 r.Level
	Confirmed bool // Applied 且是"大幅回退"凑满连续次数才写入的
	Old       int  // 写入前已确认等级
	New       int  // 本次上报值
	Pending   int  // 待确认的新值（未写入时）
	PendingN  int  // 待确认值已连续出现的次数
}

// applyLevel 等级防抖写入（r 在状态锁内，本函数不加锁）。
//
// 口径：
//   - 新值 > 0 且 已确认等级 - 新值 ≥ levelRegressThreshold → 大幅回退：**不覆盖**，
//     累计连续次数；同一新值连续 levelRegressConfirmN 次 → 切换并清零待确认态；
//   - 其余（升级 / 小幅回退 <10 / 首次上报 / 与已确认值相同）→ 立即生效并清零待确认态
//     （正确值一回来就清，防"错值闪现"让后续错值凑满连续次数）；
//   - 新值 ≤ 0（未知）→ 不判定、不覆盖已有非零等级（比旧行为更保守：旧代码会用 0 覆盖）。
func applyLevel(r *state.Robot, level int) levelUpdate {
	u := levelUpdate{Old: r.Level, New: level}
	if level <= 0 {
		return u
	}
	if r.Level > 0 && r.Level-level >= levelRegressThreshold {
		if r.LevelPending == level {
			r.LevelPendingN++
		} else {
			r.LevelPending, r.LevelPendingN = level, 1
		}
		u.Pending, u.PendingN = r.LevelPending, r.LevelPendingN
		if r.LevelPendingN < levelRegressConfirmN {
			return u // 大幅回退待确认：先不覆盖
		}
		u.Applied, u.Confirmed = true, true
	} else {
		u.Applied = true
	}
	r.Level = level
	r.LevelPending, r.LevelPendingN = 0, 0
	return u
}

// logLevelUpdate 等级防抖的可诊断日志（应用日志 + 面板运行历史各一条 warn）：
// 待确认与确认切换都带旧值/新值/连续次数 —— 排查"机器人端错值从哪来"的入口。
func (h *Handler) logLevelUpdate(account, zone string, u levelUpdate) {
	msg := ""
	switch {
	case u.Confirmed:
		msg = fmt.Sprintf("等级大幅回退已确认：%d → %d（同一值连续上报 %d 次）", u.Old, u.New, u.PendingN)
	case u.PendingN > 0 && !u.Applied:
		msg = fmt.Sprintf("等级大幅回退待确认：已确认 %d，本次上报 %d（连续 %d/%d 次，暂不覆盖、不切意图）",
			u.Old, u.New, u.PendingN, levelRegressConfirmN)
	default:
		return
	}
	h.Log.Printf("[LEVEL] %s %s", account, msg)
	h.Store.LogEvent(map[string]any{"type": "log", "level": "warn",
		"account": account, "zone": zone, "msg": msg})
}

// ---------------------------------------------------------------- 事件分支

func (h *Handler) onHello(ev map[string]any) {
	h.Log.Printf("[EVENT] 机器人握手 hello version=%v pid=%v zone=%s",
		ev["robot_version"], ev["pid"], zoneOf(ev))
}

func (h *Handler) onRobotOnline(ev map[string]any) {
	account := str(ev, "account")
	if account == "" {
		return
	}
	zone := zoneOf(ev)
	h.St.UnmarkRemoved(account)
	reported, chainDone := toInt(ev["level"]), false
	level := 0 // 防抖后的**有效等级**（单次错值不会立刻覆盖，也就不会污染意图判定）
	var lv levelUpdate
	h.St.Update(account, func(r *state.Robot) {
		r.Zone = zone
		r.Online = true
		r.State = "ONLINE"
		r.RoleName = str(ev, "role_name")
		lv = applyLevel(r, reported)
		r.MapID = toInt(ev["mapid"])
		r.LastSeen = now()
		chainDone = r.ChainDone
		level = r.Level
	})
	h.logLevelUpdate(account, zone, lv)
	// 意图判据：上线是"等级/链完成"这两条信息第一次到齐的时刻（等级<31 判新手链优先）
	h.decideIntent(account, level, chainDone, zone, "decide")
	h.Store.LogEvent(map[string]any{"type": "log", "level": "info",
		"account": account, "zone": zone, "msg": "账号上线"})
	h.Log.Printf("[EVENT] 账号上线 %s（区 %s）", account, zone)
}

// decideIntent 按判据登记/切换意图（等级未知时不登记、也不覆盖已有意图）。
func (h *Handler) decideIntent(account string, level int, chainDone bool, zone, source string) {
	if h.Intents == nil {
		return
	}
	dec := h.Intents.Decider().Decide(level, chainDone)
	prev, changed, err := h.Intents.Apply(account, dec, zone, source)
	if err != nil || !changed {
		return
	}
	msg := "意图：" + string(dec.Kind) + "（" + dec.Reason + "）"
	if prev.Kind != "" {
		msg = "意图切换：" + string(prev.Kind) + " → " + string(dec.Kind) + "（" + dec.Reason + "）"
	}
	h.Store.LogEvent(map[string]any{"type": "intent", "account": account, "zone": zone,
		"kind": string(dec.Kind), "prev": string(prev.Kind), "chain_id": dec.ChainID, "msg": msg})
	h.Log.Printf("[INTENT] %s %s", account, msg)
}

func (h *Handler) onRobotOffline(ev map[string]any) {
	account := str(ev, "account")
	if account == "" {
		return
	}
	h.St.Update(account, func(r *state.Robot) {
		r.Online = false
		r.State = "OFFLINE"
		r.HS = false // 下线即清握手标记
		r.LastSeen = now()
	})
	h.Store.LogEvent(map[string]any{"type": "log", "level": "info",
		"account": account, "zone": zoneOf(ev), "msg": "账号下线"})
}

func (h *Handler) onStatusReply(ev map[string]any) {
	zone := zoneOf(ev)
	if server := str(ev, "server"); server != "" {
		h.St.SetCurServer(zone, server)
	}
	robots, _ := ev["robots"].([]any)
	nowTS := now()
	for _, item := range robots {
		st, ok := item.(map[string]any)
		if !ok {
			continue
		}
		account := str(st, "account")
		if account == "" || h.St.IsRemoved(account) {
			continue // 已移除账号：心跳不再复活
		}
		level, chainDone := 0, false
		hatchDone, hatchReason := false, "" // 本次心跳"刚孵出"（false→true）才记一条日志，别每 3 秒刷屏
		var lv levelUpdate
		h.St.Update(account, func(r *state.Robot) {
			if zone != "" {
				r.Zone = zone
			}
			if v, ok := st["state"]; ok {
				r.State = toStr(v)
			}
			if v, ok := st["task_index"]; ok {
				r.TaskIndex = toInt(v)
			}
			if v, ok := st["done"]; ok {
				r.Done = toInt(v)
			}
			if v, ok := st["chain_done"]; ok {
				r.ChainDone = toBool(v)
			}
			if v, ok := st["role_name"]; ok {
				r.RoleName = toStr(v)
			}
			if v, ok := st["level"]; ok {
				lv = applyLevel(r, toInt(v))
			}
			if v, ok := st["mapid"]; ok {
				r.MapID = toInt(v)
			}
			if v, ok := st["fight"]; ok {
				r.Fight = toBool(v)
			}
			if v, ok := st["online"]; ok {
				r.Online = toBool(v)
			}
			if v, ok := st["pos"]; ok {
				h.setPosFromEvent(r, v)
			}
			if v, ok := st["fpp"]; ok {
				r.Fpp = toInt(v)
			}
			if v, ok := st["role_id"]; ok {
				r.RoleID = toInt(v)
			}
			if v, ok := st["ghost"]; ok {
				if m, ok := v.(map[string]any); ok {
					r.Ghost = m
				}
			}
			if v, ok := st["ghost_target"]; ok {
				r.GhostTarget = toIntSlice(v)
			}
			// 孵化会话（机器人上报原样透传；/api/status 直接把它给面板）。
			// hatched 由 false→true 的那一刻记一条日志（"从 running 移除"由 pools 实时算）。
			if v, ok := st["hatch"]; ok {
				if m, ok := v.(map[string]any); ok {
					if toBool(m["hatched"]) && !hatchHatched(r.Hatch) {
						hatchDone, hatchReason = true, toStr(m["reason"])
					}
					r.Hatch = m
				}
			}
			if v, ok := st["hp"]; ok {
				r.HP = toIntSlice(v) // [当前, 上限]
			}
			if v, ok := st["mp"]; ok {
				r.MP = toIntSlice(v)
			}
			if v, ok := st["bag"]; ok {
				r.Bag = v
			}
			if v, ok := st["equip"]; ok {
				r.Equip = v
			}
			if v, ok := st["summons"]; ok {
				r.Summons = v
			}
			if v, ok := st["booth"]; ok {
				r.Booth = v
			} else {
				r.Booth = nil // 全量上报未带 = 当前不摆摊，清残留
			}
			if v, ok := st["walk"]; ok {
				r.Walk = v
			}
			// 2026-09-22 全服在线(含真人)：机器人 @online 回执解析后上报 {count,ts(ms)}。
			// 只有"查到过"的那个号带该字段；带上就更新（保留最新读数，不带不动）。
			if v, ok := st["svr_online"].(map[string]any); ok {
				r.SvrOnline = v
			}
			if v, ok := st["route"]; ok {
				r.Route = v
			}
			r.HS = toBool(st["hs"]) // 旧版机器人不带 hs → 按未握手处理
			// 2026-09-22 孵化池过滤：机器人上报 has_egg（装备栏在孵 or 背包有蛋）
			if v, ok := st["has_egg"]; ok {
				r.HasEgg = toBool(v)
			}
			// 心跳里的「最近错误」：非空就写入（保留最后一次错误供排查；机器人端是毫秒）；
			// **机器人不带错误且不在 ERROR 态 = 已经恢复** → 清掉残留，否则面板会一直显示"需要处理"
			//（err_repeat 是熔断计数，恢复后归零：别再拿旧错误去熔断这个号）。
			if code := str(st, "err_code"); code != "" {
				r.ErrCode = code
				r.ErrTS = float64(toInt(st["err_ts"])) / 1000.0
			} else if !strings.EqualFold(str(st, "state"), "ERROR") {
				r.ErrCode, r.ErrTS, r.ErrRepeat = "", 0, 0
			}
			r.LastSeen = nowTS
			level, chainDone = r.Level, r.ChainDone
		})
		h.logLevelUpdate(account, zone, lv)
		if hatchDone {
			msg := "孵化完成（蛋已孵出）"
			if hatchReason != "" {
				msg = "孵化完成：" + hatchReason
			}
			h.Log.Printf("[HATCH] %s %s", account, msg)
			h.Store.LogEvent(map[string]any{"type": "log", "level": "info",
				"account": account, "zone": zone, "msg": msg})
		}
		// 心跳是"重连后第一次拿到等级/链完成"的地方（robot_online 不会再发一次），
		// 所以这里也判一次意图：幂等（没变化不写、不打日志），覆盖重连/补报场景。
		h.decideIntent(account, level, chainDone, zone, "status")
	}
}

func (h *Handler) onRobotManageReply(ev map[string]any) {
	h.Store.LogEvent(ev)
}

func (h *Handler) onTaskProgress(ev map[string]any) {
	account := str(ev, "account")
	if account == "" {
		return
	}
	zone := zoneOf(ev)
	h.St.Update(account, func(r *state.Robot) {
		if r.Zone == "" {
			r.Zone = zone
		}
		if v, ok := ev["done"]; ok {
			r.Done = toInt(v)
		}
		// 有进度 = 真的在推进：把"卡住"的残留错误清掉（机器人自己的错误缓存未必清，
		// 面板上别一直挂着"需要处理"；err_repeat 是熔断计数，恢复后归零）。
		if r.ErrCode != "" {
			r.ErrCode, r.ErrTS, r.ErrRepeat = "", 0, 0
		}
		if v, ok := ev["task_index"]; ok {
			r.LastTask = toInt(v)
			r.TaskIndex = toInt(v)
		}
		r.LastSeen = now()
	})
	h.Store.LogEvent(ev)
}

func (h *Handler) onRobotState(ev map[string]any) {
	account := str(ev, "account")
	zone := zoneOf(ev)
	if account != "" {
		h.St.Update(account, func(r *state.Robot) {
			if r.Zone == "" {
				r.Zone = zone
			}
			if v, ok := ev["state"]; ok {
				r.State = toStr(v)
			}
			if v, ok := ev["task_index"]; ok {
				r.TaskIndex = toInt(v)
			}
			if v, ok := ev["done"]; ok {
				r.Done = toInt(v)
			}
			if v, ok := ev["mapid"]; ok {
				r.MapID = toInt(v)
			}
			if v, ok := ev["pos"]; ok {
				h.setPosFromEvent(r, v)
			}
			for _, k := range []string{"hp", "mp", "bag", "equip"} {
				if v, ok := ev[k]; ok {
					switch k {
					case "hp":
						r.HP = toIntSlice(v) // [当前, 上限]
					case "mp":
						r.MP = toIntSlice(v)
					case "bag":
						r.Bag = v
					case "equip":
						r.Equip = v
					}
				}
			}
			if v, ok := ev["booth"]; ok {
				r.Booth = v
			} else if r.Booth != nil {
				r.Booth = nil
			}
			r.LastSeen = now()
		})
	}
	h.Store.LogEvent(ev)
}

func (h *Handler) onChainDone(ev map[string]any) {
	account := str(ev, "account")
	if account == "" {
		return
	}
	zone := zoneOf(ev)
	level := 0
	h.St.Update(account, func(r *state.Robot) {
		if r.Zone == "" {
			r.Zone = zone
		}
		r.State = "DONE"
		r.ChainDone = true
		r.LastSeen = now()
		level = r.Level
	})
	// 新手链跑完 → 意图转抓鬼（P1 才按意图自动补发 ghost_start；这里只记账）
	h.decideIntent(account, level, true, zone, "decide")
	h.Store.LogEvent(ev)
	// already_done（库中已完成、被误拉起）不下机，保持在线
	if h.autoRemove() && !toBool(ev["already_done"]) {
		h.removeAccount(account, "chain_done")
	}
}

func (h *Handler) onError(ev map[string]any) {
	account := str(ev, "account")
	if account == "" {
		return
	}
	zone := zoneOf(ev)
	if code := str(ev, "code"); code != "" {
		repeat := h.St.MarkTaskError(account, code, str(ev, "msg"))
		h.St.Update(account, func(r *state.Robot) {
			if r.Zone == "" {
				r.Zone = zone
			}
		})
		h.Store.LogEvent(map[string]any{"type": "log", "level": "warn", "account": account,
			"zone": zone,
			"msg":  "任务出错 " + code + " (第 " + strconv.Itoa(repeat) + " 次): " + str(ev, "msg")})
		// 钟馗对话卡死：机器人会自己停止抓鬼并"等中控重登后重新下发"（daily_ghost.py:441）。
		// 这里交给 reghost 去跑"下线 → 重登 → 延迟补发"，否则该号会一直空闲着不干活。
		// 当日卡死计数（不随进度清零）：反复卡死的号当天不再被自动任务拉起（防 churn）。
		if isStuckCode(code) {
			today := time.Now().Format("20060102")
			h.St.Update(account, func(r *state.Robot) {
				if r.StuckDay != today {
					r.StuckDay, r.StuckCount = today, 0
				}
				r.StuckCount++
			})
		}
		// 卡死类交给 reghost 下线重登后重新下发（与上面计数互不影响）。
		// churn 防护仍生效：reghost.Request 内部有"当日卡死 ≥ ChurnLimit 不再自动重登"
		// （internal/services/reghost/reghost.go:144，读的正是上面累计的 StuckCount）。
		if isStuckCode(code) {
			if h.reghoster != nil {
				h.reghoster(account, str(ev, "msg"))
			}
		}
	}
	h.Store.LogEvent(ev)
}

// isGhostUnavailableCode 是否"本号今天抓鬼不可用"类原因（无动作项/无令/没钱）。
// 2026-09-22：这类原因下线后，当天不再派抓鬼（跨日恢复），避免在钟馗反复空转。
func isGhostUnavailableCode(code string) bool {
	switch strings.ToUpper(strings.TrimSpace(code)) {
	case "NO_ACTION", "NO_TOKEN", "NO_MONEY", "DAILY_LIMIT":
		return true
	}
	return false
}

// isStuckCode 是否"卡死类"错误码（要计入当日 churn 计数 + 触发重登恢复）。
// GHOST_DIALOG_STUCK/TASK_STUCK 是抓鬼专属码；quest_engine 看门狗统一上报
// STUCK_<STATE>（STUCK_WAIT_NEXT/STUCK_CLICK/...）—— 2026-09-21 前只认前两个，
// 导致 8 个号卡 ERROR 25 分钟无人恢复。
func isStuckCode(code string) bool {
	switch code {
	case "GHOST_DIALOG_STUCK", "TASK_STUCK":
		return true
	}
	return strings.HasPrefix(code, "STUCK_")
}

func (h *Handler) onGhostDone(ev map[string]any) {
	h.Store.LogEvent(ev)
	account := str(ev, "account")
	if account == "" {
		return
	}
	h.Store.LogEvent(map[string]any{"type": "log", "level": "info", "account": account,
		"zone": zoneOf(ev), "msg": "抓鬼满额（业务处置如换号/补位由使用方实现）"})
	// 2026-09-22：记"今天抓鬼已满" —— 服务端满额后钟馗不再给任务，若不记住，
	// 抓鬼池会把它再拉起来 → 在钟馗空转（今日堆积分分钟涨到 26+ 的那个坑）。
	h.St.MarkGhostDoneToday(account)
	// 2026-09-22 策略修正：满额**不再自动移除**（原来 removeAccount(ghost_done)）。
	//   抓鬼满额是"每日"的（次日重置可复用），而"移除"是当天永久排除 —— 生产实测
	//   一天累积 461 个被移除（当前区可用号 544 个里 83%）→ 候选池为空、水位器
	//   补不到号（"号池里没有可上线的号"）、在线卡在 83 上不到 200。
	//   现在：满额只离线（机器人自己下线 + MarkGhostDoneToday 当天不再派），
	//   次日 0 点自然恢复复用。只有"真坏号"才移除，见 onGhostOffline 的白名单。
	h.Store.LogEvent(map[string]any{"type": "log", "level": "info", "account": account,
		"zone": zoneOf(ev), "msg": "抓鬼满额：只离线不清号（次日可复用；如需换号由池子/水位器补位）"})
}

func (h *Handler) onGhostOffline(ev map[string]any) {
	h.Store.LogEvent(ev)
	account := str(ev, "account")
	if account == "" {
		return
	}
	// 服务端要求的抓鬼等级（学习到的门槛）：登记下来，低于它的号不再派抓鬼
	// （口径同参考实现 services/ghost_fit.py 的 required_level）。
	if lv := toInt(ev["required_level"]); lv > 0 {
		h.St.Update(account, func(r *state.Robot) {
			if lv > r.GhostRequiredLevel {
				r.GhostRequiredLevel = lv
			}
		})
	}
	// 2026-09-22：抓鬼"下线换号"类原因（无动作项/无令/没钱/服务端不给任务）→ 记"今天抓鬼不可用"，
	//   当天不再把它当抓鬼候选（跨日自动恢复）。理由：机器人已先做了 3 轮退避+自愈才判下线，
	//   若控制器再把它拉起 = "满额/不可用 → 下线 → 又被拉起 → 钟馗空转"的堆积循环。
	if isGhostUnavailableCode(str(ev, "code")) {
		h.St.MarkGhostDoneToday(account)
	}
	h.Store.LogEvent(map[string]any{"type": "log", "level": "warn", "account": account,
		"zone": zoneOf(ev),
		"msg": "抓鬼不可行(" + str(ev, "code") + "): " + str(ev, "reason") +
			"（业务处置如换号/冷却由使用方实现）"})
	// 2026-09-22 策略修正：只对"真坏号"永久移除；满额/无动作项/无令/没钱等
	//   "今天不行"的原因只离线（当天不派），不进移除名单 —— 否则候选池会被吃空。
	if h.autoRemove() && ghostOfflineShouldRemove(str(ev, "code")) {
		h.removeAccount(account, "ghost_offline")
	}
}

// ghostOfflineShouldRemove 抓鬼"下线换号"时是否把该号**永久移除**（默认只离线）。
//   - 移除白名单 = 服务端明确判了"这个号接不到"（等级不足/条件不符等），
//     以及账号本身有问题（封禁/密码错）的情况；
//   - NO_ACTION / NO_TOKEN / NO_MONEY / DAILY_LIMIT 等都属于"今天不行"，
//     号明天还能用 → 只离线，不永久排除。
func ghostOfflineShouldRemove(code string) bool {
	switch strings.ToUpper(strings.TrimSpace(code)) {
	case "ACCEPT_FATAL", "BANNED", "ACCOUNT_INVALID", "PASSWORD_ERROR":
		return true
	}
	return false
}

func (h *Handler) onLog(ev map[string]any) {
	h.Store.LogEvent(ev)
}

// hatchHatched 上一次心跳里的孵化会话是否已"孵出"（用于识别 false→true 的切换）。
func hatchHatched(m map[string]any) bool {
	if m == nil {
		return false
	}
	b, _ := m["hatched"].(bool)
	return b
}

// autoRemove 是否自动下机（chain_done/ghost_done/ghost_offline）。
func (h *Handler) autoRemove() bool {
	return h.Cfg != nil && h.Cfg.AutoRemoveOnDone
}

// removeAccount 下机：标记移除（心跳不复活）+ 删行 + 通知机器人端移除。
func (h *Handler) removeAccount(account, reason string) {
	if h.St.IsRemoved(account) {
		return
	}
	h.St.MarkRemoved(account)
	h.St.Remove(account)
	go h.RemoveRobot(account, reason)
}

// ---------------------------------------------------------------- 实时位置

func (h *Handler) onRobotPos(ev map[string]any) {
	account := str(ev, "account")
	if account == "" || h.St.IsRemoved(account) {
		return
	}
	x, y := toInt(ev["x"]), toInt(ev["y"])
	mapid := toInt(ev["mapid"])
	zone := zoneOf(ev)
	h.St.Update(account, func(r *state.Robot) {
		if r.Zone == "" {
			r.Zone = zone
		}
		h.setPos(r, x, y)
		if mapid != 0 {
			r.MapID = mapid
		}
		r.LastSeen = now()
	})
	gx, gy := x/h.gridCell(), y/h.gridCell()
	h.mu.Lock()
	h.posBatch[account] = map[string]any{
		"account": account, "zone": zone, "mapid": mapid,
		"x": x, "y": y, "gx": gx, "gy": gy, // gx/gy = 客户端格子坐标
	}
	h.mu.Unlock()
	h.posFlusherOnce.Do(func() { go h.posFlusher() })
}

// posFlusher 100ms 合并推送一次位置（大批量时逐条广播会积压）。
func (h *Handler) posFlusher() {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		h.mu.Lock()
		if len(h.posBatch) == 0 {
			h.mu.Unlock()
			continue
		}
		batch := make([]map[string]any, 0, len(h.posBatch))
		for _, v := range h.posBatch {
			batch = append(batch, v)
		}
		h.posBatch = map[string]map[string]any{}
		h.mu.Unlock()
		h.Broadcast(map[string]any{"type": "robot_pos_batch", "list": batch})
	}
}

// ---------------------------------------------------------------- 状态轮询

// RunStatusPoller 3 秒轮询：清陈旧行 + 拉取机器人状态。
func (h *Handler) RunStatusPoller(ctx context.Context) {
	h.Log.Printf("[EVENT] 状态轮询已启动（3s）")
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.pollOnce()
		}
	}
}

func (h *Handler) pollOnce() {
	nowTS := now()
	// 1) 清理 10 分钟无心跳的陈旧行（被强杀进程不会发 offline 事件）
	for _, account := range h.St.StaleAccounts(nowTS, StaleRobotSec) {
		h.St.Remove(account)
		h.Log.Printf("[EVENT] 清理陈旧账号行 %s（%d 秒无心跳）", account, StaleRobotSec)
	}
	// 2) 拉取状态（机器人回 status_reply 事件）
	if h.Ctrl.Connected() {
		h.Ctrl.SendCmd(map[string]any{"cmd": "status"})
	}
}

// ---------------------------------------------------------------- 工具

func now() float64 { return float64(time.Now().Unix()) }

func str(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

func toStr(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	}
	return ""
}

func toInt(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case int64:
		return int(t)
	case string:
		n, _ := strconv.Atoi(t)
		return n
	case bool:
		if t {
			return 1
		}
	}
	return 0
}

func toBool(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case float64:
		return t != 0
	case string:
		return t == "1" || strings.EqualFold(t, "true")
	}
	return false
}

func toIntSlice(v any) []int {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]int, 0, len(arr))
	for _, item := range arr {
		out = append(out, toInt(item))
	}
	return out
}
