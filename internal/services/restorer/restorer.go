// Package restorer 恢复引擎（P1）：按**意图**补发命令，让账号一直跑在该跑的链上。
//
// 为什么需要：中控重启、机器人重连、任务丢了之后，谁都不记得"这个号原来在跑什么"。
// intent 表记了意图，这里负责把它变成命令（带冷却/重试上限/熔断，别对着卡死的号猛补）。
//
// 口径对齐参考实现 robot/ctrlcenter/services/intent_restore.py：
//
//	RESTORE_DELAY_SEC=8（启动后延迟） / READD_RETRY_SEC=300（失败重试）
//	REPULL_ERR_REPEAT=3（同错 3 次）→ 封 600~1800s（这里取 CircuitSec=1800）
//	补拉判据（idle_wander.repull_task_for）：**在线 + task_index==0 + 没在跑**才补
//
// 安全默认：Enabled 默认 false（会真给机器人发命令，得显式打开）。
// Tick 是纯决策（可用假时钟反复测），Run 才真正下发。
package restorer

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"zyctrlcenter/internal/chainlib"
	"zyctrlcenter/internal/services/intent"
	"zyctrlcenter/internal/state"
)

// 默认参数（可用 Deps 覆盖；与参考实现同口径）。
const (
	DefaultRetrySec     = 300  // 失败后多久再试
	DefaultMaxAttempts  = 3    // 连续尝试上限，超过熔断
	DefaultCircuitSec   = 1800 // 熔断时长
	DefaultInterval     = 5 * time.Second
	ErrRepeatToCircuit  = 3 // 同一错误重复这么多次就熔断，等人工
	RestoreDelayDefault = 8 * time.Second
	// DefaultMaxPerRound 单轮最多补发几个号（2026-09-23 P0）：批量上线瞬间"几百个号
	// 都没在跑"，一轮全补会直接把池子灌爆（生产实测一轮补过 87 个，抓鬼超编到 218）。
	// 与水位/游荡池的单轮 max_step=5 同口径。
	DefaultMaxPerRound = 5
)

// Action 一次补发决策（由 Run 负责下发，Tick 只产出）。
type Action struct {
	Account string    `json:"account"`
	Kind    string    `json:"kind"`
	Command string    `json:"command"` // start_chain | ghost_start
	ChainID string    `json:"chain_id,omitempty"`
	Reason  string    `json:"reason,omitempty"`
	At      time.Time `json:"at"`
}

// Group 一次**实际下发**（同命令 + 同链的账号合并成一条，见 GroupActions）。
type Group struct {
	Kind     string   `json:"kind"`
	Command  string   `json:"command"`
	ChainID  string   `json:"chain_id,omitempty"`
	Accounts []string `json:"accounts"`
	Reason   string   `json:"reason,omitempty"` // 取组内第一条决策的理由（日志用）
}

// GroupActions 把补发决策按（命令, 链）合并成下发批次（账号保持排序、去重）。
//
// 为什么必须合并：抓鬼载荷是 2MB 级导航数据，一个账号一条命令 = N×2MB 灌进同一条控制通道
// （机器人是一个进程多账号）。合并后一批只带一份载荷。
func GroupActions(acts []Action) []Group {
	idx := map[string]int{}
	out := make([]Group, 0, len(acts))
	for _, a := range acts { // acts 已按账号排序 → 组内顺序稳定
		key := a.Command + "\x00" + a.ChainID
		i, ok := idx[key]
		if !ok {
			idx[key] = len(out)
			out = append(out, Group{Kind: a.Kind, Command: a.Command, ChainID: a.ChainID,
				Accounts: []string{a.Account}, Reason: a.Reason})
			continue
		}
		dup := false
		for _, acc := range out[i].Accounts {
			if acc == a.Account {
				dup = true
				break
			}
		}
		if !dup {
			out[i].Accounts = append(out[i].Accounts, a.Account)
		}
	}
	return out
}

// Status 某账号的恢复状态（面板展示"在补哪几个、卡在哪"）。
type Status struct {
	Account   string    `json:"account"`
	Attempts  int       `json:"attempts"`
	NextAt    time.Time `json:"next_at,omitempty"`
	LastMsg   string    `json:"last_msg,omitempty"`
	Blocked   bool      `json:"blocked"`
	BlockTill time.Time `json:"block_till,omitempty"`
	// LastDispatchAt 最近一次"决定补发"的时间（2026-09-23 P0）：壳层的池闸据此把
	// "刚补发、还没跑起来"的号算作在途占位（TTL 见 api 侧 ghostInflightTTL），
	// 避免"上一批还在路上，闸门又放行下一批"。
	LastDispatchAt time.Time `json:"last_dispatch_at,omitempty"`
}

// Deps 依赖（除 Send 外都是只读读取，便于测试注入假时钟/假状态）。
type Deps struct {
	Enabled   func() bool            // 总开关（配置项；默认关，避免误发命令）
	ChannelUp func() bool            // 机器人通道是否连着
	Intents   func() []intent.Intent // 已登记意图（intent.Plan.Snapshot，已按账号排序）
	Robot     func(account string) (state.Robot, bool)
	ErrRepeat func(account string) int
	Now       func() time.Time
	Send      func(cmd map[string]any, action string) bool // Run 用；Tick 不用
	Log       func(format string, args ...any)
	// Payload 按意图 kind 补充"要随命令下发的载荷"（如抓鬼导航数据）；nil = 不带载荷（老行为）。
	// 取不到就返回 error —— Tick 会据此**跳过补发**（熔断等人工），不会发空命令。
	// 注意：壳层（api）负责缓存（2MB 级文件不能每 5 秒重读）。
	Payload func(kind, chainID string) (any, error)
	// Skip 派发前最后一道闸（可为 nil）：返回 (true, 原因) 就不补发（如抓鬼等级门槛）。
	Skip func(kind, account string) (bool, string)
	// GhostDailyLimit 抓鬼每日上限（nil → 50）：随 ghost_start 一起补发，与「启动」路径同口径
	// （参考实现 intent_restore 也是 role + limit + chain）。
	GhostDailyLimit func() int

	RetrySec    int
	MaxAttempts int
	CircuitSec  int
	Interval    time.Duration
	// Delay 启动后延迟多久才开始补（参考实现 RESTORE_DELAY_SEC=8，等机器人把状态报上来）
	Delay time.Duration
	// MaxPerRound 单轮最多补发几个号（0 → DefaultMaxPerRound）：见常量注释（2026-09-23 P0）。
	MaxPerRound int
}

// Runner 恢复器。
type Runner struct {
	d  Deps
	mu sync.Mutex
	st map[string]*Status
}

// New 创建恢复器（零值字段用默认值补齐）。
func New(d Deps) *Runner {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.RetrySec <= 0 {
		d.RetrySec = DefaultRetrySec
	}
	if d.MaxAttempts <= 0 {
		d.MaxAttempts = DefaultMaxAttempts
	}
	if d.CircuitSec <= 0 {
		d.CircuitSec = DefaultCircuitSec
	}
	if d.Interval <= 0 {
		d.Interval = DefaultInterval
	}
	if d.Delay <= 0 {
		d.Delay = RestoreDelayDefault
	}
	if d.MaxPerRound <= 0 {
		d.MaxPerRound = DefaultMaxPerRound
	}
	return &Runner{d: d, st: map[string]*Status{}}
}

// needsRestore 该不该补（kind = 该号意图，判据随 kind 收窄）。
//
// 基准判据（参考实现 idle_wander.repull_task_for）：在线 + task_index==0 + 没在跑。
//
// 2026-09-21 增补（生产：40+ 个号"重登/上线后卡 DIALOG 一动不动"）：
// 抓鬼号卡在 DIALOG **且没有活跃抓鬼会话**（ghost.enabled 非 true；**字段存在但已停的也算没有活跃会话**
// —— 2026-09-21 现场有 15 个 DIALOG 号是这种形态，旧判据漏补）时也算"没在跑"。服务端在
// 登录流程的"领双引导对话"点掉后会再推一个零选项空对话，机器人端从那一刻起不再
// 发任何动作（state=DIALOG 且抓鬼会话在重连时丢了）。旧判据把 DIALOG 一律当"在跑"，
// autotask 候选 / 保持数 / 恢复引擎三道兜底全都不管 → 号永久空转（实测 12+ 分钟）。
// 补发 ghost_start 后机器人端 30s 对话兜底会清掉对话态继续跑（重复下发幂等）；
// 新手链/捉鬼链不在此列（重发 start_chain 可能重置链进度，保持保守）。
func needsRestore(r state.Robot, kind intent.Kind) bool {
	// 2026-09-22: 正在游荡/孵化的号一律不补发 —— 游荡与抓鬼在机器人端互斥
	// （random_walk 启动会停掉抓鬼），补发 ghost_start 会把游荡号抢回抓鬼，
	// 半月岛试跑实测被抢断。游荡结束（机器人不再上报 walk.enabled）后自然恢复补发。
	if r.Walking() {
		return false
	}
	switch strings.ToUpper(strings.TrimSpace(r.State)) {
	case "IDLE", "", "ONLINE":
		return true
	case "WAIT_TASK":
		return r.TaskIndex == 0 // 有任务号说明链在推进，别打扰
	case "DIALOG":
		return kind == intent.KindGhost && !r.GhostActive() && r.TaskIndex == 0
	default:
		// NAV/CLICK/FIGHT/SHOP/ALLOC/WAIT_NEXT = 在跑；DONE = 跑完；ERROR = 卡住等人工
		return false
	}
}

// commandFor 意图 → 命令（与机器人端 dispatch_cmd 的命令名一致）。
func commandFor(it intent.Intent) (cmd string, chainID string) {
	switch it.Kind {
	case intent.KindGhost:
		return "ghost_start", ""
	case intent.KindZhuaogui:
		return "start_chain", "zhuaogui"
	case intent.KindNewbie:
		id := it.ChainID
		if id == "" {
			id = "newbie_full"
		}
		return "start_chain", id
	}
	return "", ""
}

// Tick 算一轮决策（不发送；更新冷却/熔断状态）。
func (r *Runner) Tick(now time.Time) []Action { return r.TickForce(now, false) }

// TickForce force=true 时忽略总开关（面板「立即补发」用手动触发；冷却/熔断仍然生效）。
func (r *Runner) TickForce(now time.Time, force bool) []Action {
	if !force && r.d.Enabled != nil && !r.d.Enabled() {
		return nil
	}
	if r.d.ChannelUp != nil && !r.d.ChannelUp() {
		return nil // 通道没连：补发也没人收
	}
	if r.d.Intents == nil || r.d.Robot == nil {
		return nil
	}
	items := r.d.Intents()
	out := make([]Action, 0, len(items))
	lim := r.d.MaxPerRound // 2026-09-23 P0：单轮上限（见常量注释）
	for _, it := range items {
		cmd, chainID := commandFor(it)
		if cmd == "" {
			continue // idle / 未知类型
		}
		if r.d.Skip != nil {
			if skip, why := r.d.Skip(string(it.Kind), it.Account); skip {
				st0 := r.statusFor(it.Account)
				st0.LastMsg = "不补发：" + why
				continue
			}
		}
		st := r.statusFor(it.Account)
		// 熔断中：未到期就跳过；到期自动解除
		if st.Blocked {
			if now.Before(st.BlockTill) {
				continue
			}
			st.Blocked, st.Attempts, st.NextAt, st.LastMsg = false, 0, time.Time{}, ""
		}
		robot, ok := r.d.Robot(it.Account)
		if !ok || !robot.Online {
			continue
		}
		if !needsRestore(robot, it.Kind) {
			// 在跑/跑完/出错：说明这条意图现在不用补，把计数清掉（别积累成熔断）
			if st.Attempts != 0 || st.LastMsg != "" {
				st.Attempts, st.NextAt, st.LastMsg = 0, time.Time{}, ""
			}
			continue
		}
		if lim > 0 && len(out) >= lim {
			// 2026-09-23 P0：本轮已达单轮上限 → 到此为止。未补的号**状态未被改动**
			// （不扣 Attempts/不设冷却），下一轮仍在候选里，等池闸与在途记账放行。
			break
		}
		if st.Attempts > 0 && now.Before(st.NextAt) {
			continue // 冷却中
		}
		if r.d.ErrRepeat != nil && r.d.ErrRepeat(it.Account) >= ErrRepeatToCircuit {
			st.Blocked, st.BlockTill = true, now.Add(time.Duration(r.d.CircuitSec)*time.Second)
			st.LastMsg = "同一错误重复过多：熔断等人工处理"
			continue
		}
		// 载荷可用性：抓鬼不带导航数据就是"原地不动"，与其瞎发不如熔断等人工把数据补上。
		if r.d.Payload != nil {
			if _, err := r.d.Payload(string(it.Kind), chainID); err != nil {
				st.Attempts++
				st.NextAt = now.Add(time.Duration(r.d.RetrySec) * time.Second)
				st.LastMsg = "载荷不可用，未补发：" + err.Error()
				if st.Attempts >= r.d.MaxAttempts {
					st.Blocked = true
					st.BlockTill = now.Add(time.Duration(r.d.CircuitSec) * time.Second)
					st.LastMsg = "载荷不可用（连续 " + itoa(st.Attempts) + " 次）：熔断等人工"
				}
				continue
			}
		}
		st.Attempts++
		st.NextAt = now.Add(time.Duration(r.d.RetrySec) * time.Second)
		st.LastMsg = "已补发 " + cmd
		st.LastDispatchAt = now // 在途记账（壳层池闸用；TTL 见 api 侧 ghostInflightTTL）
		if st.Attempts >= r.d.MaxAttempts {
			st.Blocked = true
			st.BlockTill = now.Add(time.Duration(r.d.CircuitSec) * time.Second)
			st.LastMsg = "连续 " + itoa(st.Attempts) + " 次没跑起来：熔断等人工"
		}
		out = append(out, Action{
			Account: it.Account, Kind: string(it.Kind), Command: cmd, ChainID: chainID,
			Reason: "意图=" + string(it.Kind) + "（" + it.Reason + "），当前 " + robot.State, At: now,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Account < out[j].Account })
	return out
}

// Run 周期 Tick 并下发命令（ctx 结束即退出）。
func (r *Runner) Run(ctx context.Context) {
	if r.d.Log != nil {
		enabled := r.d.Enabled == nil || r.d.Enabled()
		r.d.Log("[RESTORE] 恢复引擎启动（启用=%v，间隔=%s，延迟=%s，重试=%ds，上限=%d 次，熔断=%ds）",
			enabled, r.d.Interval, r.d.Delay, r.d.RetrySec, r.d.MaxAttempts, r.d.CircuitSec)
	}
	if r.d.Delay > 0 { // 启动后先等等：让机器人把状态报上来，避免刚起来就一通补发
		select {
		case <-time.After(r.d.Delay):
		case <-ctx.Done():
			return
		}
	}
	t := time.NewTicker(r.d.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, g := range GroupActions(r.Tick(r.now())) {
				cmd, err := r.groupCommand(g)
				if err != nil { // 载荷在 Tick 之后消失（文件被删/改成坏的）：不发空命令
					if r.d.Log != nil {
						r.d.Log("[RESTORE] %v 补发 %s 跳过：%v", g.Accounts, g.Command, err)
					}
					continue
				}
				if r.d.Send != nil {
					r.d.Send(cmd, "restore_"+g.Command)
				}
				if r.d.Log != nil {
					r.d.Log("[RESTORE] %v 补发 %s（%s）", g.Accounts, g.Command, g.Reason)
				}
			}
		}
	}
}

// groupCommand 把一批同命令的补发拼成一条下行命令（载荷取不到就返回 error，调用方不发）。
func (r *Runner) groupCommand(g Group) (map[string]any, error) {
	cmd := map[string]any{"cmd": g.Command, "accounts": g.Accounts}
	if g.ChainID != "" {
		cmd["chain_id"] = g.ChainID
	}
	if r.d.Payload != nil {
		payload, err := r.d.Payload(g.Kind, g.ChainID)
		if err != nil {
			return nil, err
		}
		if payload != nil {
			cmd["chain"] = payload
			// 载荷自带 chain_id（链数据文件里声明/按文件名补的）→ 顶层也带上，面板日志对得上
			if ch, ok := payload.(*chainlib.Chain); ok && ch.ChainID != "" && cmd["chain_id"] == nil {
				cmd["chain_id"] = ch.ChainID
			}
			if g.Command == "ghost_start" {
				cmd["role"] = "solo" // 与「启动」自动分配同口径
				limit := 50
				if r.d.GhostDailyLimit != nil {
					if v := r.d.GhostDailyLimit(); v > 0 {
						limit = v
					}
				}
				cmd["daily_limit"] = limit
			}
		}
	}
	return cmd, nil
}

// Status 恢复状态快照（按账号排序；面板用）。
func (r *Runner) Status() map[string]Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]Status, len(r.st))
	for k, v := range r.st {
		out[k] = *v
	}
	return out
}

func (r *Runner) statusFor(account string) *Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.st[account]
	if !ok {
		st = &Status{Account: account}
		r.st[account] = st
	}
	return st
}

func (r *Runner) now() time.Time {
	if r.d.Now != nil {
		return r.d.Now()
	}
	return time.Now()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	buf := [8]byte{}
	i := len(buf)
	for n > 0 && i > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
