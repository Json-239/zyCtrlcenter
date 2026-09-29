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
	// restartAutoAdd 机器人进程重启握手后的"自动补一次批量上线"回调（可空，由 main 注入；
	// 2026-09-29 A+C 的 C 侧；开关/节流/幂等在壳层 api.OnRobotRestartHello 里）。
	restartAutoAdd func(cleared []string)
	// levelSync 心跳"有效等级"回写账号池（可空，由 main 注入；2026-09-29 神捕闸门修复②：
	// 池内 zone-level 只在建号/验证写入 → 陈旧；壳层 api.SyncPoolLevel 负责变化判定+合并落盘）。
	levelSync func(account, zone string, level int)

	mu             sync.Mutex
	posBatch       map[string]map[string]any
	posFlusherOnce sync.Once

	// ---------------- 组队台账（2026-09-29 阶段 1/2，抓鬼试点；内存态） ----------------
	// teamMu 保护 teams / teamJobs / teamTokenLast / teamMemberState（HTTP 线程与事件循环线程并发读写）。
	teamMu   sync.Mutex
	teams    map[string]*TeamInfo // 队长账号 → 已就绪队伍（team_ready 后）
	teamJobs map[string]*TeamInfo // 队长账号 → 已下发未就绪的组队任务（state=pending）
	// teamTokenLast 账号 → 最近一条 team_token 事件（助战令链结果；阶段 2 机器人端事件源，
	// 心跳未带 token 字段——见 team-feature-plan 定稿 #6）。观测接口读，D13 编排阶段用。
	teamTokenLast map[string]map[string]any
	// teamMemberState 账号 → 最近一条 team_member_state（role/parted/setup_done 变化）。
	teamMemberState map[string]map[string]any
}

// New 创建事件处理器；Broadcast 由 API 层稍后注入。
func New(cfg *config.Config, st *state.State, runStore *store.Store, c *ctrl.Server,
	log *logging.Logger) *Handler {
	dec := intent.Decider{}
	if cfg != nil {
		dec.NewbieMaxLevel = cfg.NewbieMaxLevel
		dec.NewbieChainID = cfg.DefaultChainID
		// 分享日常判据（2026-09-23）：默认关（CTRL_SHARE_DAILY=1 才判 shenbu）
		dec.ShareDailyEnabled = cfg.ShareDailyEnabled
		dec.ShareDailyMinLevel = cfg.ShareDailyMinLevel
		dec.ShareDailyKey = cfg.ShareDailyKey
		// 烽火大唐判据（2026-09-24 P1）：同款独立开关（CTRL_FENGHUO=1 才判 fenghuo）
		dec.FenghuoEnabled = cfg.FenghuoEnabled
		dec.FenghuoMinLevel = cfg.FenghuoMinLevel
		dec.FenghuoKey = cfg.FenghuoKey
	}
	return &Handler{
		Cfg:             cfg,
		St:              st,
		Store:           runStore,
		Ctrl:            c,
		Log:             log,
		Intents:         intent.NewPlan(dec),
		posBatch:        map[string]map[string]any{},
		teams:           map[string]*TeamInfo{},
		teamJobs:        map[string]*TeamInfo{},
		teamTokenLast:   map[string]map[string]any{},
		teamMemberState: map[string]map[string]any{},
	}
}

// SetBroadcast 注入 WS 广播函数（api 层创建 Hub 后调用）。
// SetReghoster 注入"卡死自动重登恢复"入口（main 装配；nil = 不自动恢复）。
func (h *Handler) SetReghoster(fn func(account, reason string)) { h.reghoster = fn }

// SetRestartAutoAdd 注入"机器人重启后自动补一次批量上线"回调（main 装配；nil = 不自动补）。
func (h *Handler) SetRestartAutoAdd(fn func(cleared []string)) { h.restartAutoAdd = fn }

// SetLevelSync 注入"心跳有效等级回写账号池"回调（main 装配；nil = 不回写）。
func (h *Handler) SetLevelSync(fn func(account, zone string, level int)) { h.levelSync = fn }

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

// ---------------------------------------------------------------- 组队台账（2026-09-29 阶段 1）

// TeamInfo 队伍台账（阶段 1：内存态；落盘/编排避让在阶段 3）。
//
// 数据来源（机器人端 team_captain.py 契约，2026-09-29 核实）：
//   - POST /api/team/setup 成功后登记 job（state=pending，队长账号为键）；
//   - 机器人端 team_ready 事件（role=captain，members=已确认队员 role_id）→ 转 ready；
//   - team_disbanded / team_rejoined / team_return_nav 事件：更新/记录。
//
// 注意：机器人端队伍状态是内存态（无服务端全量查询协议，重启失忆），本台账只用于
// 展示与手动试点核对，阶段 1 不参与编排判据（编排避让在阶段 3）。
type TeamInfo struct {
	Captain       string   `json:"captain"`
	Members       []string `json:"members"`                   // 队员账号（不含队长）
	MemberRoleIDs []int    `json:"member_role_ids,omitempty"` // team_ready 上报的已确认队员 role_id
	Mode          string   `json:"mode"`                      // invite（队长邀请）/ apply（队员申请）
	NextAction    string   `json:"next_action,omitempty"`     // 建队后动作（阶段 1 试点不带；阶段 2 才用）
	State         string   `json:"state"`                     // pending（已下发未就绪）/ ready（队伍就绪）
	Since         int64    `json:"since"`                     // 登记时间（unix 秒）
	UpdatedAt     int64    `json:"updated_at"`
}

// TeamJobSet 登记一次组队任务（POST /api/team/setup 下发成功后调用；API 线程）。
func (h *Handler) TeamJobSet(captain string, members []string, mode, nextAction string) {
	now := time.Now().Unix()
	info := &TeamInfo{Captain: captain, Members: append([]string{}, members...), Mode: mode,
		NextAction: nextAction, State: "pending", Since: now, UpdatedAt: now}
	h.teamMu.Lock()
	if h.teamJobs == nil {
		h.teamJobs = map[string]*TeamInfo{}
	}
	h.teamJobs[captain] = info
	h.teamMu.Unlock()
}

// TeamMarkReady 队长 team_ready：job（pending）转正式队伍（ready）。
// 无登记（如手工路径）也建最小台账（成员账号缺失，仅 role_id）。
func (h *Handler) TeamMarkReady(captain string, memberRoleIDs []int) {
	now := time.Now().Unix()
	h.teamMu.Lock()
	defer h.teamMu.Unlock()
	if h.teams == nil {
		h.teams = map[string]*TeamInfo{}
	}
	info := h.teams[captain]
	if info == nil {
		info = h.teamJobs[captain]
	}
	if info == nil {
		info = &TeamInfo{Captain: captain, Members: []string{}, Since: now}
	}
	info.MemberRoleIDs = append([]int{}, memberRoleIDs...)
	info.State = "ready"
	info.UpdatedAt = now
	h.teams[captain] = info
	delete(h.teamJobs, captain)
}

// TeamNoteLeave 处理"某人离队/解散"（team_disbanded 事件）：
// 队长 → 整队（含 pending job）移除；队员 → 从所属队伍成员列表移除。
// 返回 (被移除的队长账号, 队员所属的队长账号)，二者最多一个非空。
func (h *Handler) TeamNoteLeave(account string) (removedTeam, memberOf string) {
	h.teamMu.Lock()
	defer h.teamMu.Unlock()
	if _, ok := h.teams[account]; ok {
		delete(h.teams, account)
		delete(h.teamJobs, account)
		return account, ""
	}
	if _, ok := h.teamJobs[account]; ok {
		delete(h.teamJobs, account)
		return account, ""
	}
	for cap, info := range h.teams {
		for i, m := range info.Members {
			if m != account {
				continue
			}
			rest := append([]string{}, info.Members[:i]...)
			info.Members = append(rest, info.Members[i+1:]...)
			info.UpdatedAt = time.Now().Unix()
			return "", cap
		}
	}
	return "", ""
}

// TeamRemove 按账号集合移除涉及队伍（账号可能是队长或队员），返回移除的队长账号（升序）。
// 供 /api/team/disband 清理台账用（注意与 TeamNoteLeave 的"单事件"语义区分）。
func (h *Handler) TeamRemove(accounts []string) []string {
	set := make(map[string]bool, len(accounts))
	for _, a := range accounts {
		if a != "" {
			set[a] = true
		}
	}
	h.teamMu.Lock()
	defer h.teamMu.Unlock()
	var caps []string
	for cap, info := range h.teams {
		if teamTouched(cap, info, set) {
			caps = append(caps, cap)
			delete(h.teams, cap)
		}
	}
	for cap, info := range h.teamJobs {
		if teamTouched(cap, info, set) {
			caps = append(caps, cap)
			delete(h.teamJobs, cap)
		}
	}
	sort.Strings(caps)
	return caps
}

// TeamLedger 台账快照（HTTP 只读用）：已就绪队伍 + 待就绪任务，均按队长账号排序。
func (h *Handler) TeamLedger() (teams []TeamInfo, jobs []TeamInfo) {
	h.teamMu.Lock()
	defer h.teamMu.Unlock()
	teams = make([]TeamInfo, 0, len(h.teams))
	for _, info := range h.teams {
		teams = append(teams, cloneTeam(info))
	}
	jobs = make([]TeamInfo, 0, len(h.teamJobs))
	for _, info := range h.teamJobs {
		jobs = append(jobs, cloneTeam(info))
	}
	sort.Slice(teams, func(i, j int) bool { return teams[i].Captain < teams[j].Captain })
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].Captain < jobs[j].Captain })
	return teams, jobs
}

func teamTouched(captain string, info *TeamInfo, set map[string]bool) bool {
	if set[captain] {
		return true
	}
	for _, m := range info.Members {
		if set[m] {
			return true
		}
	}
	return false
}

func cloneTeam(t *TeamInfo) TeamInfo {
	out := *t
	out.Members = append([]string{}, t.Members...)
	out.MemberRoleIDs = append([]int{}, t.MemberRoleIDs...)
	return out
}

// TeamRoleOf 该账号在中控组队台账里的角色（阶段 2 派发避让的唯一事实源）：
//
//	("captain", captain, true)  队长（含已下发未就绪的 pending job——就绪前同样不能再派单人任务）
//	("member",  captain, true)  队员
//	("", "", false)             不在台账（含已解散/未组队）
//
// 说明：心跳 team 块（机器人侧事实）由 api 层在台账未命中时兜底（见 api.teamRoleOf），
// 两侧取并集；本函数保持"只读台账"，可在任意场景安全调用。
func (h *Handler) TeamRoleOf(account string) (role, captain string, ok bool) {
	if account == "" {
		return "", "", false
	}
	h.teamMu.Lock()
	defer h.teamMu.Unlock()
	for cap, info := range h.teams {
		if cap == account {
			return "captain", cap, true
		}
		for _, m := range info.Members {
			if m == account {
				return "member", cap, true
			}
		}
	}
	for cap, info := range h.teamJobs {
		if cap == account {
			return "captain", cap, true
		}
		for _, m := range info.Members {
			if m == account {
				return "member", cap, true
			}
		}
	}
	return "", "", false
}

// TeamPromote 队长转移（S2C_TEAM_PROMOTE 90393 / D13）：newCaptain 从队员升为队长，旧队长转队员。
//
// 幂等：newCaptain 已是某队队长 → 返回 ""（不动作）。找不到所属队 → 返回 ""（调用方记日志；
// 可能是台账缺失的手工路径——等 team_ready 重建）。
// 迁移后 MemberRoleIDs 置空（旧队长的 rid 未知，等下一次 team_ready 重建；不影响派发避让——
// 避让按账号而不是 rid）。
func (h *Handler) TeamPromote(newCaptain string) (oldCaptain string) {
	if newCaptain == "" {
		return ""
	}
	h.teamMu.Lock()
	defer h.teamMu.Unlock()
	if _, ok := h.teams[newCaptain]; ok {
		return "" // 已是队长：幂等
	}
	now := time.Now().Unix()
	migrate := func(cap string, info *TeamInfo) string {
		idx := -1
		for i, m := range info.Members {
			if m == newCaptain {
				idx = i
				break
			}
		}
		if idx < 0 {
			return ""
		}
		members := append([]string{}, info.Members[:idx]...)
		members = append(members, info.Members[idx+1:]...)
		members = append(members, cap) // 旧队长转队员
		info.Captain = newCaptain
		info.Members = members
		info.MemberRoleIDs = nil
		info.UpdatedAt = now
		return cap
	}
	for cap, info := range h.teams {
		if old := migrate(cap, info); old != "" {
			delete(h.teams, cap)
			h.teams[newCaptain] = info
			return old
		}
	}
	for cap, info := range h.teamJobs {
		if old := migrate(cap, info); old != "" {
			delete(h.teamJobs, cap)
			h.teamJobs[newCaptain] = info
			return old
		}
	}
	return ""
}

// intListOf 把事件里的数组（[]any 数字，如 team_ready.members=队员 role_id 列表）转成 []int。
func intListOf(v any) []int {
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]int, 0, len(raw))
	for _, it := range raw {
		switch t := it.(type) {
		case float64:
			out = append(out, int(t))
		case int:
			out = append(out, t)
		}
	}
	return out
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
		// 组队（2026-09-29 阶段 1：协议打通 —— 只记台账/日志，不参与编排）
		"team_ready":        h.onTeamReady,
		"team_disbanded":    h.onTeamDisbanded,
		"team_rejoined":     h.onTeamRejoined,
		"team_return_nav":   h.onTeamReturnNav,
		"team_promoted":     h.onTeamPromote,     // 机器人端定稿事件名（S2C_TEAM_PROMOTE 90393）
		"team_promote":      h.onTeamPromote,     // 兼容早期命名（计划文档/手工路径）
		"team_token":        h.onTeamToken,       // 助战令链结果（阶段 2 机器人端新增）
		"team_member_state": h.onTeamMemberState, // 队员态（暂离/归队/就绪）
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

// onHello 机器人握手（type=hello，进程启动/重连一次）与 pong（心跳应答，周期）。
//
// 2026-09-23 自愈修复：机器人进程重启 ⇒ 它身上的号全掉了，但中控旧状态仍是 online →
// 水位器/自动任务判定"达标"而不补号（生产实测：重启后 230 个号没回来、游戏里看不见，
// 只能人工批量 add 恢复）。因此**收到 hello（仅进程握手；pong 不触发）**时，把该区
// 在线号标记离线，交由水位器/自动任务重新推号。
//
// 2026-09-29 重登停滞事故（A+C）：原实现只清 Online/HS —— 残留 State=FIGHT/游荡/抓鬼等在忙
// 字段 → 水位补号候选被 `Busy: hasLive && Busy(r)` 全部滤掉（544→77，池空）。
// 现在：①（A）标记离线时同步**清运行时忙态**（state.ClearRuntimeBusyOnRestart，边界见其注释）；
// ②（C）把刚清出的账号列表交给注入的"重启自动补一次批量上线"回调（壳层实现 + 开关/节流）。
func (h *Handler) onHello(ev map[string]any) {
	evType := str(ev, "type")
	h.Log.Printf("[EVENT] 机器人握手 %s version=%v pid=%v zone=%s",
		evType, ev["robot_version"], ev["pid"], zoneOf(ev))
	if evType != "hello" {
		return // pong 等周期心跳不触发
	}
	zone := zoneOf(ev)
	cleared := 0
	clearedAccs := make([]string, 0, 64)
	for _, acc := range h.St.Accounts() {
		r, ok := h.St.Get(acc)
		if !ok || !r.Online {
			continue
		}
		if zone != "" && r.Zone != "" && r.Zone != zone {
			continue // 其它区的号不动（多区部署）
		}
		h.St.Update(acc, func(rr *state.Robot) {
			rr.ClearRuntimeBusyOnRestart() // A：离线 + 清运行时忙态（只清忙态，满额/意图/台账不动）
		})
		cleared++
		clearedAccs = append(clearedAccs, acc)
	}
	if cleared > 0 {
		h.Log.Printf("[EVENT] 机器人重启握手(hello)：已把 %d 个号标记离线（含清运行时忙态）→ 水位器/自动任务将重新推号", cleared)
	}
	// C：兜底防呆 —— 机器人重启成功路径自动补一次批量上线（壳层注入；开关/节流/幂等由壳层控制）
	if h.restartAutoAdd != nil && len(clearedAccs) > 0 {
		h.restartAutoAdd(clearedAccs)
	}
	// 2026-09-23 技能策略配置：机器人（重）连上即补发一份（重启不丢）
	h.PushSkillConfig()
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
//
// 2026-09-23 分享日常：附该号心跳 daily 块（判据 ≥40 且今日未满 → 大唐神捕）；
// 2026-09-24 烽火大唐同款（同一次判定里带两个玩法的信息，shenbu 优先）；
// 老版机器人没有 daily → "未知" → 回落旧判据（≥31 全判抓鬼），行为不变。
func (h *Handler) decideIntent(account string, level int, chainDone bool, zone, source string) {
	if h.Intents == nil {
		return
	}
	dec := h.Intents.Decider().DecideDailyStates(level, chainDone, h.dailyInfoOf(account))
	// 2026-09-24（RESTORE 误补发事故）：**台账兜底** —— 机器人端分享日常无状态持久化，
	// 重登/进程重启后心跳 daily 丢失；单看心跳会把"当天在跑日常且未满"的号判回抓鬼
	//（现场 robot0001029：16:18:42 重登瞬间 fenghuo → ghost），随后 RESTORE/reghost 按
	// ghost 意图补发 ghost_start 把日常顶掉 → 点钟馗卡死 → 熔断（robot0005278 同款）。
	// 口径与 handlers.decideKind 的「续跑·台账」一致：台账命中且未见满额 → 维持该玩法意图；
	// 满额号照旧判抓鬼（名额让出来）。池停用时台账已被清（handleAutoTaskStop），不受影响。
	//
	// 2026-09-28 扩展（用户拍板"B 根治"：修"烽火抢光神捕候选"现场 5207/5256/5274）：
	//   判成 **fenghuo** 时同样走台账兜底 —— 因为"心跳只有烽火（宫廷10）"时
	//   DecideDailyStates 里 Shenbu.Known=false 会直接判 fenghuo，而"续跑·台账"的
	//   shenbu 台账明明在（今日已派未满）—— 原实现只兜 ghost, 于是"设计口径
	//   shenbu 优先（两个都未满先跑满神捕再转烽火）"在重登场景失效 → 神捕池
	//   running 永远 0（每 5 分钟重挑同一批，被烽火占着）。改判规则：
	//     · dec=ghost  → 台账命中的玩法直接接管（原逻辑不变）；
	//     · dec=fenghuo → 仅当台账命中 **shenbu** 时改判 shenbu（shenbu 优先）；
	//       台账命中 fenghuo 或均未命中 → 保持 fenghuo（心跳权威）。
	if dec.Known && (dec.Kind == intent.KindGhost || dec.Kind == intent.KindFenghuo) {
		if k, why, ok := h.dailyAssignedIntent(account); ok {
			if dec.Kind == intent.KindGhost || k == intent.KindShenbu {
				dec = intent.Decision{Known: true, Kind: k, Reason: why}
			}
		}
	}
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

// dailyAssignedIntent 台账兜底（2026-09-24）：该号今天"已派该日常且未见满额" → 维持该玩法
// 意图（按家族固定次序 shenbu → fenghuo 取第一个命中）。判据 = 中控持久台账
// （state.ShareDailyAssignedToday）+ 满额双闸（独立满额表 / 心跳 daily），与
// handlers.decideKind 的「续跑·台账」同款 —— 心跳丢失（重登/机器人进程重启）时意图不丢日常。
func (h *Handler) dailyAssignedIntent(account string) (intent.Kind, string, bool) {
	if h.St == nil || h.Intents == nil || account == "" {
		return "", "", false
	}
	r, ok := h.St.Get(account)
	if !ok {
		return "", "", false
	}
	dc := h.Intents.Decider()
	pairs := []struct {
		kind  intent.Kind
		key   string
		label string
	}{
		{intent.KindShenbu, dc.ShareDailyKeyOf(), "大唐神捕"},
		{intent.KindFenghuo, dc.FenghuoKeyOf(), "烽火大唐"},
	}
	for _, p := range pairs {
		if p.key == "" || !h.St.ShareDailyAssignedToday(account, p.key) {
			continue
		}
		if h.St.ShareDailyFullToday(account, p.key) || r.DailyFull(p.key) {
			continue // 满额：自由号 → 照旧判抓鬼（把该玩法名额让出来）
		}
		return p.kind, "今日" + p.label + "已派未满（续跑·台账）", true
	}
	return "", "", false
}

// dailyInfoOf 该号分享日常家族的判据输入（心跳 daily 块；没有 = 未知 → 不判该玩法）。
func (h *Handler) dailyInfoOf(account string) intent.DailyStates {
	if h.St == nil || h.Intents == nil {
		return intent.DailyStates{}
	}
	r, ok := h.St.Get(account)
	if !ok {
		return intent.DailyStates{}
	}
	dec := h.Intents.Decider()
	infoOf := func(key string) intent.DailyInfo {
		if key == "" {
			return intent.DailyInfo{}
		}
		if _, has := r.DailyOf(key); !has {
			return intent.DailyInfo{Known: false} // 老版上报/还没跑到：未知
		}
		return intent.DailyInfo{Known: true, Full: r.DailyFull(key)}
	}
	return intent.DailyStates{
		Shenbu:  infoOf(dec.ShareDailyKeyOf()),
		Fenghuo: infoOf(dec.FenghuoKeyOf()),
	}
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
		fullMarks := []string{}             // 满额玩法键（闭包内收集，闭包外打标：Update 持写锁，不可重入）
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
			// 货币详情（机器人端 90353 全量 / 90073 增量解析成 m_money/m_desposit/m_reserve，
			// 心跳携带）：money=银两 / deposit=钱庄存款 / reserve=储备金。MapView 选号列表用。
			if v, ok := st["money"]; ok {
				r.Money = int64(toInt(v))
			}
			if v, ok := st["deposit"]; ok {
				r.Deposit = int64(toInt(v))
			}
			if v, ok := st["reserve"]; ok {
				r.Reserve = int64(toInt(v))
			}
			// 开元通宝（2026-09-24 接入；机器人端 9727 COOKIE_KAIYUAN_MONEY → 心跳 kaiyuan）：
			// 与银两/存款/储备金同款透传，MapView 开元列显示用（缺失=0 → 前端显示"-"）。
			if v, ok := st["kaiyuan"]; ok {
				r.Kaiyuan = int64(toInt(v))
			}
			// 今日已领双倍经验日期（机器人 pre_daily 上报，YYYYMMDD；未领=空串）：
			// 心跳**全量覆盖** —— 机器人端以落盘状态为准，跨日会自然变空/换新日期。
			// 调度侧用 r.DoubleClaimedToday() 做"领双号优先抓鬼"（见 state.Robot 注释）。
			if v, ok := st["double_claim_date"]; ok {
				r.DoubleClaimDate = toStr(v)
			}
			if v, ok := st["role_id"]; ok {
				r.RoleID = toInt(v)
			}
			if v, ok := st["ghost"]; ok {
				if m, ok := v.(map[string]any); ok {
					r.Ghost = m
				}
			}
			if v, ok := st["fight_stats"]; ok {
				if m, ok := v.(map[string]any); ok {
					r.FightStats = m
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
			// 分享日常进度（2026-09-23 契约，原样透传）：{share_key,done,limit,state} 或数组；
			// 判据（intent.DecideDaily）与 /api/daily/overview 用 DailyOf/DailyFull 解析，
			// 形状容错；老版机器人不带 → 保持 nil（判据侧按"未知"保守处理）。
			if v, ok := st["daily"]; ok {
				r.Daily = v
				// 满额/不可用 → 独立表打标（跨日失效）：机器人端满额后会 request_stop，
				// 心跳 daily 随之变 None（client.py 只在 enabled=true 时上报）——不记的话
				// 候选会反复派、被 done_limit 拒（还会打断该号已转的游荡/抓鬼）。
				// ⚠️ 先收集、**闭包外**再落表：Update 持写锁，Mark 也要写锁（不可重入）。
				for _, e := range r.DailyEntries() {
					if state.DailyEntryFull(e) {
						fullMarks = append(fullMarks, e.ShareKey)
					}
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
			// 背包"包满"权威信号（2026-09-29 契约，与 pool-fix-bot 对齐口径）：
			// bag_full_age_ms = 最近一次"包裹满"类通知距今毫秒（不要求会话 enabled——
			// 因包满停止的号也要能报出）。**不带 = 未知/从未 → 清 0**（旧版机器人 / 机器人
			// 重启后不再报 → 不误拦）；消费方见 api.dailyBagTooFull（0/缺失一律不拦）。
			if v, ok := st["bag_full_age_ms"]; ok {
				r.BagFullAgeMS = int64(toInt(v))
			} else {
				r.BagFullAgeMS = 0
			}
			// 组队状态（2026-09-29 阶段 2 契约）：原样透传 {role,captain,members,parted,setup_done,token_use_ts?}；
			// 不带/为 null = 不在队 → 清残留（消费方：派发避让 api.dropTeamAccounts、状态接口）。
			if v, ok := st["team"]; ok && v != nil {
				r.Team = v
			} else {
				r.Team = nil
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
		// 2026-09-29 神捕闸门修复②：把"有效等级"（防抖后的 r.Level）回写账号池 zone-level ——
		// 池内等级原先只在建号/验证时写入，离线号候选/补拉会回落到陈旧值被等级闸淘汰。
		// 只回写 >0 的有效值；"是否变化/是否需要落盘"由壳层（api.SyncPoolLevel）判定。
		if h.levelSync != nil && level > 0 {
			h.levelSync(account, zone, level)
		}
		for _, k := range fullMarks { // 闭包外落表（见上：避免 Update 写锁重入）
			h.St.MarkShareDailyFull(account, k)
		}
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
		// 2026-09-23 分享日常「满额停止」= **正常收工**（不是故障）：机器人 __request_stop
		// 发 error 事件并带 done/limit（share_daily.py），这里落满额表后直接返回 ——
		// 不计错误、不进"任务失败待处理"（否则每天满额一次会让 ErrRepeat 逐日累加，
		// 第 3 天该号就被"同错 ≥3"判成卡住、不再自动派）。
		// 为什么不靠心跳：满额后 ~2s 就停（心跳周期 3s），最后一条带 daily 的心跳常停在
		// done=9/10，之后直接 None —— 事件流才是可靠信号（心跳路径保留作双保险）。
		if strings.EqualFold(code, "SHARE_DAILY_DAILY_LIMIT") {
			h.markShareDailyFullFromStop(account, ev)
			h.Store.LogEvent(ev)
			return
		}
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
// markShareDailyFullFromStop 从"分享日常满额停止"事件落满额表（2026-09-23）。
//
// 事件形状（机器人 share_daily.py 的 __request_stop）：
//
//	{type:"error", code:"SHARE_DAILY_<CODE>", msg, state:"STOPPED", reason, done, limit}
//
// 只有 done ≥ limit 才标（其它 code 的停止走常规错误处理）。
//
// 玩法键优先取 `ev["share_key"]`（**2026-09-24 P1 起机器人端应在事件里带**，否则烽火大唐
// 满额会被错记到神捕账上 —— 见交付报告"待联调项"）；事件没带时回落**当前配置的神捕键**
// （P0 兼容口径：当时只有一条链，不带也能对）。
func (h *Handler) markShareDailyFullFromStop(account string, ev map[string]any) {
	done, limit := toInt(ev["done"]), toInt(ev["limit"])
	if account == "" || limit <= 0 || done < limit {
		return
	}
	key := strings.TrimSpace(toStr(ev["share_key"]))
	if key == "" {
		key = intent.DefaultShareDailyKey
		if h.Intents != nil {
			if k := h.Intents.Decider().ShareDailyKeyOf(); k != "" {
				key = k
			}
		}
	}
	h.St.MarkShareDailyFull(account, key)
	h.Log.Printf("[SHAREDAILY] %s 满额停止（%d/%d）→ 记入满额表（%s），今日不再自动派",
		account, done, limit, key)
}

func isStuckCode(code string) bool {
	switch code {
	case "GHOST_DIALOG_STUCK", "TASK_STUCK",
		// 2026-09-29 P0-4：分享日常（神捕/烽火）"交付连拒 13 次后停止"——机器人端
		// share_daily.py __request_stop(g,"HANDIN_STUCK") 发 code=SHARE_DAILY_HANDIN_STUCK
		//（SHARE_DAILY_ 前缀见 share_daily.py:3932）。旧白名单只认抓鬼码 → 该号不进
		// StuckCount、不触发 reghost → 交付被拒后**全天无任何恢复路径**（现场 robot0005054
		// 02:12 停止后空转 8 小时，见 docs/04-测试/分析-20260929-烽火大唐任务链.md §4.3）。
		//
		// 选择"收录进恢复链"（reghost→重登→按意图重派）而不是只做 ErrRepeat 封禁：
		// 该码根因（服务端不标可交/包满/催交付口径）修复前，重登+重派是唯一可达的自愈路径；
		// churn 面由既有三道闸兜住，不会变成无限重登循环：
		//   ① reghost 当日卡死 ≥ ChurnLimit(3) → 熔断到次日（reghost.go:156）；
		//   ② 熔断后 restorer 补发/候选/roampool 回收全部跳过（IsRestoreCapped）；
		//   ③ restorer 的跳过日志（P0-5）按号 10 分钟节流，可观测不刷屏。
		"SHARE_DAILY_HANDIN_STUCK":
		return true
	}
	return strings.HasPrefix(code, "STUCK_")
}

// ---------------------------------------------------------------- 组队事件（2026-09-29 阶段 1）

// onTeamReady 组队就绪事件（机器人端 team_captain.on_build_team / tick 超时确认触发）：
//   - role=captain：members 为已确认队员的 role_id 列表 → 台账 pending → ready；
//   - role=member/applicant：队员侧就绪（apply 模式下队长侧不产生 team_ready，属机器人端
//     现状局限，见 internal/api/team.go 注释），只记日志。
func (h *Handler) onTeamReady(ev map[string]any) {
	account := str(ev, "account")
	if account == "" {
		return
	}
	role := str(ev, "role")
	rids := intListOf(ev["members"])
	if role == "captain" {
		h.TeamMarkReady(account, rids)
		h.Store.LogEvent(map[string]any{"type": "log", "level": "info", "account": account,
			"zone": zoneOf(ev),
			"msg":  fmt.Sprintf("[组队] 队长就绪：%s（队员 %d 人 role_id=%v）", account, len(rids), rids)})
		return
	}
	h.Store.LogEvent(map[string]any{"type": "log", "level": "info", "account": account,
		"zone": zoneOf(ev), "msg": "[组队] 就绪（" + role + "），等队长侧 team_ready 落台账"})
}

// onTeamDisbanded 队伍解散/离队（S2C_CANCEL_TEAM）：队长 → 整队清台账；队员 → 从成员列表移除。
func (h *Handler) onTeamDisbanded(ev map[string]any) {
	account := str(ev, "account")
	if account == "" {
		return
	}
	removedTeam, memberOf := h.TeamNoteLeave(account)
	msg := "[组队] 收到解散/离队：" + account
	switch {
	case removedTeam != "":
		msg = "[组队] 队伍解散（队长 " + removedTeam + "）：台账已清（含未就绪任务）"
	case memberOf != "":
		msg = "[组队] 队员离队：" + account + "（队长 " + memberOf + " 的台账已更新；如需重组请人工核对）"
	default:
		msg += "（台账无记录）"
	}
	h.Store.LogEvent(map[string]any{"type": "log", "level": "warn", "account": account,
		"zone": zoneOf(ev), "msg": msg})
}

// onTeamRejoined 暂离归队成功（team_captain.on_member_come_back）：阶段 1 只记录。
func (h *Handler) onTeamRejoined(ev map[string]any) {
	h.Store.LogEvent(map[string]any{"type": "log", "level": "info", "account": str(ev, "account"),
		"zone": zoneOf(ev), "msg": "[组队] 暂离归队成功：" + str(ev, "account")})
}

// onTeamReturnNav 归队导航请求（team_captain.on_come_back_too_far；机器人端 route_walk 模块
// 缺失 → 导航分支空转，阶段 2 接通）：阶段 1 只记录目标坐标，便于试点观察暂离场景。
func (h *Handler) onTeamReturnNav(ev map[string]any) {
	target := ""
	if v, ok := ev["target"].([]any); ok && len(v) >= 3 {
		target = fmt.Sprintf("[%v,%v,%v]", v[0], v[1], v[2])
	}
	h.Store.LogEvent(map[string]any{"type": "log", "level": "info", "account": str(ev, "account"),
		"zone": zoneOf(ev),
		"msg":  "[组队] 归队导航请求" + target + "（route_walk 模块缺失，当前仅记录；阶段 2 接通）"})
}

// onTeamPromote 队长转移（S2C_TEAM_PROMOTE 90393 / D13，2026-09-29 阶段 2）：
// 事件形状（与 team-feature-plan 对齐）：{"type":"team_promote","new_captain_role_id":RID,
// "new_team_name":NAME[,"new_captain_account":ACC]}——每个收到 90393 的号各发一条（幂等）。
//
// 台账迁移：新队长（rid→账号反查，优先事件自带账号）由队员升为队长，旧队长转队员；
// 找不到队（手工路径/台账缺失）→ 仅记日志，等下一次 team_ready 重建。
func (h *Handler) onTeamPromote(ev map[string]any) {
	// 字段兼容（机器人端定稿 = new_captain:<rid>；早期提案 new_captain_role_id；手工可直带账号）
	newCap := str(ev, "new_captain_account")
	if newCap == "" {
		newCap = h.accountByRoleID(toInt(ev["new_captain_role_id"]))
	}
	if newCap == "" {
		newCap = h.accountByRoleID(toInt(ev["new_captain"]))
	}
	if newCap == "" {
		h.Store.LogEvent(map[string]any{"type": "log", "level": "warn", "account": str(ev, "account"),
			"zone": zoneOf(ev), "msg": fmt.Sprintf(
				"[组队] 队长转移事件：新队长无法映射账号（new_captain=%v，等心跳）→ 台账未动",
				ev["new_captain"])})
		return
	}
	old := h.TeamPromote(newCap)
	msg := fmt.Sprintf("[组队] 队长转移：%s → %s（队伍 %s）", old, newCap, str(ev, "new_team_name"))
	if old == "" {
		msg = "[组队] 队长转移事件：" + newCap + "（台账无匹配队，等 team_ready 重建）"
	}
	h.Store.LogEvent(map[string]any{"type": "log", "level": "info", "account": str(ev, "account"),
		"zone": zoneOf(ev), "msg": msg})
}

// accountByRoleID 心跳 RoleID → 账号反查（角色 id 全服唯一；找不到返回 ""）。
// 事件低频（仅建队/转移），全量扫描可接受。
func (h *Handler) accountByRoleID(rid int) string {
	if rid <= 0 || h.St == nil {
		return ""
	}
	for _, r := range h.St.Snapshot() {
		if r.RoleID == rid {
			return r.Account
		}
	}
	return ""
}

// onTeamToken 助战令链结果（阶段 2 机器人端事件，team-feature-plan 定稿）：
// {"type":"team_token","account":<队长>,"ok":true|false,"reason":"READY|NO_TOKEN|NO_MONEY|BUY_FAIL",
// "count":N,"reserve":M,"use_ts":ms,"use_count":N}（失败 60s 节流）。
//
// ok:true = 队长已"有令且已使用"（免费到手也算）→ 可以接任务；ok:false = D13 判据（阶段 3
// 编排：先 team_promote 换队长，不可行再 disband）。本阶段只入台账/日志 + 供 /api/team/status 观测。
func (h *Handler) onTeamToken(ev map[string]any) {
	account := str(ev, "account")
	h.Store.LogEvent(ev)
	if account == "" {
		return
	}
	rec := map[string]any{
		"ok": toBool(ev["ok"]), "reason": str(ev, "reason"),
		"count": toInt(ev["count"]), "reserve": toInt(ev["reserve"]),
		"use_ts": toInt(ev["use_ts"]), "use_count": toInt(ev["use_count"]),
		"at": time.Now().Unix(),
	}
	h.teamMu.Lock()
	if h.teamTokenLast == nil {
		h.teamTokenLast = map[string]map[string]any{}
	}
	h.teamTokenLast[account] = rec
	h.teamMu.Unlock()
	level, msg := "info", "[组队] 助战令就绪："+account+"（可接任务）"
	if !toBool(ev["ok"]) {
		level = "warn"
		msg = fmt.Sprintf("[组队] 助战令不可用：%s（reason=%s，包内 %d，储备金 %d）→ 阶段 3 按 D13 处理（转队长/解散）",
			account, str(ev, "reason"), toInt(ev["count"]), toInt(ev["reserve"]))
	}
	h.Store.LogEvent(map[string]any{"type": "log", "level": level, "account": account,
		"zone": zoneOf(ev), "msg": msg})
}

// onTeamMemberState 队员态变化（暂离/归队/就绪；team-feature-plan 定稿）：
// {"type":"team_member_state","account":<队员>,"role":..,"parted":true|false,"setup_done":..}
// 本阶段：入台账（观测）+ 日志；编排联动（暂离告警/召唤）留阶段 3。
func (h *Handler) onTeamMemberState(ev map[string]any) {
	account := str(ev, "account")
	h.Store.LogEvent(ev)
	if account == "" {
		return
	}
	rec := map[string]any{
		"role": str(ev, "role"), "parted": toBool(ev["parted"]),
		"setup_done": toBool(ev["setup_done"]), "at": time.Now().Unix(),
	}
	h.teamMu.Lock()
	if h.teamMemberState == nil {
		h.teamMemberState = map[string]map[string]any{}
	}
	h.teamMemberState[account] = rec
	h.teamMu.Unlock()
	stateTxt := "活跃"
	if toBool(ev["parted"]) {
		stateTxt = "暂离"
	}
	h.Store.LogEvent(map[string]any{"type": "log", "level": "info", "account": account,
		"zone": zoneOf(ev), "msg": "[组队] 队员态：" + account + " " + stateTxt + "（" + str(ev, "role") + "）"})
}

// TeamTokenLast 最近一条 team_token（助战令链结果；无 → nil）。HTTP 只读用。
func (h *Handler) TeamTokenLast(account string) map[string]any {
	h.teamMu.Lock()
	defer h.teamMu.Unlock()
	if rec, ok := h.teamTokenLast[account]; ok {
		out := make(map[string]any, len(rec))
		for k, v := range rec {
			out[k] = v
		}
		return out
	}
	return nil
}

// TeamMemberStateLast 最近一条 team_member_state（队员态；无 → nil）。HTTP 只读用。
func (h *Handler) TeamMemberStateLast(account string) map[string]any {
	h.teamMu.Lock()
	defer h.teamMu.Unlock()
	if rec, ok := h.teamMemberState[account]; ok {
		out := make(map[string]any, len(rec))
		for k, v := range rec {
			out[k] = v
		}
		return out
	}
	return nil
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
