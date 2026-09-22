// event 模块测试：事件表驱动 / 状态迁移 / 错误记账 / 下机换号 / WS 广播口径。
//
// 输入全部取自 test/fixtures/events（真实抓包 + 显式构造），断言结果证据
// （状态迁移 / 落盘 / 广播内容），不只断言「调用了某个函数」。
package event_test

import (
	"strings"
	"testing"
	"time"

	"zyctrlcenter/internal/config"
	"zyctrlcenter/internal/services/intent"
	"zyctrlcenter/internal/state"
	"zyctrlcenter/internal/store"
	"zyctrlcenter/test/testsupport"
)

const (
	accProgress  = "robot0001035@xy3.com" // task_progress 夹具账号
	accState     = "robot0001005@xy3.com" // robot_state 夹具账号
	accError     = "robot0001032@xy3.com" // error 夹具账号
	accStuck     = "robot0001065@xy3.com" // STUCK_WAIT_NEXT 夹具账号（现场卡死样本）
	accGhost     = "robot0001194@xy3.com" // ghost_offline 夹具账号
	accChainDone = "robot0005281@xy3.com" // chain_done 夹具账号
)

func TestTaskProgressFixtureUpdatesStateAndLog(t *testing.T) {
	h, st, runStore, _ := testsupport.NewTestHandler(t, nil)
	fx := testsupport.LoadFixture(t, "task_progress.json")

	h.HandleEvent(fx.Event)

	r, ok := st.Get(accProgress)
	if !ok {
		t.Fatalf("task_progress 后应建立账号行 %s", accProgress)
	}
	if r.LastTask != 2019511 || r.TaskIndex != 2019511 {
		t.Fatalf("任务号应为 2019511，实际 last_task=%d task_index=%d", r.LastTask, r.TaskIndex)
	}
	if r.Done != 1 {
		t.Fatalf("done 应为 1，实际 %d", r.Done)
	}
	if !testsupport.StoreHasType(runStore, "task_progress") {
		t.Fatal("task_progress 必须落运行历史（面板日志页可见）")
	}
}

func TestRobotStateFixtureUpdatesDialogAndMap(t *testing.T) {
	h, st, _, _ := testsupport.NewTestHandler(t, nil)
	fx := testsupport.LoadFixture(t, "robot_state.json")

	h.HandleEvent(fx.Event)

	r, ok := st.Get(accState)
	if !ok {
		t.Fatalf("robot_state 后应建立账号行 %s", accState)
	}
	if r.State != "DIALOG" {
		t.Fatalf("状态应为 DIALOG，实际 %s", r.State)
	}
	if r.MapID != 24 {
		t.Fatalf("地图应为 24，实际 %d", r.MapID)
	}
	if len(r.Pos) != 2 || r.Pos[0] != 1736 || r.Pos[1] != 1064 {
		t.Fatalf("位置应为 [1736 1064]，实际 %v", r.Pos)
	}
}

func TestErrorFixtureRecordsCodeRepeatAndFailedList(t *testing.T) {
	h, st, runStore, _ := testsupport.NewTestHandler(t, nil)
	fx := testsupport.LoadFixture(t, "error_task_stuck.json")

	h.HandleEvent(fx.Event)
	r, _ := st.Get(accError)
	if r.ErrCode != "TASK_STUCK" {
		t.Fatalf("应记录 err_code=TASK_STUCK，实际 %q", r.ErrCode)
	}
	if r.ErrRepeat != 1 {
		t.Fatalf("首次错误 err_repeat 应为 1，实际 %d", r.ErrRepeat)
	}
	if r.ErrTS <= 0 {
		t.Fatal("应记录 err_ts（秒）")
	}

	// 同码再来一次：累计到 2 次（面板「任务失败待处理」阈值）
	h.HandleEvent(testsupport.CloneEvent(t, fx.Event))
	r, _ = st.Get(accError)
	if r.ErrRepeat != 2 {
		t.Fatalf("同码重复 err_repeat 应累计为 2，实际 %d", r.ErrRepeat)
	}

	failed := st.FailedTasks(2, 30)
	if failed["count"] != 1 {
		t.Fatalf("反复失败清单应有 1 条，实际 %v", failed)
	}
	items, _ := failed["items"].([]map[string]any)
	if len(items) != 1 || items[0]["account"] != accError || items[0]["code"] != "TASK_STUCK" {
		t.Fatalf("失败清单内容不符: %v", failed)
	}

	// 出错必须落盘（跨中控重启可见）
	if !testsupport.StoreHasType(runStore, "error") {
		t.Fatal("error 事件必须写运行历史")
	}
}

// 卡死类错误码要同时做两件事：计入当日 churn 计数 + 触发 reghost 重登恢复。
//
// 白名单之外，机器人端 quest_engine 看门狗上报的是 STUCK_<STATE>（quest_engine.py:3185
// `"code": "STUCK_%s" % quest.state`）——2026-09-21 现场 8 个号卡 STUCK_WAIT_NEXT 报
// ERROR 25 分钟无人恢复，就是因为只枚举了 GHOST_DIALOG_STUCK/TASK_STUCK 两个码。
func TestStuckCodeCountsAndTriggersReghost(t *testing.T) {
	h, st, runStore, _ := testsupport.NewTestHandler(t, nil)
	type call struct{ account, reason string }
	var calls []call
	h.SetReghoster(func(account, reason string) { calls = append(calls, call{account, reason}) })

	// 1) 前缀码 STUCK_WAIT_NEXT（现场样本：等下一个任务推送卡死）
	fx := testsupport.LoadFixture(t, "error_stuck_wait_next.json")
	h.HandleEvent(fx.Event)

	r, ok := st.Get(accStuck)
	if !ok {
		t.Fatalf("error 后应建立账号行 %s", accStuck)
	}
	if r.StuckCount != 1 || r.StuckDay != time.Now().Format("20060102") {
		t.Fatalf("STUCK_WAIT_NEXT 应计入当日卡死计数，实际 stuck_count=%d stuck_day=%q",
			r.StuckCount, r.StuckDay)
	}
	if len(calls) != 1 || calls[0].account != accStuck {
		t.Fatalf("STUCK_WAIT_NEXT 应触发一次重登恢复，实际 %+v", calls)
	}
	if calls[0].reason != fx.Event["msg"] {
		t.Fatalf("重登原因应是机器人原文 %q，实际 %q", fx.Event["msg"], calls[0].reason)
	}
	if !testsupport.StoreHasType(runStore, "error") {
		t.Fatal("卡死类错误也必须写运行历史")
	}

	// 2) 另一个前缀码 STUCK_CLICK：同样计数 + 触发（验证按前缀识别，而不是再枚举一个码）
	const clickMsg = "状态 CLICK 重试 4 次仍无进展"
	h.HandleEvent(map[string]any{"type": "error", "code": "STUCK_CLICK",
		"account": accStuck, "msg": clickMsg})

	r, _ = st.Get(accStuck)
	if r.StuckCount != 2 {
		t.Fatalf("同日第二个卡死码应累计为 2，实际 %d", r.StuckCount)
	}
	if len(calls) != 2 || calls[1].account != accStuck || calls[1].reason != clickMsg {
		t.Fatalf("STUCK_CLICK 应再触发一次重登恢复（reason=原文），实际 %+v", calls)
	}

	// 3) 反证：非卡死码既不计数也不触发重登（普通任务失败不误伤）
	h.HandleEvent(map[string]any{"type": "error", "code": "NO_LEGAL_ROUTE",
		"account": accStuck, "msg": "没有合法路径"})

	r, _ = st.Get(accStuck)
	if r.StuckCount != 2 {
		t.Fatalf("普通错误码不应计入卡死次数，实际 %d", r.StuckCount)
	}
	if len(calls) != 2 {
		t.Fatalf("普通错误码不应触发重登恢复，实际 %+v", calls)
	}
	if r.ErrCode != "NO_LEGAL_ROUTE" || r.ErrRepeat != 1 {
		t.Fatalf("普通错误仍要按原口径记账，实际 err_code=%q err_repeat=%d", r.ErrCode, r.ErrRepeat)
	}
}

func TestGhostOfflineAutoRemoveWhenEnabled(t *testing.T) {
	cfg := config.Default() // AutoRemoveOnDone=true
	h, st, runStore, _ := testsupport.NewTestHandler(t, cfg)
	fx := testsupport.LoadFixture(t, "ghost_offline.json")

	h.HandleEvent(fx.Event)

	if !st.IsRemoved(accGhost) {
		t.Fatalf("ghost_offline 后 %s 应进入已移除集合（防心跳复活）", accGhost)
	}
	if st.Has(accGhost) {
		t.Fatal("自动下机开启时 ghost_offline 应删除该账号行")
	}
	if !testsupport.StoreHasType(runStore, "ghost_offline") {
		t.Fatal("ghost_offline 必须写运行历史")
	}
	if !testsupport.StoreHasType(runStore, "log") {
		t.Fatal("ghost_offline 应追加一条可读的 warn 日志（原因）")
	}
	// 下机动作异步执行（不阻塞事件循环）：等待其历史落盘，确保用例结束时无后台写入
	testsupport.Eventually(t, 2*time.Second,
		func() bool { return testsupport.StoreHasType(runStore, "api") }, "下机历史应落盘")

	// 已移除账号的心跳不再复活
	h.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1", "robots": []any{
		map[string]any{"account": accGhost, "state": "IDLE", "online": true},
	}})
	if st.Has(accGhost) {
		t.Fatal("已移除账号不应被心跳复活")
	}
}

// 关闭自动下机：只记录历史，账号行保留（业务处置由使用方实现）。
func TestGhostOfflineKeepsRowWhenAutoRemoveDisabled(t *testing.T) {
	cfg := config.Default()
	cfg.AutoRemoveOnDone = false
	h, st, runStore, _ := testsupport.NewTestHandler(t, cfg)
	fx := testsupport.LoadFixture(t, "ghost_offline.json")

	// 先让该号在线（有账号行），再收到 ghost_offline
	st.Update(accGhost, func(r *state.Robot) { r.Online = true; r.State = "ONLINE" })
	h.HandleEvent(fx.Event)

	if st.IsRemoved(accGhost) {
		t.Fatal("关闭自动下机时不应标记移除")
	}
	r, ok := st.Get(accGhost)
	if !ok {
		t.Fatal("关闭自动下机时应保留账号行")
	}
	if !r.Online {
		t.Fatalf("关闭自动下机时账号行不应被改动为离线，实际 %+v", r)
	}
	if !testsupport.StoreHasType(runStore, "ghost_offline") {
		t.Fatal("无论是否下机，ghost_offline 都必须写运行历史")
	}
}

func TestChainDoneStateVisibleWhenNotAutoRemove(t *testing.T) {
	cfg := config.Default()
	cfg.AutoRemoveOnDone = false // 关闭自动下机，观察 DONE 状态可见
	h, st, _, _ := testsupport.NewTestHandler(t, cfg)
	fx := testsupport.LoadFixture(t, "chain_done.json")

	h.HandleEvent(fx.Event)

	r, ok := st.Get(accChainDone)
	if !ok {
		t.Fatalf("链完成后应保留账号行 %s", accChainDone)
	}
	if r.State != "DONE" || !r.ChainDone {
		t.Fatalf("链完成后状态应为 DONE/chain_done=true，实际 %s/%v", r.State, r.ChainDone)
	}
}

func TestChainDoneAutoRemoveDropsRow(t *testing.T) {
	cfg := config.Default()
	cfg.AutoRemoveOnDone = true
	h, st, runStore, _ := testsupport.NewTestHandler(t, cfg)
	fx := testsupport.LoadFixture(t, "chain_done.json")

	h.HandleEvent(fx.Event)

	if !st.IsRemoved(accChainDone) {
		t.Fatal("AutoRemoveOnDone=true 时链完成应标记移除")
	}
	if st.Has(accChainDone) {
		t.Fatal("AutoRemoveOnDone=true 时链完成应删除账号行（释放槽位）")
	}
	// 等待异步下机动作落盘（避免用例结束后仍有后台写入）
	testsupport.Eventually(t, 2*time.Second,
		func() bool { return testsupport.StoreHasType(runStore, "api") }, "下机历史应落盘")
}

func TestChainDoneAlreadyDoneStaysOnline(t *testing.T) {
	cfg := config.Default()
	cfg.AutoRemoveOnDone = true
	h, st, _, _ := testsupport.NewTestHandler(t, cfg)
	fx := testsupport.CloneEvent(t, testsupport.LoadFixture(t, "chain_done.json").Event)
	fx["already_done"] = true // 库中已完成（未被真正拉起跑任务）

	h.HandleEvent(fx)

	if st.IsRemoved(accChainDone) {
		t.Fatal("already_done 的链完成不应下机（保持在线）")
	}
	if !st.Has(accChainDone) {
		t.Fatal("already_done 的链完成应保留账号行")
	}
}

func TestStatusReplyUpdatesRobotsAndServer(t *testing.T) {
	h, st, _, _ := testsupport.NewTestHandler(t, nil)
	fx := testsupport.LoadFixture(t, "status_reply.json")

	h.HandleEvent(fx.Event)

	if got := st.CurServer(""); got != "47.96.8.240:2400" {
		t.Fatalf("应记录机器人上报的游戏服地址，实际 %s", got)
	}
	r1, ok := st.Get("robot0003004@xy3.com")
	if !ok {
		t.Fatal("status_reply 应建立机器人行")
	}
	if r1.State != "DIALOG" || r1.Level != 21 || r1.MapID != 5 || !r1.Online || !r1.HS {
		t.Fatalf("机器人1 状态不符: %+v", r1)
	}
	r2, _ := st.Get("robot0001004@xy3.com")
	if !r2.ChainDone {
		t.Fatal("机器人2 chain_done 应为 true")
	}
	if r2.ErrCode != "TASK_STUCK" {
		t.Fatalf("心跳里的最近错误应保留，实际 %q", r2.ErrCode)
	}
}

func TestUnknownEventOnlyLogged(t *testing.T) {
	h, st, runStore, _ := testsupport.NewTestHandler(t, nil)

	h.HandleEvent(map[string]any{"type": "brand_new_event", "account": "robotX"})

	if len(st.Snapshot()) != 0 {
		t.Fatal("未知事件不应改动状态")
	}
	if !testsupport.StoreHasType(runStore, "brand_new_event") {
		t.Fatal("未知事件应写运行历史（便于排查机器人端新事件）")
	}
}

func TestRobotPosBatchFlushAndState(t *testing.T) {
	h, st, _, _ := testsupport.NewTestHandler(t, nil)
	var got []map[string]any
	h.SetBroadcast(func(obj map[string]any) { got = append(got, obj) })

	h.HandleEvent(map[string]any{"type": "robot_pos", "account": "robotP", "mapid": 9, "x": 100, "y": 200})
	h.HandleEvent(map[string]any{"type": "robot_pos", "account": "robotQ", "mapid": 9, "x": 300, "y": 400})

	r, ok := st.Get("robotP")
	if !ok || len(r.Pos) != 2 || r.Pos[0] != 100 || r.Pos[1] != 200 {
		t.Fatalf("robot_pos 应立即更新状态（HTTP 轮询也要正确），实际 %+v", r)
	}

	// 100ms 合并窗口：等待一次 flush，应收到一条 robot_pos_batch 含 2 个账号
	testsupport.Eventually(t, 2*time.Second, func() bool { return len(got) > 0 }, "应收到位置合并推送")
	batch, _ := got[0]["list"].([]map[string]any)
	if got[0]["type"] != "robot_pos_batch" || len(batch) != 2 {
		t.Fatalf("位置应合并为 robot_pos_batch（2 条），实际 %v", got[0])
	}
}

func TestDebugLogNotBroadcast(t *testing.T) {
	h, _, _, _ := testsupport.NewTestHandler(t, nil)
	var got []map[string]any
	h.SetBroadcast(func(obj map[string]any) { got = append(got, obj) })

	// 真实抓包的 debug 日志（占日志流量大头）：只落盘，不推 WS
	h.AfterEvent(testsupport.LoadFixture(t, "log_dialog.json").Event)
	if len(got) != 0 {
		t.Fatalf("debug 日志不应推 WS，实际 %v", got)
	}

	// 普通事件应广播
	h.AfterEvent(map[string]any{"type": "task_progress", "account": "a"})
	if len(got) != 1 {
		t.Fatalf("普通事件应广播 1 条，实际 %d", len(got))
	}

	// robot_pos 不逐条广播（由合并器推送）
	h.AfterEvent(map[string]any{"type": "robot_pos", "account": "a"})
	if len(got) != 1 {
		t.Fatal("robot_pos 不应逐条广播")
	}
}

// 协议覆盖：docs/协议规范.md §1.2 事件表里的每个类型都必须有处理分支。
func TestEventTableCoversProtocolSpec(t *testing.T) {
	h, _, _, _ := testsupport.NewTestHandler(t, nil)
	handled := map[string]bool{}
	for _, tp := range h.HandledTypes() {
		handled[tp] = true
	}
	required := []string{
		"hello", "robot_online", "robot_offline", "status_reply", "robot_manage_reply",
		"task_progress", "robot_state", "robot_pos", "chain_done", "ghost_done",
		"ghost_offline", "error", "log", "pong",
	}
	for _, tp := range required {
		if !handled[tp] {
			t.Errorf("协议规范事件 %q 没有处理分支（EVENT_HANDLERS 缺失）", tp)
		}
	}
}

// 位置口径：机器人上报的是**像素**，状态行同时保存**客户端格子坐标**（像素/16）。
func TestPosConvertedToClientGrid(t *testing.T) {
	h, st, _, _ := testsupport.NewTestHandler(t, nil)

	// robot_state：像素 (2108,809) → 客户端 (131,50)
	h.HandleEvent(map[string]any{"type": "robot_state", "account": "robotG",
		"state": "NAV", "mapid": 11, "pos": []any{2108, 809}})
	r, ok := st.Get("robotG")
	if !ok {
		t.Fatal("应建立账号行")
	}
	if len(r.Pos) != 2 || r.Pos[0] != 2108 || r.Pos[1] != 809 {
		t.Fatalf("原始像素坐标应保留: %v", r.Pos)
	}
	if len(r.PosGrid) != 2 || r.PosGrid[0] != 131 || r.PosGrid[1] != 50 {
		t.Fatalf("客户端格子坐标应为 (131,50)，实际 %v", r.PosGrid)
	}

	// robot_pos：即时位置同样换算
	h.HandleEvent(map[string]any{"type": "robot_pos", "account": "robotG",
		"mapid": 24, "x": 1736, "y": 1064})
	r, _ = st.Get("robotG")
	if len(r.PosGrid) != 2 || r.PosGrid[0] != 108 || r.PosGrid[1] != 66 {
		t.Fatalf("robot_pos 格子坐标应为 (108,66)，实际 %v", r.PosGrid)
	}

	// status_reply 里的 pos 也要换算
	h.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1", "robots": []any{
		map[string]any{"account": "robotG", "online": true, "mapid": 24, "pos": []any{32, 48}},
	}})
	r, _ = st.Get("robotG")
	if len(r.PosGrid) != 2 || r.PosGrid[0] != 2 || r.PosGrid[1] != 3 {
		t.Fatalf("status_reply 格子坐标应为 (2,3)，实际 %v", r.PosGrid)
	}

	// 形状不符的 pos（只有 1 个数）→ 原样保留、不算格子（不猜）
	h.HandleEvent(map[string]any{"type": "robot_state", "account": "robotG", "pos": []any{7}})
	r, _ = st.Get("robotG")
	if len(r.Pos) != 1 || r.PosGrid != nil {
		t.Fatalf("形状不符时应原样保留且不换算: pos=%v pos_grid=%v", r.Pos, r.PosGrid)
	}
}

// 控制通道给事件打 "_zone"（当前区）标记，状态行按区归属（面板分区展示依据）。
func TestEventZoneTaggedFromChannel(t *testing.T) {
	h, st, _, c := testsupport.NewTestHandler(t, nil)
	rb := testsupport.ConnectFakeRobot(t, c)
	defer rb.Close()

	rb.SendEvent(t, map[string]any{"type": "robot_online", "account": "robotZ",
		"role_name": "甲", "level": 30, "mapid": 9})
	ev := testsupport.WaitEvent(t, c, 2*time.Second)
	if ev["_zone"] != testsupport.TestZoneKey {
		t.Fatalf("通道应给事件打区标记，实际 %v", ev["_zone"])
	}

	h.HandleEvent(ev)
	r, ok := st.Get("robotZ")
	if !ok {
		t.Fatal("事件处理后应建立账号行")
	}
	if r.Zone != testsupport.TestZoneKey {
		t.Fatalf("账号行应带区归属，实际 %q", r.Zone)
	}
	if zones := st.ZoneOfAccounts([]string{"robotZ"}); zones["robotZ"] != testsupport.TestZoneKey {
		t.Fatalf("按账号查区应返回测试区，实际 %v", zones)
	}
	// 各区计数
	if cnt := st.ZoneCounts()[testsupport.TestZoneKey]; cnt["total"] != 1 || cnt["online"] != 1 {
		t.Fatalf("zone_counts 不符: %v", st.ZoneCounts())
	}
}

// 事件处理中 panic 不应打挂中控（机器人端新字段类型不符时的兜底）。
func TestHandlerRecoversFromPanic(t *testing.T) {
	h, _, _, _ := testsupport.NewTestHandler(t, nil)
	// robots 字段给成字符串（类型不符）——handler 内部必须容错不 panic
	h.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1", "robots": "oops"})
	if got := h.HandledTypes(); len(got) == 0 {
		t.Fatal("处理器应仍然可用")
	}
}

// ---------------------------------------------------------------- 等级防抖

// storeFind 在运行历史尾部找第一条满足条件的记录。
func storeFind(st *store.Store, pred func(ev map[string]any) bool) map[string]any {
	for _, ev := range st.ReadTail(500) {
		if pred(ev) {
			return ev
		}
	}
	return nil
}

// hasMsg 找一条 msg 含 sub 的日志记录。
func hasMsg(sub string) func(map[string]any) bool {
	return func(ev map[string]any) bool {
		msg, _ := ev["msg"].(string)
		return strings.Contains(msg, sub)
	}
}

// isIntentSwitch 找一条 prev → kind 的意图切换记录。
func isIntentSwitch(prev, kind string) func(map[string]any) bool {
	return func(ev map[string]any) bool {
		return ev["type"] == "intent" && ev["kind"] == kind && ev["prev"] == prev
	}
}

// 等级"大幅回退"防抖（2026-09-21 现场 robot0001028：真实 39 级、正在正常抓鬼的号
// 被某次上报错成 9，中控立即覆盖 → 意图被切成"新手"，大屏「🆕新手」显示了抓鬼号）。
//
// 口径：新值 > 0 且 已确认等级 - 新值 ≥ 10 → 先挂"待确认"，同一新值**连续** 3 次才切换；
// 单次错值不覆盖等级、不切意图（robot_online 与 status_reply 两个入口都走防抖）。
func TestLevelBigRegressionNeedsThreeConsecutiveReports(t *testing.T) {
	const acc = "robot0001028@xy3.com" // 现场账号
	h, st, runStore, _ := testsupport.NewTestHandler(t, nil)

	// 上线报 39 级：立即生效 + 意图=抓鬼
	h.HandleEvent(map[string]any{"type": "robot_online", "account": acc, "level": 39})
	if r, _ := st.Get(acc); r.Level != 39 {
		t.Fatalf("首次上报应立即可用，实际 %d", r.Level)
	}
	if it, _ := h.Intents.Get(acc); it.Kind != intent.KindGhost {
		t.Fatalf("39 级应判抓鬼，实际 %+v", it)
	}

	// ① 单次错值（39 → 9，robot_online 入口）：不覆盖、不切意图，只挂待确认
	h.HandleEvent(map[string]any{"type": "robot_online", "account": acc, "level": 9})
	r, _ := st.Get(acc)
	if r.Level != 39 || r.LevelPending != 9 || r.LevelPendingN != 1 {
		t.Fatalf("单次大幅回退不该覆盖已确认等级: level=%d pending=%d n=%d",
			r.Level, r.LevelPending, r.LevelPendingN)
	}
	if it, _ := h.Intents.Get(acc); it.Kind != intent.KindGhost {
		t.Fatalf("单次错值不该切换意图（应仍是抓鬼）: %+v", it)
	}
	if ev := storeFind(runStore, hasMsg("等级大幅回退待确认")); ev == nil {
		t.Fatal("待确认应打一条 warn（含旧值/新值/连续次数）")
	}

	heartbeat := func(level int) {
		h.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
			"robots": []any{map[string]any{"account": acc, "level": level,
				"online": true, "state": "WAIT_GHOST"}}})
	}
	// "连续"口径：正确值插回来 → 待确认清零；之后的错值从头累计
	heartbeat(39)
	heartbeat(9)
	if r, _ = st.Get(acc); r.LevelPendingN != 1 {
		t.Fatalf("错值不连续时应从头累计，实际 n=%d", r.LevelPendingN)
	}

	// ② 同一错值连续 3 次（status_reply 入口）→ 确认切换：等级 + 意图 + 日志
	heartbeat(9) // 第 2 次
	if r, _ = st.Get(acc); r.Level != 39 {
		t.Fatalf("第 2 次仍不该切换，实际 level=%d", r.Level)
	}
	heartbeat(9) // 第 3 次 → 确认
	r, _ = st.Get(acc)
	if r.Level != 9 || r.LevelPending != 0 || r.LevelPendingN != 0 {
		t.Fatalf("连续 3 次应切换为 9 并清零待确认态: level=%d pending=%d n=%d",
			r.Level, r.LevelPending, r.LevelPendingN)
	}
	it, _ := h.Intents.Get(acc)
	if it.Kind != intent.KindNewbie {
		t.Fatalf("确认切换后意图应随之变为新手，实际 %+v", it)
	}
	if ev := storeFind(runStore, hasMsg("等级大幅回退已确认")); ev == nil {
		t.Fatal("确认切换应打一条 warn")
	}
	if ev := storeFind(runStore, isIntentSwitch("ghost", "newbie")); ev == nil {
		t.Fatal("应落一条 INTENT 日志（ghost → newbie）")
	}
}

// 等级正常变化仍**立即生效**：升级照常、小幅回退（<10）不防抖、0（未知）不覆盖已有等级。
func TestLevelNormalChangesApplyImmediately(t *testing.T) {
	h, st, _, _ := testsupport.NewTestHandler(t, nil)
	heartbeat := func(acc string, level int) {
		h.HandleEvent(map[string]any{"type": "status_reply", "server": "s:1",
			"robots": []any{map[string]any{"account": acc, "level": level,
				"online": true, "state": "WAIT_GHOST"}}})
	}

	// ③ 升级 39 → 45：立即生效
	up := "lv_up@xy3.com"
	h.HandleEvent(map[string]any{"type": "robot_online", "account": up, "level": 39})
	heartbeat(up, 45)
	if r, _ := st.Get(up); r.Level != 45 || r.LevelPendingN != 0 {
		t.Fatalf("升级应立即生效，实际 %+v", r)
	}

	// ④ 小幅回退 39 → 35（差 4 < 10）：立即生效
	down := "lv_down@xy3.com"
	h.HandleEvent(map[string]any{"type": "robot_online", "account": down, "level": 39})
	heartbeat(down, 35)
	if r, _ := st.Get(down); r.Level != 35 || r.LevelPendingN != 0 {
		t.Fatalf("小幅回退应立即生效，实际 %+v", r)
	}

	// 等级上报 0（未知）：不参与回退判定、不覆盖已有非零等级（比旧行为更保守）
	heartbeat(down, 0)
	if r, _ := st.Get(down); r.Level != 35 {
		t.Fatalf("level=0 不该覆盖已确认等级，实际 %d", r.Level)
	}
}
