package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"zyctrlcenter/internal/accountverify"
	"zyctrlcenter/internal/services/accounts"
	"zyctrlcenter/internal/services/zones"
)

// verifyMaxAccounts 单次（同步）验证的账号上限。
const verifyMaxAccounts = 200

// verifyMaxBatch 一次批量任务最多验多少个（防止误点一下拍几百个登录包给游戏服）。
// 任务带进度、可随时「停止」，所以面板默认的"本区全部"能覆盖整池（5000+）。
const verifyMaxBatch = 20000

// verifySaveEvery 批量任务写回账号池时每多少条落盘一次（其余只写内存，结束时统一落盘）。
const verifySaveEvery = 10

// handleAccountsVerify 账号可用性验证：中控**直接连游戏服**跑登录协议（102 → 300）探测，
// 不经过机器人进程、不真正进入游戏（无副作用）。两种模式：
//
//  1. **同步模式**：请求带 `accounts`（1 个或几个）→ 当场返回 `results`（面板手输单个走这条）
//  2. **批量任务模式（默认）**：请求不带 `accounts` → 按 `zone`（默认当前区）从**账号池**里选号，
//     后台跑并把结果**逐条同步回池**；立即返回 `job`，面板用
//     `GET /api/accounts/verify/job?id=` 看进度，`POST /api/accounts/verify/cancel` 可停。
//
// 请求体（两种模式共用）：
//
//	accounts    ["a"] / [["a","pwd"]] / "a,b"（给了就是同步模式；密码缺省取池 → 默认密码）
//	zone        "<服key>/<区key>" 或 "host:port"（缺省=当前区）
//	scope       批量模式选号范围：unverified（默认，未验证/验过但没通过）/ unusable / all
//	limit       批量模式一次验多少个（默认 50，上限 200）
//	skip_online 批量模式是否跳过"正在线跑着"的号（默认 true —— 它们已经能登录，别浪费一次连接）
//	password/concurrency/timeout_sec/version/query_role/persist 见下
//
// 语义：100=不存在 / 200=密码错 / 300=冻结 都算 **不可用**；网络/超时/编码错误不改变"可用"结论
// （只记 verify_msg），避免一次网络抖动把好号标成不可用。
func (a *API) handleAccountsVerify(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	zoneArg := zoneKeyOf(r, body)
	gameAddr := a.gameAddrOf(zoneArg)
	if gameAddr == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "msg": "没有可用的区：请先在「系统信息 → 区管理」配置，或显式传 zone",
		})
		return
	}
	coding := a.codingOfAddr(gameAddr)
	opt, timeoutSec := a.verifyOptions(body, coding)
	persist := toBool(body["persist"], true)
	concurrency := a.verifyConcurrency(body)

	// ---------------- 同步模式：显式给了账号 ----------------
	if pairs := a.accountPairs(body); len(pairs) > 0 {
		if len(pairs) > verifyMaxAccounts {
			writeJSON(w, http.StatusOK, map[string]any{
				"ok": false, "msg": "一次最多验证 200 个账号（更多请用批量任务：不传 accounts + limit）",
			})
			return
		}
		targets, noPwd := a.targetsFromPairs(pairs, gameAddr)
		ctx, cancel := context.WithTimeout(r.Context(), time.Duration(timeoutSec)*time.Second+5*time.Second)
		defer cancel()

		start := time.Now()
		results := accountverify.VerifyAll(ctx, gameAddr, targets, opt, concurrency)
		// 库里没密码的账号：明确报出来（不是静默跳过，也不会拿"统一密码"去试）
		for _, name := range noPwd {
			results = append(results, accountverify.Result{
				Account: name, Zone: gameAddr, RetCode: -1, At: time.Now().Unix(),
				Msg: "库里没有该账号的密码", Err: "库里没有该账号的密码（先建号/补密码；本项目没有统一密码）",
			})
		}
		summary, updated := a.collectVerifyResults(results, gameAddr, persist, false)

		a.logVerify(gameAddr, coding, "sync", summary, len(results), time.Since(start))
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "mode": "sync", "zone": zoneArg, "game_addr": gameAddr,
			"coding": coding, "version": opt.Version, "summary": summary,
			"results": results, "pool_updated": updated,
			"msg": verifyMsg(summary, time.Since(start)),
		})
		return
	}

	// ---------------- 批量任务模式（默认）：从本地库当前区选号 ----------------
	scope := verifyScopeOf(body)
	limit := toInt(body["limit"], 50)
	// limit=0（负数同义）= 不限：把范围内能验的都排上（面板"本区全部"就是这么用的）。
	// 硬上限 verifyMaxBatch 仍然兜着，防手滑。
	if limit <= 0 {
		limit = verifyMaxBatch
	}
	if limit > verifyMaxBatch {
		limit = verifyMaxBatch
	}
	skipOnline := toBool(body["skip_online"], true)

	skip := map[string]bool{}
	skippedOnline := make([]string, 0, 8)
	if skipOnline && a.St != nil {
		for _, rb := range a.St.Snapshot() {
			if rb.Online {
				skip[rb.Account] = true
				skippedOnline = append(skippedOnline, rb.Account)
			}
		}
	}

	pendingBefore := a.Accounts.CountForVerify(gameAddr)
	selected := a.Accounts.SelectForVerify(gameAddr, scope, limit, skip)
	if len(selected) == 0 {
		msg := "当前区没有待验证的账号（范围 " + string(scope) + "）"
		if len(skippedOnline) > 0 {
			msg += "；另有 " + strconv.Itoa(len(skippedOnline)) + " 个正在线的号被跳过（要一起验就传 skip_online=false）"
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "msg": msg, "zone": zoneArg, "game_addr": gameAddr,
			"scope": string(scope), "pending": pendingBefore,
			"skipped_online": skippedOnline,
		})
		return
	}

	// 密码一律从账号库按区取；库里没有的号先跳过并列出来（不浪费一次连接，也不用错密码）
	targets := make([]accountverify.Target, 0, len(selected))
	skippedNoPwd := make([]string, 0, 8)
	for _, name := range selected {
		pwd, ok := a.Accounts.PasswordFor(name, gameAddr)
		if !ok {
			skippedNoPwd = append(skippedNoPwd, name)
			continue
		}
		targets = append(targets, accountverify.Target{Account: name, Password: pwd})
	}
	if len(targets) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "zone": zoneArg, "game_addr": gameAddr,
			"skipped_no_password": skippedNoPwd, "pending": pendingBefore,
			"msg": "选中的账号在库里都没有密码（先建号或补密码）；本项目没有统一密码",
		})
		return
	}

	// 逐条写回账号池：整段走"原子写窗"（期间禁止热重载，避免并发写回被冲掉），
	// 中途每 verifySaveEvery 条顺手落盘一次（防进程半路挂掉丢结果），结束时收尾落盘。
	if persist {
		a.Accounts.BeginBatch()
	}
	var mu sync.Mutex
	written := 0
	onResult := func(res accountverify.Result) {
		if !persist {
			return
		}
		a.applyVerifyResult(res, gameAddr, true)
		mu.Lock()
		written++
		n := written
		mu.Unlock()
		if n%verifySaveEvery == 0 {
			_ = a.Accounts.Save()
		}
	}
	onDone := func(snap accountverify.JobSnapshot) {
		if persist {
			_ = a.Accounts.EndBatch() // 关窗 + 统一落盘
		}
		a.logVerify(gameAddr, coding, "job", jobSummary(snap), snap.Total,
			time.Duration(snap.ElapsedMs)*time.Millisecond)
	}

	job := a.Verify.Start(gameAddr, targets, opt, concurrency, onResult, onDone)
	msg := "已开始批量验证 " + strconv.Itoa(len(targets)) + " 个账号（范围 " + string(scope) +
		"，并发 " + strconv.Itoa(concurrency) + "，结果逐条同步回账号池）"
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "mode": "job", "job": job.Snapshot(), "zone": zoneArg, "game_addr": gameAddr,
		"coding": coding, "version": opt.Version, "scope": string(scope),
		"selected": len(targets), "skipped_online": skippedOnline,
		"skipped_no_password": skippedNoPwd, "pending": pendingBefore,
		"msg": msg,
	})
}

// handleAccountsVerifyJob 查批量验证任务进度（id 为空 = 最近一个任务）。
func (a *API) handleAccountsVerifyJob(w http.ResponseWriter, r *http.Request) {
	if a.Verify == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "验证任务管理器未启用"})
		return
	}
	job := a.Verify.Get(strings.TrimSpace(r.URL.Query().Get("id")))
	if job == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "没有验证任务（先发起一次批量验证）"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "job": job.Snapshot()})
}

// handleAccountsVerifyCancel 停止批量验证任务（在途结果丢弃，不写回池）。
func (a *API) handleAccountsVerifyCancel(w http.ResponseWriter, r *http.Request) {
	if a.Verify == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "验证任务管理器未启用"})
		return
	}
	body := readBody(r)
	id := strings.TrimSpace(toStr(body["id"]))
	if id == "" {
		id = strings.TrimSpace(r.URL.Query().Get("id"))
	}
	job := a.Verify.Get(id)
	if job == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "没有验证任务可停止"})
		return
	}
	job.Cancel()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "job": job.Snapshot(), "msg": "已请求停止"})
}

// ---------------------------------------------------------------- 共用工具

// verifyOptions 组装探测选项（编码/版本/查角色/超时）。
func (a *API) verifyOptions(body map[string]any, coding string) (accountverify.Options, int) {
	timeoutSec := toInt(body["timeout_sec"], int(accountverify.DefaultTimeout/time.Second))
	if timeoutSec < 1 {
		timeoutSec = 1
	}
	if timeoutSec > 30 {
		timeoutSec = 30
	}
	version := strings.TrimSpace(toStr(body["version"]))
	if version == "" {
		version = a.Cfg.GameVersion
	}
	opt := accountverify.DefaultOptions()
	opt.Coding = coding
	opt.Version = version
	opt.QueryRole = toBool(body["query_role"], true)
	opt.Timeout = time.Duration(timeoutSec) * time.Second
	return opt, timeoutSec
}

func (a *API) verifyConcurrency(body map[string]any) int {
	n := toInt(body["concurrency"], 2)
	if n < 1 {
		n = 1
	}
	if n > accountverify.MaxConcurrency() {
		n = accountverify.MaxConcurrency()
	}
	return n
}

// verifyScopeOf 解析批量选号范围（默认 unverified = 本地库该区待验证的号）。
func verifyScopeOf(body map[string]any) accounts.VerifyScope {
	switch strings.ToLower(strings.TrimSpace(toStr(body["scope"]))) {
	case "zone", "zone_all", "本区", "本区全部":
		return accounts.VerifyScopeZone
	case "all", "pool":
		return accounts.VerifyScopeAll
	case "unusable":
		return accounts.VerifyScopeUnusable
	case "unknown":
		return accounts.VerifyScopeUnknown
	default:
		return accounts.VerifyScopeUnverified
	}
}

// targetsFromPairs [账号,密码] → 探测目标：密码取请求里给的，否则**按区从账号库取**；
// 都拿不到就返回在该名单里（调用方据此给出明确失败，**绝不编一个默认密码**）。
func (a *API) targetsFromPairs(pairs [][2]string, zone string) ([]accountverify.Target, []string) {
	out := make([]accountverify.Target, 0, len(pairs))
	noPwd := make([]string, 0)
	for _, p := range pairs {
		pwd := p[1]
		if pwd == "" && a.Accounts != nil {
			pwd = a.Accounts.Password(p[0], zone)
		}
		if pwd == "" {
			noPwd = append(noPwd, p[0])
			continue
		}
		out = append(out, accountverify.Target{Account: p[0], Password: pwd})
	}
	return out, noPwd
}

// collectVerifyResults 汇总 + 写回池（noSave=true 时只写内存，由批量任务统一落盘）。
func (a *API) collectVerifyResults(results []accountverify.Result, addr string, persist, noSave bool) (map[string]int, int) {
	summary := map[string]int{"total": len(results)}
	updated := 0
	for i := range results {
		res := results[i]
		switch {
		case res.Err != "":
			summary["error"]++
		case !res.Exists:
			summary["not_exists"]++
		case res.Usable:
			summary["usable"]++
		default:
			summary["unusable"]++
		}
		if res.Exists {
			summary["exists"]++
		}
		if persist && a.applyVerifyResult(res, addr, noSave) {
			updated++
		}
	}
	return summary, updated
}

// applyVerifyResult 把一条探测结果写回账号池（池外账号不凭空建档）。
// noSave=true 时只写内存（批量任务逐条写、最后统一落盘）。
func (a *API) applyVerifyResult(res accountverify.Result, addr string, noSave bool) bool {
	if a.Accounts == nil {
		return false
	}
	prev, ok := a.Accounts.Get(res.Account)
	if !ok {
		return false
	}
	st := accounts.ZoneState{}
	if z := prev.Zone(addr); z != nil {
		st = *z
	}
	if res.Err != "" {
		// 网络/超时/编码问题：不改变可用性结论，只记"这次没验成"
		st.Msg = "验证失败：" + res.Err
	} else {
		st.Verified = true
		st.Usable = res.Usable
		st.Msg = res.Msg
		st.VerifiedAt = time.Now().Unix()
		if res.Level > 0 {
			st.Level = res.Level
		}
		if res.RoleName != "" {
			st.RoleName = res.RoleName
		}
	}
	if noSave {
		a.Accounts.SetZoneStateNoSave(res.Account, addr, st)
	} else {
		a.Accounts.SetZoneState(res.Account, addr, st)
	}
	return true
}

// logVerify 写一条可读的运行历史（面板「运行日志」页可见）。
func (a *API) logVerify(addr, coding, mode string, summary map[string]int, total int, d time.Duration) {
	if a.Store == nil {
		return
	}
	a.Store.LogEvent(map[string]any{
		"type": "api", "action": "accounts_verify", "mode": mode, "zone": addr,
		"coding": coding, "accounts": total, "usable": summary["usable"],
		"unusable": summary["unusable"], "not_exists": summary["not_exists"],
		"error": summary["error"], "elapsed_ms": d.Milliseconds(),
	})
}

func jobSummary(snap accountverify.JobSnapshot) map[string]int {
	return map[string]int{
		"total": snap.Total, "usable": snap.Usable, "unusable": snap.Unusable,
		"not_exists": snap.NotExists, "error": snap.Error,
	}
}

func verifyMsg(sum map[string]int, d time.Duration) string {
	s := "验证完成：可用 " + strconv.Itoa(sum["usable"]) + " · 不可用 " + strconv.Itoa(sum["unusable"]) +
		" · 不存在 " + strconv.Itoa(sum["not_exists"])
	if sum["error"] > 0 {
		s += " · 失败 " + strconv.Itoa(sum["error"])
	}
	return s + "（耗时 " + d.Round(time.Millisecond).String() + "）"
}

// codingOfAddr 按游戏服地址反查区编码（查不到按 UTF-8）。
func (a *API) codingOfAddr(addr string) string {
	if a.Zones == nil {
		return zones.CodingUTF8
	}
	for _, f := range a.Zones.Flat() {
		if f.Addr() == addr {
			return f.Coding
		}
	}
	return zones.CodingUTF8
}
