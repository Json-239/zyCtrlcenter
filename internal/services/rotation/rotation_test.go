// 号池轮换（rotation 最小版，2026-10-12）核心单测（纯逻辑）：
// 候选过滤反例（满额/战斗/孵化/组队/私池/熔断/在线<30min/领双未用…）/
// 限速（30/h、每轮 ≤10、跨小时重置）/ 预备队保护 / 一轮判决（Plan）/
// 名单持久化（存读一致、到龄回池）/ 配置夹取 / 摘要。
package rotation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zyctrlcenter/internal/state"
)

// ---------------------------------------------------------------- 造数

const testToday = "20261012"

// pureRoam 一个"满额 + 纯游荡"的基准号（各反例从这里变异）。
func pureRoam() state.Robot {
	return state.Robot{
		Account: "r1@xy3.com",
		Online:  true,
		Level:   40,
		Walk:    map[string]any{"enabled": true, "mapid": 6, "state": "walk"},
		Ghost: map[string]any{"done": 50, "limit": 50, "enabled": false,
			"count_date": testToday},
		DoubleClaimDate: testToday,
	}
}

// baseFacts 一个"全过"的 Facts。
func baseFacts() Facts {
	return Facts{GhostFull: true, OnlineFor: 31 * time.Minute}
}

func mustPass(t *testing.T, r state.Robot, f Facts, label string) {
	t.Helper()
	if ok, why := Candidate(r, f, testToday); !ok {
		t.Fatalf("%s：应通过，实得 false（%s）", label, why)
	}
}

func mustReject(t *testing.T, r state.Robot, f Facts, wantWhy, label string) {
	t.Helper()
	ok, why := Candidate(r, f, testToday)
	if ok {
		t.Fatalf("%s：应被拒绝，实得通过", label)
	}
	if wantWhy != "" && !strings.Contains(why, wantWhy) {
		t.Fatalf("%s：拒绝原因应含 %q，实得 %q", label, wantWhy, why)
	}
}

// ---------------------------------------------------------------- 候选过滤

func TestCandidateRejects(t *testing.T) {
	// 基准正例
	mustPass(t, pureRoam(), baseFacts(), "基准（满额+纯游荡+在线31min+领双已用）")

	// —— 反例 1：未满额（心跳 done 49/50 → 壳层据此给出 GhostFull=false；满额判据的
	// 心跳/满额表换算在壳层 ghostDailyFull，本包只消费布尔）——
	r := pureRoam()
	r.Ghost = map[string]any{"done": 49, "limit": 50, "enabled": false, "count_date": testToday}
	mustReject(t, r, Facts{GhostFull: false, OnlineFor: 31 * time.Minute}, "今日抓鬼未满", "抓鬼未满额（49/50）")
	// 满额但 count_date 不是今天（跨夜旧快照）→ 壳层判 GhostFull=false → 拒绝
	mustReject(t, pureRoam(), Facts{GhostFull: false, OnlineFor: 31 * time.Minute}, "今日抓鬼未满", "跨夜旧满额快照")

	// —— 反例 2：战斗中 ——
	r = pureRoam()
	r.Fight = true
	mustReject(t, r, baseFacts(), "战斗中", "战斗中")

	// —— 反例 3：孵化中（walk.enabled 也在，但孵化不算纯游荡）——
	r = pureRoam()
	r.Hatch = map[string]any{"active": true, "kind": "egg"}
	mustReject(t, r, baseFacts(), "孵化中", "孵化中")
	// 已孵出（hatched=true）→ 不算孵化 → 仍可轮换
	r = pureRoam()
	r.Hatch = map[string]any{"active": true, "hatched": true}
	mustPass(t, r, baseFacts(), "孵化已收工")

	// —— 反例 4：组队（队长 / 队员两种心跳块）——
	r = pureRoam()
	r.Team = map[string]any{"role": "captain", "captain_role_id": 1001, "members": []any{"a", "b"}}
	mustReject(t, r, baseFacts(), "在队伍中", "队长")
	r = pureRoam()
	r.Team = map[string]any{"role": "member", "captain_role_id": 1001}
	mustReject(t, r, baseFacts(), "在队伍中", "队员")

	// —— 反例 5：私人池 ——
	f := baseFacts()
	f.Personal = true
	mustReject(t, pureRoam(), f, "私人池", "私人池")

	// —— 反例 6：当日卡死熔断 ——
	r = pureRoam()
	r.RestoreCapped = true
	mustReject(t, r, baseFacts(), "当日卡死熔断", "熔断")

	// —— 反例 7：在线不足 30 分钟（边界 29m59s 拒 / 30m 整过）——
	f = baseFacts()
	f.OnlineFor = MinOnline - time.Second
	mustReject(t, pureRoam(), f, "在线不足", "在线 29m59s")
	f = baseFacts()
	f.OnlineFor = MinOnline
	mustPass(t, pureRoam(), f, "在线 30m 整（边界）")

	// —— 反例 8：领双未用（心跳有日期值且非今日）——
	r = pureRoam()
	r.DoubleClaimDate = "20261011"
	mustReject(t, r, baseFacts(), "领双", "领双日期是昨天")
	// 字段缺失（旧版机器人）→ 放行
	r = pureRoam()
	r.DoubleClaimDate = ""
	mustPass(t, r, baseFacts(), "领双字段缺失（旧版上报）")
	// 今日已领 → 放行
	r = pureRoam()
	r.DoubleClaimDate = testToday
	mustPass(t, r, baseFacts(), "今日已领双")

	// —— 反例 9：不在游荡（无 walk 块 / enabled=false）——
	r = pureRoam()
	r.Walk = nil
	mustReject(t, r, baseFacts(), "不在游荡", "无 walk 块")
	r = pureRoam()
	r.Walk = map[string]any{"enabled": false}
	mustReject(t, r, baseFacts(), "不在游荡", "walk.enabled=false")

	// —— 反例 10：抓鬼会话仍活跃 ——
	r = pureRoam()
	r.Ghost = map[string]any{"done": 50, "limit": 50, "enabled": true, "count_date": testToday}
	mustReject(t, r, baseFacts(), "抓鬼会话活跃", "抓鬼会话 active")

	// —— 反例 11：离线 / 人工暂停 / 已在移除名单 / 重登处理中 ——
	r = pureRoam()
	r.Online = false
	mustReject(t, r, baseFacts(), "离线", "离线")
	r = pureRoam()
	r.Paused = true
	mustReject(t, r, baseFacts(), "人工暂停", "人工暂停")
	f = baseFacts()
	f.Removed = true
	mustReject(t, pureRoam(), f, "已在移除名单", "已在移除名单")
	f = baseFacts()
	f.Reghosting = true
	mustReject(t, pureRoam(), f, "自动重登处理中", "自动重登处理中")
}

// ---------------------------------------------------------------- 限速

func TestHourQuota(t *testing.T) {
	var q HourQuota
	t0 := time.Date(2026, 10, 12, 10, 0, 0, 0, time.Local)

	// 每轮 ≤ MaxPerTick(10)：30/h、未被用过 → 10
	if got := q.Plan(t0, 30, MaxPerTick); got != 10 {
		t.Fatalf("首轮额度应=10，实得 %d", got)
	}
	// Plan 只读：重复调用不变
	if got := q.Plan(t0, 30, MaxPerTick); got != 10 {
		t.Fatalf("Plan 应只读（重复调用仍=10），实得 %d", got)
	}
	// Commit 10 → 本小时剩 20 → 仍受每轮上限 10
	q.Commit(t0, 10)
	if got := q.Plan(t0, 30, MaxPerTick); got != 10 {
		t.Fatalf("已用 10 后额度应=10，实得 %d", got)
	}
	// 再用 20（累计 30）→ 本小时额度用尽 → 0
	q.Commit(t0, 20)
	if got := q.Plan(t0, 30, MaxPerTick); got != 0 {
		t.Fatalf("本小时用尽应=0，实得 %d", got)
	}
	if q.Used(t0) != 30 {
		t.Fatalf("Used 应=30，实得 %d", q.Used(t0))
	}
	// 跨小时重置：11:00 → 满额 10；Used 归 0
	t1 := t0.Add(time.Hour)
	if got := q.Plan(t1, 30, MaxPerTick); got != 10 {
		t.Fatalf("跨小时应重置额度=10，实得 %d", got)
	}
	if q.Used(t1) != 0 {
		t.Fatalf("跨小时 Used 应=0，实得 %d", q.Used(t1))
	}
	// 兜底：perHour < perTick 时按剩余给（3/h → 3）
	if got := q.Plan(t1, 3, MaxPerTick); got != 3 {
		t.Fatalf("3/h 首轮额度应=3，实得 %d", got)
	}
	// Commit 跨小时先重置：11:30 记 2 笔 → Used=2（不会叠在上一小时上）
	q.Commit(t1, 2)
	if q.Used(t1) != 2 {
		t.Fatalf("跨小时 Commit 后 Used 应=2，实得 %d", q.Used(t1))
	}
	// perHour<=0 → 0（关闸）
	if got := q.Plan(t1, 0, MaxPerTick); got != 0 {
		t.Fatalf("perHour=0 应=0，实得 %d", got)
	}
}

// ---------------------------------------------------------------- 一轮判决（Plan）

// fakeSnap 造 n 个"满额纯游荡"的号（账号升序）。
func fakeSnap(n int) []state.Robot {
	out := make([]state.Robot, 0, n)
	for i := 0; i < n; i++ {
		r := pureRoam()
		r.Account = string(rune('a'+i)) + "@xy3.com"
		out = append(out, r)
	}
	return out
}

func allPassFacts(acc string) Facts { return baseFacts() }

func TestPlanReserveGuard(t *testing.T) {
	now := time.Date(2026, 10, 12, 10, 0, 0, 0, time.Local)
	cfg := DefaultConfig()
	cfg.Enabled = true

	// 预备队 49 < 下限 50 → 整轮不下线（Blocked 带原因，无候选）
	var q HourQuota
	res := Plan(now, cfg, fakeSnap(3), allPassFacts, testToday, 49, &q)
	if res.Blocked == "" || len(res.Batch) != 0 || res.Eligible != 0 {
		t.Fatalf("预备队不足应整轮阻断：%+v", res)
	}
	if !strings.Contains(res.Blocked, "预备队") {
		t.Fatalf("阻断原因应说明预备队，实得 %q", res.Blocked)
	}
	// 预备队 = 下限（50）→ 放行
	res = Plan(now, cfg, fakeSnap(3), allPassFacts, testToday, 50, &q)
	if res.Blocked != "" || len(res.Batch) != 3 {
		t.Fatalf("预备队=50 应放行 3 个：%+v", res)
	}
	// MinReserve=0 → 显式关闭保护（预备队 0 也放行）
	cfg2 := cfg
	cfg2.MinReserve = 0
	res = Plan(now, cfg2, fakeSnap(2), allPassFacts, testToday, 0, &q)
	if res.Blocked != "" || len(res.Batch) != 2 {
		t.Fatalf("MinReserve=0 应关闭保护：%+v", res)
	}
}

func TestPlanQuotaCutAndQueued(t *testing.T) {
	now := time.Date(2026, 10, 12, 10, 0, 0, 0, time.Local)
	cfg := DefaultConfig()
	cfg.Enabled = true

	// 12 个候选、每轮上限 10 → 下线 10、排队 2
	var q HourQuota
	res := Plan(now, cfg, fakeSnap(12), allPassFacts, testToday, 100, &q)
	if res.Eligible != 12 || len(res.Batch) != 10 || res.Queued != 2 {
		t.Fatalf("应 12 候选→10 下线+2 排队：%+v", res)
	}
	if res.Batch[0] != "a@xy3.com" || res.Batch[9] != "j@xy3.com" {
		t.Fatalf("批内应保持快照升序：%v", res.Batch)
	}
	// 记账 10（用掉本小时 1/3）→ 下轮仍 10
	q.Commit(now, 10)
	res = Plan(now, cfg, fakeSnap(12), allPassFacts, testToday, 100, &q)
	if len(res.Batch) != 10 {
		t.Fatalf("已用 10 后仍应下线 10：%+v", res)
	}
	q.Commit(now, 10)
	// 已用 20 → 本小时剩 10 → 仍 10；再用 10 → 用尽
	res = Plan(now, cfg, fakeSnap(12), allPassFacts, testToday, 100, &q)
	if len(res.Batch) != 10 {
		t.Fatalf("剩 10 额度应下线 10：%+v", res)
	}
	q.Commit(now, 10)
	res = Plan(now, cfg, fakeSnap(12), allPassFacts, testToday, 100, &q)
	if len(res.Batch) != 0 || res.Queued != 12 {
		t.Fatalf("本小时额度用尽应全部排队：%+v", res)
	}
	// 跨小时（11:00）→ 恢复
	next := now.Add(time.Hour)
	res = Plan(next, cfg, fakeSnap(12), allPassFacts, testToday, 100, &q)
	if len(res.Batch) != 10 {
		t.Fatalf("跨小时应恢复额度：%+v", res)
	}
	// perHour=3 → 每轮只 3
	var q2 HourQuota
	cfg3 := cfg
	cfg3.PerHour = 3
	res = Plan(now, cfg3, fakeSnap(12), allPassFacts, testToday, 100, &q2)
	if len(res.Batch) != 3 || res.Queued != 9 {
		t.Fatalf("perHour=3 应下线 3+排队 9：%+v", res)
	}
}

func TestPlanReasonsCollected(t *testing.T) {
	now := time.Date(2026, 10, 12, 10, 0, 0, 0, time.Local)
	cfg := DefaultConfig()
	var q HourQuota
	snap := fakeSnap(2)
	snap[1].Fight = true
	res := Plan(now, cfg, snap, allPassFacts, testToday, 100, &q)
	if res.Reasons["b@xy3.com"] != "战斗中" {
		t.Fatalf("被过滤号应带原因：%v", res.Reasons)
	}
	if _, ok := res.Reasons["a@xy3.com"]; ok {
		t.Fatalf("通过的号不应出现在 Reasons：%v", res.Reasons)
	}
	if len(res.Batch) != 1 || res.Batch[0] != "a@xy3.com" {
		t.Fatalf("应只有 a 下线：%+v", res)
	}
}

// ---------------------------------------------------------------- 名单持久化

func TestStorePersistAndAged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rotation.json")
	t0 := time.Date(2026, 10, 12, 10, 0, 0, 0, time.Local)

	// 文件不存在 → 空名单、无错
	s := NewStore(path)
	if n, err := s.Load(); n != 0 || err != nil {
		t.Fatalf("首次 Load 应空且无错：n=%d err=%v", n, err)
	}
	// 登记 3 个并落盘
	s.Add([]string{"a@xy3.com", "b@xy3.com", "c@xy3.com"}, t0)
	if s.Count() != 3 {
		t.Fatalf("Count 应=3：%d", s.Count())
	}
	if s.CountDay("20261012") != 3 || s.CountDay("20261011") != 0 {
		t.Fatalf("CountDay 不符：today=%d other=%d", s.CountDay("20261012"), s.CountDay("20261011"))
	}
	if err := s.Save(); err != nil {
		t.Fatalf("Save 失败：%v", err)
	}
	// 存读一致（新 Store 从盘里读回）
	s2 := NewStore(path)
	n, err := s2.Load()
	if err != nil || n != 3 {
		t.Fatalf("Load 应读回 3 条：n=%d err=%v", n, err)
	}
	got := s2.List()
	if len(got) != 3 || got[0].Account != "a@xy3.com" || got[2].Account != "c@xy3.com" {
		t.Fatalf("读回清单不符：%+v", got)
	}
	if got[0].OfflinedAt != t0.Unix() || got[0].Day != "20261012" {
		t.Fatalf("条目字段不符：%+v", got[0])
	}

	// 到龄：未满 24h 不到龄；满 24h 全到龄（升序）
	if due := s2.Aged(t0.Add(23*time.Hour), 24*time.Hour); len(due) != 0 {
		t.Fatalf("未满 24h 不应到龄：%v", due)
	}
	if due := s2.Aged(t0.Add(25*time.Hour), 24*time.Hour); len(due) != 3 ||
		due[0] != "a@xy3.com" || due[2] != "c@xy3.com" {
		t.Fatalf("满 24h 应全部到龄且升序：%v", due)
	}
	// 边界：正好 24h → 到龄（>= 语义）
	if due := s2.Aged(t0.Add(24*time.Hour), 24*time.Hour); len(due) != 3 {
		t.Fatalf("正好 24h 应到龄：%v", due)
	}

	// 回池：Remove 幂等；再落盘读回一致
	if s2.Remove([]string{"a@xy3.com", "not-exist@xy3.com"}) != 1 {
		t.Fatalf("Remove 应只删掉存在的 1 个")
	}
	if err := s2.Save(); err != nil {
		t.Fatalf("回池后 Save 失败：%v", err)
	}
	s3 := NewStore(path)
	if n, _ := s3.Load(); n != 2 {
		t.Fatalf("回池后应剩 2 条：%d", n)
	}
}

func TestStoreLoadCorrupt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rotation.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewStore(path)
	if _, err := s.Load(); err == nil {
		t.Fatal("坏文件应返回 error（壳层按空名单继续）")
	}
	// 坏文件不影响后续使用（内存态可写）
	s.Add([]string{"x@xy3.com"}, time.Now())
	if s.Count() != 1 {
		t.Fatalf("坏文件后内存名单应可用：%d", s.Count())
	}
}

func TestStoreFileFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rotation.json")
	s := NewStore(path)
	now := time.Date(2026, 10, 12, 10, 0, 0, 0, time.Local)
	s.Add([]string{"b@xy3.com", "a@xy3.com"}, now)
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Comment string  `json:"_comment"`
		Entries []Entry `json:"entries"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("落盘应为合法 JSON：%v", err)
	}
	if f.Comment == "" || len(f.Entries) != 2 {
		t.Fatalf("落盘格式不符：%+v", f)
	}
	if strings.Index(string(raw), `"a@xy3.com"`) > strings.Index(string(raw), `"b@xy3.com"`) {
		t.Fatalf("落盘条目应升序：%s", string(raw))
	}
	// path 为空 = 纯内存（不落盘、不报错）
	mem := NewStore("")
	mem.Add([]string{"z@xy3.com"}, now)
	if err := mem.Save(); err != nil {
		t.Fatalf("纯内存 Save 应 no-op 无错：%v", err)
	}
	if n, err := mem.Load(); n != 1 || err != nil {
		t.Fatalf("纯内存 Load 应保持内存态（1 条）：n=%d err=%v", n, err)
	}
}

// ---------------------------------------------------------------- 配置/摘要

func TestConfigNormalize(t *testing.T) {
	d := DefaultConfig()
	if d.Enabled || d.PerHour != 30 || d.MinReserve != 50 || d.KeepHours != 24 {
		t.Fatalf("默认值不符：%+v", d)
	}
	c := Config{PerHour: -1, MinReserve: -5, KeepHours: 0}.Normalize()
	if c.PerHour != 30 || c.MinReserve != 0 || c.KeepHours != 24 {
		t.Fatalf("归零应回默认/0：%+v", c)
	}
	c = Config{PerHour: 99999, MinReserve: 99999, KeepHours: 99999}.Normalize()
	if c.PerHour != MaxPerHour || c.MinReserve != MaxMinReserve || c.KeepHours != MaxKeepHours {
		t.Fatalf("超上限应夹取：%+v", c)
	}
}

func TestSummaryAndReasons(t *testing.T) {
	if got := Summary([]string{"a", "b"}, 10); got != "a、b" {
		t.Fatalf("Summary 不符：%q", got)
	}
	got := Summary([]string{"a", "b", "c"}, 2)
	if !strings.Contains(got, "…(+1)") {
		t.Fatalf("超长 Summary 应折叠：%q", got)
	}
	rs := map[string]string{"a": "战斗中", "b": "战斗中", "c": "离线"}
	got = ReasonSummary(rs, 5)
	if !strings.Contains(got, "战斗中×2") || !strings.Contains(got, "离线×1") {
		t.Fatalf("ReasonSummary 不符：%q", got)
	}
	// 排序：计数降序在前
	if strings.Index(got, "战斗中") > strings.Index(got, "离线") {
		t.Fatalf("ReasonSummary 应按计数降序：%q", got)
	}
	if ReasonSummary(nil, 5) != "" {
		t.Fatalf("空 Reasons 应返回空串")
	}
}
