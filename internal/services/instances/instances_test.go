// instances 包单测：
//   - 注册表读写/种子/合并补全（①）；
//   - ctrl_match / deploy_dir 匹配边界（②，Windows 真跑 PowerShell 验证 like 语义）；
//   - stop 自身拒绝（③）；
//   - delete 的 confirm 校验与归档路径（④，临时目录）；
//   - 创建预检失败矩阵 + 创建全流程（⑤，临时目录+注入 fake）；
//   - parsePIDs / normalize / 端口试绑等纯逻辑。
package instances

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"zyctrlcenter/internal/services/zones"
)

// ---------------------------------------------------------------- ① 注册表

func TestRegistrySeedAndMerge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "instances.json")
	reg := NewRegistry(path)

	// 文件不存在 → 种子一条并落盘（ctrl_match 按 data_dir 派生）
	seeded, err := reg.Load(Instance{
		WebPort: 28082, CtrlPort: 27200,
		DataDir: `F:\ZyBin\zyCtrlcenter\data`, DeployDir: `F:\ZyBin\xm\deploy\single_robot_zy`,
	})
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if !seeded {
		t.Fatal("文件不存在时应种子")
	}
	if reg.Count() != 1 {
		t.Fatalf("种子后应有 1 条，实际 %d", reg.Count())
	}
	got, ok := reg.Get("p28082")
	if !ok {
		t.Fatal("种子 id 应为 p28082（按 web_port 命名）")
	}
	if want := `--data-dir "F:\ZyBin\zyCtrlcenter\data"`; got.CtrlMatch != want {
		t.Fatalf("ctrl_match 派生错误: %q != %q", got.CtrlMatch, want)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("种子应落盘: %v", err)
	}

	// 已有文件 → 不再种子、不覆盖
	seeded2, err := reg.Load(Instance{WebPort: 1})
	if err != nil {
		t.Fatalf("二次 Load 失败: %v", err)
	}
	if seeded2 {
		t.Fatal("已有文件不应再种子")
	}
	if reg.Count() != 1 {
		t.Fatalf("二次 Load 后仍应 1 条，实际 %d", reg.Count())
	}
}

func TestRegistryMergeMissingCtrlMatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "instances.json")
	body := `{"instances":[{"id":"b","name":"试点","web_port":28083,"ctrl_port":27201,
	  "data_dir":"F:\\ZyBin\\zyCtrlcenter\\data-2300","deploy_dir":"F:\\x\\single_robot_zy_2300",
	  "start_script":"_start_2300.bat"}]}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(path)
	seeded, err := reg.Load(Instance{})
	if err != nil {
		t.Fatal(err)
	}
	if seeded {
		t.Fatal("已有文件不应种子")
	}
	got, ok := reg.Get("b")
	if !ok {
		t.Fatal("应加载到实例 b")
	}
	want := `--data-dir "F:\ZyBin\zyCtrlcenter\data-2300"`
	if got.CtrlMatch != want {
		t.Fatalf("缺 ctrl_match 时应按 data_dir 合并补全: %q != %q", got.CtrlMatch, want)
	}
	// 合并结果应回写文件（避免下次启动又缺）
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "ctrl_match") {
		t.Fatal("合并补全后应回写注册表文件")
	}
}

func TestRegistryParseFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "instances.json")
	if err := os.WriteFile(path, []byte(`{bad json`), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(path)
	if _, err := reg.Load(Instance{}); err == nil {
		t.Fatal("解析失败应返回错误（宁可不动作）")
	}
}

func TestRegistryAddRemoveConflicts(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry(filepath.Join(dir, "instances.json"))
	if _, err := reg.Load(Instance{}); err != nil {
		t.Fatal(err)
	}
	base := Instance{ID: "x", WebPort: 29001, CtrlPort: 29002, CtrlMatch: `--data-dir "x"`}
	if err := reg.Add(base); err != nil {
		t.Fatal(err)
	}
	if err := reg.Add(Instance{ID: "x", WebPort: 29011, CtrlPort: 29012}); err == nil {
		t.Fatal("重复 id 应报错")
	}
	if err := reg.Add(Instance{ID: "y", WebPort: 29001, CtrlPort: 29012}); err == nil {
		t.Fatal("web 端口冲突应报错")
	}
	if err := reg.Add(Instance{ID: "z", WebPort: 29011, CtrlPort: 29002}); err == nil {
		t.Fatal("ctrl 端口冲突应报错")
	}
	removed, ok, err := reg.Remove("x")
	if err != nil || !ok || removed.ID != "x" {
		t.Fatalf("Remove 异常: %v ok=%v", err, ok)
	}
	if reg.Count() != 0 {
		t.Fatalf("移除后应为空，实际 %d", reg.Count())
	}
}

func TestNormalizeIDFallback(t *testing.T) {
	got := normalize(Instance{WebPort: 28099})
	if got.ID != "p28099" {
		t.Fatalf("id 缺省应为 p<web_port>，实际 %q", got.ID)
	}
	got2 := normalize(Instance{WebPort: 1, CtrlMatch: "keep-me"})
	if got2.CtrlMatch != "keep-me" {
		t.Fatalf("已有 ctrl_match 不应被覆盖: %q", got2.CtrlMatch)
	}
}

// ---------------------------------------------------------------- ② 匹配边界（PowerShell 真跑）

// psLikeMatch 用生产代码的表达式片段真跑 PowerShell，验证 `-like '*Escape(m)*'` 语义。
func psLikeMatch(t *testing.T, literal, cmdline string) bool {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("PowerShell like 语义验证仅 Windows")
	}
	script := "$v = " + psQuote(cmdline) + "; if " + containsExpr("$v", literal) + " { 'T' } else { 'F' }"
	out, err := runPowerShell(script)
	if err != nil {
		t.Fatalf("PowerShell 执行失败: %v", err)
	}
	return strings.TrimSpace(string(out)) == "T"
}

// psPrefixMatch 验证前缀表达式（机器人 ExecutablePath 用）。
func psPrefixMatch(t *testing.T, literal, value string) bool {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("PowerShell like 语义验证仅 Windows")
	}
	script := "$v = " + psQuote(value) + "; if " + prefixExpr("$v", literal) + " { 'T' } else { 'F' }"
	out, err := runPowerShell(script)
	if err != nil {
		t.Fatalf("PowerShell 执行失败: %v", err)
	}
	return strings.TrimSpace(string(out)) == "T"
}

func TestCtrlMatchBoundary(t *testing.T) {
	const (
		// 生产 A 栈的实际形态（启动脚本 = _restart_warm_20261014.bat）
		lineA = `"F:\ZyBin\zyCtrlcenter\zyctrlcenter.exe" --deploy "F:\ZyBin\xm\2d-xiyou-server\robot\deploy\single_robot_zy"`
		// 生产 B 栈的实际形态（启动脚本 = _start_2300.bat）
		lineB = `"F:\ZyBin\zyCtrlcenter\zyctrlcenter.exe" --web-port 28083 --ctrl-port 27201 --base-dir "F:\ZyBin\zyCtrlcenter_2300" --data-dir "F:\ZyBin\zyCtrlcenter\data-2300" --deploy "F:\ZyBin\xm\2d-xiyou-server\robot\deploy\single_robot_zy_2300" --auto-robot`
	)
	matchA := `--deploy "F:\ZyBin\xm\2d-xiyou-server\robot\deploy\single_robot_zy"`
	matchB := `--data-dir "F:\ZyBin\zyCtrlcenter\data-2300"`

	if !psLikeMatch(t, matchA, lineA) {
		t.Fatal("A 的 ctrl_match 应命中 A 命令行")
	}
	// 关键边界：`single_robot_zy"`（闭合引号）不匹配 `single_robot_zy_2300"` —— 不会误杀 B。
	if psLikeMatch(t, matchA, lineB) {
		t.Fatal("A 的 ctrl_match 不应命中 B 命令行（single_robot_zy\" vs single_robot_zy_2300\"）")
	}
	if !psLikeMatch(t, matchB, lineB) {
		t.Fatal("B 的 ctrl_match 应命中 B 命令行")
	}
	if psLikeMatch(t, matchB, lineA) {
		t.Fatal("B 的 ctrl_match 不应命中 A 命令行")
	}

	// 灵敏度反例：去掉闭合引号的"坏 match"会前缀命中 B —— 证明闭合引号是边界关键（表达式有判别力）。
	bad := `--deploy "F:\ZyBin\xm\2d-xiyou-server\robot\deploy\single_robot_zy`
	if !psLikeMatch(t, bad, lineB) {
		t.Fatal("坏 match（无闭合引号）应命中 B —— 反例不成立说明验证无灵敏度")
	}
}

func TestRobotExePathBoundary(t *testing.T) {
	dirA := `F:\ZyBin\xm\2d-xiyou-server\robot\deploy\single_robot_zy`
	exeA := dirA + `\robot_single_robot.exe`
	exeB := `F:\ZyBin\xm\2d-xiyou-server\robot\deploy\single_robot_zy_2300\robot_single_robot.exe`

	if !psPrefixMatch(t, dirA+`\`, exeA) {
		t.Fatal("A 的 deploy 前缀应命中 A 的机器人")
	}
	if psPrefixMatch(t, dirA+`\`, exeB) {
		t.Fatal("A 的 deploy 前缀不应命中 B 的机器人（反斜杠边界）")
	}
	// 灵敏度反例：不带反斜杠边界的"坏前缀"会把 zy_2300 一起命中。
	if !psPrefixMatch(t, dirA, exeB) {
		t.Fatal("坏前缀（无 \\ 边界）应命中 B —— 反例不成立说明验证无灵敏度")
	}
}

func TestPsQuoteEscape(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("PowerShell 语义验证仅 Windows")
	}
	// 单引号转义：字面串里的 ' 不破坏脚本结构。
	literal := `C:\it's\data-dir "x"`
	script := "$v = " + psQuote(literal) + "; $v"
	out, err := runPowerShell(script)
	if err != nil {
		t.Fatalf("PowerShell 执行失败: %v", err)
	}
	if strings.TrimSpace(string(out)) != literal {
		t.Fatalf("单引号转义后往返不一致: %q", strings.TrimSpace(string(out)))
	}
	// like 特殊字符（[ ]）按字面匹配（WildcardPattern::Escape）。
	if !psLikeMatch(t, `--data-dir "F:\a[b]\data"`, `x --data-dir "F:\a[b]\data" y`) {
		t.Fatal("[ ] 应按字面匹配")
	}
	if psLikeMatch(t, `--data-dir "F:\a[b]\data"`, `x --data-dir "F:\ab\data" y`) {
		t.Fatal("[b] 不应被当字符类通配")
	}
}

// ---------------------------------------------------------------- ③ stop 自身拒绝

func newTestService(t *testing.T, webPort int) *Service {
	t.Helper()
	dir := t.TempDir()
	svc := New(Options{ExeDir: filepath.Join(dir, "exe"), DataDir: filepath.Join(dir, "data"), WebPort: webPort})
	// 种子 = 本进程自身（id=p<webPort>），供"自身拒绝"用例
	if _, err := svc.Load(Instance{WebPort: webPort, CtrlPort: webPort + 1000,
		DataDir: filepath.Join(dir, "data"), DeployDir: filepath.Join(dir, "deploy")}); err != nil {
		t.Fatal(err)
	}
	// 默认注入 fake：不跑真 PowerShell / 不杀真进程 / 不 sleep
	svc.RunPowerShell = func(script string) ([]byte, error) { return []byte(""), nil }
	svc.Sleep = func(time.Duration) {}
	svc.Now = func() time.Time { return time.Date(2026, 10, 14, 12, 0, 0, 0, time.Local) }
	return svc
}

func TestStopSelfRejected(t *testing.T) {
	svc := newTestService(t, 28082) // 种子 p28082 即自身
	killed := false
	svc.KillPID = func(int) error { killed = true; return nil }

	if _, err := svc.Stop("p28082"); !errors.Is(err, ErrSelfOperation) {
		t.Fatalf("自身实例 stop 应返回 ErrSelfOperation，实际 %v", err)
	}
	if killed {
		t.Fatal("自身实例不得执行任何杀进程")
	}

	// restart 的 controller/cold 同样拒绝
	if _, err := svc.Restart("p28082", "controller"); !errors.Is(err, ErrSelfOperation) {
		t.Fatalf("自身实例 controller 重启应拒绝，实际 %v", err)
	}
	if _, err := svc.Restart("p28082", "cold"); !errors.Is(err, ErrSelfOperation) {
		t.Fatalf("自身实例 cold 重启应拒绝，实际 %v", err)
	}
	// delete 同样拒绝
	if _, err := svc.Delete("p28082", "p28082"); !errors.Is(err, ErrSelfOperation) {
		t.Fatalf("自身实例 delete 应拒绝，实际 %v", err)
	}
}

func TestStopKillsByPIDs(t *testing.T) {
	svc := newTestService(t, 28082)
	if err := svc.reg.Add(Instance{ID: "p9999", WebPort: 9999, CtrlPort: 9998,
		DataDir: "d", DeployDir: "d", CtrlMatch: `--data-dir "d"`}); err != nil {
		t.Fatal(err)
	}
	// fake：ctrl_match 查询 → 两个 PID；deploy 查询 → 一个 PID
	svc.RunPowerShell = func(script string) ([]byte, error) {
		if strings.Contains(script, "CommandLine") {
			return []byte("101\r\n102\r\n"), nil
		}
		return []byte("201\n"), nil
	}
	var killed []int
	svc.KillPID = func(pid int) error { killed = append(killed, pid); return nil }

	res, err := svc.Stop("p9999")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Ctrl) != 2 || res.Ctrl[0] != 101 || res.Ctrl[1] != 102 {
		t.Fatalf("中控 PID 解析错误: %v", res.Ctrl)
	}
	if len(res.Robot) != 1 || res.Robot[0] != 201 {
		t.Fatalf("机器人 PID 解析错误: %v", res.Robot)
	}
	if len(killed) != 3 {
		t.Fatalf("应逐个杀 3 个 PID，实际 %v", killed)
	}
	// 定位失败 → 空杀（宁可不动作）
	svc.RunPowerShell = func(string) ([]byte, error) { return nil, errors.New("powershell 挂了") }
	res2, err := svc.Stop("p9999")
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Ctrl) != 0 || len(res2.Robot) != 0 {
		t.Fatalf("定位失败时应空杀，实际 %+v", res2)
	}
}

// ---------------------------------------------------------------- ④ delete 归档

func TestDeleteConfirmAndArchive(t *testing.T) {
	svc := newTestService(t, 28082)
	base := t.TempDir()
	dataDir := filepath.Join(base, "data-9999")
	deployDir := filepath.Join(base, "single_robot_zy_9999")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(deployDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := svc.reg.Add(Instance{ID: "p9999", WebPort: 9999, CtrlPort: 9998,
		DataDir: dataDir, DeployDir: deployDir, CtrlMatch: `--data-dir "` + dataDir + `"`}); err != nil {
		t.Fatal(err)
	}
	svc.KillPID = func(int) error { return nil }

	// confirm 不匹配 → 拒绝且不动目录
	if _, err := svc.Delete("p9999", "wrong"); err == nil {
		t.Fatal("confirm 不匹配应拒绝")
	}
	if _, err := os.Stat(dataDir); err != nil {
		t.Fatal("被拒绝时不得归档目录")
	}

	res, err := svc.Delete("p9999", "p9999")
	if err != nil {
		t.Fatalf("Delete 失败: %v", err)
	}
	tag := "20261014" // Now 已注入固定时刻
	if want := dataDir + "_archived_" + tag; res.Archived[dataDir] != want {
		t.Fatalf("data 归档路径错误: %v（want %s）", res.Archived, want)
	}
	if want := deployDir + "_archived_" + tag; res.Archived[deployDir] != want {
		t.Fatalf("deploy 归档路径错误: %v（want %s）", res.Archived, want)
	}
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Fatal("data 原目录应已归档（不存在）")
	}
	if _, err := os.Stat(dataDir + "_archived_" + tag); err != nil {
		t.Fatal("data 归档目录应存在")
	}
	if _, ok := svc.Get("p9999"); ok {
		t.Fatal("注册表应已移除该实例")
	}

	// rename 失败不阻断：目标已存在同名归档 → 记 warning 但仍移除注册表
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := svc.reg.Add(Instance{ID: "p9998", WebPort: 9998, CtrlPort: 9997,
		DataDir: dataDir, DeployDir: deployDir, CtrlMatch: `--data-dir "x"`}); err != nil {
		t.Fatal(err)
	}
	res2, err := svc.Delete("p9998", "p9998")
	if err != nil {
		t.Fatalf("归档失败不应阻断删除: %v", err)
	}
	if len(res2.Warnings) == 0 {
		t.Fatal("归档冲突应产生 warning")
	}
	if _, ok := svc.Get("p9998"); ok {
		t.Fatal("注册表应仍被移除")
	}
}

// ---------------------------------------------------------------- ⑤ 创建预检矩阵 + 全流程

// newCreateEnv 构造一个可用的创建环境（模板 deploy + 源 data + 空闲端口）。
func newCreateEnv(t *testing.T) (*Service, string, int, int) {
	t.Helper()
	root := t.TempDir()
	exeDir := filepath.Join(root, "zyCtrlcenter")
	if err := os.MkdirAll(exeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 模板 deploy（含垃圾文件，供排除规则断言）
	tpl := filepath.Join(root, "single_robot_zy")
	writeFileT(t, filepath.Join(tpl, "script", "quest_engine.py"), "print('ok')")
	writeFileT(t, filepath.Join(tpl, "robot_single_robot.exe"), "fake-exe")
	writeFileT(t, filepath.Join(tpl, "quest_engine.py.bak_20261014"), "backup")
	writeFileT(t, filepath.Join(tpl, "quest_progress_1000.json"), "{}")
	writeFileT(t, filepath.Join(tpl, "diag.log.1"), "log")
	writeFileT(t, filepath.Join(tpl, "console_1.log"), "log")
	writeFileT(t, filepath.Join(tpl, "state", "x.json"), "{}")
	writeFileT(t, filepath.Join(tpl, "__pycache__", "x.pyc"), "pyc")
	// 源 data
	srcData := filepath.Join(exeDir, "data")
	writeFileT(t, filepath.Join(srcData, "chains", "newbie_full.json"), `{"chain":1}`)
	writeFileT(t, filepath.Join(srcData, "zones.json"), `{"current":{"server":"prod-240","zone":"2400"},
	  "servers":[{"key":"prod-240","name":"生产网关","host":"47.96.8.240","coding":"UTF-8",
	  "zones":[{"key":"2400","port":2400},{"key":"2300","port":2300}]},
	  {"key":"201-test","name":"测试网关","host":"192.168.0.201","coding":"UTF-8",
	  "zones":[{"key":"2300","port":2300}]}]}`)
	writeFileT(t, filepath.Join(srcData, "maps.json"), `{}`)
	writeFileT(t, filepath.Join(srcData, "item_usage_catalog.json"), `{}`)
	writeFileT(t, filepath.Join(srcData, "accounts.json"), `{"_comment":"src","generated_at":1,"source":"s",
	  "servers":{"1":"47.96.8.240:2300"},
	  "accounts":[
	    {"name":"u1","zones":{"47.96.8.240:2300":{"usable":true}}},
	    {"name":"u2","zones":{"47.96.8.240:2300":{"usable":false}}},
	    {"name":"u3","zones":{"47.96.8.240:2400":{"usable":true}}},
	    {"name":"t1","zones":{"192.168.0.201:2300":{"usable":true}}}]}`)

	// zones 注册表（与源 zones.json 同文件）：允许用例建"同区号跨服"实例。
	zreg := zones.New(filepath.Join(srcData, "zones.json"))
	if _, err := zreg.Load(); err != nil {
		t.Fatalf("加载测试 zones 注册表失败: %v", err)
	}

	svc := New(Options{ExeDir: exeDir, DataDir: srcData, WebPort: 28082, Zones: zreg,
		WebDistDir: filepath.Join(root, "web", "dist"), GameConfigDir: filepath.Join(root, "game-config")})
	// 注册一条"模板实例"（deploy 存在，供 templateDeployDir / 端口冲突用例）
	if err := svc.reg.Add(Instance{ID: "a", WebPort: 28082, CtrlPort: 27200,
		DataDir: srcData, DeployDir: tpl, CtrlMatch: `--deploy "x"`}); err != nil {
		t.Fatal(err)
	}
	webPort := freePortT(t)
	ctrlPort := freePortT(t)
	return svc, root, webPort, ctrlPort
}

func writeFileT(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func freePortT(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// findCheck 按 name 取预检项。
func findCheck(checks []CheckResult, name string) (CheckResult, bool) {
	for _, c := range checks {
		if c.Name == name {
			return c, true
		}
	}
	return CheckResult{}, false
}

func TestCreateChecksMatrix(t *testing.T) {
	svc, _, webPort, ctrlPort := newCreateEnv(t)
	valid := CreateRequest{Name: "新实例", ZoneServer: "prod-240", ZoneKey: "2300",
		WebPort: webPort, CtrlPort: ctrlPort}

	// 基线：全部通过
	checks := svc.checkCreate(valid)
	for _, c := range checks {
		if !c.Pass {
			t.Fatalf("基线预检应全过，%s 失败: %s", c.Name, c.Detail)
		}
	}

	// 1) params 失败：web==ctrl
	c, _ := findCheck(svc.checkCreate(CreateRequest{ZoneKey: "2300", WebPort: 5, CtrlPort: 5}), "params")
	if c.Pass {
		t.Fatal("web==ctrl 应 params 失败")
	}
	// zone_key 空
	c, _ = findCheck(svc.checkCreate(CreateRequest{WebPort: webPort, CtrlPort: ctrlPort}), "params")
	if c.Pass {
		t.Fatal("zone_key 空应 params 失败")
	}

	// 2) 端口被占用（真监听一个端口）
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	busy := ln.Addr().(*net.TCPAddr).Port
	c, _ = findCheck(svc.checkCreate(CreateRequest{ZoneKey: "2300", ZoneServer: "prod-240",
		WebPort: busy, CtrlPort: ctrlPort}), "web_port")
	if c.Pass {
		t.Fatal("被占用端口应试绑失败")
	}

	// 3) 注册表端口冲突（28082 已注册）
	c, _ = findCheck(svc.checkCreate(CreateRequest{ZoneKey: "2300", ZoneServer: "prod-240",
		WebPort: 28082, CtrlPort: ctrlPort}), "web_port")
	if c.Pass || !strings.Contains(c.Detail, "a") {
		t.Fatalf("注册表冲突应失败并指出实例 id: %+v", c)
	}

	// 4) zone 非法（SeedServers 也无该区）
	c, _ = findCheck(svc.checkCreate(CreateRequest{ZoneServer: "prod-240", ZoneKey: "9999",
		WebPort: webPort, CtrlPort: ctrlPort}), "zone")
	if c.Pass {
		t.Fatal("不存在的区应失败")
	}

	// 5) deploy 目标已存在
	target := filepath.Join(filepath.Dir(svc.templateDeployDirOrFatal(t)), "single_robot_zy_prod-240_2300")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	c, _ = findCheck(svc.checkCreate(valid), "deploy_target")
	if c.Pass {
		t.Fatal("deploy 目标已存在应失败")
	}
	_ = os.RemoveAll(target)

	// 6) data 目标已存在
	dataTarget := svc.dataDirFor("prod-240", "2300")
	if err := os.MkdirAll(dataTarget, 0o755); err != nil {
		t.Fatal(err)
	}
	c, _ = findCheck(svc.checkCreate(valid), "data_target")
	if c.Pass {
		t.Fatal("data 目标已存在应失败")
	}
	_ = os.RemoveAll(dataTarget)

	// 7) 启动脚本已存在
	scriptPath := filepath.Join(svc.opts.ExeDir, "_start_prod-240_2300.bat")
	writeFileT(t, scriptPath, "@echo off")
	c, _ = findCheck(svc.checkCreate(valid), "start_script")
	if c.Pass {
		t.Fatal("启动脚本已存在应失败")
	}
	_ = os.Remove(scriptPath)

	// 预检失败时 Create 不做任何变更
	os.MkdirAll(target, 0o755)
	out := svc.Create(valid)
	if out.OK {
		t.Fatal("有失败项时 Create 应拒绝")
	}
	if len(out.Checks) == 0 {
		t.Fatal("拒绝时应返回 checks")
	}
	_ = os.RemoveAll(target)
}

// templateDeployDirOrFatal 测试辅助：取模板 deploy（必存在）。
func (s *Service) templateDeployDirOrFatal(t *testing.T) string {
	t.Helper()
	dir, ok := s.templateDeployDir()
	if !ok {
		t.Fatal("测试环境应存在模板 deploy")
	}
	return dir
}

func TestCreateEndToEnd(t *testing.T) {
	svc, _, webPort, ctrlPort := newCreateEnv(t)
	req := CreateRequest{Name: "试点实例", ZoneServer: "prod-240", ZoneKey: "2300",
		WebPort: webPort, CtrlPort: ctrlPort, Note: "e2e"}
	out := svc.Create(req)
	if !out.OK {
		t.Fatalf("Create 应成功: %s", out.Msg)
	}
	if out.ID != "p"+itoa(webPort) {
		t.Fatalf("id 应为 p<web_port>，实际 %s", out.ID)
	}
	if out.StartScript != "_start_prod-240_2300.bat" {
		t.Fatalf("脚本名错误: %s", out.StartScript)
	}

	// 1) deploy 目录：存在 + 排除规则生效
	tpl, _ := svc.templateDeployDir()
	deployDir := filepath.Join(filepath.Dir(tpl), "single_robot_zy_prod-240_2300")
	if _, err := os.Stat(filepath.Join(deployDir, "script", "quest_engine.py")); err != nil {
		t.Fatal("deploy 正常文件应被复制")
	}
	for _, junk := range []string{"quest_engine.py.bak_20261014", "quest_progress_1000.json",
		"diag.log.1", "console_1.log", "state", "__pycache__"} {
		if _, err := os.Stat(filepath.Join(deployDir, junk)); !os.IsNotExist(err) {
			t.Fatalf("垃圾项应被排除: %s", junk)
		}
	}

	// 2) data 目录：chains/zones/accounts/waterline
	dataDir := svc.dataDirFor("prod-240", "2300")
	if _, err := os.Stat(filepath.Join(dataDir, "chains", "newbie_full.json")); err != nil {
		t.Fatal("chains 应被复制")
	}
	rawZones, err := os.ReadFile(filepath.Join(dataDir, "zones.json"))
	if err != nil {
		t.Fatal(err)
	}
	var zf struct {
		Current struct{ Server, Zone string }
	}
	if err := json.Unmarshal(rawZones, &zf); err != nil {
		t.Fatal(err)
	}
	if zf.Current.Server != "prod-240" || zf.Current.Zone != "2300" {
		t.Fatalf("zones.json current 应为 prod-240/2300，实际 %+v", zf.Current)
	}
	var af struct {
		Comment  string            `json:"_comment"`
		Accounts []map[string]any  `json:"accounts"`
		Servers  map[string]string `json:"servers"`
	}
	rawAcc, err := os.ReadFile(filepath.Join(dataDir, "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(rawAcc, &af); err != nil {
		t.Fatal(err)
	}
	if len(af.Accounts) != 1 || af.Accounts[0]["name"] != "u1" {
		t.Fatalf("accounts 应只保留 2300 区 usable 的 u1，实际 %v", af.Accounts)
	}
	if af.Comment == "" || af.Servers["1"] != "47.96.8.240:2300" {
		t.Fatal("accounts 顶层字段（_comment/servers）应保留")
	}
	rawWl, err := os.ReadFile(filepath.Join(dataDir, "waterline.json"))
	if err != nil {
		t.Fatal(err)
	}
	var wl struct {
		Enabled bool `json:"enabled"`
		Target  int  `json:"target"`
	}
	if err := json.Unmarshal(rawWl, &wl); err != nil {
		t.Fatal(err)
	}
	if !wl.Enabled || wl.Target != 30 {
		t.Fatalf("waterline 应为 enabled=true target=30，实际 %+v", wl)
	}
	// autotask/booth_config 缺失 → warning
	foundWarn := false
	for _, w := range out.Warnings {
		if strings.Contains(w, "autotask.json") {
			foundWarn = true
		}
	}
	if !foundWarn {
		t.Fatal("缺失源文件应产生 warning")
	}

	// 3) 启动脚本：ASCII + CRLF + 必须含 ROBOT_CTRL_PORT / --web-port / --auto-robot
	batPath := filepath.Join(svc.opts.ExeDir, "_start_prod-240_2300.bat")
	bat, err := os.ReadFile(batPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(bat)
	for _, needle := range []string{
		"set ROBOT_CTRL_PORT=" + itoa(ctrlPort),
		"--web-port " + itoa(webPort),
		"--ctrl-port " + itoa(ctrlPort),
		"--auto-robot",
		"\r\n",
	} {
		if !strings.Contains(text, needle) {
			t.Fatalf("启动脚本缺少 %q", needle)
		}
	}
	for _, b := range bat {
		if b > 127 {
			t.Fatalf("启动脚本必须纯 ASCII，发现字节 %d", b)
		}
	}
	if strings.Contains(strings.ReplaceAll(text, "\r\n", ""), "\n") {
		t.Fatal("启动脚本应是纯 CRLF 行尾")
	}

	// 4) 注册表新条目
	inst, ok := svc.Get(out.ID)
	if !ok {
		t.Fatal("注册表应包含新实例")
	}
	want := `--data-dir "` + dataDir + `"`
	if inst.CtrlMatch != want {
		t.Fatalf("ctrl_match 应为 %q，实际 %q", want, inst.CtrlMatch)
	}
	if inst.ZoneKey != "2300" || inst.ZoneServer != "prod-240" {
		t.Fatalf("zone 字段错误: %+v", inst)
	}
	// 新建实例默认人工停止态（2026-10-14 口径修复）：看门狗不得自动拉起"只准备未启动"的实例
	if !inst.Stopped {
		t.Fatal("新建实例应带 stopped=true（未启动态）")
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// TestInstanceSlugSanitize slug 命名与路径安全化（2026-10-14 命名带 server 修复）。
func TestInstanceSlugSanitize(t *testing.T) {
	cases := []struct{ server, zone, want string }{
		{"prod-240", "2300", "prod-240_2300"}, // 生产 2300 区
		{"201-test", "2300", "201-test_2300"}, // 测试服同区号（修复前与上行撞名）
		{"a b/c", "23.00", "a_b_c_23_00"},     // 空格/斜杠/点 → 下划线
		{"..", "x", "___x"},                   // 点点遍历被消毒
		{"  pad  ", "2300", "pad_2300"},       // 首尾空白剥除
		{"", "2300", "_2300"},                 // 空 server（正常流程由 zone 校验拦截）
	}
	for _, c := range cases {
		if got := instanceSlug(c.server, c.zone); got != c.want {
			t.Fatalf("instanceSlug(%q,%q) = %q，期望 %q", c.server, c.zone, got, c.want)
		}
	}
	if got := sanitizeKey("x\\y:z*?"); got != "x_y_z__" {
		t.Fatalf("sanitizeKey 对非法字符应替换为下划线: %q", got)
	}
}

// TestCreateSameZoneDifferentServers 同区号跨服并存（2026-10-14 修复的回归用例）：
// 生产 prod-240/2300 与测试服 201-test/2300 区号相同，命名带 server 后互不撞名；
// 修复前两者推导出同一组 deploy/data/脚本目标，第二个创建必被预检拒绝。
func TestCreateSameZoneDifferentServers(t *testing.T) {
	svc, root, webPort1, ctrlPort1 := newCreateEnv(t)

	// 1) 先建生产 prod-240/2300
	out1 := svc.Create(CreateRequest{Name: "生产2300", ZoneServer: "prod-240", ZoneKey: "2300",
		WebPort: webPort1, CtrlPort: ctrlPort1})
	if !out1.OK {
		t.Fatalf("prod-240/2300 创建应成功: %s", out1.Msg)
	}

	// 2) 再建测试服 201-test/2300：预检应全过（修复前 deploy/data/脚本三项撞名失败）
	webPort2 := freePortT(t)
	ctrlPort2 := freePortT(t)
	req2 := CreateRequest{Name: "测试服", ZoneServer: "201-test", ZoneKey: "2300",
		WebPort: webPort2, CtrlPort: ctrlPort2}
	for _, c := range svc.checkCreate(req2) {
		if !c.Pass {
			t.Fatalf("201-test/2300 预检应全过（修复前撞名失败），%s: %s", c.Name, c.Detail)
		}
	}
	out2 := svc.Create(req2)
	if !out2.OK {
		t.Fatalf("201-test/2300 创建应成功: %s", out2.Msg)
	}
	if out2.StartScript != "_start_201-test_2300.bat" {
		t.Fatalf("脚本名应带 server: %s", out2.StartScript)
	}

	// 3) 两条实例的目标路径互不重叠
	dataDir1 := svc.dataDirFor("prod-240", "2300")
	dataDir2 := svc.dataDirFor("201-test", "2300")
	if dataDir1 == dataDir2 {
		t.Fatal("同区号跨服 data 目标不应相同")
	}
	if filepath.Base(dataDir2) != "data-201-test_2300" || !strings.HasPrefix(dataDir2, svc.opts.ExeDir) {
		t.Fatalf("data 目标命名错误: %s", dataDir2)
	}
	tpl, _ := svc.templateDeployDir()
	dpl1 := filepath.Join(filepath.Dir(tpl), "single_robot_zy_prod-240_2300")
	dpl2 := filepath.Join(filepath.Dir(tpl), "single_robot_zy_201-test_2300")
	if _, err := os.Stat(dpl1); err != nil {
		t.Fatal("生产实例 deploy 目录应存在")
	}
	if _, err := os.Stat(dpl2); err != nil {
		t.Fatal("测试服实例 deploy 目录应存在")
	}
	if !strings.HasSuffix(svc.baseDirFor("201-test", "2300"), "zyCtrlcenter_201-test_2300") {
		t.Fatalf("base 目标命名错误: %s", svc.baseDirFor("201-test", "2300"))
	}

	// 4) 测试服实例 data：zones.json current=201-test/2300；accounts 只留该区 usable
	rawZones, err := os.ReadFile(filepath.Join(dataDir2, "zones.json"))
	if err != nil {
		t.Fatal(err)
	}
	var zf struct {
		Current struct{ Server, Zone string }
	}
	if err := json.Unmarshal(rawZones, &zf); err != nil {
		t.Fatal(err)
	}
	if zf.Current.Server != "201-test" || zf.Current.Zone != "2300" {
		t.Fatalf("zones.json current 应为 201-test/2300，实际 %+v", zf.Current)
	}
	rawAcc, err := os.ReadFile(filepath.Join(dataDir2, "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	var af struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(rawAcc, &af); err != nil {
		t.Fatal(err)
	}
	if len(af.Accounts) != 1 || af.Accounts[0]["name"] != "t1" {
		t.Fatalf("201-test/2300 分片应只留 t1，实际 %v", af.Accounts)
	}

	// 5) 注册表两条实例并存（id 不同）
	inst1, ok1 := svc.Get(out1.ID)
	inst2, ok2 := svc.Get(out2.ID)
	if !ok1 || !ok2 || out1.ID == out2.ID {
		t.Fatalf("两条实例应并存且 id 不同: %s / %s", out1.ID, out2.ID)
	}
	if inst1.ZoneServer != "prod-240" || inst2.ZoneServer != "201-test" {
		t.Fatalf("注册表 zone_server 字段错误: %+v / %+v", inst1, inst2)
	}
	// 旧命名残留检查：修复后不应再产生裸 zone 命名目录
	if _, err := os.Stat(filepath.Join(root, "single_robot_zy_2300")); !os.IsNotExist(err) {
		t.Fatal("不应产生 zone-only 命名的 deploy 目录")
	}
	if _, err := os.Stat(filepath.Join(svc.opts.ExeDir, "data-2300")); !os.IsNotExist(err) {
		t.Fatal("不应产生 zone-only 命名的 data 目录")
	}
}

// ---------------------------------------------------------------- 端口/工具

func TestPortListenable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	busy := ln.Addr().(*net.TCPAddr).Port
	if err := portListenable(busy); err == nil {
		t.Fatal("被占用端口应报错")
	}
	_ = ln.Close()
	if err := portListenable(busy); err != nil {
		t.Fatalf("释放后应可试绑: %v", err)
	}
}

func TestParsePIDs(t *testing.T) {
	got := parsePIDs("101\r\n102\n\nnot-a-pid\n0\n  103  \n")
	if len(got) != 3 || got[0] != 101 || got[1] != 102 || got[2] != 103 {
		t.Fatalf("parsePIDs 结果错误: %v", got)
	}
	if len(parsePIDs("")) != 0 {
		t.Fatal("空输出应为空")
	}
}

// ---------------------------------------------------------------- ⑥ 人工停止标记（2026-10-14）

// TestStopSetsStoppedFlag：停止写 stopped 标记并落盘；重复停止幂等；启动/重启清标记。
func TestStopSetsStoppedFlag(t *testing.T) {
	svc := newTestService(t, 28082)
	if err := svc.reg.Add(Instance{ID: "b", WebPort: 28083, CtrlPort: 27201,
		DataDir: "d-b", DeployDir: "d-b", CtrlMatch: `--data-dir "nomatch"`,
		StartScript: "_start_missing.bat"}); err != nil {
		t.Fatal(err)
	}
	svc.KillPID = func(int) error { return nil }

	// 1) 停止 → stopped=true 且落盘
	res, err := svc.Stop("b")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("正常停止不应有 warnings: %v", res.Warnings)
	}
	if inst, _ := svc.Get("b"); !inst.Stopped {
		t.Fatal("停止后 stopped 应为 true")
	}
	raw, err := os.ReadFile(svc.RegistryPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"stopped": true`) {
		t.Fatalf("注册表文件应持久化 stopped=true:\n%s", raw)
	}

	// 2) 重复停止幂等（标记已 true 时不再写盘、不报错）
	if _, err := svc.Stop("b"); err != nil {
		t.Fatalf("重复停止应幂等: %v", err)
	}

	// 3) 启动：先清标记再跑脚本（脚本缺失 → 报错，但标记必须已清）
	if _, err := svc.Start("b"); err == nil {
		t.Fatal("脚本缺失时 Start 应报错")
	}
	if inst, _ := svc.Get("b"); inst.Stopped {
		t.Fatal("Start 后 stopped 应被清除（脚本执行失败也清）")
	}

	// 4) 重启（cold）：同样清标记
	if _, err := svc.Stop("b"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Restart("b", "cold"); err == nil {
		t.Fatal("脚本缺失时 Restart cold 应报错")
	}
	if inst, _ := svc.Get("b"); inst.Stopped {
		t.Fatal("Restart 后 stopped 应被清除")
	}

	// 5) 不存在的实例
	if _, err := svc.Stop("nope"); err == nil {
		t.Fatal("不存在实例 Stop 应报错")
	}
}

// TestRegistryUpdateAndStoppedCompat：Update 整条替换落盘；旧注册表（无 stopped 字段）兼容读取。
func TestRegistryUpdateAndStoppedCompat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "instances.json")
	writeFileT(t, path, `{"instances":[{"id":"a","web_port":28082,"ctrl_port":27200,
	  "data_dir":"d","deploy_dir":"p","start_script":"s.bat"}]}`)
	reg := NewRegistry(path)
	if _, err := reg.Load(Instance{}); err != nil {
		t.Fatal(err)
	}
	inst, ok := reg.Get("a")
	if !ok || inst.Stopped {
		t.Fatalf("旧文件应兼容：stopped 缺省 false，实际 %+v", inst)
	}
	inst.Stopped = true
	found, err := reg.Update(inst)
	if err != nil || !found {
		t.Fatalf("Update 应成功: found=%v err=%v", found, err)
	}
	reg2 := NewRegistry(path)
	if _, err := reg2.Load(Instance{}); err != nil {
		t.Fatal(err)
	}
	if inst2, _ := reg2.Get("a"); !inst2.Stopped {
		t.Fatal("Update 落盘后重载应 stopped=true")
	}
	found, err = reg.Update(Instance{ID: "nope", WebPort: 1, CtrlPort: 2})
	if err != nil || found {
		t.Fatalf("Update 不存在 id 应 found=false: found=%v err=%v", found, err)
	}
}
