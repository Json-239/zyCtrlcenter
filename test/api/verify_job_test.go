// 账号可用性验证的「默认从本地库批量」接口测试：
// 不传 accounts 时按当前区从账号池选号 → 后台任务（带进度/可停止）→ 每验一个就同步回池。
package api_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"zyctrlcenter/internal/gameproto"
	"zyctrlcenter/internal/services/accounts"
	"zyctrlcenter/internal/state"
	"zyctrlcenter/test/testsupport"
)

// jobGame 假游戏服：robot0002000 登录成功带角色、robot0002001 不存在；
// slowGate 非空时 robot0002000 会被卡住（它名字最小 → 并发 1 时第一个跑，便于测"停止"）。
func jobGame(slowGate <-chan struct{}) fakeGame {
	return fakeGame{ack: map[string]struct {
		code  int32
		role  string
		level int16
	}{
		"robot0002000@xy3.com": {code: gameproto.LoginOK, role: "批量甲", level: 25},
		"robot0002001@xy3.com": {code: gameproto.LoginInvalidAccount},
	}, gate: slowGate, slowAccount: "robot0002000@xy3.com"}
}

func waitVerifyJob(t *testing.T, url, id string, timeout time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		body := getJSON(t, url+"/api/accounts/verify/job?id="+id)
		if body["ok"] != true {
			t.Fatalf("查询任务失败: %v", body)
		}
		job, _ := body["job"].(map[string]any)
		if job == nil {
			t.Fatalf("任务快照为空: %v", body)
		}
		if job["status"] != "running" {
			return job
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("任务未在超时内结束")
	return nil
}

func TestAccountsVerifyDefaultsToPoolBatchJob(t *testing.T) {
	env := newTestEnv(t, "")
	gs := testsupport.StartGameServer(t, jobGame(nil).handler())

	// 池里两个该区的号（新加的 → 未验证）
	env.pool.Add([]string{"robot0002000@xy3.com", "robot0002001@xy3.com"}, "123456", gs.Addr, "")

	// 关键：**不传 accounts** —— 默认从本地库按区批量验证
	code, body := postJSON(t, env.srv.URL+"/api/accounts/verify", map[string]any{
		"zone": gs.Addr, "scope": "unverified", "limit": 10, "concurrency": 1,
	}, nil)
	if code != http.StatusOK || body["ok"] != true {
		t.Fatalf("批量验证应被接受: %d %v", code, body)
	}
	if body["mode"] != "job" {
		t.Fatalf("不传 accounts 应走后台任务模式: %v", body)
	}
	if body["selected"] != float64(2) {
		t.Fatalf("应选中 2 个待验证账号: %v", body["selected"])
	}
	job0, _ := body["job"].(map[string]any)
	id, _ := job0["id"].(string)
	if id == "" {
		t.Fatalf("应返回任务 ID: %v", body)
	}

	job := waitVerifyJob(t, env.srv.URL, id, 5*time.Second)
	if job["status"] != "done" || job["total"] != float64(2) || job["done"] != float64(2) {
		t.Fatalf("任务应跑完 2/2: %v", job)
	}
	if job["usable"] != float64(1) || job["not_exists"] != float64(1) {
		t.Fatalf("分类计数不符: %v", job)
	}

	// 结果同步回池：可用 + 已验 + 角色/等级 + 验证时间
	list := getJSON(t, env.srv.URL+"/api/accounts?zone="+gs.Addr+"&keyword=robot0002000")
	rows, _ := list["accounts"].([]any)
	if len(rows) != 1 {
		t.Fatalf("应检索到 1 行: %v", list["accounts"])
	}
	row, _ := rows[0].(map[string]any)
	if row["usable"] != true || row["verified"] != true {
		t.Fatalf("验证结果应写回池: %v", row)
	}
	if row["role_name"] != "批量甲" || row["level"] != float64(25) {
		t.Fatalf("角色/等级应写回池: %v", row)
	}
	if at, _ := row["verified_at"].(float64); at <= 0 {
		t.Fatalf("应记录验证时间: %v", row)
	}

	// 不存在的号：verified 但不可用
	list2 := getJSON(t, env.srv.URL+"/api/accounts?zone="+gs.Addr+"&keyword=robot0002001")
	rows2, _ := list2["accounts"].([]any)
	row2 := rows2[0].(map[string]any)
	if row2["verified"] != true || row2["usable"] != false {
		t.Fatalf("不存在的号应 verified+不可用: %v", row2)
	}

	// 再查一次：已经没有待验证的了（否则会反复重验）
	stats := getJSON(t, env.srv.URL+"/api/accounts/stats?zone="+gs.Addr)
	pending, _ := stats["pending"].(map[string]any)
	if pending["unverified"] != float64(0) {
		t.Fatalf("全验完后 pending.unverified 应为 0: %v", stats)
	}
}

func TestAccountsVerifyJobPreviewCounts(t *testing.T) {
	env := newTestEnv(t, "")
	gs := testsupport.StartGameServer(t, jobGame(nil).handler())
	env.pool.Add([]string{"robot0003000@xy3.com", "robot0003001@xy3.com", "robot0003002@xy3.com"}, "123456", gs.Addr, "")

	stats := getJSON(t, env.srv.URL+"/api/accounts/stats?zone="+gs.Addr)
	pending, _ := stats["pending"].(map[string]any)
	// 3 个新加的号在该区有记录且未验证 → unverified=3；
	// 夹具里的 3 个号是别的区（该区没记录）→ unknown=3；池内总共 6
	if pending["unverified"] != float64(3) {
		t.Fatalf("unverified 应只统计『该区有记录但没验证』的号: %v", pending)
	}
	if pending["unknown"] != float64(3) || pending["all"] != float64(6) {
		t.Fatalf("unknown/all 口径不符: %v", pending)
	}
}

func TestAccountsVerifyJobSkipsOnlineByDefault(t *testing.T) {
	env := newTestEnv(t, "")
	gs := testsupport.StartGameServer(t, jobGame(nil).handler())
	env.pool.Add([]string{"robot0004000@xy3.com", "robot0004001@xy3.com"}, "123456", gs.Addr, "")

	// robot0004000 当前在线（正在跑）→ 默认跳过，不浪费一次游戏服登录
	env.st.Update("robot0004000@xy3.com", func(r *state.Robot) { r.Online = true; r.State = "DIALOG" })

	code, body := postJSON(t, env.srv.URL+"/api/accounts/verify",
		map[string]any{"zone": gs.Addr, "limit": 10}, nil)
	if code != http.StatusOK || body["ok"] != true {
		t.Fatalf("应接受: %d %v", code, body)
	}
	if body["selected"] != float64(1) {
		t.Fatalf("在线号应被跳过，只剩 1 个待验证: %v", body)
	}
	skipped, _ := body["skipped_online"].([]any)
	if len(skipped) != 1 || skipped[0] != "robot0004000@xy3.com" {
		t.Fatalf("应说明跳过了谁: %v", body["skipped_online"])
	}
	job0, _ := body["job"].(map[string]any)
	job := waitVerifyJob(t, env.srv.URL, job0["id"].(string), 5*time.Second)
	if job["done"] != float64(1) {
		t.Fatalf("应验证 1 个: %v", job)
	}

	// 显式要求带上在线号时也要能验（上一步已把另一个号验过，这里只剩那个在线号）
	code, body2 := postJSON(t, env.srv.URL+"/api/accounts/verify",
		map[string]any{"zone": gs.Addr, "limit": 10, "skip_online": false}, nil)
	if code != http.StatusOK || body2["selected"] != float64(1) {
		t.Fatalf("skip_online=false 时应选到在线号: %v", body2)
	}
	if skipped, _ := body2["skipped_online"].([]any); len(skipped) != 0 {
		t.Fatalf("skip_online=false 时不该有跳过清单: %v", body2["skipped_online"])
	}
	job2, _ := body2["job"].(map[string]any)
	waitVerifyJob(t, env.srv.URL, job2["id"].(string), 5*time.Second)
	// 在线号也真的被验了（该假服对它回 100 → 存在库里为 verified 且不可用）
	rowOnline := getJSON(t, env.srv.URL+"/api/accounts?zone="+gs.Addr+"&keyword=robot0004000")
	rowsOnline, _ := rowOnline["accounts"].([]any)
	if len(rowsOnline) != 1 {
		t.Fatalf("应能检索到在线号: %v", rowOnline["accounts"])
	}
	ro, _ := rowsOnline[0].(map[string]any)
	if ro["verified"] != true {
		t.Fatalf("在线号也应写回验证结果: %v", ro)
	}
}

func TestAccountsVerifyJobEmptySelectionExplains(t *testing.T) {
	env := newTestEnv(t, "")
	gs := testsupport.StartGameServer(t, jobGame(nil).handler())

	// 池里一个该区的号都没有 → 不该静默成功
	code, body := postJSON(t, env.srv.URL+"/api/accounts/verify", map[string]any{"zone": gs.Addr}, nil)
	if code != http.StatusOK || body["ok"] != false {
		t.Fatalf("空选号应明确失败: %d %v", code, body)
	}
	msg, _ := body["msg"].(string)
	if !strings.Contains(msg, "待验证") {
		t.Fatalf("提示应说明没有待验证账号: %v", body["msg"])
	}
}

// 面板默认：**本区全部**（scope=zone，limit=0=不限）。
// 该区有记录的号全进来（含已验过的），无记录的不进来；limit=0 不能被当成 1。
func TestAccountsVerifyZoneScopeAll(t *testing.T) {
	env := newTestEnv(t, "")
	gs := testsupport.StartGameServer(t, func(c *testsupport.GameConn, msgID int32, _ []byte) [][]byte {
		switch msgID {
		case gameproto.MsgConnect:
			return [][]byte{
				testsupport.FrameBytes(gameproto.MsgConnectQuery, []any{}, []any{}, gameproto.UTF8),
				testsupport.FrameBytes(gameproto.MsgConnectBack, []any{4}, []any{int64(200)}, gameproto.UTF8),
			}
		case gameproto.MsgConnectBack:
			return [][]byte{testsupport.FrameBytes(gameproto.MsgVerBack, []any{4, ""}, []any{int64(0), ""}, gameproto.UTF8)}
		case gameproto.MsgLogin:
			return [][]byte{testsupport.FrameBytes(gameproto.MsgAckAccount, []any{4, ""}, []any{int64(0), "登录成功"}, gameproto.UTF8)}
		}
		return nil
	})
	zone := gs.Addr

	a1, a2, a3 := "zone9001@xy3.com", "zone9002@xy3.com", "zone9003@xy3.com"
	env.pool.Add([]string{a1, a2, a3}, "", zone, "")
	env.pool.SetZoneState(a1, zone, accounts.ZoneState{Verified: true, Usable: true, Password: "pwd12345678"})
	env.pool.SetZoneState(a2, zone, accounts.ZoneState{Verified: true, Usable: false, Password: "pwd12345678"})
	env.pool.SetZoneState(a3, zone, accounts.ZoneState{Password: "pwd12345678"}) // 有记录、没验过

	_, body := postJSON(t, env.srv.URL+"/api/accounts/verify", map[string]any{
		"zone": zone, "scope": "zone", "limit": 0, "concurrency": 1,
		"skip_online": false, "query_role": false, "timeout_sec": 3,
	}, nil)
	if body["ok"] != true || body["mode"] != "job" {
		t.Fatalf("本区全部验证应被接受: %v", body)
	}
	if body["scope"] != "zone" {
		t.Fatalf("范围应原样回带 zone: %v", body["scope"])
	}
	if body["selected"] != float64(3) {
		t.Fatalf("该区有记录的 3 个都要验（含已验过的；limit=0=不限）: %v", body["selected"])
	}
	if body["pending"] == nil {
		t.Fatal("应回带 pending（面板预览用）")
	}

	// 等任务跑完再结束：否则后台 goroutine 会在 t.TempDir() 清理时才写回账号库，
	// Windows 上 RemoveAll 会因"目录非空"失败（偶发，且是测试自身的竞态）。
	job0, _ := body["job"].(map[string]any)
	id, _ := job0["id"].(string)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		d := getJSON(t, env.srv.URL+"/api/accounts/verify/job?id="+id)
		j, _ := d["job"].(map[string]any)
		if j == nil || j["status"] != "running" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAccountsVerifyJobCancel(t *testing.T) {
	env := newTestEnv(t, "")
	gate := make(chan struct{})
	defer close(gate)
	gs := testsupport.StartGameServer(t, jobGame(gate).handler())

	// robot0002000 名字最小 → 并发 1 时第一个跑（会被 gate 卡住），所以 done 应保持 0
	env.pool.Add([]string{"robot0002000@xy3.com", "robot0002001@xy3.com"}, "123456", gs.Addr, "")

	_, body := postJSON(t, env.srv.URL+"/api/accounts/verify",
		map[string]any{"zone": gs.Addr, "limit": 10, "concurrency": 1}, nil)
	job0, _ := body["job"].(map[string]any)
	id, _ := job0["id"].(string)

	time.Sleep(150 * time.Millisecond) // 让第一个账号（卡住的）真正开始
	cancelBody := map[string]any{"id": id}
	code, resp := postJSON(t, env.srv.URL+"/api/accounts/verify/cancel", cancelBody, nil)
	if code != http.StatusOK || resp["ok"] != true {
		t.Fatalf("停止请求应被接受: %d %v", code, resp)
	}
	job := waitVerifyJob(t, env.srv.URL, id, 2*time.Second)
	if job["status"] != "canceled" {
		t.Fatalf("停止后状态应为 canceled: %v", job)
	}
	if job["done"] != float64(0) {
		t.Fatalf("被停止时未完成的不该计入: %v", job)
	}
}

// 显式传 accounts 仍是"同步验证这几个"（旧行为不变，面板手输单个走这条）。
func TestAccountsVerifyExplicitAccountsStaysSynchronous(t *testing.T) {
	env := newTestEnv(t, "")
	gs := testsupport.StartGameServer(t, jobGame(nil).handler())
	env.pool.Add([]string{"robot0002000@xy3.com"}, "123456", gs.Addr, "")

	code, body := postJSON(t, env.srv.URL+"/api/accounts/verify",
		map[string]any{"accounts": []string{"robot0002000@xy3.com"}, "zone": gs.Addr}, nil)
	if code != http.StatusOK || body["ok"] != true {
		t.Fatalf("同步验证应成功: %d %v", code, body)
	}
	if body["mode"] != "sync" {
		t.Fatalf("显式 accounts 应走同步模式: %v", body)
	}
	results, _ := body["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("应同步返回结果: %v", body)
	}
	if _, hasJob := body["job"]; hasJob {
		t.Fatalf("同步模式不该有 job: %v", body)
	}
}

func TestAccountsVerifyJobLookupWithoutID(t *testing.T) {
	env := newTestEnv(t, "")
	// 从没跑过任务 → 明确说没有
	body := getJSON(t, env.srv.URL+"/api/accounts/verify/job")
	if body["ok"] != false {
		t.Fatalf("没有任务时应 ok=false: %v", body)
	}
	if msg, _ := body["msg"].(string); !strings.Contains(msg, "任务") {
		t.Fatalf("应说明没有任务: %v", body["msg"])
	}
}
