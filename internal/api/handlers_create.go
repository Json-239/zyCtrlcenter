package api

import (
	"context"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"zyctrlcenter/internal/accountcreate"
	"zyctrlcenter/internal/accountverify"
	"zyctrlcenter/internal/config"
	"zyctrlcenter/internal/gameproto"
	"zyctrlcenter/internal/services/accounts"
)

// createMaxBatch 单次建号上限（注册是"写库"操作，且同 IP 过频会触发风控：112）。
const createMaxBatch = accounts.MaxCreateNames

// ---------------------------------------------------------------- 注册自适应限速（2026-09-22）
//
// 现状：并发 ≤8、批量 ≤20；**同 IP 过频 → 风控码 112**（规则在服务端 C++，改不动）。
// 这里只改"节奏"，**不动协议字段**（106/104/700 与 8 个字段保持原样）：
//   - 连续 112 → 有效并发减半（下限 MinConcurrency，默认 2）、批间隔加倍（上限 CreateMaxIntervalSec=60s）；
//   - 连续成功 CreateRecoverAfter（10）次 → 并发 +1（回到 MaxConcurrency 止）、批间隔减半（回到基准止）；
//   - 批间隔带 [−JitterSec, +JitterSec] 抖动（默认 ±2s），避免整齐节拍被风控盯上；
//   - 统计随建号响应回带：throttle{concurrency,batch_interval_sec,streak_112,total_112,total_ok}。
//
// 进程内一个实例（api.New 按 config 的 CTRL_CREATE_* 建；带锁：并发建号的 goroutine 可安全喂结果）。
const (
	// CreateRecoverAfter 连续成功多少次 → 回升一步（并发 +1 / 批间隔减半）。
	CreateRecoverAfter = 10
	// CreateMaxIntervalSec 批间隔上限（秒）：撞 112 时逐次加倍，到顶为止。
	CreateMaxIntervalSec = 60
)

// CreateRateConfig 限速器配置（来自 config 的 CTRL_CREATE_*；进程启动时定一次）。
type CreateRateConfig struct {
	Adaptive        bool // 自适应开关（false = 只统计、不调速，抖动仍生效）
	MaxConcurrency  int  // 并发上限（默认 8）
	MinConcurrency  int  // 并发下限（降速降到这里为止，默认 2）
	BaseIntervalSec int  // 批间隔基准（秒，默认 5）
	JitterSec       int  // 批间隔抖动（±秒，默认 2）
}

// ThrottleStats 限速器对外统计（建号响应的 throttle 字段；面板/运维可观测）。
type ThrottleStats struct {
	Concurrency      int `json:"concurrency"`       // 当前有效并发
	BatchIntervalSec int `json:"batch_interval_sec"` // 当前批间隔（不含抖动）
	Streak112        int `json:"streak_112"`        // 当前连续撞 112 次数
	Total112         int `json:"total_112"`         // 累计撞 112 次数
	TotalOK          int `json:"total_ok"`          // 累计成功次数
}

// CreateThrottle 注册自适应限速器（进程内共享、带锁）。
//
// 导出是为了让外部测试包（test/api）直接驱动"112 降速 → 成功回升"的状态机；
// 生产代码只用 api 实例上那一个（api.New 建）。
type CreateThrottle struct {
	mu   sync.Mutex
	cfg  CreateRateConfig
	conc int // 当前有效并发
	gap  int // 当前批间隔（秒，不含抖动）

	streak112 int
	streakOK  int
	total112  int
	totalOK   int
}

// NewCreateThrottle 创建限速器（参数先夹到合法范围：脏配置不会让并发变 0、间隔变负）。
func NewCreateThrottle(cfg CreateRateConfig) *CreateThrottle {
	if cfg.MaxConcurrency < 1 {
		cfg.MaxConcurrency = 1
	}
	if cfg.MinConcurrency < 1 {
		cfg.MinConcurrency = 1
	}
	if cfg.MinConcurrency > cfg.MaxConcurrency {
		cfg.MinConcurrency = cfg.MaxConcurrency
	}
	if cfg.BaseIntervalSec < 0 {
		cfg.BaseIntervalSec = 0
	}
	if cfg.JitterSec < 0 {
		cfg.JitterSec = 0
	}
	return &CreateThrottle{cfg: cfg, conc: cfg.MaxConcurrency, gap: cfg.BaseIntervalSec}
}

// Observe 喂一次单号建号结果（code：112=风控降速；0=成功；其它=中性错误，只中断"连续成功"）。
func (t *CreateThrottle) Observe(code int32) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if code == gameproto.RegErrServerFail { // 112：同 IP 频控
		t.streak112++
		t.streakOK = 0
		t.total112++
		if t.cfg.Adaptive {
			t.slowLocked()
		}
		return
	}
	if code != 0 { // 网络/协议等中性错误：不算成功（中断回升），但也不降速
		t.streakOK = 0
		return
	}
	t.totalOK++
	t.streak112 = 0
	t.streakOK++
	if t.cfg.Adaptive && t.streakOK >= CreateRecoverAfter {
		t.streakOK = 0
		t.speedUpLocked()
	}
}

// slowLocked 撞 112：并发减半（下限）、批间隔加倍（上限）。调用方必须持有锁。
func (t *CreateThrottle) slowLocked() {
	if next := t.conc / 2; next < t.cfg.MinConcurrency {
		t.conc = t.cfg.MinConcurrency
	} else {
		t.conc = next
	}
	next := t.gap * 2
	if next < 1 {
		next = 1 // 基准是 0（不要批间隔）时，撞 112 也要能降速
	}
	if next > CreateMaxIntervalSec {
		next = CreateMaxIntervalSec
	}
	t.gap = next
}

// speedUpLocked 连续成功：并发 +1（上限）、批间隔减半（下限=基准）。调用方必须持有锁。
func (t *CreateThrottle) speedUpLocked() {
	if t.conc < t.cfg.MaxConcurrency {
		t.conc++
	}
	if t.gap > t.cfg.BaseIntervalSec {
		if next := t.gap / 2; next < t.cfg.BaseIntervalSec {
			t.gap = t.cfg.BaseIntervalSec
		} else {
			t.gap = next
		}
	}
}

// Stats 当前统计（响应回带 / 测试断言）。
func (t *CreateThrottle) Stats() ThrottleStats {
	t.mu.Lock()
	defer t.mu.Unlock()
	return ThrottleStats{Concurrency: t.conc, BatchIntervalSec: t.gap,
		Streak112: t.streak112, Total112: t.total112, TotalOK: t.totalOK}
}

// Pace 取本轮的节奏：返回 (有效并发, 批间隔含抖动)。
//
//   - 并发 = min(请求并发, 限速器当前并发)：**请求只能更保守**（限速器降速后压得住）；
//   - 批间隔 = max(请求基准, 限速器当前值) + [−jitter, +jitter]（抖动按秒取整、不落负）；
//   - rnd 传 nil 用 math/rand（测试可注入固定源）。
func (t *CreateThrottle) Pace(reqConcurrency, reqIntervalMs int, rnd func(int) int) (int, time.Duration) {
	t.mu.Lock()
	conc, gap, jitter := t.conc, t.gap, t.cfg.JitterSec
	t.mu.Unlock()
	if reqConcurrency > 0 && reqConcurrency < conc {
		conc = reqConcurrency
	}
	gapMs := gap * 1000
	if reqIntervalMs > gapMs {
		gapMs = reqIntervalMs
	}
	if jitter > 0 {
		if rnd == nil {
			rnd = rand.Intn
		}
		delta := (rnd(2*jitter+1) - jitter) * 1000 // [-jitter, +jitter] 秒
		if gapMs+delta < 0 {
			gapMs = 0
		} else {
			gapMs += delta
		}
	}
	return conc, time.Duration(gapMs) * time.Millisecond
}

// createRateConfigOf 从全局配置取限速参数（缺省保守：8 并发 / ≥2 / 5s 基准 / ±2s 抖动 / 自适应开）。
func createRateConfigOf(cfg *config.Config) CreateRateConfig {
	out := CreateRateConfig{Adaptive: true, MaxConcurrency: 8, MinConcurrency: 2, BaseIntervalSec: 5, JitterSec: 2}
	if cfg == nil {
		return out
	}
	out.Adaptive = cfg.CreateAdaptive
	if cfg.CreateMaxConcurrency > 0 {
		out.MaxConcurrency = cfg.CreateMaxConcurrency
	}
	if cfg.CreateMinConcurrency > 0 {
		out.MinConcurrency = cfg.CreateMinConcurrency
	}
	if cfg.CreateBatchIntervalSec >= 0 {
		out.BaseIntervalSec = cfg.CreateBatchIntervalSec
	}
	if cfg.CreateJitterSec >= 0 {
		out.JitterSec = cfg.CreateJitterSec
	}
	return out
}

// createThrottleSignal 把单号建号结果翻译成限速信号：
// 112（注册被风控）→ 降速；有错误（网络/协议/探测失败）→ 中性；其余（成功/已存在/密码不符）→ 成功。
func createThrottleSignal(out apiCreateResult) int32 {
	switch {
	case out.ErrID == gameproto.RegErrServerFail:
		return gameproto.RegErrServerFail
	case out.Err != "":
		return -1
	}
	return 0
}

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
// 节奏（**只改节奏，不动协议字段**）：并发/批间隔由进程内自适应限速器兜底——
// 撞 112 降速（并发减半、批间隔加倍）、连续成功回升；请求里的 concurrency / batch_interval_ms
// 只能**更保守**（不会比限速器当前值更快）。配置见 CTRL_CREATE_*（config.go）。
//
// 返回：`{ok, mode:"create", zone, game_addr, coding, results[…], created, failed, pool_updated,
//
//	throttle:{concurrency,batch_interval_sec,streak_112,total_112,total_ok}, msg}`
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
	reqConc := toInt(body["concurrency"], 2)
	if reqConc < 1 {
		reqConc = 1
	}
	// 请求里的 batch_interval_ms 给的是**基准**：限速器降速后以限速器为准（请求只能更保守）。
	reqBatchMs := toInt(body["batch_interval_ms"], 0)
	if reqBatchMs < 0 {
		reqBatchMs = 0
	}
	if reqBatchMs > 60000 {
		reqBatchMs = 60000
	}
	// 自适应限速：并发上限 = min(请求并发, 限速器当前值)；批间隔 = max(请求基准, 限速器当前值) + 抖动。
	// 撞 112 时限速器会降速（并发减半/批间隔加倍），连续成功后再逐步回升。
	lim := a.createLimiter()
	concurrency, batchGap := lim.Pace(reqConc, reqBatchMs, nil)
	batchInterval := int(batchGap / time.Millisecond)

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
			"elapsed_ms": time.Since(start).Milliseconds(),
			"concurrency": concurrency, "throttle_112": lim.Stats().Total112})
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
	stats := lim.Stats()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": created+existing > 0, "mode": "create", "zone": zoneKeyOf(r, body), "game_addr": gameAddr,
		"coding": coding, "version": opt.Version, "results": results,
		"first": preview["first"], "last": preview["last"], "names_count": len(names),
		"batch_size": batchSize, "concurrency": concurrency,
		"throttle": stats, // 注册自适应限速的当前节奏（112 降速 / 成功回升 / 累计统计）
		"created": created, "existing": existing, "password_mismatch": mismatch,
		"failed": failed, "pool_updated": updated, "msg": msg,
	})
}

// createLimiter 本进程的注册自适应限速器（api.New 建；一个进程一份，跨请求共享状态）。
func (a *API) createLimiter() *CreateThrottle { return a.throttle }

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
	defer func() {
		out.ElapsedMs = time.Since(start).Milliseconds()
		// 注册自适应限速：把这一轮的结果喂给限速器（112 → 降速；成功 → 连续计数，够了再回升）。
		// 自动注册（autotask 的 registerForAuto）也走这个函数 → 两条建号路径共享同一节奏。
		a.createLimiter().Observe(createThrottleSignal(out))
	}()

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
