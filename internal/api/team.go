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

// handleTeamStatus GET /api/team/status
// 返回：{teams:[已就绪], jobs:[未就绪], robots:{账号→摘要}}（只读；供手动试点核对）。
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
			robots[acc] = map[string]any{"online": rb.Online, "role_id": rb.RoleID,
				"state": rb.State, "mapid": rb.MapID, "pos": rb.Pos, "level": rb.Level}
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
