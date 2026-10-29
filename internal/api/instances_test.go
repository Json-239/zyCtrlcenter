// 实例管理 API 层单测（2026-10-14 实例管理页 V1）：
//   - /api/ping：本机 200 {"ok":true,"ts"}；非本机 403；
//   - GET /api/instances：列表形状（注册字段 + status 全字段）；非本机 403；
//   - POST /{id}/stop：自身实例拒绝（"不能停止自身实例"，不杀任何进程）；
//   - DELETE /{id}：confirm 校验（不匹配拒绝/匹配归档）；
//   - ctrlMatchHint：--data-dir / --deploy / = 形式 / 缺失。
package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"zyctrlcenter/internal/config"
	"zyctrlcenter/internal/services/event"
	"zyctrlcenter/internal/services/instances"
	"zyctrlcenter/internal/services/process"
	"zyctrlcenter/internal/state"
	"zyctrlcenter/internal/store"
	"zyctrlcenter/test/testsupport"
)

// newInstancesEnv API 测试环境（真实 API 路由 + 注入 fake 的实例服务；不跑 PowerShell/不杀进程）。
func newInstancesEnv(t *testing.T) (*API, *instances.Service) {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.ChainDir = filepath.Join(cfg.DataDir, "chains")
	cfg.DeployDir = t.TempDir()
	cfg.RobotExe = filepath.Join(cfg.DeployDir, "robot_single_robot.exe")
	st := state.New()
	runStore := store.New(t.TempDir(), 30, 500)
	log := testsupport.NewTestLogger(t)
	ch := testsupport.NewTestChannel(t)
	ev := event.New(cfg, st, runStore, ch, log)
	a := New(Deps{
		Cfg: cfg, St: st, Store: runStore, Ctrl: ch, Log: log,
		Proc:   process.New(cfg.RobotExe, cfg.DeployDir, "robot_single_robot.exe", nil),
		Zones:  testsupport.NewTestZones(t),
		Events: ev, WS: NewWSHub(nil),
	})
	svc := instances.New(instances.Options{
		ExeDir: t.TempDir(), DataDir: cfg.DataDir, WebPort: cfg.WebPort,
	})
	svc.RunPowerShell = func(string) ([]byte, error) { return []byte(""), nil }
	svc.KillPID = func(int) error { return nil }
	svc.Sleep = func(time.Duration) {}
	svc.Now = func() time.Time { return time.Date(2026, 10, 14, 12, 0, 0, 0, time.Local) }
	a.Instances = svc
	return a, svc
}

// doAPI 直发请求（remote 控制 RemoteAddr；body 空=不带体），返回 recorder + JSON 文档。
func doAPI(t *testing.T, a *API, method, target, remote, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	if remote != "" {
		req.RemoteAddr = remote
	}
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, req)
	var doc map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &doc)
	}
	return rec, doc
}

func TestPingEndpoint(t *testing.T) {
	a, _ := newInstancesEnv(t)

	rec, doc := doAPI(t, a, http.MethodGet, "/api/ping", "127.0.0.1:5000", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/ping 本机应 200，实际 %d", rec.Code)
	}
	if doc["ok"] != true {
		t.Fatalf("ok 应为 true: %v", doc)
	}
	if ts, ok := doc["ts"].(float64); !ok || ts <= 0 {
		t.Fatalf("ts 应为 unix 秒: %v", doc["ts"])
	}

	// 非本机 → 403
	rec2, doc2 := doAPI(t, a, http.MethodGet, "/api/ping", "192.0.2.1:5000", "")
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("/api/ping 非本机应 403，实际 %d", rec2.Code)
	}
	if doc2["ok"] != false {
		t.Fatalf("403 响应 ok 应为 false: %v", doc2)
	}

	// ::1 也算本机
	rec3, _ := doAPI(t, a, http.MethodGet, "/api/ping", "[::1]:5000", "")
	if rec3.Code != http.StatusOK {
		t.Fatalf("/api/ping IPv6 回环应 200，实际 %d", rec3.Code)
	}
}

func TestInstancesListShapeAndLocalOnly(t *testing.T) {
	a, svc := newInstancesEnv(t)
	// 种子（用冷端口，避免测试连到真实服务）+ 一条附加实例
	if _, err := svc.Load(instances.Instance{WebPort: 28111, CtrlPort: 28112,
		DataDir: "d1", DeployDir: "de1", CtrlMatch: `--data-dir "d1"`}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Add(instances.Instance{ID: "cold", Name: "冷实例", WebPort: 39999, CtrlPort: 39998,
		DataDir: "d2", DeployDir: "de2", StartScript: "_start_x.bat", CtrlMatch: `--data-dir "d2"`}); err != nil {
		t.Fatal(err)
	}

	rec, doc := doAPI(t, a, http.MethodGet, "/api/instances", "127.0.0.1:5000", "")
	if rec.Code != http.StatusOK || doc["ok"] != true {
		t.Fatalf("列表应 200/ok，实际 %d %v", rec.Code, doc)
	}
	rows, _ := doc["instances"].([]any)
	if len(rows) != 2 {
		t.Fatalf("应有 2 条实例，实际 %d", len(rows))
	}
	row := rows[1].(map[string]any) // 附加的 cold（顺序 = 注册表顺序）
	for _, k := range []string{"id", "name", "zone_server", "zone_key", "web_port", "ctrl_port",
		"data_dir", "deploy_dir", "start_script", "ctrl_match", "created_at", "status"} {
		if _, ok := row[k]; !ok {
			t.Fatalf("列表行缺字段 %s: %v", k, row)
		}
	}
	st, _ := row["status"].(map[string]any)
	if st == nil {
		t.Fatalf("status 缺失: %v", row)
	}
	for _, k := range []string{"ctrl_alive", "ctrl_pid", "http_ok", "online", "robot_alive", "robot_pid", "checked_at"} {
		if _, ok := st[k]; !ok {
			t.Fatalf("status 缺字段 %s: %v", k, st)
		}
	}
	// 冷端口（39999 无人监听）→ http_ok=false 且 online=-1
	if st["http_ok"] != false || st["online"].(float64) != -1 {
		t.Fatalf("冷实例应 http_ok=false online=-1，实际 %v", st)
	}

	// 非本机 → 403
	rec2, _ := doAPI(t, a, http.MethodGet, "/api/instances", "192.0.2.1:5000", "")
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("列表非本机应 403，实际 %d", rec2.Code)
	}
}

func TestInstancesStopSelfOverHTTP(t *testing.T) {
	a, svc := newInstancesEnv(t)
	if _, err := svc.Load(instances.Instance{WebPort: svc.WebPort(), CtrlPort: svc.WebPort() + 1,
		DataDir: "self", DeployDir: "self", CtrlMatch: `--data-dir "self"`}); err != nil {
		t.Fatal(err)
	}
	killed := false
	svc.KillPID = func(int) error { killed = true; return nil }

	id := "p" + strconv.Itoa(svc.WebPort())
	rec, doc := doAPI(t, a, http.MethodPost, "/api/instances/"+id+"/stop", "127.0.0.1:1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("stop 应 200（业务拒绝），实际 %d", rec.Code)
	}
	if doc["ok"] != false || doc["msg"] != "不能停止自身实例" {
		t.Fatalf("自身 stop 应拒绝: %v", doc)
	}
	if killed {
		t.Fatal("自身实例不得杀任何进程")
	}

	// 未知实例 → ok:false
	_, doc2 := doAPI(t, a, http.MethodPost, "/api/instances/nope/stop", "127.0.0.1:1", "")
	if doc2["ok"] != false {
		t.Fatalf("未知实例应 ok:false: %v", doc2)
	}
}

func TestInstancesDeleteOverHTTP(t *testing.T) {
	a, svc := newInstancesEnv(t)
	base := t.TempDir()
	dataDir := filepath.Join(base, "data-7777")
	deployDir := filepath.Join(base, "single_robot_zy_7777")
	if err := mkdirAllT(dataDir, deployDir); err != nil {
		t.Fatal(err)
	}
	if err := svc.Add(instances.Instance{ID: "p7777", WebPort: 7777, CtrlPort: 7776,
		DataDir: dataDir, DeployDir: deployDir, CtrlMatch: `--data-dir "x"`}); err != nil {
		t.Fatal(err)
	}

	// confirm 不匹配 → 拒绝
	_, doc := doAPI(t, a, http.MethodDelete, "/api/instances/p7777", "127.0.0.1:1", `{"confirm":"nope"}`)
	if doc["ok"] != false {
		t.Fatalf("confirm 不匹配应拒绝: %v", doc)
	}
	if _, err := statT(dataDir); err != nil {
		t.Fatal("被拒绝时不得归档")
	}

	// confirm 匹配 → 归档 + 注册表移除
	_, doc2 := doAPI(t, a, http.MethodDelete, "/api/instances/p7777", "127.0.0.1:1", `{"confirm":"p7777"}`)
	if doc2["ok"] != true {
		t.Fatalf("正确 confirm 应成功: %v", doc2)
	}
	arch, _ := doc2["archived"].(map[string]any)
	if len(arch) == 0 {
		t.Fatalf("应返回 archived 映射: %v", doc2)
	}
	want := dataDir + "_archived_20261014" // Now 已注入固定日期
	if arch[dataDir] != want {
		t.Fatalf("data 归档路径错误: %v（want %s）", arch, want)
	}
	if _, err := statT(dataDir); err == nil {
		t.Fatal("原 data 目录应已归档")
	}
	if _, err := statT(want); err != nil {
		t.Fatal("归档目录应存在")
	}
}

func TestInstancesUnavailable503(t *testing.T) {
	a, _ := newInstancesEnv(t)
	a.Instances = nil // 模拟注册表加载失败
	rec, doc := doAPI(t, a, http.MethodGet, "/api/instances", "127.0.0.1:1", "")
	if rec.Code != http.StatusServiceUnavailable || doc["ok"] != false {
		t.Fatalf("未装配应 503: %d %v", rec.Code, doc)
	}
}

func TestCtrlMatchHint(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"--deploy", `F:\x\deploy\single_robot_zy`}, `--deploy "F:\x\deploy\single_robot_zy"`},
		{[]string{"--web-port", "28083", "--data-dir", `F:\d\data-2300`}, `--data-dir "F:\d\data-2300"`},
		{[]string{`--data-dir=F:\d\data-9`}, `--data-dir "F:\d\data-9"`},
		{[]string{`--deploy=F:\x\deploy`}, `--deploy "F:\x\deploy"`},
		{[]string{"--auto-robot"}, ""},
		{nil, ""},
	}
	for _, c := range cases {
		if got := ctrlMatchHint(c.args); got != c.want {
			t.Fatalf("ctrlMatchHint(%v) = %q，want %q", c.args, got, c.want)
		}
	}
}

// ---------------------------------------------------------------- 小工具

func mkdirAllT(dirs ...string) error {
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}

func statT(path string) (os.FileInfo, error) { return os.Stat(path) }
