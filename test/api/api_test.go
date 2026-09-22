// api 模块测试：路由 / 状态总览 / 下发内容（机器人实收）/ 多区配置（切换·增删·应用）/ 鉴权。
package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zyctrlcenter/internal/accountverify"
	"zyctrlcenter/internal/api"
	"zyctrlcenter/internal/config"
	"zyctrlcenter/internal/ctrl"
	"zyctrlcenter/internal/logging"
	"zyctrlcenter/internal/maplib"
	"zyctrlcenter/internal/services/accounts"
	"zyctrlcenter/internal/services/autotask"
	"zyctrlcenter/internal/services/event"
	"zyctrlcenter/internal/services/intent"
	"zyctrlcenter/internal/services/process"
	"zyctrlcenter/internal/services/reghost"
	"zyctrlcenter/internal/services/restorer"
	"zyctrlcenter/internal/services/zones"
	"zyctrlcenter/internal/state"
	"zyctrlcenter/internal/store"
	"zyctrlcenter/internal/taskname"
	"zyctrlcenter/test/testsupport"
)

// testEnv 一个完整的测试环境：HTTP 测试服务器 + 已启动的测试控制通道 + 多区注册表。
type testEnv struct {
	srv        *httptest.Server
	st         *state.State
	store      *store.Store
	ctrl       *ctrl.Server // 已启动的测试通道（假机器人连它）
	pool       *accounts.Pool
	ev         *event.Handler // 事件处理器（用例里直接 HandleEvent 驱动分支）
	zreg       *zones.Registry
	cfg        *config.Config
	zoneKey    string
	gameConfig string   // 游戏配置目录（测试内构造 map.csv + blockfile）
	api        *api.API // API 实例（需要直调非 HTTP 方法时用，如 LoadAutoTask）
}

func newTestEnv(t *testing.T, token string) *testEnv {
	return newTestEnvInDir(t, token, "")
}

// newTestEnvInDir 同 newTestEnv，但可复用给定数据目录（模拟"重启中控后"再建一个环境：
// 落盘文件还在、内存态全新）。
func newTestEnvInDir(t *testing.T, token, dataDir string) *testEnv {
	t.Helper()
	cfg := config.Default()
	cfg.APIToken = token
	if dataDir != "" {
		cfg.DataDir = dataDir
	} else {
		cfg.DataDir = t.TempDir()
	}
        cfg.ChainDir = filepath.Join(cfg.DataDir, "chains")
        cfg.DeployDir = t.TempDir() // 单进程：部署目录（config.py 写入目标）
        cfg.RobotExe = filepath.Join(cfg.DeployDir, "robot_single_robot.exe")
        // 建号节奏：测试里的假游戏服没有"同 IP 风控"，批间隔/抖动置 0（跑得快、不空等）；
        // 生产默认（5s 基准 + ±2s 抖动）由 test/api/create_throttle_test.go 断言 config.Default()。
        cfg.CreateBatchIntervalSec, cfg.CreateJitterSec = 0, 0

	c := testsupport.NewTestChannel(t) // 端口 0 + 区标记 test-srv/z1
	zreg := testsupport.NewTestZones(t)
	maps := testsupport.NewTestMaps(t) // 裁剪后的真实地图表（夹具）
	pool := testsupport.NewTestAccounts(t)
	gameConfig := filepath.Join(t.TempDir(), "game-config") // 地图网格（用例内按需构造）
	grids := maplib.NewGridReader(gameConfig)

	st := state.New()
	runStore := store.New(t.TempDir(), 30, 500)
	log := logging.New(t.TempDir(), 30)
	t.Cleanup(func() { _ = log.Close() })
	proc := process.New(cfg.RobotExe, cfg.DeployDir, "robot_single_robot.exe", nil)
	ev := event.New(cfg, st, runStore, c, log)
	wsHub := api.NewWSHub(nil)
	var skipHook func(kind, account string) (bool, string) // 抓鬼等级门槛（api 层实现，晚绑定）
	payloads := api.NewPayloads(cfg)                       // 链载荷（抓鬼导航数据）——api 与恢复引擎共用一份缓存
	a := api.New(api.Deps{
		Cfg: cfg, St: st, Store: runStore, Ctrl: c, Log: log,
		Proc: proc, Zones: zreg, Maps: maps, Grids: grids, TaskNames: taskname.New(gameConfig), Accounts: pool,
		Verify: accountverify.NewManager(),
		Events: ev, WS: wsHub,
		Restorer: restorer.New(restorer.Deps{ // 手动补发用；Enabled 关着（与线上默认一致）
			Enabled:   func() bool { return false },
			ChannelUp: func() bool { return c.Connected() },
			Intents:   func() []intent.Intent { return ev.Intents.Snapshot() },
			Robot:     func(a string) (state.Robot, bool) { return st.Get(a) },
			ErrRepeat: func(a string) int { r, _ := st.Get(a); return r.ErrRepeat },
			Send:      ev.SendCmd,
			Payload:   payloads.For,
			Skip: func(kind, account string) (bool, string) {
				if skipHook == nil {
					return false, ""
				}
				return skipHook(kind, account)
			},
			GhostDailyLimit: func() int { return payloads.GhostDailyLimit() },
		}),
		Payloads: payloads,
	})

	skipHook = a.GhostSkipFunc()
	a.AutoTask = autotask.New(a.AutoTaskDeps()) // 定时任务：用 api 的候选/上线/注册/下发适配器
	a.Reghost = reghost.New(a.ReghostDeps())
	ev.SetReghoster(a.Reghost.Request) // 卡死自动重登恢复：测试里也要接上

	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	return &testEnv{srv: srv, st: st, store: runStore, ctrl: c, pool: pool, ev: ev,
		zreg: zreg, cfg: cfg, zoneKey: testsupport.TestZoneKey, gameConfig: gameConfig, api: a}
}

func getJSON(t *testing.T, url string) map[string]any {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s 失败: %v", url, err)
	}
	defer resp.Body.Close()
	return decodeBody(t, resp.Body)
}

func postJSON(t *testing.T, url string, body any, headers map[string]string) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s 失败: %v", url, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, decodeBody(t, resp.Body)
}

func decodeBody(t *testing.T, r io.Reader) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.NewDecoder(r).Decode(&out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	return out
}

// writeChainFile 造一个链数据文件。
func writeChainFile(t *testing.T, cfg *config.Config, id, content string) {
	t.Helper()
	if err := os.MkdirAll(cfg.ChainDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.ChainDir, id+".json"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeRobotConfig 在部署目录下造 script/config.py（内容取自夹具），返回其路径。
func writeRobotConfig(t *testing.T, deployDir string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(testsupport.TestRoot(t), "fixtures", "robotcfg", "config_py.sample.json"))
	if err != nil {
		t.Fatalf("读取 config.py 夹具失败: %v", err)
	}
	var fx struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(deployDir, "script")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.py")
	if err := os.WriteFile(path, []byte(fx.Content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// ---------------------------------------------------------------- 基础

func TestIndexReturnsHint(t *testing.T) {
	env := newTestEnv(t, "")
	body := getJSON(t, env.srv.URL+"/")
	if name, _ := body["name"].(string); !strings.Contains(name, "zyctrlcenter") {
		t.Fatalf("入口应返回中控提示，实际 %v", body)
	}
	if body["ws"] != "/ws" {
		t.Fatalf("入口应提示 WS 端点，实际 %v", body["ws"])
	}
	ports, _ := body["ports"].(map[string]any)
	if ports["web"] != float64(env.cfg.WebPort) || ports["ctrl"] != float64(env.cfg.CtrlPort) {
		t.Fatalf("入口应透出端口配置，实际 %v", ports)
	}
}

func TestStatusIncludesZonesAndCurrent(t *testing.T) {
	env := newTestEnv(t, "")
	env.st.Update("robotA", func(r *state.Robot) {
		r.Zone = env.zoneKey
		r.Online = true
		r.State = "DIALOG"
		r.HS = true
	})

	body := getJSON(t, env.srv.URL+"/api/status")
	if body["ok"] != true {
		t.Fatalf("status 应返回 ok=true，实际 %v", body["ok"])
	}
	robots, _ := body["robots"].([]any)
	if len(robots) != 1 {
		t.Fatalf("robots 应有 1 行，实际 %v", body["robots"])
	}
	row, _ := robots[0].(map[string]any)
	if row["zone"] != env.zoneKey {
		t.Fatalf("机器人行应带区归属，实际 %v", row["zone"])
	}
	// 区列表：注册表里的候选区（本用例只有 1 个），并标出当前区
	zs, _ := body["zones"].([]any)
	if len(zs) != 1 {
		t.Fatalf("zones 应有 1 条，实际 %v", body["zones"])
	}
	zone, _ := zs[0].(map[string]any)
	if zone["key"] != env.zoneKey || zone["current"] != true {
		t.Fatalf("区列表不符: %v", zone)
	}
	if zone["addr"] != "127.0.0.1:2300" || zone["coding"] != "UTF-8" {
		t.Fatalf("区地址/编码不符: %v", zone)
	}
	cur, _ := body["current"].(map[string]any)
	if cur["key"] != env.zoneKey {
		t.Fatalf("current 不符: %v", cur)
	}
	// 单通道：状态里透出通道地址与当前区标记
	if body["ctrl_addr"] == "" || body["ctrl_zone"] != env.zoneKey {
		t.Fatalf("应透出控制通道地址与区标记: ctrl_addr=%v ctrl_zone=%v",
			body["ctrl_addr"], body["ctrl_zone"])
	}
	counts, _ := body["counts"].(map[string]any)
	if counts["online"] != float64(1) || counts["handshake"] != float64(1) {
		t.Fatalf("counts 不符: %v", counts)
	}
}

// ---------------------------------------------------------------- 链与命令

func TestStartWithoutRobotExplainsReason(t *testing.T) {
	env := newTestEnv(t, "")
	code, body := postJSON(t, env.srv.URL+"/api/start", map[string]any{"accounts": []string{"robotA"}}, nil)
	if code != http.StatusOK {
		t.Fatalf("HTTP 状态应为 200，实际 %d", code)
	}
	if body["ok"] != false {
		t.Fatalf("通道未连接时 ok 应为 false，实际 %v", body["ok"])
	}
	if msg, _ := body["msg"].(string); !strings.Contains(msg, "通道未连接") {
		t.Fatalf("失败必须给出原因（通道未连接），实际 %q", msg)
	}
	if body["chain_source"] != "none" {
		t.Fatalf("无链文件时 chain_source 应为 none，实际 %v", body["chain_source"])
	}
}

func TestStartDeliversChainFromFile(t *testing.T) {
	env := newTestEnv(t, "")
	writeChainFile(t, env.cfg, "my_chain_file", `{
	  "chain_id": "inner_chain",
	  "start_task": 7001001,
	  "end_task": 5010105,
	  "npcs": {"13035": [[12, 3931, 882]]},
	  "task_order": [{"task_index": 7001001, "name": "天命所归", "catcher_npc": "10103", "next": [7001002]}],
	  "custom_block": {"keep": true}
	}`)

	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	code, body := postJSON(t, env.srv.URL+"/api/start",
		map[string]any{"chain_id": "my_chain_file", "accounts": []string{"robotA"}}, nil)
	if code != http.StatusOK || body["ok"] != true {
		t.Fatalf("下发失败: %d %v", code, body)
	}
	if body["chain_source"] != "file" || body["chain_id"] != "inner_chain" {
		t.Fatalf("链来源/ID 不符: %v", body)
	}

	cmd := rb.ReadCmd(t, 2*time.Second)
	if cmd["cmd"] != "start_chain" || cmd["chain_id"] != "inner_chain" {
		t.Fatalf("机器人收到的命令不符（应以文件内 chain_id 为准）: %v", cmd)
	}
	chain, _ := cmd["chain"].(map[string]any)
	if chain["chain_id"] != "inner_chain" || chain["grid_cell"] != float64(16) {
		t.Fatalf("链数据缺省补齐不符: %v", chain)
	}
	if _, ok := chain["custom_block"]; !ok {
		t.Fatalf("链数据未知字段必须原样透传: %v", chain)
	}
	if accounts, _ := cmd["accounts"].([]any); len(accounts) != 1 || accounts[0] != "robotA" {
		t.Fatalf("accounts 应透传: %v", cmd["accounts"])
	}
}

func TestStartPassesThroughInlineChain(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	inline := map[string]any{"chain_id": "inline", "start_task": 1, "end_task": 2, "my_ext": []any{1, 2, 3}}
	_, body := postJSON(t, env.srv.URL+"/api/start", map[string]any{"chain": inline}, nil)
	if body["chain_source"] != "request" {
		t.Fatalf("chain_source 应为 request，实际 %v", body["chain_source"])
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	chain, _ := cmd["chain"].(map[string]any)
	if chain["chain_id"] != "inline" || len(chain["my_ext"].([]any)) != 3 {
		t.Fatalf("内联 chain 必须原样下发: %v", chain)
	}
}

// 地图名表 + 客户端坐标口径：面板据此把 mapid 显示成中文名、把像素显示成格子。
func TestMapsEndpoint(t *testing.T) {
	env := newTestEnv(t, "")
	body := getJSON(t, env.srv.URL+"/api/maps")
	if body["ok"] != true {
		t.Fatalf("maps 应返回 ok=true: %v", body)
	}
	if body["grid_cell"] != float64(env.cfg.GridCell) || env.cfg.GridCell != 16 {
		t.Fatalf("应透出坐标口径 grid_cell=16，实际 %v", body["grid_cell"])
	}
	maps, _ := body["maps"].(map[string]any)
	if len(maps) == 0 {
		t.Fatalf("地图表不应为空: %v", body)
	}
	// 真实数据抽查：11=长安东市集、24=幽冥界（来自 config/map.csv）
	if maps["11"] != "长安东市集" || maps["24"] != "幽冥界" {
		t.Fatalf("地图名不符: 11=%v 24=%v", maps["11"], maps["24"])
	}
	// status 里也应给出条目数与口径（表本身在 /api/maps）
	st := getJSON(t, env.srv.URL+"/api/status")
	if st["maps_count"] != float64(len(maps)) {
		t.Fatalf("status.maps_count 不符: %v", st["maps_count"])
	}
}

func TestChainsEndpointFileDriven(t *testing.T) {
	env := newTestEnv(t, "")

	body := getJSON(t, env.srv.URL+"/api/chains")
	if chains, _ := body["chains"].([]any); len(chains) != 0 {
		t.Fatalf("空目录应返回空列表，实际 %v", body["chains"])
	}

	writeChainFile(t, env.cfg, "chain_b", `{"chain_id":"chain_b","start_task":100,"end_task":200,"task_order":[{},{}]}`)
	writeChainFile(t, env.cfg, "chain_a", `{"chain_id":"inner_a","start_task":1,"end_task":2,"task_order":[{}]}`)
	writeChainFile(t, env.cfg, "_template", `{"chain_id":"template"}`)

	body = getJSON(t, env.srv.URL+"/api/chains")
	chains, _ := body["chains"].([]any)
	if len(chains) != 2 {
		t.Fatalf("应列出 2 条链（跳过 _template），实际 %v", body["chains"])
	}
	first, _ := chains[0].(map[string]any)
	if first["id"] != "chain_a" || first["task_count"] != float64(1) || first["chain_id"] != "inner_a" {
		t.Fatalf("链列表条目不符（id=文件名，chain_id=文件内声明）: %v", chains)
	}

	detail := getJSON(t, env.srv.URL+"/api/chains?id=chain_a")
	if detail["chain_found"] != true {
		t.Fatalf("chain_a 应能找到: %v", detail)
	}
	if chain, _ := detail["chain"].(map[string]any); chain["chain_id"] != "inner_a" {
		t.Fatalf("链详情不符: %v", detail["chain"])
	}
	missing := getJSON(t, env.srv.URL+"/api/chains?id=nope")
	if missing["chain_found"] != false || missing["msg"] == nil {
		t.Fatalf("缺失链应给出提示: %v", missing)
	}
}

func TestRobotsManageAddDeliversPairs(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	// robotA 不给密码 → 密码从账号库按区取（**没有统一密码**）
	env.pool.Add([]string{"robotA"}, "pwd-from-pool", env.zoneKey, "")

	code, body := postJSON(t, env.srv.URL+"/api/robots/manage",
		map[string]any{"action": "add", "accounts": []any{"robotA", []any{"robotB", "pwdB"}}}, nil)
	if code != http.StatusOK || body["ok"] != true {
		t.Fatalf("add 下发失败: %d %v", code, body)
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	if cmd["cmd"] != "robot_manage" || cmd["action"] != "add" {
		t.Fatalf("机器人收到的命令不符: %v", cmd)
	}
	accounts, _ := cmd["accounts"].([]any)
	if len(accounts) != 2 {
		t.Fatalf("应有 2 个账号，实际 %v", cmd["accounts"])
	}
	b, _ := accounts[1].([]any)
	if b[0] != "robotB" || b[1] != "pwdB" {
		t.Fatalf("显式密码应透传: %v", accounts)
	}
	a, _ := accounts[0].([]any)
	if a[0] != "robotA" || a[1] != "pwd-from-pool" {
		t.Fatalf("未给密码时应用库里的密码: %v", accounts)
	}

	// 库里没密码的账号：明确拒绝（不拿默认密码去试）
	_, body = postJSON(t, env.srv.URL+"/api/robots/manage",
		map[string]any{"action": "add", "accounts": []any{"not-in-pool"}}, nil)
	if body["ok"] != false {
		t.Fatalf("库里没密码应拒绝下发: %v", body)
	}
	if msg, _ := body["msg"].(string); !strings.Contains(msg, "没有密码") {
		t.Fatalf("提示应说明库里没密码: %v", body["msg"])
	}

	_, body = postJSON(t, env.srv.URL+"/api/robots/manage",
		map[string]any{"action": "remove", "accounts": []string{"robotA"}}, nil)
	if body["ok"] != true {
		t.Fatalf("remove 下发失败: %v", body)
	}
	if !env.st.IsRemoved("robotA") {
		t.Fatal("remove 后应本地标记已移除")
	}
	if cmd := rb.ReadCmd(t, 2*time.Second); cmd["action"] != "remove" {
		t.Fatalf("机器人收到的 remove 命令不符: %v", cmd)
	}
}

func TestStopAndResetDeliverAccounts(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	if _, body := postJSON(t, env.srv.URL+"/api/stop", map[string]any{"accounts": []string{"robotA"}}, nil); body["ok"] != true {
		t.Fatalf("停链下发失败: %v", body)
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	if cmd["cmd"] != "stop" {
		t.Fatalf("应下发 stop: %v", cmd)
	}
	if accounts, _ := cmd["accounts"].([]any); len(accounts) != 1 {
		t.Fatalf("stop 应带 accounts: %v", cmd)
	}

	if _, body := postJSON(t, env.srv.URL+"/api/reset", map[string]any{}, nil); body["ok"] != true {
		t.Fatalf("重置下发失败: %v", body)
	}
	if cmd := rb.ReadCmd(t, 2*time.Second); cmd["cmd"] != "reset" {
		t.Fatalf("应下发 reset: %v", cmd)
	}
}

// ---------------------------------------------------------------- 多区配置（切换式）

func TestConfigGetAndSwitch(t *testing.T) {
	env := newTestEnv(t, "")

	body := getJSON(t, env.srv.URL+"/api/config")
	if body["ok"] != true {
		t.Fatalf("config 应返回 ok=true: %v", body)
	}
	if servers, _ := body["servers"].([]any); len(servers) != 1 {
		t.Fatalf("应有 1 个服，实际 %v", body["servers"])
	}
	if _, ok := body["zones_file"].(string); !ok {
		t.Fatalf("应透出 zones.json 路径: %v", body["zones_file"])
	}
	defaults, _ := body["defaults"].(map[string]any)
	if defaults["default_coding"] != "UTF-8" {
		t.Fatalf("默认编码应为 UTF-8: %v", defaults)
	}
	if path, _ := defaults["config_path"].(string); !strings.Contains(path, "script") {
		t.Fatalf("应给出机器人 config.py 路径: %v", defaults)
	}

	// 新增一个区（2400），再切换到它
	if _, resp := postJSON(t, env.srv.URL+"/api/config/zones", map[string]any{
		"server": testsupport.TestServerKey,
		"zone":   map[string]any{"key": "2400", "name": "2区", "port": 2400},
	}, nil); resp["ok"] != true {
		t.Fatalf("新增区失败: %v", resp)
	}

	code, sw := postJSON(t, env.srv.URL+"/api/config/switch",
		map[string]any{"key": testsupport.TestServerKey + "/2400"}, nil)
	if code != http.StatusOK || sw["ok"] != true {
		t.Fatalf("切换失败: %d %v", code, sw)
	}
	if cur, _ := sw["current"].(map[string]any); cur["key"] != testsupport.TestServerKey+"/2400" {
		t.Fatalf("切换后 current 不符: %v", sw["current"])
	}
	// 状态接口也应反映当前区（含控制通道的事件标记）
	st := getJSON(t, env.srv.URL+"/api/status")
	if cur, _ := st["current"].(map[string]any); cur["key"] != testsupport.TestServerKey+"/2400" {
		t.Fatalf("status.current 不符: %v", st["current"])
	}
	if st["ctrl_zone"] != testsupport.TestServerKey+"/2400" {
		t.Fatalf("切换后控制通道的事件标记应同步，实际 %v", st["ctrl_zone"])
	}

	// 切换到不存在的区 → 失败但 HTTP 200
	_, bad := postJSON(t, env.srv.URL+"/api/config/switch", map[string]any{"key": "srv/nope"}, nil)
	if bad["ok"] != false {
		t.Fatalf("不存在的区应失败: %v", bad)
	}
}

func TestConfigZoneUpsertAndDelete(t *testing.T) {
	env := newTestEnv(t, "")

	_, resp := postJSON(t, env.srv.URL+"/api/config/zones", map[string]any{
		"server": testsupport.TestServerKey,
		"zone":   map[string]any{"key": "2301", "name": "3区", "port": 2301},
	}, nil)
	if resp["ok"] != true {
		t.Fatalf("新增区失败: %v", resp)
	}
	if zones := getJSON(t, env.srv.URL+"/api/config")["zones"].([]any); len(zones) != 2 {
		t.Fatalf("应有 2 个区，实际 %v", zones)
	}

	// 校验失败：端口非法
	_, bad := postJSON(t, env.srv.URL+"/api/config/zones", map[string]any{
		"server": testsupport.TestServerKey,
		"zone":   map[string]any{"key": "x", "port": 99999},
	}, nil)
	if bad["ok"] != false {
		t.Fatalf("非法端口应失败: %v", bad)
	}

	// 删除
	_, del := postJSON(t, env.srv.URL+"/api/config/zones/delete", map[string]any{
		"server": testsupport.TestServerKey, "zone": "2301"}, nil)
	if del["ok"] != true || del["removed"] != true {
		t.Fatalf("删除区失败: %v", del)
	}
	if got := getJSON(t, env.srv.URL+"/api/config")["zones"].([]any); len(got) != 1 {
		t.Fatalf("删除后应剩 1 个区，实际 %v", got)
	}
}

func TestConfigApplyWritesRobotConfig(t *testing.T) {
	env := newTestEnv(t, "")
	confPath := writeRobotConfig(t, env.cfg.DeployDir)

	// 把当前区端口改成 2400（夹具里是 2300，这样三个键都会被改写），编码继承服级 UTF-8
	_, upd := postJSON(t, env.srv.URL+"/api/config/zones", map[string]any{
		"server": testsupport.TestServerKey,
		"zone":   map[string]any{"key": "z1", "name": "测试区", "port": 2400},
	}, nil)
	if upd["ok"] != true {
		t.Fatalf("更新区失败: %v", upd)
	}

	_, apply := postJSON(t, env.srv.URL+"/api/config/apply", map[string]any{}, nil)
	if apply["ok"] != true {
		t.Fatalf("应用失败: %v", apply)
	}
	if apply["addr"] != "127.0.0.1:2400" || apply["coding"] != "UTF-8" {
		t.Fatalf("应用目标不符: %v", apply)
	}
	if apply["config_path"] != confPath {
		t.Fatalf("应写入部署目录下的 config.py: %v（期望 %s）", apply["config_path"], confPath)
	}
	res, _ := apply["result"].(map[string]any)
	if res["applied"] != true || len(res["changed"].([]any)) != 3 {
		t.Fatalf("应改动三个键: %v", res)
	}
	raw, _ := os.ReadFile(confPath)
	content := string(raw)
	for _, want := range []string{`ip = "127.0.0.1"`, "port = 2400", `PROTOCOL_CODING = "UTF-8"`} {
		if !strings.Contains(content, want) {
			t.Fatalf("config.py 未写入 %q\n%s", want, content)
		}
	}
	if !strings.Contains(content, "robot_task_tester = True") {
		t.Fatal("其它配置不应被改动")
	}
	if _, ok := res["backup"].(string); !ok {
		t.Fatal("应有备份路径")
	}

	// 再应用一次：内容一致 → 不改写
	_, again := postJSON(t, env.srv.URL+"/api/config/apply", map[string]any{}, nil)
	res2, _ := again["result"].(map[string]any)
	if res2["applied"] != false {
		t.Fatalf("内容一致时不应改写: %v", res2)
	}
}

// 指定别的区应用：会顺便切换当前区（保持"当前区 = 机器人登录的区"一致）。
func TestConfigApplyOtherZoneSwitchesCurrent(t *testing.T) {
	env := newTestEnv(t, "")
	writeRobotConfig(t, env.cfg.DeployDir)
	if _, resp := postJSON(t, env.srv.URL+"/api/config/zones", map[string]any{
		"server": testsupport.TestServerKey,
		"zone":   map[string]any{"key": "2400", "name": "2区", "port": 2400},
	}, nil); resp["ok"] != true {
		t.Fatalf("新增区失败: %v", resp)
	}

	_, apply := postJSON(t, env.srv.URL+"/api/config/apply",
		map[string]any{"zone": testsupport.TestServerKey + "/2400"}, nil)
	if apply["ok"] != true || apply["addr"] != "127.0.0.1:2400" {
		t.Fatalf("应用失败: %v", apply)
	}
	if _, zk := env.zreg.CurrentKeys(); zk != "2400" {
		t.Fatalf("应用指定区后当前区应同步，实际 %q", zk)
	}
	raw, _ := os.ReadFile(filepath.Join(env.cfg.DeployDir, "script", "config.py"))
	if !strings.Contains(string(raw), "port = 2400") {
		t.Fatalf("config.py 未写入目标区端口\n%s", string(raw))
	}
}

// ---------------------------------------------------------------- 账号池 / 批量上下线 / 地图网格

func TestAccountsListMergesPoolAndLive(t *testing.T) {
	env := newTestEnv(t, "")
	// 让其中一个池内账号"在线"（模拟已上线）
	env.st.Update("robot0001000@xy3.com", func(r *state.Robot) {
		r.Zone = env.zoneKey
		r.Online = true
		r.State = "NAV"
		r.Level = 33
		r.RoleName = "金永祥"
	})

	body := getJSON(t, env.srv.URL+"/api/accounts?zone=47.96.8.240:2300")
	if body["ok"] != true {
		t.Fatalf("accounts 应返回 ok=true: %v", body)
	}
	if body["pool"] != float64(3) {
		t.Fatalf("池内应有 3 个（真实夹具），实际 %v", body["pool"])
	}
	rows, _ := body["accounts"].([]any)
	if len(rows) < 3 {
		t.Fatalf("列表应有 ≥3 行，实际 %v", rows)
	}
	byName := map[string]map[string]any{}
	for _, item := range rows {
		row, _ := item.(map[string]any)
		if name, _ := row["name"].(string); name != "" {
			byName[name] = row
		}
	}
	live := byName["robot0001000@xy3.com"]
	if live["online"] != true || live["state"] != "NAV" || live["level"] != float64(33) {
		t.Fatalf("在线账号应合并运行时状态: %v", live)
	}
	if _, hasPwd := live["password"]; hasPwd {
		t.Fatal("列表接口不应返回明文密码（批量上线由服务端从池里取密码）")
	}
	if live["has_password"] != true {
		t.Fatalf("应标记已设密码: %v", live)
	}
	// 不可用号应如实标记（真实夹具：robot0001001 msg=账号不存在）
	bad := byName["robot0001001@xy3.com"]
	if bad["usable"] != false {
		t.Fatalf("不可用号应标记 usable=false: %v", bad)
	}
	// 按可用过滤
	filtered := getJSON(t, env.srv.URL+"/api/accounts?zone=47.96.8.240:2300&usable=1")
	if filtered["count"] != float64(2) {
		t.Fatalf("该区可用应 2 个，实际 %v", filtered["count"])
	}
	// 统计
	stats := getJSON(t, env.srv.URL+"/api/accounts/stats?zone=47.96.8.240:2300")
	st, _ := stats["stats"].(map[string]any)
	if st["total"] != float64(3) || st["usable"] != float64(2) {
		t.Fatalf("统计不符: %v", st)
	}
}

func TestAccountsAddAndRemove(t *testing.T) {
	env := newTestEnv(t, "")
	// 加号（带密码与区）
	_, add := postJSON(t, env.srv.URL+"/api/accounts/add", map[string]any{
		"accounts": []string{"robot0009001@xy3.com"}, "password": "pwd9001", "zone": "47.96.8.240:2300",
	}, nil)
	if add["ok"] != true {
		t.Fatalf("加号失败: %v", add)
	}
	// 批量上线时服务端能从池里取到密码（用假机器人断言下发内容）
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	_, up := postJSON(t, env.srv.URL+"/api/robots/batch", map[string]any{
		"action": "online", "accounts": []string{"robot0009001@xy3.com"},
		"zone": "47.96.8.240:2300", "interval_ms": 0, "chunk": 5,
	}, nil)
	if up["ok"] != true {
		t.Fatalf("批量上线失败: %v", up)
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	accounts, _ := cmd["accounts"].([]any)
	if len(accounts) != 1 {
		t.Fatalf("应有 1 个账号: %v", cmd)
	}
	pair, _ := accounts[0].([]any)
	if pair[0] != "robot0009001@xy3.com" || pair[1] != "pwd9001" {
		t.Fatalf("应下发 [账号,池内密码]: %v", pair)
	}

	// 下线（本地立即标记移除）
	_, down := postJSON(t, env.srv.URL+"/api/robots/batch", map[string]any{
		"action": "offline", "accounts": []string{"robot0009001@xy3.com"}, "interval_ms": 0,
	}, nil)
	if down["ok"] != true {
		t.Fatalf("批量下线失败: %v", down)
	}
	if !env.st.IsRemoved("robot0009001@xy3.com") {
		t.Fatal("下线后应本地标记移除（防心跳复活）")
	}
	if cmd := rb.ReadCmd(t, 2*time.Second); cmd["action"] != "remove" {
		t.Fatalf("机器人应收到 remove: %v", cmd)
	}

	// 从池里删除
	_, del := postJSON(t, env.srv.URL+"/api/accounts/remove", map[string]any{
		"accounts": []string{"robot0009001@xy3.com"},
	}, nil)
	if del["removed"] != float64(1) {
		t.Fatalf("删号失败: %v", del)
	}
}

// 面板上线靠"勾选集合"，跨页勾选/手滑很容易带上空白与重复：
// 服务端要归一化（去空白、丢空串、保序去重），否则重复账号会被下发两次。
func TestRobotsBatchSelectionTrimsAndDedups(t *testing.T) {
	env := newTestEnv(t, "")
	a1, a2 := "robot0009101@xy3.com", "robot0009102@xy3.com"
	_, add := postJSON(t, env.srv.URL+"/api/accounts/add", map[string]any{
		"accounts": []string{a1, a2}, "password": "pwdSel01", "zone": "47.96.8.240:2300",
	}, nil)
	if add["ok"] != true {
		t.Fatalf("加号失败: %v", add)
	}
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	_, up := postJSON(t, env.srv.URL+"/api/robots/batch", map[string]any{
		"action":   "online",
		"accounts": []string{" " + a1 + " ", a1, "", "  ", a2, a2},
		"zone":     "47.96.8.240:2300", "interval_ms": 0, "chunk": 10,
	}, nil)
	if up["sent"] != float64(2) {
		t.Fatalf("带空白/重复的勾选集合应归一化成 2 个: %v", up)
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	got, _ := cmd["accounts"].([]any)
	if len(got) != 2 {
		t.Fatalf("应下发 2 个（去重后）: %v", cmd)
	}
	for i, want := range []string{a1, a2} {
		pair, _ := got[i].([]any)
		if pair[0] != want {
			t.Fatalf("第 %d 个应=%s（保序）: %v", i, want, got[i])
		}
	}
}

func TestRobotsBatchOnlineAutoPick(t *testing.T) {
	env := newTestEnv(t, "")
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()

	// 不给账号：从池里按区选可用号（真实夹具里该区可用 2 个）
	_, up := postJSON(t, env.srv.URL+"/api/robots/batch", map[string]any{
		"action": "online", "zone": "47.96.8.240:2300", "limit": 5, "chunk": 1, "interval_ms": 0,
	}, nil)
	if up["ok"] != true || up["sent"] != float64(2) {
		t.Fatalf("自动选号上线应下发 2 个可用号: %v", up)
	}
	for i := 0; i < 2; i++ {
		cmd := rb.ReadCmd(t, 2*time.Second)
		if cmd["cmd"] != "robot_manage" || cmd["action"] != "add" {
			t.Fatalf("第 %d 批命令不符: %v", i+1, cmd)
		}
	}
	// 已在线的不再重复上线
	env.st.Update("robot0001000@xy3.com", func(r *state.Robot) { r.Online = true })
	_, again := postJSON(t, env.srv.URL+"/api/robots/batch", map[string]any{
		"action": "online", "zone": "47.96.8.240:2300", "interval_ms": 0,
	}, nil)
	if again["sent"] != float64(1) {
		t.Fatalf("应跳过已在线号（剩 1 个可用），实际 %v", again)
	}
}

func TestMapGridEndpoint(t *testing.T) {
	env := newTestEnv(t, "")
	// 构造游戏配置目录：map.csv + map_file/blockfile/<file>（4×3 网格，含阻挡）
	writeGameConfig(t, env.gameConfig)

	body := getJSON(t, env.srv.URL+"/api/map/grid?mapid=11")
	if body["ok"] != true {
		t.Fatalf("应返回网格: %v", body)
	}
	if body["name"] != "长安东市集" || body["grid_cell"] != float64(16) {
		t.Fatalf("地图名/坐标口径不符: %v %v", body["name"], body["grid_cell"])
	}
	grid, _ := body["grid"].(map[string]any)
	if grid["w"] != float64(4) || grid["h"] != float64(3) {
		t.Fatalf("网格尺寸不符: %v", grid)
	}
	rows, _ := grid["rows"].([]any)
	if len(rows) != 3 || rows[0] != "0110" || rows[1] != "1100" {
		t.Fatalf("网格行不符: %v", rows)
	}
	// 未知 mapid → 失败但 HTTP 200
	if _, bad := func() (int, map[string]any) {
		return 200, getJSON(t, env.srv.URL+"/api/map/grid?mapid=9999")
	}(); bad["ok"] != false {
		t.Fatalf("未知 mapid 应返回 ok=false: %v", bad)
	}
}

// writeGameConfig 造一个最小的游戏配置目录（map.csv + 阻挡文件）。
func writeGameConfig(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "map_file", "blockfile"), 0o755); err != nil {
		t.Fatal(err)
	}
	csv := "地图表,地图名,地图分类,地图障碍文件\n" +
		"map_index,map_name,map_type,block_file\n" +
		"11,长安东市集,普通,test_map.data\n"
	if err := os.WriteFile(filepath.Join(dir, "map.csv"), []byte(csv), 0o644); err != nil {
		t.Fatal(err)
	}
	// blockfile：w=4,h=3（uint32 LE）+ 分隔字节 + 3 行 × 4 列
	raw := []byte{4, 0, 0, 0, 3, 0, 0, 0, '\n'}
	raw = append(raw, []byte("0110\n1100\n0000\n")...)
	if err := os.WriteFile(filepath.Join(dir, "map_file", "blockfile", "test_map.data"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRobotRestartFailsWhenExeMissing(t *testing.T) {
	env := newTestEnv(t, "")
	_, resp := postJSON(t, env.srv.URL+"/api/robot/restart", map[string]any{}, nil)
	if resp["ok"] != false {
		t.Fatalf("程序不存在时应失败: %v", resp)
	}
	if msg, _ := resp["msg"].(string); !strings.Contains(msg, "不存在") {
		t.Fatalf("应说明原因（程序不存在），实际 %q", msg)
	}
}

// ---------------------------------------------------------------- 日志/协议/鉴权

func TestLogsEndpointAndClear(t *testing.T) {
	env := newTestEnv(t, "")
	env.store.LogEvent(map[string]any{"type": "log", "msg": "hello"})

	body := getJSON(t, env.srv.URL+"/api/logs?n=10")
	if logs, _ := body["logs"].([]any); len(logs) != 1 {
		t.Fatalf("日志应返回 1 条，实际 %v", body["logs"])
	}

	code, cleared := postJSON(t, env.srv.URL+"/api/logs/clear", map[string]any{}, nil)
	if code != http.StatusOK || cleared["ok"] != true {
		t.Fatalf("清空日志失败: %d %v", code, cleared)
	}
	after := getJSON(t, env.srv.URL+"/api/logs?n=10")
	if logs, _ := after["logs"].([]any); len(logs) != 1 { // 只剩清空动作自己写的审计日志
		t.Fatalf("清空后应只剩审计日志，实际 %v", after["logs"])
	}
}

func TestProtocolsEndpointSections(t *testing.T) {
	env := newTestEnv(t, "")
	body := getJSON(t, env.srv.URL+"/api/protocols")
	if body["ok"] != true {
		t.Fatal("protocols 应返回 ok=true")
	}
	sections, _ := body["sections"].([]any)
	if len(sections) != 4 {
		t.Fatalf("协议映射应有 4 组（命令/事件/HTTP API/游戏服探测），实际 %d", len(sections))
	}
	keys := map[string]bool{}
	for _, s := range sections {
		m, _ := s.(map[string]any)
		k, _ := m["key"].(string)
		keys[k] = true
		for _, f := range []string{"title", "desc", "columns", "rows"} {
			if _, ok := m[f]; !ok {
				t.Fatalf("协议节 %s 缺字段 %s: %v", k, f, m)
			}
		}
	}
	for _, want := range []string{"cmd", "evt", "api", "game"} {
		if !keys[want] {
			t.Fatalf("协议映射缺 %q 节：%v", want, keys)
		}
	}
	// 账号可用性验证的接口与协议都要在映射页可见（面板据此自查）
	game := sections[3].(map[string]any)
	if rows, _ := game["rows"].([]any); len(rows) < 9 {
		t.Fatalf("游戏服探测节应有 9 条消息，实际 %d", len(rows))
	}
	apiRows, _ := sections[2].(map[string]any)["rows"].([]any)
	found := false
	for _, r := range apiRows {
		if line, _ := r.([]any); len(line) > 1 {
			if p, _ := line[1].(string); p == "/api/accounts/verify" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("HTTP API 映射里应有 /api/accounts/verify")
	}

	cur, _ := body["current"].(map[string]any)
	if cur["current_zone"] != env.zoneKey {
		t.Fatalf("协议页应透出当前区: %v", cur)
	}
}

func TestOptionalAuthToken(t *testing.T) {
	env := newTestEnv(t, "secret-token")

	code, body := postJSON(t, env.srv.URL+"/api/robots/manage",
		map[string]any{"action": "add", "accounts": []string{"robotA"}}, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("高危写接口未带 token 应 401，实际 %d %v", code, body)
	}

	code, body = postJSON(t, env.srv.URL+"/api/robots/manage",
		map[string]any{"action": "add", "accounts": []string{"robotA"}},
		map[string]string{"X-API-Token": "secret-token"})
	if code != http.StatusOK {
		t.Fatalf("携带正确 token 应放行，实际 %d %v", code, body)
	}

	// 多区配置类写接口同样受保护
	if code, _ = postJSON(t, env.srv.URL+"/api/config/zones",
		map[string]any{"server": testsupport.TestServerKey, "zone": map[string]any{"key": "x", "port": 2300}}, nil); code != http.StatusUnauthorized {
		t.Fatalf("config/zones 未带 token 应 401，实际 %d", code)
	}
	// 切换当前区是低风险接口：不鉴权
	if code, _ = postJSON(t, env.srv.URL+"/api/config/switch", map[string]any{"key": env.zoneKey}, nil); code != http.StatusOK {
		t.Fatalf("config/switch 不应被鉴权拦截，实际 %d", code)
	}
}

func TestReadOnlyEndpointsNotProtected(t *testing.T) {
	env := newTestEnv(t, "secret-token")
	for _, path := range []string{"/api/status", "/api/config", "/api/chains", "/api/protocols"} {
		resp, err := http.Get(env.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("读接口 %s 不应被鉴权拦截，实际 %d", path, resp.StatusCode)
		}
	}
}

func TestNotFoundPath(t *testing.T) {
	env := newTestEnv(t, "")
	resp, err := http.Get(env.srv.URL + "/api/no_such")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("未知路径应 404，实际 %d", resp.StatusCode)
	}
}
