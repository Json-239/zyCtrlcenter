package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"zyctrlcenter/internal/accountcreate"
	"zyctrlcenter/internal/accountverify"
	"zyctrlcenter/internal/gameproto"
	"zyctrlcenter/internal/services/accounts"
)

// createMaxBatch 单次建号上限（注册是"写库"操作，且同 IP 过频会触发风控：112）。
const createMaxBatch = accounts.MaxCreateNames

// createMaxConcurrency 建号并发上限（再高更容易触发同 IP 频控）。
const createMaxConcurrency = 8

// handleAccountsCreate 建号：中控**直接连游戏服**走注册协议（106 取验证码 → 104 注册 → 700 判结果）。
//
// 密码规则（**没有统一密码**）：每个号一个**随机密码**，注册成功后写进账号库（该区），
// 之后登录/验证一律用库里这个密码。请求里给了 `password` 才用请求的（一般不用给）。
//
// 请求体：
//
//	accounts    ["a","b"] / "a,b"   必填（要建的账号名，上限 20 个/次）
//	zone        "<服key>/<区key>" 或 "host:port"（缺省=当前区）
//	password    可选：指定密码（默认随机生成 16 位）
//	password_len 随机密码长度（默认 16，最小 8）
//	agent_key   可选：写进注册包"姓名"位（落库用于区分机器人）
//	interval_ms 每个号之间间隔（默认 300ms，降低风控概率）
//	timeout_sec 单号超时（默认 12s）
//	persist     是否把成功的号写进账号库（默认 true）
//
// 返回：`{ok, mode:"create", zone, game_addr, coding, results[…], created, failed, pool_updated, msg}`
func (a *API) handleAccountsCreate(w http.ResponseWriter, r *http.Request) {
	body := readBody(r)
	// 账号名两种给法：显式列表，或"按编号"（前缀 + 起始序号 + 数量 + 邮箱后缀）自动生成
	names := bodyAccounts(body)
	if len(names) == 0 {
		names = splitList(toStr(body["names"]))
	}
	spec := accounts.NameSpec{
		Prefix:    strings.TrimSpace(toStr(body["prefix"])),
		Start:     toInt(body["start"], 0),
		Count:     toInt(body["count"], 0),
		Pad:       toInt(body["pad"], 0),
		Suffix:    strings.TrimSpace(toStr(body["suffix"])),
		AutoStart: toBool(body["auto_start"], false),
	}
	if len(names) == 0 && spec.Prefix != "" && a.Accounts != nil {
		generated, err := a.Accounts.BuildNames(spec)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": err.Error()})
			return
		}
		names = generated
	}
	names = normAccounts(names) // 去空白/空串/重复（面板点选与手输都会带这些）
	if len(names) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "msg": "请给账号：accounts（指定名字）或 前缀（prefix）+起始序号（start）+数量（count）按编号生成",
		})
		return
	}
	if len(names) > createMaxBatch {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "msg": "一次最多建 " + strconv.Itoa(createMaxBatch) + " 个号（注册是同 IP 写库操作，过频会触发风控）",
		})
		return
	}
	if a.Accounts == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "账号库未启用"})
		return
	}

	gameAddr := a.gameAddrOf(zoneKeyOf(r, body))
	if gameAddr == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "没有可用的区：请先配置区，或显式传 zone"})
		return
	}
	coding := a.codingOfAddr(gameAddr)

	opt := accountcreate.DefaultOptions()
	opt.Coding = coding
	if v := strings.TrimSpace(toStr(body["version"])); v != "" {
		opt.Version = v
	} else {
		opt.Version = a.Cfg.GameVersion
	}
	timeoutSec := toInt(body["timeout_sec"], int(accountcreate.DefaultTimeout/time.Second))
	if timeoutSec < 1 {
		timeoutSec = 1
	}
	if timeoutSec > 30 {
		timeoutSec = 30
	}
	opt.Timeout = time.Duration(timeoutSec) * time.Second
	opt.AgentKey = strings.TrimSpace(toStr(body["agent_key"]))

	pwdLen := toInt(body["password_len"], accountcreate.DefaultPasswordLen)
	if pwdLen < accountcreate.MinPasswordLen {
		pwdLen = accountcreate.DefaultPasswordLen
	}
	fixedPwd := strings.TrimSpace(toStr(body["password"]))
	intervalMs := toInt(body["interval_ms"], 300)
	if intervalMs < 0 {
		intervalMs = 0
	}
	if intervalMs > 10000 {
		intervalMs = 10000
	}
	persist := toBool(body["persist"], true)

	// 分批 + 并发（参考实现同样是"每批 N 个、并发 M 个"；过高易触发同 IP 频控 112）
	batchSize := toInt(body["batch_size"], 10)
	if batchSize < 1 {
		batchSize = 1
	}
	if batchSize > len(names) {
		batchSize = len(names)
	}
	concurrency := toInt(body["concurrency"], 2)
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > createMaxConcurrency {
		concurrency = createMaxConcurrency
	}
	batchInterval := toInt(body["batch_interval_ms"], 800)
	if batchInterval < 0 {
		batchInterval = 0
	}
	if batchInterval > 10000 {
		batchInterval = 10000
	}

	ctx, cancel := context.WithTimeout(r.Context(),
		time.Duration(len(names)*(timeoutSec+1))*time.Second+10*time.Second)
	defer cancel()

	// 先验证用的探测选项（与建号共用编码/版本/超时）
	probeOpt := accountverify.DefaultOptions()
	probeOpt.Coding = coding
	probeOpt.Version = opt.Version
	probeOpt.Timeout = opt.Timeout
	probeOpt.QueryRole = true

	// 分批 + 批内并发：结果按下标存，**顺序与输入一致**（面板好对应）
	// 整批走"原子写窗"（BeginBatch/EndBatch）：期间禁止热重载，结束后统一落盘 ——
	// 否则并发写回会被另一次 refresh 的重载冲掉（密码就丢了）。
	start := time.Now()
	slots := make([]apiCreateResult, len(names))
	filled := make([]bool, len(names))
	if persist {
		a.Accounts.BeginBatch()
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for from := 0; from < len(names); from += batchSize {
		to := from + batchSize
		if to > len(names) {
			to = len(names)
		}
		for i := from; i < to; i++ {
			if ctx.Err() != nil {
				break
			}
			sem <- struct{}{}
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				defer func() { <-sem }()
				slots[i] = a.createOrReuse(ctx, names[i], gameAddr, fixedPwd, pwdLen, opt, probeOpt, persist)
				filled[i] = true
			}(i)
			if intervalMs > 0 && i < to-1 {
				select {
				case <-time.After(time.Duration(intervalMs) * time.Millisecond):
				case <-ctx.Done():
				}
			}
		}
		wg.Wait()
		if to < len(names) && batchInterval > 0 {
			select {
			case <-time.After(time.Duration(batchInterval) * time.Millisecond):
			case <-ctx.Done():
			}
		}
	}

	results := make([]apiCreateResult, 0, len(names))
	status := map[string]int{}
	for i := range names {
		if !filled[i] { // 被 ctx 掐断的：明确标未执行，不静默丢
			slots[i] = apiCreateResult{Account: names[i], Zone: gameAddr, Status: "failed",
				RetCode: -1, Msg: "未执行（请求已取消/超时）", At: time.Now().Unix()}
		}
		results = append(results, slots[i])
		status[slots[i].Status]++
	}
	created, existing := status["created"], status["existing"]
	mismatch, failed := status["password_mismatch"], status["failed"]
	updated := 0
	if persist {
		if err := a.Accounts.EndBatch(); err == nil {
			updated = created + existing + mismatch
		}
	}

	if a.Store != nil {
		a.Store.LogEvent(map[string]any{"type": "api", "action": "accounts_create",
			"zone": gameAddr, "coding": coding, "requested": len(names), "created": created,
			"existing": existing, "password_mismatch": mismatch, "failed": failed,
			"elapsed_ms": time.Since(start).Milliseconds()})
	}

	msg := "建号/校验完成：新建 " + strconv.Itoa(created) + " · 已存在 " + strconv.Itoa(existing)
	if mismatch > 0 {
		msg += " · 密码不符 " + strconv.Itoa(mismatch)
	}
	msg += " · 失败 " + strconv.Itoa(failed)
	if created > 0 {
		msg += "（新号密码已写入账号库，登录用库里的密码）"
	}
	if created == 0 && existing == 0 && mismatch == 0 {
		msg = "没有成功建号（详见 results）。提示：注册过频会回 errid=112，可稍后重试"
	}
	preview := spec.Preview(names)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": created+existing > 0, "mode": "create", "zone": zoneKeyOf(r, body), "game_addr": gameAddr,
		"coding": coding, "version": opt.Version, "results": results,
		"first": preview["first"], "last": preview["last"], "names_count": len(names),
		"batch_size": batchSize, "concurrency": concurrency,
		"created": created, "existing": existing, "password_mismatch": mismatch,
		"failed": failed, "pool_updated": updated, "msg": msg,
	})
}

// applyCreateResult 把"建号成功"的结果写进账号库：该区密码 + 标记未验证（还没登录过）。
func (a *API) applyCreateResult(res accountcreate.Result, addr string) {
	if a.Accounts == nil || !res.OK {
		return
	}
	st := accounts.ZoneState{Password: res.Password, Verified: false, Msg: "建号成功（待验证）"}
	a.Accounts.SetZoneStateNoSave(res.Account, addr, st)
}

// apiCreateResult 建号/校验的单个结果（status 区分分支）。
type apiCreateResult struct {
	Account   string `json:"account"`
	Zone      string `json:"zone"`
	Status    string `json:"status"` // created / existing / password_mismatch / failed
	OK        bool   `json:"ok"`
	Password  string `json:"password,omitempty"`
	RetCode   int32  `json:"ret_code"`
	Exists    bool   `json:"exists"`
	Usable    bool   `json:"usable"`
	RoleName  string `json:"role_name,omitempty"`
	Level     int    `json:"level,omitempty"`
	DbRet     int32  `json:"db_ret,omitempty"`
	ErrID     int32  `json:"err_id,omitempty"`
	Retryable bool   `json:"retryable,omitempty"`
	Msg       string `json:"msg"`
	ElapsedMs int64  `json:"elapsed_ms"`
	At        int64  `json:"at"`
	Err       string `json:"err,omitempty"`
}

// createOrReuse 单个账号的"先验证、再注册"流程（参考 py 中控的做法，实现自研）：
//
//  1. 定密码：请求指定 → **账号库里该服的密码**（按服通用）→ 都没有才随机生成
//  2. 先按这个密码跑一次登录探测（102）：
//     ret=0    → 账号已存在且密码可用：**跳过注册**，写回池（可用 + 角色/等级）
//     200/300  → 账号存在但密码不符/冻结：**不注册**（注册必回 111），标"存在但不可用"
//     100      → 该服没有这个号 → 走注册（104）
//  3. 注册成功 → 密码写进账号库；失败（111/112/116…）→ 带原因返回（112 可退避重试）
//
// 探测出错（网络/超时/编码）时**不注册**（避免重复建号），也不改动库里的可用性结论。
func (a *API) createOrReuse(ctx context.Context, name, addr, fixedPwd string, pwdLen int,
	regOpt accountcreate.Options, probeOpt accountverify.Options, persist bool) (out apiCreateResult) {
	start := time.Now()
	out = apiCreateResult{Account: name, Zone: addr, RetCode: -1, At: start.Unix()}
	defer func() { out.ElapsedMs = time.Since(start).Milliseconds() }()

	pwd := fixedPwd
	if pwd == "" && a.Accounts != nil { // 库里该服的密码（没有统一密码）
		if v, ok := a.Accounts.PasswordFor(name, addr); ok {
			pwd = v
		}
	}
	// 库里是"脏密码"（老数据长度不合服务端 6~20 规则）→ 建号时换新的随机密码，
	// 否则拿它去注册必然失败（也解释不清为什么失败）。
	replacedOld := false
	if pwd != "" && (len([]rune(pwd)) < 6 || len([]rune(pwd)) > 20) {
		pwd, replacedOld = "", true
	}
	if pwd == "" {
		gen, err := accountcreate.NewPassword(pwdLen)
		if err != nil {
			out.Status, out.Msg, out.Err = "failed", err.Error(), err.Error()
			return out
		}
		pwd = gen
	}

	probe := accountverify.Probe(ctx, addr, name, pwd, probeOpt)
	out.RetCode, out.Exists, out.Usable = probe.RetCode, probe.Exists, probe.Usable
	out.RoleName, out.Level = probe.RoleName, probe.Level

	switch {
	case probe.Err != "":
		out.Status = "failed"
		out.Err = probe.Err
		out.Msg = "校验账号失败（未注册，避免重复建号）：" + probe.Err
	case probe.RetCode == gameproto.LoginOK:
		out.Status, out.OK, out.Password = "existing", true, pwd
		out.Msg = "账号已存在且库里密码可用（跳过注册）"
		if persist {
			a.writeBackVerify(name, addr, pwd, probe, "校验通过（已存在）")
		}
	case probe.Exists:
		out.Status = "password_mismatch"
		out.Password = pwd
		out.Msg = "账号已存在但库里密码不匹配（" + probe.Msg + "）：把库里密码改成服务端密码，或删号重建"
		if persist {
			a.writeBackVerify(name, addr, pwd, probe, out.Msg)
		}
	default:
		reg := accountcreate.Register(ctx, addr, name, pwd, regOpt)
		out.DbRet, out.ErrID, out.Retryable, out.Err = reg.DbRet, reg.ErrID, reg.Retryable, reg.Err
		if reg.OK {
			out.Status, out.OK, out.Password = "created", true, pwd
			out.Msg = "建号成功（密码已写入账号库）"
			if replacedOld {
				out.Msg = "建号成功（库里的老密码不合规则，已改用新的随机密码并写入账号库）"
			}
			if persist {
				a.Accounts.Add([]string{name}, "", addr, "建号")
				a.applyCreateResult(reg, addr)
			}
		} else {
			out.Status, out.Msg = "failed", reg.Msg
		}
	}
	return out
}

// writeBackVerify 把"已存在账号"的校验结果写回库（保留原密码，只更新可用性/角色/等级）。
func (a *API) writeBackVerify(name, addr, pwd string, probe accountverify.Result, msg string) {
	if a.Accounts == nil {
		return
	}
	a.Accounts.Add([]string{name}, "", addr, "")
	a.Accounts.SetZoneStateNoSave(name, addr, accounts.ZoneState{
		Password:   pwd,
		Verified:   true,
		Usable:     probe.Usable,
		Msg:        msg,
		RoleName:   probe.RoleName,
		Level:      probe.Level,
		VerifiedAt: time.Now().Unix(),
	})
}
