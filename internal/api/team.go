// 组队（抓鬼试点）——阶段 1：协议打通（机器人端零改动）。
//
// 机器人端契约（2026-09-29 核实于 deploy/single_robot_zy/script/）：
//
//	client.py:574-595   cmd_name.startswith("team_") → team_captain.dispatch_cmd，
//	                    按 accounts 过滤目标机器人（未登录则挂 g_pending_cmds 等上线重放）。
//	team_captain.py:100-186（命令入口）：
//	  team_clear          {accounts}                                    退队/清旧队（队长队员都有效，C2S_TEAM_QUIT）
//	  team_setup_member   {accounts:[队员], captain_role_id, captain_account}  队员进入等待邀请
//	  team_setup_captain  {accounts:[队长], members:[{account,role_id}]}        建队（C2S_TEAM_CREATE）+ 逐个邀请
//	  team_setup_applicant{accounts:[队员], captain_role_id, captain_account}  队员主动申请（C2S_TEAM_OFFER）
//	  team_disband        {accounts:[队长]}                              解散（C2S_TEAM_DISMISS，仅队长有效）
//	回执事件（team_captain.__emit）→ internal/services/event 的 team_* 分支落台账：
//	  team_ready（队长：members=已确认队员 role_id）/ team_disbanded / team_rejoined / team_return_nav
//
// 试点口径（用户 2026-09-29 批准）："先完成手头工作单元 → 建队集结（人齐）→ 再开接"——
// 本文件只负责建队/集结/解散/查看，**不自动派任务**（next_action 缺省不带；阶段 2 再接）。
//
// 已知机器人端局限（阶段 1 不修，试点用 invite 模式）：
//   - apply 模式：队长侧 member_role_ids 恒为空 → 队长端不产生 team_ready 事件（只有队员侧发）；
//     台账就绪判据失效 → 试点与阶段 2 统一用 invite 模式。
//   - 邀请 10s 无确认时机器人端会"乐观自动确认"并发 team_ready（有误判风险）→ 试点时以
//     服务端 S2C_BUILD_TEAM/成员事件为准核对（见计划文档 §2.1 G5）。
package api

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"zyctrlcenter/internal/state"
)

// handleTeamSetup POST /api/team/setup
//
// 请求：{captain, members:[...], mode:"invite"|"apply", next_action?, next_action_hint?}
// 行为：全员在线校验 → team_clear 清旧队 → 按模式下发组队命令 → 登记台账（pending）。
func (a *API) handleTeamSetup(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	captain := strings.TrimSpace(toStr(body["captain"]))
	members := teamMemberList(body["members"])
	mode := strings.TrimSpace(toStr(body["mode"]))
	if mode == "" {
		mode = "invite"
	}
	nextAction := strings.TrimSpace(toStr(body["next_action"]))
	var hint map[string]any
	if h, ok := body["next_action_hint"].(map[string]any); ok {
		hint = h
	}

	fail := func(msg string, extra map[string]any) {
		obj := map[string]any{"ok": false, "msg": msg}
		for k, v := range extra {
			obj[k] = v
		}
		writeJSON(w, http.StatusOK, obj)
	}

	if captain == "" {
		fail("captain 必填（队长账号）", nil)
		return
	}
	if len(members) == 0 {
		fail("members 必填（至少 1 名队员）", nil)
		return
	}
	for _, m := range members {
		if m == captain {
			fail("members 不能包含队长自己（"+captain+"）", nil)
			return
		}
	}
	// 服务端上限：const.MAX_TEAM_MEMBERS=4（队长+4=5 人；config/script/const.py:117）
	if len(members) > 4 {
		fail(fmt.Sprintf("队员 %d 人超过上限：服务端队伍上限 = 队长+4（MAX_TEAM_MEMBERS=4）", len(members)), nil)
		return
	}
	if mode != "invite" && mode != "apply" {
		fail(`mode 只支持 "invite"（队长邀请，试点推荐）或 "apply"（队员申请）`, nil)
		return
	}
	if a.Events == nil {
		fail("事件通道不可用（命令下发不了）", nil)
		return
	}

	all := append([]string{captain}, members...)
	// 2026-09-30 私人池：私号不参与组队（建队/入队都拒；如需组队先移出私人池）。
	for _, acc := range all {
		if a.IsPersonal(acc) {
			fail("私人池账号不参与组队："+acc+"（如需组队请先移出私人池）", nil)
			return
		}
	}
	if _, failedAcc := a.onlineAccounts(all); len(failedAcc) > 0 {
		fail("有账号不在线（试点口径：先等工作单元完成、全员在线再建队）："+failedText(failedAcc),
			map[string]any{"failed": failedAcc})
		return
	}
	capRobot, ok := a.St.Get(captain)
	if !ok || capRobot.RoleID <= 0 {
		fail("队长 role_id 未知（等机器人上线并心跳上报后再试；队员同样需要心跳）", nil)
		return
	}
	// invite 模式：邀请按 role_id 定位目标 → 每个队员都必须已有 role_id
	memberInfos := make([]map[string]any, 0, len(members))
	var noRID []string
	for _, m := range members {
		rid := 0
		if rb, ok := a.St.Get(m); ok {
			rid = rb.RoleID
		}
		if rid <= 0 {
			noRID = append(noRID, m)
		}
		memberInfos = append(memberInfos, map[string]any{"account": m, "role_id": rid})
	}
	if mode == "invite" && len(noRID) > 0 {
		fail("invite 模式需要每个队员的 role_id（等上线心跳）："+strings.Join(noRID, "、"),
			map[string]any{"no_role_id": noRID})
		return
	}

	// 1) 先清旧队（队长/队员都发 team_clear=C2S_TEAM_QUIT）：残留队伍会让建队被服务端忽略、
	//    邀请被拒（参考实现 routers/team.py _setup_team 同款顺序）。
	if !a.Events.SendCmd(map[string]any{"cmd": "team_clear", "accounts": all}, "team_clear") {
		fail("命令下发失败：机器人控制通道未连接", nil)
		return
	}
	// 2) 等游戏服处理 QUIT（回 S2C_CANCEL_TEAM）后建队（参考实现 sleep 0.3s）
	time.Sleep(300 * time.Millisecond)

	// 3) 按模式下发组队命令（机器人端 team_captain.dispatch_cmd）
	okSend := true
	if mode == "apply" {
		mc := map[string]any{"cmd": "team_setup_applicant", "accounts": members,
			"captain_role_id": capRobot.RoleID, "captain_account": captain}
		setTeamOpt(mc, nextAction, hint)
		okSend = a.Events.SendCmd(mc, "team_setup_applicant") && okSend
		cc := map[string]any{"cmd": "team_setup_captain", "accounts": []string{captain},
			"members": []map[string]any{}}
		okSend = a.Events.SendCmd(cc, "team_setup_captain") && okSend
	} else {
		mc := map[string]any{"cmd": "team_setup_member", "accounts": members,
			"captain_role_id": capRobot.RoleID, "captain_account": captain}
		setTeamOpt(mc, nextAction, hint)
		okSend = a.Events.SendCmd(mc, "team_setup_member") && okSend
		cc := map[string]any{"cmd": "team_setup_captain", "accounts": []string{captain},
			"members": memberInfos}
		setTeamOpt(cc, nextAction, hint)
		okSend = a.Events.SendCmd(cc, "team_setup_captain") && okSend
	}
	if !okSend {
		fail("组队命令部分下发失败（通道断开？）：建议先 disband 再重试", nil)
		return
	}

	// 4) 登记台账（pending；收到 team_ready 转 ready），并落审计日志
	a.Events.TeamJobSet(captain, members, mode, nextAction)
	if a.Store != nil {
		a.Store.LogEvent(map[string]any{"type": "api", "action": "team_setup", "sub": mode,
			"zone": a.currentZoneKey(), "captain": captain, "members": members,
			"captain_role_id": capRobot.RoleID, "next_action": nextAction})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "captain": captain,
		"members": members, "mode": mode, "captain_role_id": capRobot.RoleID,
		"msg": fmt.Sprintf("已下发组队（%s）：队长 %s + 队员 %d 人；等 team_ready 事件确认",
			mode, captain, len(members))})
}

// handleTeamDisband POST /api/team/disband
//
// 请求：{accounts:[...]} 或 {all:true}（二选一，**必须给**——防误解散整队）。
// 行为：按台账把账号归属到队伍 → 队长 team_disband（整队解散）+ 队员 team_clear 兜底
// （队员发 disband 无效，参考实现同款）→ 清台账。
func (a *API) handleTeamDisband(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	accs := normAccounts(bodyAccounts(body))
	all := toBool(body["all"], false)
	fail := func(msg string) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": msg})
	}
	if a.Events == nil {
		fail("事件通道不可用（命令下发不了）")
		return
	}
	if !all && len(accs) == 0 {
		fail("accounts 或 all:true 必须给一个（防误解散整队）")
		return
	}
	teams, jobs := a.Events.TeamLedger()
	set := make(map[string]bool, len(accs))
	for _, a0 := range accs {
		set[a0] = true
	}
	type target struct {
		captain string
		members []string
	}
	var targets []target
	seen := map[string]bool{}
	addT := func(cap string, members []string) {
		if seen[cap] {
			return
		}
		seen[cap] = true
		targets = append(targets, target{cap, members})
	}
	for _, t := range teams {
		if all || teamTouchedAPI(t.Captain, t.Members, set) {
			addT(t.Captain, t.Members)
		}
	}
	for _, j := range jobs {
		if all || teamTouchedAPI(j.Captain, j.Members, set) {
			addT(j.Captain, j.Members)
		}
	}
	if len(targets) == 0 {
		fail(fmt.Sprintf("没有匹配的队伍（台账 %d 队 + %d 个未就绪）", len(teams), len(jobs)))
		return
	}
	var allAccs []string
	okAll := true
	for _, t := range targets {
		if !a.Events.SendCmd(map[string]any{"cmd": "team_disband", "accounts": []string{t.captain}}, "team_disband") {
			okAll = false
		}
		if len(t.members) > 0 {
			if !a.Events.SendCmd(map[string]any{"cmd": "team_clear", "accounts": t.members}, "team_clear") {
				okAll = false
			}
		}
		allAccs = append(allAccs, append([]string{t.captain}, t.members...)...)
		time.Sleep(200 * time.Millisecond) // 逐队下发，避免命令堆积（参考实现同款）
	}
	a.Events.TeamRemove(allAccs)
	if a.Store != nil {
		a.Store.LogEvent(map[string]any{"type": "api", "action": "team_disband", "zone": a.currentZoneKey(),
			"teams": len(targets), "accounts": allAccs, "sent": okAll})
	}
	msg := fmt.Sprintf("已下发解散 %d 队（%d 人）", len(targets), len(allAccs))
	if !okAll {
		msg = okMsg(false, "") + "；（台账已清，建议核对机器人端是否仍有队伍）"
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "teams": len(targets),
		"accounts": allAccs, "msg": msg})
}

// handleTeamDispatch POST /api/team/dispatch
//
// 请求：{captain, daily_limit?}；行为：校验队长在线 + 队伍就绪 → 给**队长单号**下发
// `ghost_start role=captain`（带导航载荷/daily_limit/done），**队员一条命令都不发**（待命）。
//
// 为什么是独立接口而不是 setup 加 dispatch:true（阶段 2 评估）：
//   - 时序：setup 立即返回，而派发要等 team_ready（集结完成）——"先完成手头工作单元→建队
//     集结（人齐）→再开接"是用户口径；两步分开才能把"等就绪"表达清楚（dispatch 就绪前
//     直接拒绝并提示）；
//   - 复用：D13 队长转移后新队长接续、阶段 3/4 编排补派，都走"给（新）队长派任务"这一动作，
//     独立接口便于复用与审计（/api/team/dispatch 出现即"这队开抓了"）。
func (a *API) handleTeamDispatch(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	captain := strings.TrimSpace(toStr(body["captain"]))
	fail := func(msg string, extra map[string]any) {
		obj := map[string]any{"ok": false, "msg": msg}
		for k, v := range extra {
			obj[k] = v
		}
		writeJSON(w, http.StatusOK, obj)
	}
	if captain == "" {
		fail("captain 必填（队长账号）", nil)
		return
	}
	if a.Events == nil {
		fail("事件通道不可用（命令下发不了）", nil)
		return
	}
	rb, ok := a.St.Get(captain)
	if !ok || !rb.Online {
		fail("队长不在线（等上线并收到心跳后再派）", nil)
		return
	}
	// 就绪判据：台账 ready 队优先；兼容手工路径（心跳 team.role=captain 且 setup_done）。
	ready := false
	teams, _ := a.Events.TeamLedger()
	for _, t := range teams {
		if t.Captain == captain {
			ready = true
			break
		}
	}
	if !ready && rb.TeamRole() == "captain" && rb.TeamSetupDone() {
		ready = true
	}
	if !ready {
		fail("队伍未就绪：等 team_ready 事件（建队后集结完成）再派；GET /api/team/status 可核对", nil)
		return
	}
	limit := toInt(body["daily_limit"], 0)
	if limit <= 0 {
		limit = a.chainPayloads().GhostDailyLimit()
	}
	cmd, err := a.ghostStartCmdOf([]string{captain}, "captain", limit)
	if err != nil {
		fail("队长派发失败："+err.Error(), nil)
		return
	}
	if !a.Events.SendCmd(cmd, "team_dispatch_ghost_start") {
		fail("下发失败：机器人控制通道未连接", nil)
		return
	}
	a.markGhostDispatch([]string{captain}) // 在途记账（与手动/池派发同口径，防重复下发）
	if a.Store != nil {
		a.Store.LogEvent(map[string]any{"type": "api", "action": "team_dispatch",
			"zone": a.currentZoneKey(), "captain": captain, "daily_limit": limit,
			"chain_id": cmd["chain_id"]})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "captain": captain,
		"daily_limit": limit, "chain_id": cmd["chain_id"],
		"msg": "已给队长下发抓鬼（role=captain）：" + captain + "；队员保持待命，不单独派任务"})
}

// teamRoleOf 该账号的"组队角色"（中控判断派发避让用）：台账（下发/事件）∪ 心跳（机器人事实）。
//
//	role="captain"/"member"/"applicant"：队内/在队流程中（applicant=申请入队等待期，同样不打扰）
//	不清空：两边都没有 → ("", "")（不在队，正常派发）
//
// 心跳块 2026-09-29 定稿**只有 role_id**：队长账号先取 `captain`（无则）用 `captain_role_id`
// 反查（见 heartbeatCaptainOf）。
func (a *API) teamRoleOf(account string) (role, captain string) {
	if a.Events != nil {
		if r0, cap, ok := a.Events.TeamRoleOf(account); ok {
			return r0, cap
		}
	}
	if a.St != nil {
		if rb, ok := a.St.Get(account); ok {
			if r0 := rb.TeamRole(); r0 != "" {
				return r0, a.heartbeatCaptainOf(rb, account)
			}
		}
	}
	return "", ""
}

// heartbeatCaptainOf 心跳 team 块的队长账号解析：captain(账号) → captain_role_id 反查；
// 自己就是队长（rid==自身 RoleID）→ 返回自己。
func (a *API) heartbeatCaptainOf(rb state.Robot, account string) string {
	if cap := rb.TeamCaptainAccount(); cap != "" {
		return cap
	}
	rid := rb.TeamCaptainRoleID()
	if rid <= 0 {
		return ""
	}
	if rb.RoleID == rid {
		return account
	}
	return a.accountOfRoleID(rid)
}

// accountOfRoleID 心跳 RoleID → 账号反查（心跳 team 块 rid→账号；找不到返回 ""）。
func (a *API) accountOfRoleID(rid int) string {
	if rid <= 0 || a.St == nil {
		return ""
	}
	for _, r := range a.St.Snapshot() {
		if r.RoleID == rid {
			return r.Account
		}
	}
	return ""
}

// dropTeamAccounts 从批量派发里剔除"队内号"（队长/队员/待就绪 job）——阶段 2 最小避让：
// 普通任务下发（池/补发/启动自动分配）不打扰队伍；队长任务只能走 /api/team/dispatch（阶段 3
// 再做全量编排避让：候选过滤/水位/回收）。返回（保留, 剔除）两份；命中合并记一条日志。
func (a *API) dropTeamAccounts(accs []string, what string) (keep, skipped []string) {
	if len(accs) == 0 {
		return accs, nil
	}
	keep = make([]string, 0, len(accs))
	var desc []string
	for _, acc := range accs {
		role, cap := a.teamRoleOf(acc)
		if role == "" {
			keep = append(keep, acc)
			continue
		}
		skipped = append(skipped, acc)
		desc = append(desc, fmt.Sprintf("%s(%s→%s)", acc, role, cap))
	}
	if len(skipped) > 0 {
		a.Log.Printf("[组队] %s 跳过队内号 %d 个：%s（走组队编排 /api/team/dispatch）",
			what, len(skipped), strings.Join(desc, "、"))
	}
	return keep, skipped
}

// handleTeamStatus GET /api/team/status
//
// 返回：{teams:[已就绪], jobs:[未就绪], robots:{账号→摘要}}（只读；供手动试点核对）。
//
// 2026-09-29 阶段 2 扩展：robots 摘要带组队观测字段 ——
//
//	team     心跳 team 块原样（定稿：role/captain_role_id/member_role_ids/parted/setup_done/
//	         token_use_ts/token_use_count/token_count(仅队长)；机器人事实）
//	team_role / team_captain  心跳角色 + 队长账号（rid→账号反查；定稿块只有 rid）
//	ledger_role / ledger_captain  中控台账角色（captain|member；含 pending job——与派发避让同源）
//	parted   暂离标记（心跳；无心跳块 → false）
//	token_use_ts 助战令最近使用（机器人本地 ms；0=未知——用于换算"令剩余时长"）
//	token_last / member_state  事件来源（team_token / team_member_state；定稿未把 token 放心跳时的权威观测）
func (a *API) handleTeamStatus(w http.ResponseWriter, r *http.Request) {
	if a.Events == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "事件通道不可用"})
		return
	}
	teams, jobs := a.Events.TeamLedger()
	robots := map[string]any{}
	note := func(acc string) {
		if acc == "" {
			return
		}
		if _, done := robots[acc]; done {
			return
		}
		if rb, ok := a.St.Get(acc); ok {
			item := map[string]any{"online": rb.Online, "role_id": rb.RoleID,
				"state": rb.State, "mapid": rb.MapID, "pos": rb.Pos, "level": rb.Level,
				"parted": rb.TeamParted()}
			if tb := rb.TeamBlock(); tb != nil {
				item["team"] = tb // 心跳原样（定稿：role/captain_role_id/member_role_ids/parted/setup_done/token_*）
				if role := rb.TeamRole(); role != "" {
					item["team_role"] = role // 心跳事实（captain|member|applicant）
					item["team_captain"] = a.heartbeatCaptainOf(rb, acc)
				}
			}
			if role, cap, ok := a.Events.TeamRoleOf(acc); ok {
				item["ledger_role"] = role // 中控台账（下发/事件来源；含 pending job）
				item["ledger_captain"] = cap
			}
			if ts := rb.TeamTokenUseTS(); ts > 0 {
				item["token_use_ts"] = ts // 助战令最近使用（机器人本地 ms；1 令=60min 口径，计划 §3.4/D14）
			}
			// 助战令链结果 / 队员态：来自事件（阶段 2 机器人端未加心跳 token 字段，
			// 见 team-feature-plan 定稿 #6 → 观测以事件为准）。
			if rec := a.Events.TeamTokenLast(acc); rec != nil {
				item["token_last"] = rec // {ok,reason,count,reserve,use_ts,use_count,at}；ok:false = D13 判据
			}
			if rec := a.Events.TeamMemberStateLast(acc); rec != nil {
				item["member_state"] = rec // {role,parted,setup_done,at}
			}
			robots[acc] = item
		} else {
			robots[acc] = map[string]any{"online": false, "msg": "无状态记录"}
		}
	}
	for _, t := range teams {
		note(t.Captain)
		for _, m := range t.Members {
			note(m)
		}
	}
	for _, j := range jobs {
		note(j.Captain)
		for _, m := range j.Members {
			note(m)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "teams": teams, "jobs": jobs, "robots": robots})
}

// ---------------------------------------------------------------- 工具

// teamMemberList 解析 members 字段：支持 ["a","b"] / "a,b" / [["a",...]]（保序去重）。
func teamMemberList(v any) []string {
	if s, ok := v.(string); ok {
		return normAccounts(splitList(s))
	}
	raw, _ := v.([]any)
	out := make([]string, 0, len(raw))
	for _, it := range raw {
		switch t := it.(type) {
		case string:
			out = append(out, t)
		case []any:
			if len(t) >= 1 {
				if name, _ := t[0].(string); name != "" {
					out = append(out, name)
				}
			}
		}
	}
	return normAccounts(out)
}

// teamTouchedAPI 账号集合是否触及该队（队长或任一队员）。
func teamTouchedAPI(captain string, members []string, set map[string]bool) bool {
	if set[captain] {
		return true
	}
	for _, m := range members {
		if set[m] {
			return true
		}
	}
	return false
}

// setTeamOpt 把可选的 next_action / next_action_hint 塞进命令（阶段 1 试点不传 → 缺省不带，
// 机器人端 dispatch_cmd 缺省 None/{}）。
func setTeamOpt(cmd map[string]any, nextAction string, hint map[string]any) {
	if nextAction == "" {
		return
	}
	cmd["next_action"] = nextAction
	if hint != nil {
		cmd["next_action_hint"] = hint
	}
}
