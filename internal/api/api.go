// Package api HTTP API + WebSocket（单机器人进程 / 多区可切换）。
//
// 路由总表（单一事实源见 docs/03-协议/HTTP-API.md）：
//
//	GET  /                    入口提示
//	GET  /api/status          机器人/通道/进程状态总览（含多区列表与当前区）
//	GET  /api/chains          链列表（文件驱动）+ 指定链详情 ?id=<文件名>
//	GET  /api/modules         模块地图（注册表 + 最近一次测试报告）
//	GET  /api/maps            地图名表（mapid → 名称）+ 客户端坐标口径 grid_cell
//	GET  /api/tasknames       任务号 → 任务名（读游戏配置 task/*.xml；只读）
//	GET  /api/map/grid        某地图的网格（阻挡位图）+ 地图名（地图可视化页用）
//	GET  /api/accounts        账号池列表（池 + 运行时状态合并）+ 统计
//	GET  /api/accounts/stats  账号池统计（按区）
//	POST /api/accounts/add    加号入池（可分配区）（可选鉴权）
//	POST /api/accounts/remove 从池里删号（可选鉴权）
//	POST /api/accounts/verify 账号可用性验证（直接连游戏服跑登录协议 102→300，可选鉴权）
//	                           带 accounts = 同步验证这几个；不带 = 从账号池按区选号批量跑（返回 job）
//	GET  /api/accounts/verify/job    批量验证任务进度（?id=，缺省最近一个）
//	POST /api/accounts/verify/cancel 停止批量验证任务（在途结果丢弃）（可选鉴权）
//	POST /api/robots/batch    批量上线/下线机器人（分批发 + 间隔）（可选鉴权）
//	GET  /api/logs?n=         运行历史尾部
//	POST /api/logs/clear      清空运行日志（可选鉴权）
//	GET  /api/protocols       协议映射（只读）
//	GET  /api/config          多区配置（服/区列表 + 当前区）
//	POST /api/config/switch   切换当前区（低风险：只改"当前选择"）
//	POST /api/config/servers  新增/更新服（含其下区列表）（可选鉴权）
//	POST /api/config/zones    新增/更新区（可选鉴权）
//	POST /api/config/servers/delete  删除服（可选鉴权）
//	POST /api/config/zones/delete    删除区（可选鉴权）
//	POST /api/config/apply    把区写进机器人 config.py（可选重启机器人）（可选鉴权）
//	POST /api/start           启动任务链（可带 chain 原样透传）
//	POST /api/stop            停链
//	POST /api/reset           重置重跑
//	POST /api/robots/manage   动态添加/移除机器人（可选鉴权）
//	POST /api/robot/restart   重启机器人进程（可选鉴权）
//	GET  /api/autotask        定时自动任务状态（新手链/抓鬼/孵化三套策略 + 候选数 + 待恢复名单）
//	POST /api/autotask/start  启动/改参数某套定时任务（可选鉴权）
//	POST /api/autotask/stop   停止某套定时任务（可选鉴权）
//	POST /api/autotask/run    立即跑一轮（可选鉴权）
//	POST /api/random_walk     下发游荡（目标图 + 链载荷；可选鉴权）
//	POST /api/random_walk/stop    停止游荡（可选鉴权）
//	POST /api/reghost/cancel  取消某个号的卡死自动重登恢复（可选鉴权）
//	GET  /ws                  实时事件推送（WebSocket）
package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"zyctrlcenter/internal/accountverify"
	"zyctrlcenter/internal/chainlib"
	"zyctrlcenter/internal/config"
	"zyctrlcenter/internal/ctrl"
	"zyctrlcenter/internal/logging"
	"zyctrlcenter/internal/maplib"
	"zyctrlcenter/internal/services/accounts"
	"zyctrlcenter/internal/services/autotask"
	"zyctrlcenter/internal/services/event"
	"zyctrlcenter/internal/services/process"
	"zyctrlcenter/internal/services/reghost"
	"zyctrlcenter/internal/services/restorer"
	"zyctrlcenter/internal/services/zones"
	"zyctrlcenter/internal/state"
	"zyctrlcenter/internal/taskname"
	"zyctrlcenter/internal/store"
)

// Deps API 的依赖集合（装配在 main.go，避免 New 参数越加越长）。
type Deps struct {
	Cfg      *config.Config
	St       *state.State
	Store    *store.Store
	Ctrl     *ctrl.Server
	Log      *logging.Logger
	Proc     *process.Manager
	Zones    *zones.Registry
	Maps     *maplib.Table      // 地图名表（mapid → 名称）
	Grids    *maplib.GridReader // 地图网格（阻挡位图，地图可视化用）
	TaskNames *taskname.Table   // 任务号 → 任务名（只读游戏配置 <GameConfigDir>/task/*.xml）
	Accounts *accounts.Pool     // 账号池
	Verify   *accountverify.Manager
	// Restorer 恢复引擎（P1，可为 nil：面板只显示意图，不给"立即补发"）
	Restorer *restorer.Runner // 批量可用性验证任务（后台跑 + 进度查询 + 停止）
	// AutoTask 定时自动任务引擎（可为 nil：面板只显示不可用；main 用 AutoTaskDeps 装配）
	AutoTask *autotask.Runner
	// Reghost 卡死自动重登恢复（可为 nil；main 用 ReghostDeps 装配）
	Reghost *reghost.Runner
	// Payloads 链数据载荷提供者（可为 nil：按 Cfg 懒建。与 restorer 共用同一份缓存时显式注入）
	Payloads *Payloads
	Events   *event.Handler
	WS       *WSHub
}

// API 聚合全部依赖（路由与服务通过它访问）。
type API struct {
	Deps

	payloadOnce sync.Once
	// hatch 孵化会话记账（到期收工/完成清理；见 internal/api/hatch.go）。
	hatch *hatchSessions
}

// New 创建 API。
func New(d Deps) *API { return &API{Deps: d, hatch: newHatchSessions()} }

// chainPayloads 取载荷提供者（未显式注入时按 Cfg 懒建，测试/嵌入式用法不必装配）。
func (a *API) chainPayloads() *Payloads {
	a.payloadOnce.Do(func() {
		if a.Payloads == nil {
			a.Payloads = NewPayloads(a.Cfg)
		}
	})
	return a.Payloads
}

// Register 注册全部路由。
func (a *API) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", a.handleIndex) // 仅精确匹配 "/"
	mux.HandleFunc("/", a.handleNotFound)     // 其余路径统一 404 JSON
	mux.HandleFunc("GET /api/status", a.handleStatus)
	mux.HandleFunc("GET /api/chains", a.handleChains)
	mux.HandleFunc("GET /api/modules", a.handleModules) // 只读：模块地图（read 接口不鉴权）
	mux.HandleFunc("GET /api/maps", a.handleMaps)
	mux.HandleFunc("GET /api/tasknames", a.handleTaskNames) // 只读：任务号 → 任务名（面板把编号翻成人话）
	mux.HandleFunc("GET /api/map/grid", a.handleMapGrid)
	mux.HandleFunc("GET /api/accounts", a.handleAccountsList)
	mux.HandleFunc("GET /api/accounts/stats", a.handleAccountsStats)
	mux.HandleFunc("POST /api/accounts/add", a.requireToken(a.handleAccountsAdd))
	mux.HandleFunc("POST /api/accounts/remove", a.requireToken(a.handleAccountsRemove))
	mux.HandleFunc("POST /api/accounts/create", a.requireToken(a.handleAccountsCreate))
	mux.HandleFunc("POST /api/accounts/verify", a.requireToken(a.handleAccountsVerify))
	mux.HandleFunc("GET /api/accounts/verify/job", a.handleAccountsVerifyJob)
	mux.HandleFunc("POST /api/accounts/verify/cancel", a.requireToken(a.handleAccountsVerifyCancel))
	mux.HandleFunc("POST /api/robots/batch", a.requireToken(a.handleRobotsBatch))
	mux.HandleFunc("GET /api/logs", a.handleLogs)
	mux.HandleFunc("POST /api/logs/clear", a.requireToken(a.handleLogsClear))
	mux.HandleFunc("GET /api/intents", a.handleIntents)
	mux.HandleFunc("POST /api/intents/restore", a.requireToken(a.handleIntentsRestore))
	// 定时自动任务（新手链 / 抓鬼 / 孵化 三套独立策略）+ 卡死自动重登恢复
	mux.HandleFunc("GET /api/autotask", a.handleAutoTaskGet)
	mux.HandleFunc("POST /api/autotask/start", a.requireToken(a.handleAutoTaskStart))
	mux.HandleFunc("POST /api/autotask/stop", a.requireToken(a.handleAutoTaskStop))
	mux.HandleFunc("POST /api/autotask/run", a.requireToken(a.handleAutoTaskRun))
	// 游荡（通用入口：孵化去半月岛 / 其它巡游图）——命令带链载荷，机器人端 random_walk.py 执行
	mux.HandleFunc("POST /api/random_walk", a.requireToken(a.handleRandomWalk))
	mux.HandleFunc("POST /api/random_walk/stop", a.requireToken(a.handleRandomWalkStop))
	mux.HandleFunc("POST /api/reghost/cancel", a.requireToken(a.handleRegHostCancel))
	mux.HandleFunc("GET /api/protocols", a.handleProtocols)
	mux.HandleFunc("GET /api/config", a.handleConfigGet)
	mux.HandleFunc("POST /api/config/switch", a.handleConfigSwitch)
	mux.HandleFunc("POST /api/config/servers", a.requireToken(a.handleConfigServerUpsert))
	mux.HandleFunc("POST /api/config/zones", a.requireToken(a.handleConfigZoneUpsert))
	mux.HandleFunc("POST /api/config/servers/delete", a.requireToken(a.handleConfigServerDelete))
	mux.HandleFunc("POST /api/config/zones/delete", a.requireToken(a.handleConfigZoneDelete))
	mux.HandleFunc("POST /api/config/apply", a.requireToken(a.handleConfigApply))
	mux.HandleFunc("POST /api/start", a.handleStart)
	mux.HandleFunc("POST /api/stop", a.handleStop)
	mux.HandleFunc("POST /api/reset", a.handleReset)
	mux.HandleFunc("POST /api/robots/manage", a.requireToken(a.handleRobotsManage))
	mux.HandleFunc("POST /api/robot/restart", a.requireToken(a.handleRobotRestart))
	mux.HandleFunc("POST /api/robot/reload_scripts", a.requireToken(a.handleReloadScripts)) // 脚本热更（importlib.reload，免重启机器人）
	mux.HandleFunc("/ws", a.handleWS)
}

// Handler 返回带 CORS 的根 handler。
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	a.Register(mux)
	return withCORS(mux)
}

// ---------------------------------------------------------------- 中间件

// withCORS 放开跨源（面板可部署在任意端口/机器访问中控）。
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-API-Token")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireToken 可选鉴权：配置了 token 才校验（默认关闭）。
func (a *API) requireToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimSpace(a.Cfg.APIToken)
		if token != "" && !tokenOK(r, token) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"ok": false, "msg": "未授权：请携带 Authorization: Bearer <token> 或 X-API-Token",
			})
			return
		}
		next(w, r)
	}
}

func tokenOK(r *http.Request, token string) bool {
	if strings.TrimSpace(r.Header.Get("X-API-Token")) == token {
		return true
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	return strings.HasPrefix(auth, "Bearer ") &&
		strings.TrimSpace(strings.TrimPrefix(auth, "Bearer ")) == token
}

// ---------------------------------------------------------------- 工具

func writeJSON(w http.ResponseWriter, code int, obj any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(obj)
}

func readBody(r *http.Request) map[string]any {
	if r.Body == nil {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.NewDecoder(r.Body).Decode(&out); err != nil || out == nil {
		return map[string]any{}
	}
	return out
}

// bodyAccounts 解析 accounts 字段：支持 ["a","b"] / "a,b" / [["a","pwd"]]
// normAccounts 归一化"勾选/粘贴来的账号集合"：去首尾空白、丢空串、**保序去重**。
// 面板上线是点选（跨页勾选、快捷选择）来的，重复或带空白很常见，不能把同一个号下发两次。
func normAccounts(in []string) []string {
	if len(in) == 0 {
		return in
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, n := range in {
		n = strings.TrimSpace(n)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

func bodyAccounts(body map[string]any) []string {
	raw, ok := body["accounts"].([]any)
	if !ok {
		if s, ok := body["accounts"].(string); ok {
			return splitList(s)
		}
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		switch t := v.(type) {
		case string:
			if t != "" {
				out = append(out, t)
			}
		case []any:
			if len(t) >= 1 {
				if name, _ := t[0].(string); name != "" {
					out = append(out, name)
				}
			}
		}
	}
	return out
}

// accountPairs 解析 accounts 字段为 [账号, 密码] 对。
// **密码只有两个来源**：请求里显式给的，或账号库里存的（由调用方按区去取）；
// 这里不给任何"默认密码"（没有统一密码这回事）。
func (a *API) accountPairs(body map[string]any) [][2]string {
	raw, _ := body["accounts"].([]any)
	if len(raw) == 0 {
		names := splitList(toStr(body["accounts"]))
		pairs := make([][2]string, 0, len(names))
		for _, n := range names {
			pairs = append(pairs, [2]string{n, ""})
		}
		return pairs
	}
	pairs := make([][2]string, 0, len(raw))
	for _, item := range raw {
		switch t := item.(type) {
		case string:
			if t != "" {
				pairs = append(pairs, [2]string{t, ""})
			}
		case []any:
			if len(t) == 0 {
				continue
			}
			name, _ := t[0].(string)
			if name == "" {
				continue
			}
			pwd := ""
			if len(t) >= 2 {
				pwd, _ = t[1].(string)
			}
			pairs = append(pairs, [2]string{name, pwd})
		}
	}
	return pairs
}

func splitList(s string) []string {
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == ' ' || r == '，' || r == '、'
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
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

func toInt(v any, def int) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(t)); err == nil {
			return n
		}
	}
	return def
}

func toBool(v any, def bool) bool {
	switch t := v.(type) {
	case bool:
		return t
	case float64:
		return t != 0
	}
	return def
}

func okMsg(ok bool, okText string) string {
	if ok {
		return okText
	}
	return "下发失败：机器人通道未连接"
}

// zoneKeyOf 从请求体/查询串取区 key（"<服key>/<区key>"）——用于 /api/config/apply 等配置接口。
func zoneKeyOf(r *http.Request, body map[string]any) string {
	if v := toStr(body["zone"]); strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return strings.TrimSpace(r.URL.Query().Get("zone"))
}

// chainInfo 链列表（文件驱动）。
func (a *API) chainInfo() []chainlib.Info {
	return chainlib.List(a.Cfg.ChainDir)
}
