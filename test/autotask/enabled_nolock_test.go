// EnabledNoLock：Runner 各策略启用状态的**无锁查询**（2026-09-24 新增）。
//
// 背景：Runner 持锁回调（Deps.Candidates / Deps.OnlineCount）里禁止调用 States()（sync.Mutex
// 不可重入 → 同 goroutine 永久死锁，2026-09-24 生产事故）。壳层需要在候选判定里判断
// "神捕池当前是否启用"（池停用时，当天已派神捕的号要放行给抓鬼，否则两头不跑），据此新增
// 本方法（Start/Stop 在持 r.mu 时同步一份 atomic 镜像）。本组用例钉住：
//   - 默认 false；Start→true；Stop→false；各 kind 互不影响；与 States() 快照一致；
//   - 持锁回调内调用**不阻塞**（若实现退化为取 r.mu，用例会超时失败）；
//   - 启停与查询并发无数据竞争（配合 go test -race 生效）。
package autotask_test

import (
	"sync"
	"testing"
	"time"

	"zyctrlcenter/internal/services/autotask"
)

func TestEnabledNoLockTracksStartStop(t *testing.T) {
	r := autotask.New(newFake().deps())
	for _, k := range autotask.Kinds {
		if r.EnabledNoLock(k) {
			t.Fatalf("新建 Runner：%s 应默认未启用", k)
		}
	}
	if err := r.Start(autotask.KindShenbu, cfg()); err != nil {
		t.Fatal(err)
	}
	if !r.EnabledNoLock(autotask.KindShenbu) {
		t.Fatal("Start(shenbu) 后应读到 enabled=true")
	}
	if r.EnabledNoLock(autotask.KindGhost) {
		t.Fatal("只启动了 shenbu，ghost 应仍为未启用（各 kind 互不影响）")
	}
	if !r.States()[autotask.KindShenbu].Enabled {
		t.Fatal("States().Enabled 与 EnabledNoLock 漂移")
	}
	r.Stop(autotask.KindShenbu)
	if r.EnabledNoLock(autotask.KindShenbu) {
		t.Fatal("Stop(shenbu) 后应读到 enabled=false")
	}
	if r.States()[autotask.KindShenbu].Enabled {
		t.Fatal("Stop 后 States().Enabled 仍为 true（漂移）")
	}
	// 未知 kind：不 panic、返回 false（值域外的防御）。
	if r.EnabledNoLock(autotask.Kind("nope")) {
		t.Fatal("未知 kind 应返回 false")
	}
	// nil Receiver：壳层可能拿到未装配的 Runner（测试/嵌入式），不得 panic。
	var nilR *autotask.Runner
	if nilR.EnabledNoLock(autotask.KindShenbu) {
		t.Fatal("nil Runner 应返回 false")
	}
}

// 持锁回调（Candidates 在 tickKind 里持 r.mu 调用）内调用 EnabledNoLock 必须立即返回：
// 这正是生产用法（抓鬼候选判定读"神捕池是否启用"），退化为取锁即死锁。
func TestEnabledNoLockCallableInsideLockedCallback(t *testing.T) {
	var r *autotask.Runner
	inside := make(chan bool, 1)
	d := autotask.Deps{
		Now: time.Now,
		Candidates: func(k autotask.Kind, c autotask.Config) []autotask.Candidate {
			inside <- r.EnabledNoLock(autotask.KindShenbu) // 此刻 r.mu 已被 Runner 持有
			return nil
		},
		OnlineCount: func(autotask.Kind) int { return 0 },
	}
	r = autotask.New(d)
	if err := r.Start(autotask.KindShenbu, cfg()); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { r.Tick(time.Now()); close(done) }()
	select {
	case on := <-inside:
		if !on {
			t.Fatal("持锁回调里应该读到 enabled=true")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("EnabledNoLock 在持锁回调里阻塞（疑似取了 r.mu → 生产会死锁）")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Tick 未结束（回调死锁）")
	}
}

// 启停与查询并发（-race 下验证 atomic 镜像与只读 map 无数据竞争）。
func TestEnabledNoLockConcurrentStartStop(t *testing.T) {
	r := autotask.New(newFake().deps())
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = r.Start(autotask.KindShenbu, cfg())
			r.Stop(autotask.KindShenbu)
		}
		close(stop)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = r.EnabledNoLock(autotask.KindShenbu)
			}
		}
	}()
	wg.Wait()
	if r.EnabledNoLock(autotask.KindShenbu) {
		t.Fatal("循环以 Stop 收尾：最终应为未启用")
	}
}
