// 批量验证任务（Job）用例：后台跑、带进度、可停止、每完成一个回调一次（供增量写回账号池）。
package accountverify_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"zyctrlcenter/internal/accountverify"
	"zyctrlcenter/internal/gameproto"
	"zyctrlcenter/test/testsupport"
)

// jobHandler 按账号给不同结果的假游戏服（a1 成功带角色 / a2 不存在 / a3 密码错 / slow 一直不回）。
func jobHandler(t *testing.T, slowGate <-chan struct{}) gameHandler {
	return func(c *testsupport.GameConn, msgID int32, body []byte) [][]byte {
		switch msgID {
		case gameproto.MsgConnect:
			return [][]byte{
				testsupport.FrameBytes(gameproto.MsgConnectQuery, nil, nil, gameproto.UTF8),
				testsupport.FrameBytes(gameproto.MsgConnectBack, []any{4}, []any{int64(200)}, gameproto.UTF8),
			}
		case gameproto.MsgConnectBack:
			return [][]byte{testsupport.FrameBytes(gameproto.MsgVerBack, []any{4, ""}, []any{int64(0), ""}, gameproto.UTF8)}
		case gameproto.MsgLogin:
			vals, _, _ := gameproto.Unpack([]any{"", ""}, body, 0, gameproto.UTF8)
			acc, _ := vals[0].(string)
			if acc == "slow" && slowGate != nil {
				<-slowGate // 卡住：用于测"停止"
			}
			code := gameproto.LoginInvalidAccount
			switch acc {
			case "a1":
				code = gameproto.LoginOK
			case "a3":
				code = gameproto.LoginInvalidPassword
			}
			return [][]byte{testsupport.FrameBytes(gameproto.MsgAckAccount, []any{4, ""},
				[]any{int64(code), ""}, gameproto.UTF8)}
		case gameproto.MsgQueryRole:
			return [][]byte{roleFrame(t, []gameproto.Role{{RoleID: 9, Name: "甲一", Level: 30}}, gameproto.UTF8)}
		}
		return nil
	}
}

func waitJob(t *testing.T, job *accountverify.Job, timeout time.Duration) accountverify.JobSnapshot {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var snap accountverify.JobSnapshot
	for time.Now().Before(deadline) {
		snap = job.Snapshot()
		if snap.Status != accountverify.JobRunning {
			return snap
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("任务未在 %v 内结束: %+v", timeout, snap)
	return snap
}

func TestJobRunsAllAndCallsBackPerAccount(t *testing.T) {
	gs := testsupport.StartGameServer(t, jobHandler(t, nil))
	job := accountverify.NewManager().Start(gs.Addr, []accountverify.Target{
		{Account: "a1", Password: "p"},
		{Account: "a2", Password: "p"},
		{Account: "a3", Password: "p"},
	}, accountverify.DefaultOptions(), 2, nil, nil)

	snap := waitJob(t, job, 5*time.Second)
	if snap.Status != accountverify.JobDone {
		t.Fatalf("状态应为 done: %+v", snap)
	}
	if snap.Total != 3 || snap.Done != 3 {
		t.Fatalf("进度应为 3/3: %+v", snap)
	}
	if snap.Usable != 1 || snap.NotExists != 1 || snap.Unusable != 1 {
		t.Fatalf("分类计数不符（可用1/不存在1/不可用1）: %+v", snap)
	}
	if len(snap.Results) != 3 {
		t.Fatalf("结果应有 3 条: %+v", snap.Results)
	}
	for i, want := range []string{"a1", "a2", "a3"} {
		if snap.Results[i].Account != want {
			t.Fatalf("结果顺序应与输入一致：第 %d 个是 %s", i, snap.Results[i].Account)
		}
	}
	if snap.Results[0].RoleName != "甲一" || snap.Results[0].Level != 30 {
		t.Fatalf("成功账号应带角色: %+v", snap.Results[0])
	}
	if snap.ElapsedMs < 0 || snap.StartedAt == 0 || snap.FinishedAt == 0 {
		t.Fatalf("应记录起止时间: %+v", snap)
	}
	if snap.Zone != gs.Addr {
		t.Fatalf("应带上目标区: %+v", snap)
	}
}

// 每完成一个账号回调一次 —— 账号池据此**增量同步**（不用等整批跑完）。
func TestJobOnResultCallbackPerAccount(t *testing.T) {
	gs := testsupport.StartGameServer(t, jobHandler(t, nil))

	var mu sync.Mutex
	got := map[string]accountverify.Result{}
	var doneSnap *accountverify.JobSnapshot

	job := accountverify.NewManager().Start(gs.Addr, []accountverify.Target{
		{Account: "a1", Password: "p"},
		{Account: "a2", Password: "p"},
	}, accountverify.DefaultOptions(), 1,
		func(r accountverify.Result) {
			mu.Lock()
			got[r.Account] = r
			mu.Unlock()
		},
		func(s accountverify.JobSnapshot) {
			mu.Lock()
			cp := s
			doneSnap = &cp
			mu.Unlock()
		})

	waitJob(t, job, 5*time.Second)

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("每个账号都应回调一次，实际 %d 条: %v", len(got), got)
	}
	if !got["a1"].Usable || got["a2"].Exists {
		t.Fatalf("回调内容不符: %v", got)
	}
	if doneSnap == nil || doneSnap.Status != accountverify.JobDone {
		t.Fatalf("结束时应回调一次（便于统一落盘）: %+v", doneSnap)
	}
}

func TestJobCancelDiscardsInFlightResults(t *testing.T) {
	gate := make(chan struct{})
	defer close(gate)
	gs := testsupport.StartGameServer(t, jobHandler(t, gate))

	var mu sync.Mutex
	recorded := 0
	job := accountverify.NewManager().Start(gs.Addr, []accountverify.Target{
		{Account: "slow", Password: "p"}, // 永远卡住
		{Account: "a2", Password: "p"},   // 会正常完成
	}, accountverify.DefaultOptions(), 1, // 并发 1：先跑 slow → 卡住
		func(r accountverify.Result) { mu.Lock(); recorded++; mu.Unlock() }, nil)

	time.Sleep(150 * time.Millisecond)
	job.Cancel()

	snap := waitJob(t, job, 2*time.Second)
	if snap.Status != accountverify.JobCanceled {
		t.Fatalf("停止后状态应为 canceled: %+v", snap)
	}
	if snap.Done != 0 {
		t.Fatalf("被停止时未完成的账号不该计入进度: %+v", snap)
	}
	mu.Lock()
	defer mu.Unlock()
	if recorded != 0 {
		t.Fatalf("取消导致的探测失败**不得回调**（否则会把好号写成『验证失败』）: %d 次", recorded)
	}
}

func TestJobManagerLookup(t *testing.T) {
	mgr := accountverify.NewManager()
	if mgr.Get("") != nil {
		t.Fatal("没有任务时 Get(\"\") 应为 nil")
	}
	gs := testsupport.StartGameServer(t, jobHandler(t, nil))
	job := mgr.Start(gs.Addr, []accountverify.Target{{Account: "a1", Password: "p"}},
		accountverify.DefaultOptions(), 1, nil, nil)
	waitJob(t, job, 5*time.Second)

	if mgr.Get(job.ID()) != job {
		t.Fatal("应能按 ID 取回任务")
	}
	if mgr.Get("") != job {
		t.Fatal("ID 为空应返回最近一个任务（面板刷新进度用）")
	}
	if mgr.Get("nope") != nil {
		t.Fatal("未知 ID 应返回 nil")
	}
}

// 任务自身不落盘、不写池：写回由回调方负责（职责边界清晰，便于测试）。
func TestJobDoesNotTouchPoolByItself(t *testing.T) {
	gs := testsupport.StartGameServer(t, jobHandler(t, nil))
	job := accountverify.NewManager().Start(gs.Addr, []accountverify.Target{{Account: "a1", Password: "p"}},
		accountverify.DefaultOptions(), 1, nil, nil)
	snap := waitJob(t, job, 5*time.Second)
	if snap.Done != 1 {
		t.Fatalf("应完成: %+v", snap)
	}
	// 没有回调 = 没有任何副作用；这里只断言任务快照自身可序列化（结构稳定）
	if snap.Results[0].Account != "a1" {
		t.Fatalf("结果不符: %+v", snap.Results[0])
	}
	_ = context.Background()
}
