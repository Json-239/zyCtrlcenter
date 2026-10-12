// 号池轮换（rotation 最小版，2026-10-12）api 壳层单测（**内部包**，可注入 tick 时刻）：
//   - 预备队统计（level≥31 过滤 / 已移除 / 暂停 / 在线 的剔除口径）；
//   - 一轮 tick 全链路（复用批量下线通路：MarkRemoved+删行+robot_manage remove+名单落盘）；
//   - 预备队保护（<50 整轮不下线）；
//   - 到龄回池（UnmarkRemoved + 移出名单；开关关闭也执行）；
//   - "自动重登处理中"跳过。
//
// 选择内部包的原因：tick 的"在线 ≥30 分钟"判据要靠"首见在线表 + 注入时刻"驱动
// （两次不同 now 的 tick），黑盒 HTTP 无法注入时刻；纯逻辑部分已由
// internal/services/rotation 单测覆盖。
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zyctrlcenter/internal/config"
	"zyctrlcenter/internal/ctrl"
	"zyctrlcenter/internal/services/accounts"
	"zyctrlcenter/internal/services/event"
	"zyctrlcenter/internal/services/reghost"
	"zyctrlcenter/internal/services/rotation"
	"zyctrlcenter/internal/state"
	"zyctrlcenter/internal/store"
	"zyctrlcenter/test/testsupport"
)

const rotZoneAddr = "127.0.0.1:2300" // 测试 zones.json 的当前区地址（NewTestZones）

// rotEnv 轮换测试环境（只装配 rotation 用到的依赖，不含 autotask/waterline 等。
// 这样 tick 里不会有其它自动模块发命令干扰断言）。
type rotEnv struct {
	a    *API
	st   *state.State
	ch   *ctrl.Server
	pool *accounts.Pool
	cfg  *config.Config
}

func newRotEnv(t *testing.T) *rotEnv {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	st := state.New()
	runStore := store.New(t.TempDir(), 30, 500)
	log := testsupport.NewTestLogger(t)
	ch := testsupport.NewTestChannel(t)
	ev := event.New(cfg, st, runStore, ch, log)
	pool := testsupport.NewTestAccounts(t)
	a := New(Deps{Cfg: cfg, St: st, Store: runStore, Ctrl: ch, Log: log,
		Zones: testsupport.NewTestZones(t), Accounts: pool, Events: ev})
	return &rotEnv{a: a, st: st, ch: ch, pool: pool, cfg: cfg}
}

// addUsable 往池里加一个"当前区可用"的号（含区级等级）。
func (e *rotEnv) addUsable(t *testing.T, name string, level int) {
	t.Helper()
	e.pool.Add([]string{name}, "pwd-"+name, "", "")
	e.pool.SetZoneState(name, rotZoneAddr, accounts.ZoneState{
		Verified: true, Usable: true, Level: level, Password: "pwd-" + name,
	})
}

// markFullRoam 把号标成"今日抓鬼已满 + 纯游荡"（心跳口径：done≥limit 且 count_date=今日）。
func (e *rotEnv) markFullRoam(t *testing.T, acc, countDay, claimDay string) {
	t.Helper()
	e.st.Update(acc, func(r *state.Robot) {
		r.Online = true
		r.Walk = map[string]any{"enabled": true, "mapid": 6, "state": "walk"}
		r.Ghost = map[string]any{"done": 50, "limit": 50, "enabled": false, "count_date": countDay}
		r.DoubleClaimDate = claimDay
	})
}

// lastAction 读运行态"上一轮动作"。
func (e *rotEnv) lastAction() string {
	rt := e.a.rotationOf()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.lastAction
}

// ---------------------------------------------------------------- 预备队统计

func TestRotationReserveCount(t *testing.T) {
	env := newRotEnv(t)
	// 夹具池的区是 47.96.8.240:2300（不是测试区）→ 不计入，断言只针对本用例新增的号。
	env.addUsable(t, "r5101@xy3.com", 40) // 计入（离线+毕业）
	env.addUsable(t, "r5102@xy3.com", 40) // 计入
	env.addUsable(t, "r5103@xy3.com", 31) // 计入（边界：毕业线）
	env.addUsable(t, "r5104@xy3.com", 30) // 不计（未毕业）
	env.addUsable(t, "r5105@xy3.com", 40) // 不计（已移除）
	env.addUsable(t, "r5106@xy3.com", 40) // 不计（人工暂停）
	env.addUsable(t, "r5107@xy3.com", 40) // 不计（在线）
	env.st.MarkRemoved("r5105@xy3.com")
	env.st.MarkPaused("r5106@xy3.com")
	env.st.Update("r5107@xy3.com", func(r *state.Robot) { r.Online = true })

	if got := env.a.rotationReserveCount(); got != 3 {
		t.Fatalf("预备队应=3（毕业且离线且可拉起），实得 %d", got)
	}

	// 账号级等级兜底（区级 Level=0 时用 pa.Level）
	env.pool.Add([]string{"r5108@xy3.com"}, "pwd", "", "")
	env.pool.SetZoneState("r5108@xy3.com", rotZoneAddr, accounts.ZoneState{Verified: true, Usable: true})
	env.pool.SetFields("r5108@xy3.com", func(a *accounts.Account) { a.Level = 45 })
	if got := env.a.rotationReserveCount(); got != 4 {
		t.Fatalf("账号级等级兜底应计入（=4），实得 %d", got)
	}
}

// ---------------------------------------------------------------- 一轮 tick 全链路

func TestRotationTickOfflinesFullRoam(t *testing.T) {
	env := newRotEnv(t)
	env.cfg.RotationEnabled = true

	// 预备队：60 个可用离线毕业号（候选自己上线后 reserve=59 ≥ 50）。
	for i := 0; i < 60; i++ {
		env.addUsable(t, fmt.Sprintf("rot%04d@xy3.com", i), 40)
	}
	cand := "rot0000@xy3.com"
	t0 := time.Now()
	t1 := t0.Add(31 * time.Minute) // 第二轮：已在线 ≥30 分钟
	// count_date 对齐**真实时钟**（ghostDailyFull 用 todayKey()）；领双对齐**注入时刻**（Plan 的 today）。
	env.markFullRoam(t, cand, time.Now().Format("20060102"), t1.Format("20060102"))
	rb := testsupport.ConnectFakeRobot(t, env.ch)

	// 第一轮：首见在线 → 在线不足 30 分钟 → 不下线、不发命令。
	env.a.rotationTick(t0)
	if env.st.IsRemoved(cand) {
		t.Fatal("第一轮（刚见在线）不应下线")
	}
	if cmd := rb.TryReadCmd(200 * time.Millisecond); cmd != nil {
		t.Fatalf("第一轮不应发命令：%v", cmd)
	}

	// 第二轮（+31min）：满额 + 纯游荡 + 在线够长 → 走批量下线通路（robot_manage remove）。
	env.a.rotationTick(t1)
	cmd := rb.ReadCmd(t, 2*time.Second)
	if cmd["cmd"] != "robot_manage" || cmd["action"] != "remove" {
		t.Fatalf("应下发 robot_manage remove：%v", cmd)
	}
	accs, _ := cmd["accounts"].([]any)
	if len(accs) != 1 || accs[0] != cand {
		t.Fatalf("remove 名单应只含候选：%v", cmd["accounts"])
	}
	// 本地语义：MarkRemoved（防心跳复活）+ 删行。
	if !env.st.IsRemoved(cand) {
		t.Fatal("下线后应本地标记移除（防心跳复活）")
	}
	if env.st.Has(cand) {
		t.Fatal("下线后应删掉状态行")
	}
	// 名单落盘：data/rotation.json 有 1 条，且账号/日期正确。
	rt := env.a.rotationOf()
	if rt.store.Count() != 1 {
		t.Fatalf("名单应 1 条，实得 %d", rt.store.Count())
	}
	raw, err := os.ReadFile(filepath.Join(env.cfg.DataDir, "rotation.json"))
	if err != nil {
		t.Fatalf("名单文件应已落盘：%v", err)
	}
	var f struct {
		Entries []struct {
			Account string `json:"account"`
			Day     string `json:"day"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(raw, &f); err != nil || len(f.Entries) != 1 {
		t.Fatalf("名单文件不符：err=%v %+v", err, f)
	}
	if f.Entries[0].Account != cand || f.Entries[0].Day != t1.Format("20060102") {
		t.Fatalf("名单条目不符：%+v", f.Entries[0])
	}

	// 第三轮：号已不在快照（行已删）→ 不重复动作。
	env.a.rotationTick(t1.Add(time.Minute))
	if cmd := rb.TryReadCmd(200 * time.Millisecond); cmd != nil {
		t.Fatalf("已下线号不应被重复下发：%v", cmd)
	}
	if rt.store.Count() != 1 {
		t.Fatalf("名单不应重复登记：%d", rt.store.Count())
	}
}

// ---------------------------------------------------------------- 预备队保护

func TestRotationTickReserveGuardBlocks(t *testing.T) {
	env := newRotEnv(t)
	env.cfg.RotationEnabled = true
	// 只加 1 个号（候选自己）→ 预备队 0 < 50 → 整轮不下线（即使条件全满足）。
	cand := "solo@xy3.com"
	env.addUsable(t, cand, 40)
	t0 := time.Now()
	t1 := t0.Add(31 * time.Minute)
	env.markFullRoam(t, cand, time.Now().Format("20060102"), t1.Format("20060102"))
	rb := testsupport.ConnectFakeRobot(t, env.ch)

	env.a.rotationTick(t0)
	env.a.rotationTick(t1)
	if env.st.IsRemoved(cand) {
		t.Fatal("预备队不足时不应下线")
	}
	if cmd := rb.TryReadCmd(200 * time.Millisecond); cmd != nil {
		t.Fatalf("预备队不足时不应发命令：%v", cmd)
	}
	if act := env.lastAction(); !strings.Contains(act, "预备队") {
		t.Fatalf("上一轮动作应说明预备队阻断，实得 %q", act)
	}

	// 反证：把下限调低（=0 关闭保护）→ 同条件下线成功（证明阻断来自预备队闸，
	// 而不是别的判据恰好不满足）。
	env.cfg.RotationMinReserve = 0
	env.a.rotationTick(t1)
	if !env.st.IsRemoved(cand) {
		t.Fatal("关闭预备队保护后应能下线")
	}
}

// ---------------------------------------------------------------- 到龄回池

func TestRotationReturnAged(t *testing.T) {
	env := newRotEnv(t)
	now := time.Now()
	// 预置名单：old=25h 前下线（到龄）、fresh=1h 前（未到龄）。
	path := filepath.Join(env.cfg.DataDir, "rotation.json")
	body := fmt.Sprintf(`{"entries":[
	  {"account":"old@xy3.com","offlined_at":%d,"day":%q},
	  {"account":"fresh@xy3.com","offlined_at":%d,"day":%q}]}`,
		now.Add(-25*time.Hour).Unix(), now.Add(-25*time.Hour).Format("20060102"),
		now.Add(-1*time.Hour).Unix(), now.Format("20060102"))
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	env.st.MarkRemoved("old@xy3.com")
	env.st.MarkRemoved("fresh@xy3.com")

	// 开关默认关：回池仍应执行（自记账治理，防号永久卡在移除名单）。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	env.a.StartRotation(ctx) // 同步：载入名单 → ctx 已取消 → 立即返回

	env.a.rotationTick(now)
	if env.st.IsRemoved("old@xy3.com") {
		t.Fatal("到龄（25h ≥ 24h）号应解除移除")
	}
	if !env.st.IsRemoved("fresh@xy3.com") {
		t.Fatal("未到龄（1h < 24h）号不应被解除")
	}
	rt := env.a.rotationOf()
	if rt.store.Count() != 1 || rt.store.List()[0].Account != "fresh@xy3.com" {
		t.Fatalf("名单应只剩 fresh：%+v", rt.store.List())
	}
	// 盘上文件同步：新 Store 读回应只剩 1 条（原子落盘）。
	s2 := rotation.NewStore(path)
	if n, err := s2.Load(); err != nil || n != 1 {
		t.Fatalf("回池后落盘应剩 1 条：n=%d err=%v", n, err)
	}
	// 再跑一轮：幂等（未到龄号仍保持移除，不被误伤）。
	env.a.rotationTick(now)
	if !env.st.IsRemoved("fresh@xy3.com") {
		t.Fatal("第二轮不该把未到龄号解除")
	}
}

// ---------------------------------------------------------------- 壳层取数反证

// 壳层必须按心跳口径判"今日已满"（done≥limit 且 count_date=今日）：未满的号不能被轮换。
func TestRotationTickSkipsNotFullGhost(t *testing.T) {
	env := newRotEnv(t)
	env.cfg.RotationEnabled = true
	for i := 0; i < 60; i++ {
		env.addUsable(t, fmt.Sprintf("nf%04d@xy3.com", i), 40)
	}
	cand := "nf0000@xy3.com"
	t0 := time.Now()
	t1 := t0.Add(31 * time.Minute)
	env.markFullRoam(t, cand, time.Now().Format("20060102"), t1.Format("20060102"))
	// 改回未满：done 49/50
	env.st.Update(cand, func(r *state.Robot) {
		r.Ghost = map[string]any{"done": 49, "limit": 50, "enabled": false, "count_date": time.Now().Format("20060102")}
	})
	rb := testsupport.ConnectFakeRobot(t, env.ch)

	env.a.rotationTick(t0)
	env.a.rotationTick(t1)
	if env.st.IsRemoved(cand) {
		t.Fatal("抓鬼未满不应轮换（壳层必须读心跳满额口径）")
	}
	if cmd := rb.TryReadCmd(200 * time.Millisecond); cmd != nil {
		t.Fatalf("未满额号不应下发 remove：%v", cmd)
	}
}

// ---------------------------------------------------------------- 重登处理中

func TestRotationReghostSetSkipsPending(t *testing.T) {
	env := newRotEnv(t)
	runner := reghost.New(reghost.Deps{})
	runner.Request("rg@xy3.com", "单测")
	env.a.Reghost = runner
	set := env.a.rotationReghostSet()
	if !set["rg@xy3.com"] {
		t.Fatalf("等待离线的号应算处理中：%v", set)
	}
	if set["other@xy3.com"] {
		t.Fatalf("无关号不应命中：%v", set)
	}
}
