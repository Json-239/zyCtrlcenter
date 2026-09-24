// 2026-09-24 游荡"任务图降权"用例（用户口径：任务链图机器人已很多 → 游荡"降权少去，
// 具体根据当前图数量动态"）：
//
//   - 纯函数：MapLoadsAs 的任务图虚拟负载（有效负载 = 实际人数 + 偏移）；
//     PickReclaimAs 优先回收任务图上的游荡号（别滞留在任务图）；
//   - 一轮 Tick：BalanceAssign 选图自动避开任务图（人少时仍可能去）；偏移 0=默认 8、
//     <0 关闭；配置 task_maps 完全覆盖、Deps.TaskMaps 热读与内置兜底集合合并。
//
// 造数用 44/45（不在内置兜底集合 defaultTaskMaps 里）—— 由 Deps.TaskMaps/配置显式指定，
// 断言不会被兜底集合干扰。
package roampool_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zyctrlcenter/internal/services/roampool"
	"zyctrlcenter/internal/state"
)

// MapLoadsAs：任务图的 Count 加虚拟偏移；bias=0 等价 MapLoads。
func TestMapLoadsAsTaskBias(t *testing.T) {
	robots := []state.Robot{online("a1", 44), online("a2", 44), online("a3", 45)}
	loads := roampool.MapLoadsAs(robots, []int{44, 45}, map[int]bool{44: true}, 8)
	if len(loads) != 2 {
		t.Fatalf("应有 2 行，实际 %v", loads)
	}
	if loads[0].MapID != 44 || loads[0].Count != 2+8 {
		t.Fatalf("任务图 44：2 人 + 偏移 8 = 10，实际 %+v", loads[0])
	}
	if loads[1].MapID != 45 || loads[1].Count != 1 {
		t.Fatalf("非任务图 45：原样 1，实际 %+v", loads[1])
	}
	// bias=0 → 与 MapLoads 完全一致（不降权）
	plain := roampool.MapLoads(robots, []int{44, 45})
	if got := roampool.MapLoadsAs(robots, []int{44, 45}, map[int]bool{44: true}, 0); len(got) != 2 ||
		got[0].Count != plain[0].Count || got[1].Count != plain[1].Count {
		t.Fatalf("bias=0 应与 MapLoads 等价: got=%v plain=%v", got, plain)
	}
}

// PickReclaimAs：任务图有效负载更高 → 其上的游荡号更早回收（"别滞留在任务图"）。
// 对照：无偏移时先回收人多的图（图45 有 2 个游荡号 > 图44 的 1 个）。
func TestPickReclaimAsPrefersTaskMap(t *testing.T) {
	robots := []state.Robot{walking("w44", 44), walking("w45a", 45), walking("w45b", 45)}
	if got := roampool.PickReclaim(robots, 1, nil); join(got) != "w45a" {
		t.Fatalf("无偏移：应回收人多的图45（账号序）: %v", got)
	}
	// 图44 是任务图：1+8=9 > 图45 的 2 → 先回收图44 的
	if got := roampool.PickReclaimAs(robots, 1, nil, map[int]bool{44: true}, 8); join(got) != "w44" {
		t.Fatalf("有偏移：任务图44 的有效负载更高，应优先回收: %v", got)
	}
	// 并列/资格闸口径不变：eligible 拦下的号不回收
	if got := roampool.PickReclaimAs(robots, 2, func(r state.Robot) bool { return r.Account != "w44" },
		map[int]bool{44: true}, 8); join(got) != "w45a,w45b" {
		t.Fatalf("资格闸优先于降权评分（w44 被拦）: %v", got)
	}
}

// 一轮 Tick：默认偏移 8 时补位全去非任务图（任务图44 只有 1 人，但有效 1+8=9 > 图45 的 2..
// 7）；对照（bias<0 关闭）：图44 人最少 → 会先被派 —— 证明是降权在起作用（同造数两态对照）。
func TestTickTaskMapBiasAvoidsTaskMap(t *testing.T) {
	robots := []state.Robot{
		online("m44a", 44),
		online("m45a", 45), online("m45b", 45),
		online("i1", 99), online("i2", 99), online("i3", 99), online("i4", 99), online("i5", 99),
	}
	f := &fakeDeps{robots: robots, maps: []int{44, 45}, taskMaps: []int{44}}
	k := newKeeper(t, f, nil) // 默认 task_map_bias=8
	if !k.Tick(time.Now()) {
		t.Fatal("缺补位应下发")
	}
	if len(f.disp) == 0 {
		t.Fatalf("应有派发，实际 %v", f.disp)
	}
	for _, d := range f.disp {
		if d.mapid != 45 {
			t.Fatalf("任务图44 降权 +8 后不应被派到（有效 9 > 图45 的 2..7），实际 %v", f.disp)
		}
	}
	if !containsLog(f.logs, "任务图降权 +8") {
		t.Fatalf("动作文案应说明任务图降权偏移，实际 %v", f.logs)
	}

	// 对照：关闭降权（bias=-1 → Normalize 夹为 0）→ 图44 人最少 → 会先被派
	f2 := &fakeDeps{robots: robots, maps: []int{44, 45}, taskMaps: []int{44}}
	k2 := newKeeper(t, f2, func(c *roampool.Config) { c.TaskMapBias = -1 })
	if !k2.Tick(time.Now()) {
		t.Fatal("对照：应下发")
	}
	saw44 := false
	for _, d := range f2.disp {
		if d.mapid == 44 {
			saw44 = true
		}
	}
	if !saw44 {
		t.Fatalf("关闭降权后任务图应照常被派（对照），实际 %v", f2.disp)
	}
	if k2.Status().TaskMapBias != 0 || k2.Status().TaskMapCount != 0 {
		t.Fatalf("关闭降权：Status 应显示 bias=0 / 0 张，实际 %d/%d",
			k2.Status().TaskMapBias, k2.Status().TaskMapCount)
	}
}

// 配置 task_maps 非空 = 完全覆盖（连内置兜底集合也不看）：只降权配置里那几张图。
func TestTickTaskMapBiasConfigOverride(t *testing.T) {
	robots := []state.Robot{
		online("m45a", 45), online("m45b", 45), online("m45c", 45), online("m45d", 45),
		online("m10a", 10), // 图10 在内置兜底集合里，但配置覆盖后不再降权
		online("i1", 99), online("i2", 99),
	}
	f := &fakeDeps{robots: robots, maps: []int{10, 45}, taskMaps: []int{10}} // 热读说 10 是任务图
	k := newKeeper(t, f, func(c *roampool.Config) {
		c.TaskMaps = []int{45} // 显式覆盖：只有 45 降权（10 不再降权）
	})
	if !k.Tick(time.Now()) {
		t.Fatal("应下发")
	}
	for _, d := range f.disp {
		if d.mapid != 10 {
			t.Fatalf("覆盖后只降权图45 → 补位应去图10（有效 1 < 4+8），实际 %v", f.disp)
		}
	}
	if n := k.Status().TaskMapCount; n != 1 {
		t.Fatalf("覆盖后降权集合应只有 1 张，实际 %d", n)
	}
}

// 参数口径：默认 8 / 负数关闭 / 超上限校验拒绝；热读为空时兜底集合仍生效（非空）。
func TestTaskMapBiasConfigNormalize(t *testing.T) {
	def := roampool.DefaultConfig()
	def.Enabled = true
	if got := def.Normalize().TaskMapBias; got != roampool.DefaultTaskMapBias {
		t.Fatalf("默认配置 TaskMapBias 应为 %d，实际 %d", roampool.DefaultTaskMapBias, got)
	}
	off := def
	off.TaskMapBias = -1
	if got := off.Normalize().TaskMapBias; got != 0 {
		t.Fatalf("负数应夹为 0（关闭降权），实际 %d", got)
	}
	over := def
	over.TaskMapBias = roampool.MaxTaskMapBias + 1
	if err := over.Validate(); err == nil {
		t.Fatal("超过上限应校验失败")
	}
	bad := def
	bad.TaskMaps = []int{0}
	if err := bad.Validate(); err == nil {
		t.Fatal("task_maps 里带非法图号应校验失败")
	}
	// 热读为空：兜底集合仍生效（降权不依赖链数据可用性）
	f := &fakeDeps{robots: []state.Robot{online("x", 10), online("y", 26), online("i", 99)},
		maps: []int{10, 26}} // 10/26 都在内置兜底集合 defaultTaskMaps 里
	k := roampool.New(filepath.Join(t.TempDir(), "roampool.json"), f.deps())
	cfg := roampool.DefaultConfig()
	cfg.Enabled = true
	if err := k.SetConfig(cfg); err != nil {
		t.Fatalf("设置参数失败: %v", err)
	}
	if n := k.Status().TaskMapCount; n <= 0 {
		t.Fatalf("热读为空时兜底任务图集合应非空（降权不依赖链数据），实际 %d", n)
	}
}

func containsLog(logs []string, sub string) bool {
	for _, s := range logs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
