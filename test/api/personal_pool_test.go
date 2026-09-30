// 私人池 Wave 1（2026-09-30）契约测试：
//
//	注册表生命周期（acounts/add 勾选 / add·remove 端点 / poolOf 覆盖 / 删号清标记）；
//	各排除闸（LaunchTask / GhostSkipFunc / reghost / 候选 / waterline 视野·读数 / A+C / 组队 / startAuto）；
//	手动任务端点（四任务 start/stop 载荷、非私号拒绝、ensure_online、无台账副作用）。
package api_test

import (
	"strings"
	"testing"
	"time"

	"zyctrlcenter/internal/services/accounts"
	"zyctrlcenter/internal/services/autotask"
	"zyctrlcenter/test/testsupport"
)

// accountRow 取账号池列表里某号的合并行（keyword 精确查）。
func accountRow(t *testing.T, env *testEnv, acc string) map[string]any {
	t.Helper()
	body := getJSON(t, env.srv.URL+"/api/accounts?keyword="+acc)
	rows, _ := body["accounts"].([]any)
	for _, it := range rows {
		if m, ok := it.(map[string]any); ok && m["name"] == acc {
			return m
		}
	}
	return nil
}

func TestPersonalRegistryLifecycle(t *testing.T) {
	env := newTestEnv(t, "")
	acc := "pv_a@xy3.com"
	addUsableAccount(t, env, acc, 45)
	if row := accountRow(t, env, acc); row == nil || row["pool"] != "ghost" {
		t.Fatalf("初始应按等级推导为 ghost 池: %v", row)
	}

	// accounts/add 勾选 personal → 注册表 + poolOf 覆盖
	_, body := postJSON(t, env.srv.URL+"/api/accounts/add", map[string]any{
		"accounts": []string{acc}, "personal": true}, nil)
	if body["ok"] != true || body["personal"] != true {
		t.Fatalf("勾选私人池的加号应回执 personal: %v", body)
	}
	pb := getJSON(t, env.srv.URL+"/api/personal")
	got, _ := pb["accounts"].([]any)
	if pb["count"] != float64(1) || len(got) != 1 || got[0] != acc {
		t.Fatalf("私人池名单应含该号: %v", pb)
	}
	if row := accountRow(t, env, acc); row == nil || row["pool"] != "personal" {
		t.Fatalf("poolOf 应被 personal 覆盖: %v", row)
	}

	// 移出 → 回到推导池
	_, rb := postJSON(t, env.srv.URL+"/api/personal/remove", map[string]any{"accounts": []string{acc}}, nil)
	if rb["ok"] != true || rb["removed"] != float64(1) {
		t.Fatalf("移出应 removed=1: %v", rb)
	}
	if row := accountRow(t, env, acc); row == nil || row["pool"] != "ghost" {
		t.Fatalf("移出后应回推导池 ghost: %v", row)
	}

	// 再入池 + 删号 → 标记同步清理（防悬垂）
	postJSON(t, env.srv.URL+"/api/personal/add", map[string]any{"accounts": []string{acc}}, nil)
	postJSON(t, env.srv.URL+"/api/accounts/remove", map[string]any{"accounts": []string{acc}}, nil)
	pb2 := getJSON(t, env.srv.URL+"/api/personal")
	if pb2["count"] != float64(0) {
		t.Fatalf("删号应清私人标记: %v", pb2)
	}
}

func TestPersonalExcludedFromAutoGates(t *testing.T) {
	env := newTestEnv(t, "")
	testsupport.InstallGhostNav(t, env.cfg.ChainDir)
	testsupport.InstallShareDailyNav(t, env.cfg.ChainDir)
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	pv, nm := "pv_g@xy3.com", "pv_n@xy3.com"
	for _, acc := range []string{pv, nm} {
		addUsableAccount(t, env, acc, 45)
		// 两号都带"已派神捕"心跳（正常口径下都会成为候选）
		feedRobot(t, env, acc, map[string]any{"hs": true, "daily": map[string]any{
			"share_key": "share_daily_大唐神捕", "done": 0, "limit": 10, "state": "STOPPED"}})
	}
	postJSON(t, env.srv.URL+"/api/personal/add", map[string]any{"accounts": []string{pv}}, nil)
	// 入池即推名单（robot_manage action=personal；机器人端 Wave 2 消费）——先消费掉再验后续
	pushCmd := rb.ReadCmd(t, 2*time.Second)
	if pushCmd["cmd"] != "robot_manage" || pushCmd["action"] != "personal" {
		t.Fatalf("入池应推送名单: %v", pushCmd)
	}
	if a := asSlice(pushCmd["accounts"]); len(a) != 1 || a[0] != pv {
		t.Fatalf("名单应含该私号: %v", pushCmd["accounts"])
	}

	// ① autotask 候选：私号被排除，正常号仍在（差分）
	body := getJSON(t, env.srv.URL+"/api/autotask")
	cands, _ := body["candidates"].(map[string]any)
	if n, _ := cands["shenbu"].(float64); n != 1 {
		t.Fatalf("候选应只剩正常号（差分=1），实际 %v", n)
	}

	// ② LaunchTask：整批私号拒发
	if ok, msg := env.api.LaunchTask(autotask.KindGhost, []string{pv}); ok ||
		!strings.Contains(msg, "私人池") {
		t.Fatalf("私号应被自动派发剔除，实际 ok=%v msg=%s", ok, msg)
	}

	// ③ restorer Skip（同时挡 d5bc942 离线补拉）
	if skip, why := env.api.GhostSkipFunc()("shenbu", pv); !skip || !strings.Contains(why, "私人池") {
		t.Fatalf("Skip 闸应拦私号，实际 skip=%v why=%s", skip, why)
	}

	// ④ reghost 三入口：不自动 remove/add/补发
	rd := env.api.ReghostDeps()
	if rd.Launch(pv) || rd.Add(pv) || rd.Remove(pv) {
		t.Fatal("reghost 三入口都应拒绝私号")
	}

	// ⑤ waterline：候选/视野/本地读数都不含私号
	wl := env.api.WaterlineDeps()
	for _, c := range wl.Candidates() {
		if c.Account == pv {
			t.Fatal("私号不该在补号候选里")
		}
	}
	for _, r := range wl.Robots() {
		if r.Account == pv {
			t.Fatal("私号不该在水位视野（压号/队列）里")
		}
	}
	if n := wl.Local(); n != 1 {
		t.Fatalf("本地读数应只算正常号（1），实际 %d", n)
	}

	// ⑥ A+C 重启自动补：私号不补（无命令）
	env.api.OnRobotRestartHello([]string{pv})
	if cmd := rb.TryReadCmd(400 * time.Millisecond); cmd != nil {
		t.Fatalf("重启自动补不该拉私号，实际 %v", cmd)
	}

	// ⑦ 组队：建队/入队拒绝
	_, tb := postJSON(t, env.srv.URL+"/api/team/setup", map[string]any{
		"captain": nm, "members": []string{pv}}, nil)
	if tb["ok"] != false || !strings.Contains(toStrAny(tb["msg"]), "私人池") {
		t.Fatalf("私号不该能建/入队，实际 %v", tb)
	}

	// ⑧ 启动(自动分配)：私号剔除 + 回带说明；不发命令
	_, sb := postJSON(t, env.srv.URL+"/api/start", map[string]any{
		"auto": true, "accounts": []string{pv}}, nil)
	if sb["ok"] != false {
		t.Fatalf("只有私号时不应下发成功: %v", sb)
	}
	asg, _ := sb["assignments"].([]any)
	found := false
	for _, it := range asg {
		m, _ := it.(map[string]any)
		if m["account"] == pv {
			found = true
			if m["command"] != "" || !strings.Contains(toStrAny(m["reason"]), "私人池") {
				t.Fatalf("私号应回带 command=\"\" + 私人池原因: %v", m)
			}
		}
	}
	if !found {
		t.Fatalf("assignments 应含私号说明行: %v", asg)
	}
	if cmd := rb.TryReadCmd(300 * time.Millisecond); cmd != nil {
		t.Fatalf("自动分配不该给私号发命令，实际 %v", cmd)
	}
}

func TestPersonalTaskDispatch(t *testing.T) {
	env := newTestEnv(t, "")
	testsupport.InstallGhostNav(t, env.cfg.ChainDir)
	testsupport.InstallShareDailyNav(t, env.cfg.ChainDir)
	testsupport.InstallFenghuoNav(t, env.cfg.ChainDir)
	rb := testsupport.ConnectFakeRobot(t, env.ctrl)
	defer rb.Close()
	acc := "pv_t@xy3.com"
	env.pool.Add([]string{acc}, "pwd-pv", testZoneAddr, "")
	env.pool.SetZoneState(acc, testZoneAddr, accounts.ZoneState{Verified: true, Usable: true, Level: 45, Password: "pwd-pv"})
	postJSON(t, env.srv.URL+"/api/personal/add", map[string]any{"accounts": []string{acc}}, nil)
	rb.ReadCmd(t, 2*time.Second) // 消费入池名单推送（robot_manage action=personal）

	// 抓鬼 start：solo + 导航载荷 + 账号
	_, b := postJSON(t, env.srv.URL+"/api/personal/task", map[string]any{
		"accounts": []string{acc}, "task": "ghost", "action": "start"}, nil)
	if b["ok"] != true || b["sent"] != true || b["command"] != "ghost_start" {
		t.Fatalf("抓鬼 start 应直发成功: %v", b)
	}
	cmd := rb.ReadCmd(t, 2*time.Second)
	if cmd["cmd"] != "ghost_start" || cmd["role"] != "solo" || cmd["chain_id"] != "zhongkui_nav" {
		t.Fatalf("抓鬼载荷应同自动口径（role=solo+导航）: %v", cmd)
	}
	if a := asSlice(cmd["accounts"]); len(a) != 1 || a[0] != acc {
		t.Fatalf("只该带目标私号: %v", cmd["accounts"])
	}

	// 抓鬼 stop / 新手 start·stop / 神捕 start / 烽火 start
	cases := []struct {
		task, action, wantCmd, wantChain string
	}{
		{"ghost", "stop", "ghost_stop", ""},
		{"newbie", "start", "start_chain", "newbie_full"},
		{"newbie", "stop", "stop", ""},
		{"shenbu", "start", "share_daily_start", "shenbu_nav"},
		{"fenghuo", "start", "share_daily_start", "fenghuo_nav"},
	}
	for _, c := range cases {
		_, bb := postJSON(t, env.srv.URL+"/api/personal/task", map[string]any{
			"accounts": []string{acc}, "task": c.task, "action": c.action}, nil)
		if bb["ok"] != true || bb["command"] != c.wantCmd {
			t.Fatalf("%s %s 应发 %s: %v", c.task, c.action, c.wantCmd, bb)
		}
		cm := rb.ReadCmd(t, 2*time.Second)
		if cm["cmd"] != c.wantCmd {
			t.Fatalf("%s %s 命令不符: %v", c.task, c.action, cm)
		}
		if c.wantChain != "" && cm["chain_id"] != c.wantChain {
			t.Fatalf("%s 应带 %s: %v", c.task, c.wantChain, cm["chain_id"])
		}
	}

	// 非私号：明确拒绝且零命令
	other := "pv_x@xy3.com"
	env.pool.Add([]string{other}, "pwd-x", testZoneAddr, "")
	_, nb := postJSON(t, env.srv.URL+"/api/personal/task", map[string]any{
		"accounts": []string{other}, "task": "ghost", "action": "start"}, nil)
	if nb["ok"] != false || !strings.Contains(toStrAny(nb["msg"]), "不在私人池") {
		t.Fatalf("非私号应被拒绝: %v", nb)
	}
	if cmd := rb.TryReadCmd(300 * time.Millisecond); cmd != nil {
		t.Fatalf("非私号拒绝时不该发命令: %v", cmd)
	}

	// 不写台账（无在途去重）：连续两次 start 都真实下发
	_, b1 := postJSON(t, env.srv.URL+"/api/personal/task", map[string]any{
		"accounts": []string{acc}, "task": "ghost", "action": "start"}, nil)
	rb.ReadCmd(t, 2*time.Second)
	_, b2 := postJSON(t, env.srv.URL+"/api/personal/task", map[string]any{
		"accounts": []string{acc}, "task": "ghost", "action": "start"}, nil)
	if b1["sent"] != true || b2["sent"] != true {
		t.Fatalf("私号任务不应写自动在途台账（两次都应发出）: %v / %v", b1, b2)
	}
	rb.ReadCmd(t, 2*time.Second)

	// ensure_online：离线号先补 add，任务延迟下发（scheduled）
	env2 := newTestEnv(t, "")
	testsupport.InstallGhostNav(t, env2.cfg.ChainDir)
	rb2 := testsupport.ConnectFakeRobot(t, env2.ctrl)
	defer rb2.Close()
	acc2 := "pv_e@xy3.com"
	env2.pool.Add([]string{acc2}, "pwd-e", testZoneAddr, "")
	env2.pool.SetZoneState(acc2, testZoneAddr, accounts.ZoneState{Verified: true, Usable: true, Level: 45, Password: "pwd-e"})
	postJSON(t, env2.srv.URL+"/api/personal/add", map[string]any{"accounts": []string{acc2}}, nil)
	rb2.ReadCmd(t, 2*time.Second) // 消费入池名单推送
	_, eb := postJSON(t, env2.srv.URL+"/api/personal/task", map[string]any{
		"accounts": []string{acc2}, "task": "ghost", "action": "start", "ensure_online": true}, nil)
	if eb["ok"] != true || eb["scheduled"] != true {
		t.Fatalf("ensure_online 应受理并转延迟下发: %v", eb)
	}
	addCmd := rb2.ReadCmd(t, 2*time.Second)
	if addCmd["cmd"] != "robot_manage" || addCmd["action"] != "add" {
		t.Fatalf("应先补 robot_manage add: %v", addCmd)
	}
	if cmd := rb2.TryReadCmd(1500 * time.Millisecond); cmd != nil {
		t.Fatalf("任务应延迟（8s）下发，不应立即到: %v", cmd)
	}
}
