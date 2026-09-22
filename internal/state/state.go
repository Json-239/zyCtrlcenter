// Package state 全局状态容器（骨架层：只存中控运行期状态，不含业务编排状态）。
//
// 所有跨模块共享的可变状态收敛于此；路由/服务通过注入的 *State 访问。
// 业务状态（如定时编排/游荡池/任务意图）请在使用方模块内自行维护。
package state

import (
	"sort"
	"sync"
	"time"
)

// Robot 单个机器人状态（对应 status_reply/robot_state 上报字段）。
type Robot struct {
	Account string `json:"account"`
	// Zone 中控侧归属区（"<服key>/<区key>"，由控制通道在事件入队时打标）——命令路由依据。
	Zone string `json:"zone,omitempty"`
	// Server 机器人上报的游戏服地址（如 "47.96.8.240:2400"）。
	Server    string `json:"server,omitempty"`
	Online    bool   `json:"online"`
	State     string `json:"state"`
	TaskIndex int    `json:"task_index"`
	Done      int    `json:"done"`
	ChainDone bool   `json:"chain_done"`
	// GhostDoneToday 今天抓鬼已满/不可用（面板标记；跨日自动 false）。
	// 由 Snapshot() 从 ghostUnavail 表填入 —— 不存 robot 行：行会被 removeAccount
	// 删掉、标记会随之丢失（2026-09-22 踩过坑）。
	GhostDoneToday bool `json:"ghost_done_today,omitempty"`
	RoleName  string `json:"role_name,omitempty"`
	Level     int    `json:"level"`
	// LevelPending / LevelPendingN 等级"大幅回退"待确认（连续 LevelPendingN 次上报同一新值才真切换，
	// 见 event.applyLevel）：单次错值不覆盖已确认等级、不触发意图切换。
	// json:"-"：纯内部防抖状态，不进 status 接口/面板。
	LevelPending  int            `json:"-"`
	LevelPendingN int            `json:"-"`
	MapID         int            `json:"mapid"`
	Fight         bool           `json:"fight"`
	Pos           []int          `json:"pos,omitempty"`      // 服务端像素坐标（机器人上报原值）
	PosGrid       []int          `json:"pos_grid,omitempty"` // 客户端显示的格子坐标 = 像素 / GridCell(16)
	Fpp           int            `json:"fpp,omitempty"`
	RoleID        int            `json:"role_id,omitempty"`
	Ghost         map[string]any `json:"ghost,omitempty"`
	GhostTarget   []int          `json:"ghost_target,omitempty"`
	// Hatch 孵化会话（机器人上报原样透传）：{active,kind,egg_item,mapid,battles,hatched,reason,since_ms}。
	// 与 ghost 同口径：字段存在但 active=false 不算在孵化。
	Hatch map[string]any `json:"hatch,omitempty"`
	// GhostRequiredLevel 服务端要求的抓鬼等级（来自 ghost_offline.required_level）。
	// 低于它的号不再派抓鬼（等级当天不会变，所以不做当日失效）。
	GhostRequiredLevel int `json:"ghost_required_level,omitempty"`
	// StuckCount / StuckDay 当日"卡死"次数（GHOST_DIALOG_STUCK / TASK_STUCK，以及看门狗的
	// STUCK_<STATE> 一律计入，**不随进度清零**）：
	// 反复卡死又被重登拉起的号会 churn，达阈值当天不再派（跨日自动归零）。
	StuckCount int    `json:"stuck_count,omitempty"`
	StuckDay   string `json:"stuck_day,omitempty"`
	HP         []int  `json:"hp,omitempty"` // [当前, 上限]（机器人上报就是二元数组）
	MP         []int  `json:"mp,omitempty"` // [当前, 上限]
	Bag        any    `json:"bag,omitempty"`
	Equip      any    `json:"equip,omitempty"`
	Summons    any    `json:"summons,omitempty"`
	Booth      any    `json:"booth,omitempty"`
	// Walk 游荡状态（机器人上报原样透传）：{enabled,mapid,state}。
	// 2026-09-22 试跑实测：号刚被派去半月岛，恢复引擎就把它当"没任务"补发 ghost_start
	// （游荡内部会互相排斥地停掉抓鬼），游荡被打断 → 加这个字段让恢复/自动任务让路。
	Walk any `json:"walk,omitempty"`
	// SvrOnline 全服在线（含真实玩家）：机器人 `@online` 回执解析后上报，{count,ts}。
	// 只有"查到过"的那个号会带这个字段（整进程 2 分钟查一次），中控取最新一条展示。
	SvrOnline map[string]any `json:"svr_online,omitempty"`
	Route     any            `json:"route,omitempty"`
	HS        bool           `json:"hs"` // 游戏服握手完成（≠登录成功）
	// HasEgg 是否有坐骑蛋（装备栏五行珠槽在孵 or 背包里有）—— 孵化池过滤用；机器人上报。
	HasEgg    bool    `json:"has_egg,omitempty"`
	ErrCode   string  `json:"err_code,omitempty"`
	ErrTS     float64 `json:"err_ts,omitempty"`
	ErrRepeat int     `json:"err_repeat,omitempty"`
	ErrMsg    string  `json:"err_msg,omitempty"`
	LastTask  int     `json:"last_task,omitempty"`
	LastSeen  float64 `json:"last_seen"`
}

// GhostActive 是否有**活跃**抓鬼会话（机器人上报的 ghost.enabled=true；字段存在但已停
// 不算）。与前端 Dashboard「👻抓鬼」筛选用同一口径。老版上报缺 enabled 字段时按
// "有会话"保守处理（不误伤）。
func (r Robot) GhostActive() bool {
	if r.Ghost == nil {
		return false
	}
	if en, ok := r.Ghost["enabled"].(bool); ok {
		return en
	}
	return true
}

// HatchActive 是否有**活跃**孵化会话（机器人上报的 hatch.active=true 且未孵出）。
//
// 口径：
//   - 缺 active 字段 / active=false → 不算在孵化（等机器人下一次心跳补报，别按"有会话"乐观处理：
//     孵化是有时长的动作，误算会让保持数永远"已达标"、不再补号）；
//   - hatched=true（已孵出）→ **不算**在孵化（该号已收工，running 必须立刻减 1）。
func (r Robot) HatchActive() bool {
	if r.Hatch == nil {
		return false
	}
	if b, ok := r.Hatch["hatched"].(bool); ok && b {
		return false
	}
	en, ok := r.Hatch["active"].(bool)
	return ok && en
}

// Walking 是否正在"游荡"（或正在孵化 —— 孵化内部就是游荡到孵化图打暗雷）。
//
// 恢复引擎/自动任务据此**让路**：游荡与抓鬼在机器人端互斥（random_walk 启动时会
// 停掉抓鬼），若恢复引擎按"无抓鬼会话"补发 ghost_start，会把游荡号抢回抓鬼
// （2026-09-22 半月岛试跑实测：2 个号刚下发挥荡就被补发打断）。
func (r Robot) Walking() bool {
	if r.HatchActive() {
		return true
	}
	m, ok := r.Walk.(map[string]any)
	if !ok {
		return false
	}
	en, ok := m["enabled"].(bool)
	return ok && en
}

// State 全局状态（并发安全）。
type State struct {
	// ghostUnavail 账号 → "今天抓鬼不可用/已满"的日期串（跨日自动失效）。
	//   独立于 robots 行：行会被 removeAccount 删掉，标记必须留着（否则会被反复拉起）。
	ghostUnavail map[string]string

	mu sync.RWMutex

	robots  map[string]*Robot
	removed map[string]bool
	curServ map[string]string // zone -> "host:port"（当前单区，key="default"）

	connConnected bool
	connAddr      string
}

// New 创建空状态。
func New() *State {
	return &State{
		robots:  make(map[string]*Robot),
		removed: make(map[string]bool),
		curServ: make(map[string]string),
	}
}

// ---------------------------------------------------------------- robots

// Update 对指定账号执行更新（不存在则创建）；fn 在锁内执行。
// MarkGhostDoneToday 记下"该号今天抓鬼不可用/已满额"。
//
// ⚠️ 必须存在**独立的表**里（不能写 robot 行）—— 调用方紧接着就 `removeAccount`
// 把行删掉了，写在行上的标记会随之丢失（2026-09-22 踩过：标记数一直是 0）。
func (s *State) MarkGhostDoneToday(account string) {
	if account == "" {
		return
	}
	day := time.Now().Format("20060102")
	s.mu.Lock()
	if s.ghostUnavail == nil {
		s.ghostUnavail = map[string]string{}
	}
	s.ghostUnavail[account] = day
	s.mu.Unlock()
}

// GhostDoneToday 该号今天是否已抓满/不可用（跨日自动 false）。
func (s *State) GhostDoneToday(account string) bool {
	s.mu.RLock()
	day, ok := s.ghostUnavail[account]
	s.mu.RUnlock()
	return ok && day == time.Now().Format("20060102")
}

// GhostUnavailableTodayCount 今天已标"抓鬼不可用"的号数（面板展示/排查用）。
func (s *State) GhostUnavailableTodayCount() int {
	today := time.Now().Format("20060102")
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, d := range s.ghostUnavail {
		if d == today {
			n++
		}
	}
	return n
}

func (s *State) Update(account string, fn func(r *Robot)) *Robot {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.robots[account]
	if !ok {
		r = &Robot{Account: account}
		s.robots[account] = r
	}
	if fn != nil {
		fn(r)
	}
	return r
}

// Get 读取机器人状态副本。
func (s *State) Get(account string) (Robot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.robots[account]
	if !ok {
		return Robot{}, false
	}
	return *r, true
}

// Has 是否存在该账号行。
func (s *State) Has(account string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.robots[account]
	return ok
}

// Remove 删除账号行。
func (s *State) Remove(account string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.robots, account)
}

// Snapshot 返回全部机器人的稳定排序副本（按账号升序）。
func (s *State) Snapshot() []Robot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	today := time.Now().Format("20060102")
	out := make([]Robot, 0, len(s.robots))
	for _, r := range s.robots {
		cp := *r
		// 面板标记：今天抓鬼已满/不可用（同一把锁内查 ghostUnavail，不二次加锁）
		cp.GhostDoneToday = s.ghostUnavail[cp.Account] == today
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Account < out[j].Account })
	return out
}

// Accounts 返回全部账号名（升序）。
func (s *State) Accounts() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.robots))
	for a := range s.robots {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// Counts 返回 (在线数, 握手数, 总数)。
func (s *State) Counts() (online, handshake, total int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.robots {
		total++
		if r.Online {
			online++
		}
		if r.HS {
			handshake++
		}
	}
	return
}

// SvrOnlineLatest 返回最近一次"全服在线(含真人)"读数与时间戳(ms)。
//
// 数据来源：机器人定时发 `@online`（服务端 DEPLOY 权限即可）→ 回执里带在线角色数
// （role_manager 全部在线角色 = 真人 + 我们的号）→ 机器人上报 svr_online{count,ts}。
// 多个号里取 ts 最新的一条（只有查到过的那一个号带该字段）。ok=false 表示还没有读数。
func (s *State) SvrOnlineLatest() (count int, tsMS float64, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.robots {
		if r.SvrOnline == nil {
			continue
		}
		c := numToInt(r.SvrOnline["count"])
		t := numToFloat(r.SvrOnline["ts"])
		if c <= 0 || t <= 0 {
			continue
		}
		if t > tsMS {
			count, tsMS, ok = c, t, true
		}
	}
	return
}

// numToInt / numToFloat：JSON 解出来的数字是 float64，这里做宽松转换（失败返回 0）。
func numToInt(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case float32:
		return int(x)
	case int:
		return x
	case int64:
		return int(x)
	case uint64:
		return int(x)
	}
	return 0
}

func numToFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case uint64:
		return float64(x)
	}
	return 0
}

// StaleAccounts 返回超过 ttl 秒未心跳的账号（清理陈旧行用）。
func (s *State) StaleAccounts(now float64, ttl float64) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []string
	for a, r := range s.robots {
		if now-r.LastSeen > ttl {
			out = append(out, a)
		}
	}
	sort.Strings(out)
	return out
}

// MarkTaskError 记「最近任务错误 + 重复次数」；同码累计、换码重计，返回最新重复次数。
func (s *State) MarkTaskError(account, code, msg string) int {
	if account == "" || code == "" {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.robots[account]
	if !ok {
		r = &Robot{Account: account}
		s.robots[account] = r
	}
	if r.ErrCode == code {
		r.ErrRepeat++
	} else {
		r.ErrRepeat = 1
	}
	r.ErrCode = code
	r.ErrTS = float64(time.Now().Unix())
	if len(msg) > 120 {
		msg = msg[:120]
	}
	r.ErrMsg = msg
	return r.ErrRepeat
}

// FailedTasks 反复失败（>= minRepeat 次）的任务清单（面板「任务失败待处理」）。
func (s *State) FailedTasks(minRepeat, limit int) map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now().Unix()
	items := make([]map[string]any, 0)
	for a, r := range s.robots {
		if r.ErrRepeat < minRepeat {
			continue
		}
		age := int64(0)
		if now > int64(r.ErrTS) {
			age = now - int64(r.ErrTS)
		}
		items = append(items, map[string]any{
			"account": a, "code": r.ErrCode, "repeat": r.ErrRepeat,
			"age": age, "msg": r.ErrMsg,
		})
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i]["repeat"].(int) > items[j]["repeat"].(int)
	})
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	return map[string]any{"count": len(items), "items": items}
}

// ---------------------------------------------------------------- removed

// MarkRemoved 标记账号已移除（心跳不再复活）。
func (s *State) MarkRemoved(account string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removed[account] = true
}

// UnmarkRemoved 取消已移除标记。
func (s *State) UnmarkRemoved(account string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.removed, account)
}

// IsRemoved 是否已标记移除。
func (s *State) IsRemoved(account string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.removed[account]
}

// RemovedList 返回已移除账号列表（升序）。
func (s *State) RemovedList() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.removed))
	for a := range s.removed {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// ClearRemoved 清空"已移除"名单，返回被清掉的账号（升序）。
//
// 2026-09-22 生产背景：AutoRemoveOnDone 原先把"抓鬼满额/下线换号"的号也永久移除，
// 一天累积 461 个（当前区可用号 544 个里 83% 被排除）→ 水位器候选池为空
// （"号池里没有可上线的号"）→ 在线数卡在 83 上不到 200。这里提供一键恢复：
// 清空后候选池立即恢复，水位器/池子会在下一轮（60s 内）按需补号。
func (s *State) ClearRemoved() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.removed))
	for a := range s.removed {
		out = append(out, a)
	}
	s.removed = make(map[string]bool)
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------- 连接/服务器

// SetCurServer 记录某区机器人上报的游戏服地址（zone 为空时记到 "default"）。
func (s *State) SetCurServer(zone, server string) {
	if server == "" {
		return
	}
	if zone == "" {
		zone = "default"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.curServ[zone] = server
}

// CurServer 返回某区最近上报的游戏服地址（空区 / 无记录返回 "--"）。
func (s *State) CurServer(zone string) string {
	if zone == "" {
		zone = "default"
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if v := s.curServ[zone]; v != "" {
		return v
	}
	return "--"
}

// CurServers 返回全部区的游戏服地址快照（zone -> "host:port"）。
func (s *State) CurServers() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]string, len(s.curServ))
	for k, v := range s.curServ {
		out[k] = v
	}
	return out
}

// ZoneCounts 按区统计 (在线, 握手, 总数)。
func (s *State) ZoneCounts() map[string]map[string]int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]map[string]int{}
	for _, r := range s.robots {
		z := r.Zone
		if z == "" {
			z = "default"
		}
		c, ok := out[z]
		if !ok {
			c = map[string]int{"total": 0, "online": 0, "handshake": 0}
			out[z] = c
		}
		c["total"]++
		if r.Online {
			c["online"]++
		}
		if r.HS {
			c["handshake"]++
		}
	}
	return out
}

// ZoneOfAccounts 返回这些账号各自所属区（未知账号不出现在结果里）。
func (s *State) ZoneOfAccounts(accounts []string) map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]string, len(accounts))
	for _, a := range accounts {
		if r, ok := s.robots[a]; ok && r.Zone != "" {
			out[a] = r.Zone
		}
	}
	return out
}
