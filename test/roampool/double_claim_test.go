// 2026-09-23 有领双的必须优先抓鬼（用户口径）：今日已领双倍的号 ——
//   · 不转游荡（PickExcess 跳过）：双倍有时长，从抓鬼拉去游荡 = 把加成浪费掉；
//   · 在游荡的优先回收（PickReclaim 置顶）：先把它们收回任务池去抓鬼。
//
// 口径：DoubleClaimDate 为 YYYYMMDD（机器人端心跳上报），只有"今天"才算；
// 旧日期（跨日残留）不生效（state.Robot.DoubleClaimedToday 判定）。
package roampool_test

import (
	"testing"
	"time"

	"zyctrlcenter/internal/services/roampool"
	"zyctrlcenter/internal/state"
)

func doubleToday(r state.Robot) state.Robot {
	r.DoubleClaimDate = time.Now().Format("20060102")
	return r
}

func TestPickExcessSkipsDoubleClaimed(t *testing.T) {
	robots := []state.Robot{
		ghostAt("g1", "WAIT_GHOST"),
		doubleToday(ghostAt("g2", "READY")), // 今日已领双倍 → 不转游荡
		ghostAt("g3", "IDLE"),
	}
	if got := roampool.PickExcess(robots, 10); join(got) != "g1,g3" {
		t.Fatalf("领双号不该被转游荡（应只挑 g1,g3），实际 %s", join(got))
	}
}

func TestPickReclaimPrioritizesDoubleClaimed(t *testing.T) {
	// z9 所在图人最少，但今日已领双倍 → 仍应被**优先回收**去抓鬼
	robots := []state.Robot{
		walking("a1", 10), walking("a2", 10),
		doubleToday(walking("z9", 24)),
	}
	if got := roampool.PickReclaim(robots, 1, nil); join(got) != "z9" {
		t.Fatalf("今日领双的游荡号应优先回收，实际 %s", join(got))
	}
	// 取 2 个：领双号 + 人多图的号（输出按挑中顺序：先置顶的领双号）
	if got := roampool.PickReclaim(robots, 2, nil); join(got) != "z9,a1" {
		t.Fatalf("第 2 个应回到「人多图优先」的旧口径，实际 %s", join(got))
	}
}

func TestDoubleClaimStaleDateIgnored(t *testing.T) {
	// 昨天领的（跨日残留）不该再影响调度
	stale := walking("z9", 24)
	stale.DoubleClaimDate = time.Now().AddDate(0, 0, -1).Format("20060102")
	robots := []state.Robot{walking("a1", 10), walking("a2", 10), stale}
	if got := roampool.PickReclaim(robots, 1, nil); join(got) != "a1" {
		t.Fatalf("旧日期不该置顶（应仍按人多图优先取 a1），实际 %s", join(got))
	}
	if got := roampool.PickExcess([]state.Robot{ghostAt("g1", "READY"), ghostAt("g2", "READY"), func() state.Robot {
		r := ghostAt("g3", "READY")
		r.DoubleClaimDate = time.Now().AddDate(0, 0, -1).Format("20060102")
		return r
	}()}, 10); join(got) != "g1,g2,g3" {
		t.Fatalf("旧日期不该拦超编收敛，实际 %s", join(got))
	}
}
