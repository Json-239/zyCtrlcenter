// 人工暂停（2026-09-23 操作健壮性审计 R3）。
//
// 生产口径：`CTRL_AUTO_RESTORE=1` 开着恢复引擎，定时任务也在跑 —— 用户点「停止/停链」
// 之后号会在 5~60 秒内被按意图补发拉起（观感"停不住"，实测工单来源）。
// 这里给"停止"一个明确语义：**人工暂停** ——
//
//   - 打标：POST /api/stop（停链；不带账号=对当时全部在线号打标）；
//   - 闸门：恢复引擎（GhostSkipFunc）/ 定时任务（autotaskCandidates + LaunchTask）/
//     水位（上线候选与压号）/ 游荡池（派游荡与回收）派发前跳过暂停号；
//   - 清标：用户再次显式表达"让它跑" —— POST /api/start（启动/自动分配）、
//     POST /api/intents/restore（立即补发）、POST /api/robots/batch(online) 与
//     POST /api/robots/manage(add)（上线）；
//   - 不清标：自动通道（autotask 定时/水位/reghost 重登）不会解除暂停（这正是"暂停"的意义）。
//
// 落点：标记放 state（内存态，独立于 robots 行）—— 与 removed / ghostUnavail 同款：
// 号下线/移除后行会被删掉，暂停意图必须独立存活；Snapshot() 会把它填进
// Robot.Paused，所以 GET /api/status.robots[].paused 与顶层 paused 列表都能看到
// （前端 UI 展示另行安排，本轮不动 .vue）。
package api

// pauseAccounts 给这些号打"人工暂停"，返回实际打标的号数。
// accounts 为空 = 当前全部在线号（与 /api/stop 不带账号 = 停全部同口径）。
func (a *API) pauseAccounts(accounts []string) int {
	if a.St == nil {
		return 0
	}
	if len(accounts) == 0 {
		n := 0
		for _, r := range a.St.Snapshot() {
			if r.Account == "" || !r.Online {
				continue
			}
			a.St.MarkPaused(r.Account)
			n++
		}
		return n
	}
	n := 0
	for _, acc := range accounts {
		if acc == "" {
			continue
		}
		a.St.MarkPaused(acc)
		n++
	}
	return n
}

// resumeAccounts 解除这些号的人工暂停，返回被解除的账号（升序，日志用）。
// accounts 为空 = 全部解除（"启动全部"= 全部恢复自动编排）。
func (a *API) resumeAccounts(accounts []string) []string {
	if a.St == nil {
		return nil
	}
	if len(accounts) == 0 {
		return a.St.ClearAllPaused()
	}
	out := make([]string, 0, len(accounts))
	seen := make(map[string]bool, len(accounts))
	for _, acc := range accounts {
		if acc == "" || seen[acc] {
			continue
		}
		seen[acc] = true
		if !a.St.IsPaused(acc) {
			continue
		}
		a.St.ClearPaused(acc)
		out = append(out, acc)
	}
	return out
}

// dropPaused 返回"去掉人工暂停号"的新切片（R3 派发闸；不改入参）。
func (a *API) dropPaused(accs []string) []string {
	if a.St == nil || len(accs) == 0 {
		return accs
	}
	out := make([]string, 0, len(accs))
	for _, acc := range accs {
		if a.St.IsPaused(acc) {
			continue
		}
		out = append(out, acc)
	}
	return out
}
